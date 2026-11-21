package stickers

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// Errors of the ZIP export.
var (
	// ErrArchiveUnavailable: no file downloader is wired.
	ErrArchiveUnavailable = errors.New("sticker ZIP export is not available")
	// ErrArchiveTooLarge: the sticker files or the archive exceed the caps.
	ErrArchiveTooLarge = errors.New("the sticker set is too large for a ZIP export")
	// ErrArchiveTimeout: the whole export exceeded its deadline.
	ErrArchiveTimeout = errors.New("the ZIP export took too long")
	// ErrUnsafeFileName: a sticker identity cannot be used as an archive name.
	ErrUnsafeFileName = errors.New("a sticker identity is not a safe archive file name")
	// ErrFileTooLarge is returned by a FileDownloader for an oversized file.
	ErrFileTooLarge = errors.New("sticker file is too large")
)

const (
	// archiveTimeout bounds the whole operation: fetch, downloads, assembly.
	archiveTimeout = 2 * time.Minute
	// archiveFileTimeout bounds one file download.
	archiveFileTimeout = 20 * time.Second
)

// Download failure reasons carried by DownloadError.
const (
	ReasonTooLarge    = "too large"
	ReasonUnavailable = "could not be downloaded"
	ReasonTimedOut    = "timed out"
	ReasonEmpty       = "is empty"
)

// DownloadError names the sticker whose file made the export fail. It never
// carries a transport cause (request URLs embed the bot token).
type DownloadError struct {
	FileUniqueID string
	Reason       string
}

func (e *DownloadError) Error() string {
	return fmt.Sprintf("sticker %s: file %s", e.FileUniqueID, e.Reason)
}

// FileDownloader fetches the bytes of one Telegram file by its current
// file_id, reading at most maxBytes. Implementations return ErrFileTooLarge
// for an oversized declared or actual size and otherwise only sanitized
// errors.
type FileDownloader interface {
	DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error)
}

var archiveUniqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ArchiveFilename names the exported ZIP of a set.
func ArchiveFilename(set string) string { return set + ".zip" }

// ArchiveEntryName is the in-archive path of a sticker file: the
// file_unique_id verbatim plus an extension from the format. Identities are
// never rewritten, so names stay in exact correspondence with the YAML.
func ArchiveEntryName(s RemoteSticker) (string, error) {
	if !archiveUniqueID.MatchString(s.FileUniqueID) {
		return "", ErrUnsafeFileName
	}
	var ext string
	switch s.Format {
	case FormatStatic:
		ext = ".webp"
	case FormatAnimated:
		ext = ".tgs"
	case FormatVideo:
		ext = ".webm"
	default:
		return "", ErrUnsafeFileName
	}

	return "stickers/" + s.FileUniqueID + ext, nil
}

// ExportArchive builds a ZIP with the set's YAML template and every available
// sticker file, from ONE getStickerSet snapshot. It is read-only (no store
// call, no catalog or index change) and all-or-nothing: any failure returns
// an error and no bytes. One deadline covers the fetch, the downloads and the
// assembly.
func (s *Service) ExportArchive(ctx context.Context, reference string) (filename string, data []byte, err error) {
	if s.files == nil {
		return "", nil, ErrArchiveUnavailable
	}
	name, err := ParseSetReference(reference)
	if err != nil {
		return "", nil, err
	}
	select {
	case s.archiveSlots <- struct{}{}:
		defer func() { <-s.archiveSlots }()
	default:
		return "", nil, ErrBusy
	}

	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	started := time.Now()

	remote, err := s.fetch(ctx, name)
	if err != nil {
		return "", nil, archiveCtxError(ctx, err)
	}
	yamlBytes, err := BuildTemplate(s.current(), remote)
	if err != nil {
		return "", nil, err
	}

	names := make([]string, len(remote.Stickers))
	seen := make(map[string]struct{}, len(names))
	for i, st := range remote.Stickers {
		entry, nameErr := ArchiveEntryName(st)
		if nameErr != nil {
			return "", nil, fmt.Errorf("%w: %q", nameErr, truncateRunesForError(st.FileUniqueID))
		}
		if _, dup := seen[entry]; dup {
			return "", nil, fmt.Errorf("%w: duplicate name", ErrUnsafeFileName)
		}
		seen[entry] = struct{}{}
		names[i] = entry
	}

	if err := ctx.Err(); err != nil {
		return "", nil, archiveCtxError(ctx, err)
	}
	files, err := s.downloadAll(ctx, remote.Stickers)
	if err != nil {
		return "", nil, archiveCtxError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return "", nil, archiveCtxError(ctx, err)
	}

	zipName := TemplateFilename(remote.Name)
	archive, err := assembleZip(ctx, zipName, yamlBytes, names, files)
	if err != nil {
		return "", nil, archiveCtxError(ctx, err)
	}
	s.logger.Info("sticker archive exported", "set", remote.Name, "stickers", len(remote.Stickers),
		"bytes", len(archive), "duration", time.Since(started).Round(time.Millisecond))

	return ArchiveFilename(remote.Name), archive, nil
}

