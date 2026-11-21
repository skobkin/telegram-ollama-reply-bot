package stickers

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFiles serves bytes per file_id and records what was requested.
type fakeFiles struct {
	mu       sync.Mutex
	content  map[string][]byte // file_id -> bytes
	errs     map[string]error
	requests []string
	inFlight int
	maxSeen  int
	// onCall runs inside DownloadFile (after the content was chosen).
	onCall func(ctx context.Context, fileID string)
	// block parks a download until ctx is done.
	block   map[string]bool
	entered chan string
}

func (f *fakeFiles) DownloadFile(ctx context.Context, fileID string, _ int64) ([]byte, error) {
	f.mu.Lock()
	f.requests = append(f.requests, fileID)
	f.inFlight++
	f.maxSeen = max2(f.maxSeen, f.inFlight)
	data, err := f.content[fileID], f.errs[fileID]
	block, onCall, entered := f.block[fileID], f.onCall, f.entered
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if entered != nil {
		entered <- fileID
	}
	if block {
		<-ctx.Done()

		return nil, ctx.Err()
	}
	if onCall != nil {
		onCall(ctx, fileID)
	}

	return data, err
}

func max2(a, b int) int {
	if a > b {
		return a
	}

	return b
}

func (f *fakeFiles) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.requests...)
}

func mixedSet() (RemoteSet, *fakeFiles) {
	rs := RemoteSet{Name: "Kitsu", Title: "KITSU", Stickers: []RemoteSticker{
		{FileUniqueID: "AgADaa-1", FileID: "fid-a", Emoji: "🦊", Format: FormatStatic},
		{FileUniqueID: "AgADbb_2", FileID: "fid-b", Emoji: "🦊", Format: FormatAnimated},
		{FileUniqueID: "AgADcc3", FileID: "fid-c", Emoji: "🦊", Format: FormatVideo},
	}}
	files := &fakeFiles{content: map[string][]byte{
		"fid-a": []byte("RIFF-webp-bytes"), "fid-b": {0x1f, 0x8b, 0, 1, 2}, "fid-c": []byte("webm\x00\x01"),
	}}

	return rs, files
}

func archiveService(t *testing.T, store *fakeStore, rs RemoteSet, files *fakeFiles) *Service {
	t.Helper()
	fetcher := &fakeFetcher{sets: map[string]RemoteSet{rs.Name: rs}}
	svc := NewService(store, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)), WithFileDownloader(files))
	svc.now = func() time.Time { return testNow }
	if err := svc.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	store.calls = nil

	return svc
}

func readZip(t *testing.T, data []byte) ([]string, map[string][]byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	var order []string
	content := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		order = append(order, f.Name)
		content[f.Name] = b
	}

	return order, content
}

func TestArchiveStructureNamesExtensionsAndBytes(t *testing.T) {
	rs, files := mixedSet()
	svc := archiveService(t, &fakeStore{}, rs, files)

	name, data, err := svc.ExportArchive(context.Background(), "https://t.me/addstickers/Kitsu")
	if err != nil {
		t.Fatalf("ExportArchive: %v", err)
	}
	if name != "Kitsu.zip" {
		t.Fatalf("filename %q", name)
	}
	order, content := readZip(t, data)
	want := []string{"stickers-Kitsu.yaml", "stickers/AgADaa-1.webp", "stickers/AgADbb_2.tgs", "stickers/AgADcc3.webm"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("entries %v, want %v", order, want)
	}
	for fid, entry := range map[string]string{"fid-a": want[1], "fid-b": want[2], "fid-c": want[3]} {
		if !bytes.Equal(content[entry], files.content[fid]) {
			t.Fatalf("%s bytes changed", entry)
		}
	}

	// Every archived stem is a YAML file_unique_id and vice versa.
	tmpl, err := ParseTemplate(content["stickers-Kitsu.yaml"])
	if err != nil {
		t.Fatalf("yaml in archive does not parse: %v", err)
	}
	stems := map[string]bool{}
	for _, e := range order[1:] {
		stems[strings.TrimSuffix(strings.TrimPrefix(e, "stickers/"), e[strings.LastIndex(e, "."):])] = true
	}
	if len(tmpl.Stickers) != len(stems) {
		t.Fatalf("yaml has %d stickers, archive %d files", len(tmpl.Stickers), len(stems))
	}
	for _, s := range tmpl.Stickers {
		if !stems[s.FileUniqueID] {
			t.Fatalf("yaml sticker %s has no file", s.FileUniqueID)
		}
	}
}

