package adminui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"telegram-ollama-reply-bot/internal/stickers"
)

// StickerAdmin is the catalog surface the admin UI manages. It is
// implemented by *stickers.Service; the controller never touches Telegram or
// SQLite directly.
type StickerAdmin interface {
	// Export fetches the set and returns its YAML template. Read-only.
	Export(ctx context.Context, reference string) (filename string, data []byte, err error)
	// Import validates and commits an edited template atomically.
	Import(ctx context.Context, raw []byte) (stickers.ImportResult, error)
	// ExportArchive builds a ZIP of the YAML template and the sticker files
	// from one Telegram snapshot. Read-only and potentially slow.
	ExportArchive(ctx context.Context, reference string) (filename string, data []byte, err error)
	Sets() []stickers.SetSummary
	Status() stickers.Status
	SetEnabled(ctx context.Context, name string, enabled bool) error
	RemoveSet(ctx context.Context, name string) error
}

// WithStickerAdmin wires the sticker catalog manager.
func WithStickerAdmin(admin StickerAdmin) ControllerOption {
	return func(o *ControllerOptions) {
		o.Stickers = admin
	}
}

// stickerSetsPageSize bounds the set buttons of one stickers-screen page.
const stickerSetsPageSize = 8

// maxStickerFailureRunes keeps a failure text (itemized validation issues
// included) far below Telegram's message limit once embedded in a prompt.
const maxStickerFailureRunes = 2400

// stickerFailureText turns a catalog error into an actionable admin message.
// Messages name identities and rules, never annotation text.
func stickerFailureText(err error) string {
	var (
		verr *stickers.ValidationError
		derr *stickers.DownloadError
	)
	switch {
	case errors.Is(err, stickers.ErrSetNotFound):
		return "Telegram does not know this sticker set — check the name or link."
	case errors.Is(err, stickers.ErrUnsupportedSet):
		return "Only regular sticker sets are supported (not mask or custom emoji sets)."
	case errors.Is(err, stickers.ErrFetchUnavailable):
		return "Telegram is unavailable right now — try again in a moment."
	case errors.Is(err, stickers.ErrBusy):
		return "Another sticker operation is running — try again in a moment."
	case errors.Is(err, stickers.ErrConflict):
		return "The catalog changed while this was processed — export the set again and retry."
	case errors.Is(err, stickers.ErrUnknownSet):
		return "That set is no longer in the catalog."
	case errors.Is(err, stickers.ErrArchiveUnavailable):
		return "ZIP export is not available in this deployment."
	case errors.Is(err, stickers.ErrArchiveTimeout):
		return "The ZIP export took too long — try again in a moment."
	case errors.Is(err, stickers.ErrArchiveTooLarge):
		return "The sticker files are too large to fit into a ZIP archive."
	case errors.Is(err, stickers.ErrUnsafeFileName):
		return "A sticker has an identity that cannot be used as a file name; use the YAML export."
	case errors.Is(err, context.Canceled):
		return "The ZIP export was cancelled."
	case errors.As(err, &derr):
		return "Sticker " + truncateRunes(derr.FileUniqueID, 24) + ": its file " + derr.Reason + "."
	case errors.As(err, &verr):
		text := verr.Error()
		if verr.Stale {
			text += ". Export the set again and re-apply your edits to the new file."
		} else {
			text += ". Nothing was imported."
		}

		return truncateRunes(text, maxStickerFailureRunes)
	default:
		return truncateRunes(err.Error(), maxStickerFailureRunes)
	}
}

func (c *Controller) stickerSetNames() []string {
	sets := c.opts.Stickers.Sets()
	names := make([]string, len(sets))
	for i, set := range sets {
		names[i] = set.Name
	}

	return names
}

// refreshStickerSets re-snapshots the set names of the stickers screen after
// the catalog changed, so button indexes keep matching what is rendered.
func (c *Controller) refreshStickerSets(s *Session) {
	if c.opts.Stickers != nil && s.Screen.Kind == ScreenStickers {
		s.Screen.StickerSets = c.stickerSetNames()
		if pages := stickerPages(len(s.Screen.StickerSets)); s.Screen.Page > pages {
			s.Screen.Page = pages
		}
	}
}