// archiveCtxError maps a failure to the overall deadline or cancellation when
// that is the true cause.
func archiveCtxError(ctx context.Context, err error) error {
	switch cerr := ctx.Err(); {
	case errors.Is(cerr, context.DeadlineExceeded):
		return ErrArchiveTimeout
	case cerr != nil:
		return cerr
	}

	return err
}

func truncateRunesForError(s string) string {
	if r := []rune(s); len(r) > 24 {
		return string(r[:24]) + "…"
	}

	return s
}

// downloadAll fetches every file with at most ArchiveConcurrency workers.
// The first failure cancels the rest. Results are in sticker order.
func (s *Service) downloadAll(ctx context.Context, stickers []RemoteSticker) ([][]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([][]byte, len(stickers))
	var (
		total    atomic.Int64
		once     sync.Once
		firstErr error
	)
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}

	indexes := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(ArchiveConcurrency, len(stickers)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indexes {
				if ctx.Err() != nil {
					continue
				}
				data, err := s.downloadOne(ctx, stickers[i])
				if err != nil {
					fail(err)

					continue
				}
				if total.Add(int64(len(data))) > MaxArchiveDownloadBytes {
					fail(ErrArchiveTooLarge)

					continue
				}
				results[i] = data
			}
		}()
	}
feed:
	for i := range stickers {
		select {
		case indexes <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(indexes)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return results, nil
}

func (s *Service) downloadOne(ctx context.Context, st RemoteSticker) ([]byte, error) {
	fctx, cancel := context.WithTimeout(ctx, archiveFileTimeout)
	defer cancel()

	data, err := s.files.DownloadFile(fctx, st.FileID, MaxStickerFileBytes)
	switch {
	case errors.Is(err, ErrFileTooLarge):
		return nil, &DownloadError{FileUniqueID: st.FileUniqueID, Reason: ReasonTooLarge}
	case errors.Is(err, context.DeadlineExceeded):
		return nil, &DownloadError{FileUniqueID: st.FileUniqueID, Reason: ReasonTimedOut}
	case err != nil:
		return nil, &DownloadError{FileUniqueID: st.FileUniqueID, Reason: ReasonUnavailable}
	case len(data) == 0:
		return nil, &DownloadError{FileUniqueID: st.FileUniqueID, Reason: ReasonEmpty}
	case len(data) > MaxStickerFileBytes: // the real byte count, not the reported size
		return nil, &DownloadError{FileUniqueID: st.FileUniqueID, Reason: ReasonTooLarge}
	}

	return data, nil
}

// zipEntryOverhead covers the local header, data descriptor, central
// directory record and extended-timestamp extras of one entry, excluding the
// two copies of its name.
const zipEntryOverhead = 128

var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// zipSizeBound is an upper bound of the assembled archive, used to allocate
// its buffer exactly once.
func zipSizeBound(yamlName string, yamlLen int, names []string, files [][]byte) int {
	n := 22 + 64 // end record plus slack
	n += zipEntryOverhead + 2*len(yamlName) + yamlLen + yamlLen/8 + 128
	for i, name := range names {
		n += zipEntryOverhead + 2*len(name) + len(files[i])
	}

	return n
}

// assembleZip writes the YAML first (deflated), then the sticker files in
// order, stored byte for byte. Metadata is fixed, so equal input gives equal
// output. Cancellation is honoured before every entry.
func assembleZip(ctx context.Context, yamlName string, yamlBytes []byte, names []string, files [][]byte) ([]byte, error) {
	bound := zipSizeBound(yamlName, len(yamlBytes), names, files)
	if bound > MaxArchiveBytes {
		return nil, ErrArchiveTooLarge
	}
	buf := bytes.NewBuffer(make([]byte, 0, bound))
	zw := zip.NewWriter(buf)

	write := func(name string, method uint16, content []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		header := &zip.FileHeader{Name: name, Method: method, Modified: zipEpoch}
		header.SetMode(0o644)
		w, err := zw.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("write archive: %w", err)
		}
		if _, err := w.Write(content); err != nil {
			return fmt.Errorf("write archive: %w", err)
		}

		return nil
	}

	if err := write(yamlName, zip.Deflate, yamlBytes); err != nil {
		return nil, err
	}
	for i, name := range names {
		if err := write(name, zip.Store, files[i]); err != nil {
			return nil, err
		}
		files[i] = nil // release as soon as written
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("write archive: %w", err)
	}
	if buf.Len() > MaxArchiveBytes {
		return nil, ErrArchiveTooLarge
	}

	return buf.Bytes(), nil
}