func TestArchiveIsDeterministic(t *testing.T) {
	rs, files := mixedSet()
	svc := archiveService(t, &fakeStore{}, rs, files)
	_, one, err := svc.ExportArchive(context.Background(), "Kitsu")
	if err != nil {
		t.Fatal(err)
	}
	_, two, err := svc.ExportArchive(context.Background(), "Kitsu")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Fatal("equal input produced different archives")
	}
}

func TestArchiveBufferIsAllocatedOnceAndBoundHolds(t *testing.T) {
	rs, files := mixedSet()
	yamlBytes := bytes.Repeat([]byte("x: y\n"), 2000)
	names := []string{"stickers/AgADaa-1.webp", "stickers/AgADbb_2.tgs", "stickers/AgADcc3.webm"}
	blobs := [][]byte{files.content["fid-a"], files.content["fid-b"], files.content["fid-c"]}
	_ = rs
	bound := zipSizeBound("stickers-Kitsu.yaml", len(yamlBytes), names, blobs)

	data, err := assembleZip(context.Background(), "stickers-Kitsu.yaml", yamlBytes, names, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > bound {
		t.Fatalf("archive %d exceeds bound %d", len(data), bound)
	}
	if cap(data) != bound {
		t.Fatalf("buffer regrew: cap %d, bound %d", cap(data), bound)
	}
}

func TestArchiveKeepsAnnotationsAndBlanksNewStickers(t *testing.T) {
	rs, files := mixedSet()
	stored := stateFrom(t,
		annotated("AgADbb_2", "id_b", "Fox B", "tag_b"),
		annotated("gone", "id_gone", "Retired", "old"),
	)
	// Telegram order differs from stored order; "gone" left the set.
	svc := archiveService(t, &fakeStore{state: stored}, rs, files)

	_, data, err := svc.ExportArchive(context.Background(), "Kitsu")
	if err != nil {
		t.Fatal(err)
	}
	_, content := readZip(t, data)
	tmpl, err := ParseTemplate(content["stickers-Kitsu.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TemplateSticker{}
	for _, s := range tmpl.Stickers {
		byID[s.FileUniqueID] = s
	}
	if b := byID["AgADbb_2"]; b.ID != "id_b" || b.Description != "Fox B" || len(b.Tags) != 1 {
		t.Fatalf("annotations lost: %+v", b)
	}
	if a := byID["AgADaa-1"]; a.ID != "" || a.Description != "" || len(a.Tags) != 0 {
		t.Fatalf("new sticker not blank: %+v", a)
	}
	if len(tmpl.Missing) != 1 || tmpl.Missing[0].FileUniqueID != "gone" {
		t.Fatalf("missing section wrong: %+v", tmpl.Missing)
	}
	for _, e := range []string{"gone"} {
		for _, name := range []string{"stickers/" + e + ".webp"} {
			if _, ok := content[name]; ok {
				t.Fatalf("retained missing sticker got a file: %s", name)
			}
		}
	}
	for _, fid := range files.requested() {
		if fid == "fid-gone" || strings.Contains(fid, "gone") {
			t.Fatalf("downloaded a missing sticker: %s", fid)
		}
	}
	if len(files.requested()) != 3 {
		t.Fatalf("expected 3 downloads, got %v", files.requested())
	}
}

func TestArchiveIsReadOnly(t *testing.T) {
	rs, files := mixedSet()
	store := &fakeStore{state: stateFrom(t, annotated("AgADaa-1", "id_a", "Fox", "t"))}
	svc := archiveService(t, store, rs, files)
	beforeState, beforeIndex := svc.current(), svc.Snapshot()

	if _, _, err := svc.ExportArchive(context.Background(), "Kitsu"); err != nil {
		t.Fatal(err)
	}
	if store.callCount() != 0 {
		t.Fatalf("ZIP export touched the store: %v", store.calls)
	}
	if svc.Snapshot() != beforeIndex || len(svc.current().Entries) != len(beforeState.Entries) {
		t.Fatal("ZIP export changed the catalog")
	}
}

func TestArchiveFailureYieldsNoBytes(t *testing.T) {
	cases := map[string]struct {
		err    error
		reason string
	}{
		"transport": {errors.New("https://api/file/botSECRET/x boom"), ReasonUnavailable},
		"too large": {ErrFileTooLarge, ReasonTooLarge},
		"deadline":  {context.DeadlineExceeded, ReasonTimedOut},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rs, files := mixedSet()
			files.errs = map[string]error{"fid-b": tc.err}
			svc := archiveService(t, &fakeStore{}, rs, files)

			filename, data, err := svc.ExportArchive(context.Background(), "Kitsu")
			var derr *DownloadError
			if !errors.As(err, &derr) || derr.FileUniqueID != "AgADbb_2" || derr.Reason != tc.reason {
				t.Fatalf("err = %v", err)
			}
			if data != nil || filename != "" {
				t.Fatal("partial archive returned")
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "boom") {
				t.Fatalf("cause leaked: %v", err)
			}
		})
	}
}

func TestArchiveRejectsEmptyAndOversizedActualBytes(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":     {},
		"oversized": bytes.Repeat([]byte{1}, MaxStickerFileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			rs, files := mixedSet()
			files.content["fid-c"] = content
			svc := archiveService(t, &fakeStore{}, rs, files)
			_, data, err := svc.ExportArchive(context.Background(), "Kitsu")
			var derr *DownloadError
			if !errors.As(err, &derr) || data != nil {
				t.Fatalf("err = %v, data %d bytes", err, len(data))
			}
		})
	}
}