func stickerPages(total int) int {
	if total <= 0 {
		return 1
	}

	return (total + stickerSetsPageSize - 1) / stickerSetsPageSize
}

func (c *Controller) stickerSetSummary(name string) (stickers.SetSummary, bool) {
	for _, set := range c.opts.Stickers.Sets() {
		if set.Name == name {
			return set, true
		}
	}

	return stickers.SetSummary{}, false
}

func (c *Controller) handleStickerVerb(ctx context.Context, s *Session, action Action, fx *fx) error {
	if c.opts.Stickers == nil {
		return errInvalidArg
	}
	now := c.manager.Now()

	switch action.Verb {
	case VerbStickers:
		page := 1
		if len(action.Args) == 1 {
			n, err := strconv.Atoi(action.Args[0])
			if err != nil || n < 1 {
				return errInvalidArg
			}
			page = n
		} else if len(action.Args) > 1 {
			return errInvalidArg
		}
		names := c.stickerSetNames()
		if pages := stickerPages(len(names)); page > pages {
			page = pages
		}
		if s.Screen.Kind == ScreenStickers {
			s.Screen.Page, s.Screen.StickerSets, s.Screen.Notice = page, names, ""
			s.MarkRerender()

			return nil
		}
		s.PushScreen(ScreenState{Kind: ScreenStickers, Page: page, StickerSets: names})
		s.MarkRerender()

		return nil

	case VerbStickerSet:
		if s.Screen.Kind != ScreenStickers || len(action.Args) != 1 {
			return errInvalidArg
		}
		index, err := strconv.Atoi(action.Args[0])
		if err != nil || index < 0 || index >= len(s.Screen.StickerSets) {
			return errInvalidArg
		}
		s.PushScreen(ScreenState{Kind: ScreenStickerSet, StickerSet: s.Screen.StickerSets[index]})
		s.MarkRerender()

		return nil

	case VerbStickerExportNew:
		if s.Screen.Kind != ScreenStickers {
			return errInvalidArg
		}
		s.orphanPickerKeyboard() // a new input abandons a live picker keyboard
		s.Pending = &Pending{ID: s.NextPendingID(), Kind: PendingStickerSetRef, ExpiresAt: now.Add(PendingTTL)}
		fx.input = &InputRequest{PendingID: s.Pending.ID, Text: stickerSetRefPrompt(), Placeholder: "Sticker set name or link"}

		return nil

	case VerbStickerExportCur:
		if s.Screen.Kind != ScreenStickerSet {
			return errInvalidArg
		}
		filename, data, err := c.opts.Stickers.Export(ctx, s.Screen.StickerSet)
		if err != nil {
			s.Screen.Notice = stickerFailureText(err)
			s.MarkRerender()

			return nil
		}
		fx.document = &DocumentPayload{Filename: filename, Content: data}
		s.Screen.Notice = "Template sent below. Edit it, then use Import YAML."
		s.MarkRerender()

		return nil

	case VerbStickerZipNew:
		if s.Screen.Kind != ScreenStickers {
			return errInvalidArg
		}
		s.orphanPickerKeyboard()
		s.Pending = &Pending{ID: s.NextPendingID(), Kind: PendingStickerZipRef, ExpiresAt: now.Add(PendingTTL)}
		fx.input = &InputRequest{PendingID: s.Pending.ID, Text: stickerZipRefPrompt(), Placeholder: "Sticker set name or link"}

		return nil

	case VerbStickerZipCur:
		if s.Screen.Kind != ScreenStickerSet {
			return errInvalidArg
		}
		// Only the quick part runs under the session lock: the download and
		// packaging happen afterwards, in RunStickerArchive.
		s.Screen.Notice = stickerArchivePreparingNotice(s.Screen.StickerSet)
		s.MarkRerender()
		fx.archive = &StickerArchiveJob{Set: s.Screen.StickerSet}

		return nil

	case VerbStickerImport:
		if s.Screen.Kind != ScreenStickers && s.Screen.Kind != ScreenStickerSet {
			return errInvalidArg
		}
		s.orphanPickerKeyboard()
		s.Pending = &Pending{ID: s.NextPendingID(), Kind: PendingStickerDocument, ExpiresAt: now.Add(PendingTTL)}
		fx.input = &InputRequest{PendingID: s.Pending.ID, Text: stickerUploadPrompt()}

		return nil

	case VerbStickerToggle:
		if s.Screen.Kind != ScreenStickerSet {
			return errInvalidArg
		}
		set, ok := c.stickerSetSummary(s.Screen.StickerSet)
		if !ok {
			s.Screen.Notice = stickerFailureText(stickers.ErrUnknownSet)
			s.MarkRerender()

			return nil
		}
		if err := c.opts.Stickers.SetEnabled(ctx, set.Name, !set.Enabled); err != nil {
			s.Screen.Notice = stickerFailureText(err)
			s.MarkRerender()

			return nil
		}
		if set.Enabled {
			s.Screen.Notice = "Set disabled — its stickers are hidden from the model."
		} else {
			s.Screen.Notice = "Set enabled."
		}
		s.MarkRerender()

		return nil

	case VerbStickerRemove:
		if s.Screen.Kind != ScreenStickerSet {
			return errInvalidArg
		}
		name := s.Screen.StickerSet
		if _, ok := c.stickerSetSummary(name); !ok {
			s.Screen.Notice = stickerFailureText(stickers.ErrUnknownSet)
			s.MarkRerender()

			return nil
		}
		s.PushScreen(confirmState(fmt.Sprintf(
			"Remove the sticker set %s from the catalog?\n\nAll its annotations (ids, descriptions, tags) are deleted permanently and the model can no longer send its stickers. Export the set first if you may want them back. This cannot be undone.", name),
			true, 0, ""))
		s.Confirm = &PendingConfirm{Kind: ConfirmStickerRemove, StickerSet: name}
		s.MarkRerender()

		return nil
	}

	return errInvalidArg
}

// StickerArchiveJob is a validated request for a sticker ZIP export.
type StickerArchiveJob struct {
	Set string
}

func stickerArchivePreparingNotice(set string) string {
	return "Preparing the ZIP of " + set + " — it arrives as a separate message (or an error message) when ready."
}

// RunStickerArchive performs a requested ZIP export. It runs outside every
// session lock and never touches the session, so the admin can keep using
// the menu while the files download and no commit can race with it.
func (c *Controller) RunStickerArchive(ctx context.Context, adminID int64, job StickerArchiveJob) Outcome {
	if !c.IsAdmin(adminID) || c.opts.Stickers == nil {
		return Outcome{}
	}
	filename, data, err := c.opts.Stickers.ExportArchive(ctx, job.Set)
	if err != nil {
		return Outcome{Message: &TextMessage{Text: fmt.Sprintf("ZIP export of %s failed: %s No archive was sent.", job.Set, stickerFailureText(err))}}
	}

	return Outcome{Document: &DocumentPayload{Filename: filename, Content: data}}
}

// processStickerReply handles a correlated reply to a sticker prompt. A
// returned error is an input problem shown to the admin with a retry prompt.
func (c *Controller) processStickerReply(ctx context.Context, p *Pending, in ReplyInput) (string, *DocumentPayload, error) {
	if c.opts.Stickers == nil {
		return "", nil, errors.New("the sticker catalog is not available")
	}

	switch p.Kind {
	case PendingStickerSetRef:
		if in.Document != nil {
			return "", nil, errors.New("send the sticker set name or link as text")
		}
		filename, data, err := c.opts.Stickers.Export(ctx, strings.TrimSpace(in.Text))
		if err != nil {
			return "", nil, errors.New(stickerFailureText(err))
		}

		return "Template sent below. Edit it, then use Import YAML.", &DocumentPayload{Filename: filename, Content: data}, nil

	case PendingStickerDocument:
		doc := in.Document
		switch {
		case doc == nil:
			return "", nil, errors.New("reply with a .yaml, .yml or .txt document")
		case !hasStickerFileExtension(doc.Filename):
			return "", nil, errors.New("only .yaml, .yml and .txt files are accepted")
		case doc.Oversized || len(doc.Content) > stickers.MaxUploadBytes:
			return "", nil, fmt.Errorf("the document exceeds %d KiB", stickers.MaxUploadBytes>>10)
		case !utf8.Valid(doc.Content):
			return "", nil, errors.New("the document must be UTF-8 text")
		}
		result, err := c.opts.Stickers.Import(ctx, doc.Content)
		if err != nil {
			return "", nil, errors.New(stickerFailureText(err))
		}

		return importNotice(result), nil, nil
	}

	return "", nil, errInvalidArg
}