func TestArchiveTotalDownloadCap(t *testing.T) {
	n := MaxArchiveDownloadBytes/MaxStickerFileBytes + 2
	rs := RemoteSet{Name: "Kitsu", Title: "K"}
	files := &fakeFiles{content: map[string][]byte{}}
	chunk := bytes.Repeat([]byte{7}, MaxStickerFileBytes)
	for i := 0; i < n; i++ {
		id := "u" + strings.Repeat("a", i%5) + string(rune('A'+i/26)) + string(rune('a'+i%26))
		rs.Stickers = append(rs.Stickers, RemoteSticker{FileUniqueID: id, FileID: "f" + id, Format: FormatStatic})
		files.content["f"+id] = chunk
	}
	svc := archiveService(t, &fakeStore{}, rs, files)

	_, data, err := svc.ExportArchive(context.Background(), "Kitsu")
	if !errors.Is(err, ErrArchiveTooLarge) || data != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestArchiveRejectsUnsafeIdentitiesBeforeDownloading(t *testing.T) {
	for _, bad := range []string{"../x", "a/b", "a\\b", "", strings.Repeat("a", 65), "ключ", "a b", "x\x00y", "a.b"} {
		t.Run(bad, func(t *testing.T) {
			rs := RemoteSet{Name: "Kitsu", Title: "K", Stickers: []RemoteSticker{
				{FileUniqueID: bad, FileID: "f1", Format: FormatStatic},
			}}
			files := &fakeFiles{content: map[string][]byte{"f1": []byte("x")}}
			svc := archiveService(t, &fakeStore{}, rs, files)
			_, data, err := svc.ExportArchive(context.Background(), "Kitsu")
			// BuildTemplate rejects an empty identity first; both are refusals.
			if err == nil || data != nil {
				t.Fatalf("accepted %q", bad)
			}
			if len(files.requested()) != 0 {
				t.Fatal("downloaded before validating names")
			}
		})
	}
	if _, err := ArchiveEntryName(RemoteSticker{FileUniqueID: "ok", Format: "weird"}); !errors.Is(err, ErrUnsafeFileName) {
		t.Fatalf("unknown format accepted: %v", err)
	}
}

func TestArchiveConcurrencyIsBounded(t *testing.T) {
	rs := RemoteSet{Name: "Kitsu", Title: "K"}
	files := &fakeFiles{content: map[string][]byte{}}
	for i := 0; i < 20; i++ {
		id := "id" + string(rune('A'+i))
		rs.Stickers = append(rs.Stickers, RemoteSticker{FileUniqueID: id, FileID: "f" + id, Format: FormatStatic})
		files.content["f"+id] = []byte("data")
	}
	files.onCall = func(context.Context, string) { time.Sleep(time.Millisecond) } // widen the overlap window only
	svc := archiveService(t, &fakeStore{}, rs, files)
	if _, _, err := svc.ExportArchive(context.Background(), "Kitsu"); err != nil {
		t.Fatal(err)
	}
	if files.maxSeen > ArchiveConcurrency || files.maxSeen < 1 {
		t.Fatalf("max in flight = %d, limit %d", files.maxSeen, ArchiveConcurrency)
	}
}

func TestArchiveFirstFailureCancelsOtherDownloads(t *testing.T) {
	rs, files := mixedSet()
	files.errs = map[string]error{"fid-a": errors.New("boom")}
	files.block = map[string]bool{"fid-b": true, "fid-c": true}
	files.entered = make(chan string, 3)
	svc := archiveService(t, &fakeStore{}, rs, files)

	done := make(chan error, 1)
	go func() {
		_, _, err := svc.ExportArchive(context.Background(), "Kitsu")
		done <- err
	}()
	select {
	case err := <-done: // would hang forever if siblings were not cancelled
		var derr *DownloadError
		if !errors.As(err, &derr) || derr.FileUniqueID != "AgADaa-1" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("parked downloads were not cancelled by the first failure")
	}
}

func TestArchiveOverallDeadlineCoversEveryStage(t *testing.T) {
	t.Run("cancelled during fetch", func(t *testing.T) {
		rs, files := mixedSet()
		fetcher := &fakeFetcher{sets: map[string]RemoteSet{"Kitsu": rs}, entered: make(chan struct{}, 1), release: make(chan struct{})}
		svc := NewService(&fakeStore{}, fetcher, nil, WithFileDownloader(files))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, data, err := svc.ExportArchive(ctx, "Kitsu")
			if data != nil {
				err = errors.New("bytes returned")
			}
			done <- err
		}()
		<-fetcher.entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if len(files.requested()) != 0 {
			t.Fatal("downloads started after cancellation")
		}
	})

	t.Run("cancelled after downloads before assembly", func(t *testing.T) {
		rs := RemoteSet{Name: "Kitsu", Title: "K", Stickers: []RemoteSticker{{FileUniqueID: "one", FileID: "f1", Format: FormatStatic}}}
		files := &fakeFiles{content: map[string][]byte{"f1": []byte("x")}}
		svc := archiveService(t, &fakeStore{}, rs, files)
		ctx, cancel := context.WithCancel(context.Background())
		files.onCall = func(context.Context, string) { cancel() } // the last download finishes, then the caller is gone
		_, data, err := svc.ExportArchive(ctx, "Kitsu")
		if !errors.Is(err, context.Canceled) || data != nil {
			t.Fatalf("err = %v, data %d", err, len(data))
		}
	})

	t.Run("cancelled between entries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := assembleZip(ctx, "stickers-X.yaml", []byte("a"), nil, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("deadline maps to timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if err := archiveCtxError(ctx, errors.New("x")); !errors.Is(err, ErrArchiveTimeout) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("per-file timeout is not the overall timeout", func(t *testing.T) {
		rs, files := mixedSet()
		files.errs = map[string]error{"fid-a": context.DeadlineExceeded}
		svc := archiveService(t, &fakeStore{}, rs, files)
		_, _, err := svc.ExportArchive(context.Background(), "Kitsu")
		var derr *DownloadError
		if !errors.As(err, &derr) || derr.Reason != ReasonTimedOut || errors.Is(err, ErrArchiveTimeout) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestArchiveSecondConcurrentExportIsBusy(t *testing.T) {
	rs, files := mixedSet()
	files.block = map[string]bool{"fid-a": true}
	files.entered = make(chan string, 3)
	svc := archiveService(t, &fakeStore{}, rs, files)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		_, _, _ = svc.ExportArchive(ctx, "Kitsu")
		close(finished)
	}()
	<-files.entered // parked inside a download, holding the slot

	if _, _, err := svc.ExportArchive(context.Background(), "Kitsu"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	cancel()
	<-finished
}

func TestArchiveWithoutDownloaderIsUnavailable(t *testing.T) {
	svc := NewService(&fakeStore{}, kitsuFetcher(), nil)
	if _, _, err := svc.ExportArchive(context.Background(), "Kitsu"); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestYAMLExportUnaffectedByArchiveOption(t *testing.T) {
	rs, files := mixedSet()
	svc := archiveService(t, &fakeStore{}, rs, files)
	name, data, err := svc.Export(context.Background(), "Kitsu")
	if err != nil || name != "stickers-Kitsu.yaml" || len(files.requested()) != 0 {
		t.Fatalf("name %q err %v downloads %v", name, err, files.requested())
	}
	if _, err := ParseTemplate(data); err != nil {
		t.Fatal(err)
	}
}