func hasStickerFileExtension(filename string) bool {
	lower := strings.ToLower(filename)
	for _, ext := range []string{".yaml", ".yml", ".txt"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}

	return false
}

// importNotice summarizes a committed import. Only non-zero counts appear.
func importNotice(res stickers.ImportResult) string {
	r := res.Report
	var parts []string
	add := func(n int, format string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf(format, n))
		}
	}
	add(r.Added, "%d added")
	if r.Updated > 0 {
		part := fmt.Sprintf("%d updated", r.Updated)
		if r.Renamed > 0 {
			part += fmt.Sprintf(" (%d renamed)", r.Renamed)
		}
		parts = append(parts, part)
	}
	add(r.Reset, "%d reset")
	add(r.Unchanged, "%d unchanged")

	text := "Imported " + res.Set + ": " + strings.Join(parts, ", ") + "."
	if len(parts) == 0 {
		text = "Imported " + res.Set + ": nothing to change."
	}
	var notes []string
	addNote := func(n int, format string) {
		if n > 0 {
			notes = append(notes, fmt.Sprintf(format, n))
		}
	}
	addNote(r.Incomplete, "%d sticker(s) still lack an id or description and stay inactive")
	addNote(r.NowUnavailable, "%d no longer in Telegram (annotations kept)")
	addNote(r.Forgotten, "%d forgotten")
	addNote(r.PreservedBlank, "%d kept saved values where fields were blank")
	addNote(r.NotInFile, "%d sticker(s) not in the file — export again to annotate them")
	addNote(r.IgnoredByReset, "%d value(s) ignored because reset was set")
	addNote(r.IgnoredMissingEdits, "%d edit(s) to missing entries ignored (only forget applies)")
	if len(notes) > 0 {
		text += "\n" + strings.Join(notes, "; ") + "."
	}

	return text
}

func stickerSetRefPrompt() string {
	return AdminInputMarker + " — Export sticker set\n\n" +
		"Reply with a sticker set name or a t.me/addstickers/<name> link. " +
		"The bot sends back a YAML template: your saved annotations are prefilled and new stickers are blank."
}

func stickerZipRefPrompt() string {
	return AdminInputMarker + " — Export sticker set as ZIP\n\n" +
		"Reply with a sticker set name or a t.me/addstickers/<name> link. " +
		"The bot sends back a ZIP with the YAML template and the sticker files named by file_unique_id."
}

func stickerUploadPrompt() string {
	return AdminInputMarker + " — Import sticker template\n\n" +
		fmt.Sprintf("Reply to this message with the edited .yaml document (UTF-8, at most %d KiB). ", stickers.MaxUploadBytes>>10) +
		"Blank id/description/tags keep the saved values; set reset: true to clear a sticker."
}

// stickersData assembles the stickers screen from the snapshot of set names
// the session holds, so buttons and rendering agree on indexes.
func (c *Controller) stickersData(state ScreenState) StickersData {
	byName := map[string]stickers.SetSummary{}
	for _, set := range c.opts.Stickers.Sets() {
		byName[set.Name] = set
	}
	pages := stickerPages(len(state.StickerSets))
	page := state.Page
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	start := (page - 1) * stickerSetsPageSize
	end := min(start+stickerSetsPageSize, len(state.StickerSets))

	data := StickersData{Status: c.opts.Stickers.Status(), Page: page, Pages: pages}
	for i := start; i < end; i++ {
		name := state.StickerSets[i]
		set, ok := byName[name]
		if !ok {
			set = stickers.SetSummary{Name: name, Title: name}
		}
		data.Sets = append(data.Sets, StickerSetRef{Index: i, Summary: set, Gone: !ok})
	}

	return data
}

func (c *Controller) stickerSetData(state ScreenState) StickerSetData {
	set, ok := c.stickerSetSummary(state.StickerSet)

	return StickerSetData{Name: state.StickerSet, Summary: set, Found: ok}
}
