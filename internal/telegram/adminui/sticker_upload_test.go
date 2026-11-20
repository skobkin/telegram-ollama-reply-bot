package adminui

import (
	"context"
	"strings"
	"sync"
	"testing"

	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/stickers"

	tg "github.com/mymmrac/telego"
)

type recordingStickerAdmin struct {
	mu      sync.Mutex
	imports [][]byte
}

func (r *recordingStickerAdmin) Export(context.Context, string) (string, []byte, error) {
	return "stickers-x.yaml", []byte("x"), nil
}

func (r *recordingStickerAdmin) Import(_ context.Context, raw []byte) (stickers.ImportResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.imports = append(r.imports, append([]byte(nil), raw...))

	return stickers.ImportResult{Set: "Alpha", Report: stickers.Report{Added: 1}}, nil
}

func (r *recordingStickerAdmin) importCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.imports)
}

func (r *recordingStickerAdmin) Sets() []stickers.SetSummary {
	return []stickers.SetSummary{{Name: "Alpha", Title: "Alpha", Enabled: true, Total: 1}}
}
func (r *recordingStickerAdmin) Status() stickers.Status { return stickers.Status{Sets: 1, Entries: 1} }
func (r *recordingStickerAdmin) SetEnabled(context.Context, string, bool) error {
	return nil
}
func (r *recordingStickerAdmin) RemoveSet(context.Context, string) error { return nil }

func stickerHarness(t *testing.T) (*harness, *recordingStickerAdmin) {
	t.Helper()
	admin := &recordingStickerAdmin{}
	h := newHarnessFull(t, []int64{testAdmin}, []core.ControllerOption{core.WithStickerAdmin(admin)})
	h.open(t)
	h.tap(t, core.VerbStickers)
	h.tap(t, core.VerbStickerImport)

	return h, admin
}

func yamlUpload(replyTo tg.Message, from int64, fileSize int64) tg.Update {
	return tg.Update{UpdateID: 47, Message: &tg.Message{
		MessageID:      54,
		Chat:           tg.Chat{ID: testAdmin, Type: tg.ChatTypePrivate},
		From:           &tg.User{ID: from},
		ReplyToMessage: &replyTo,
		Document:       &tg.Document{FileID: "file-1", FileName: "stickers-Alpha.yaml", FileSize: fileSize},
	}}
}

func stickerImportPrompt(h *harness) tg.Message {
	return botPromptMessage(int(h.api.lastSendMessageID()), core.AdminInputMarker+" — Import sticker template")
}

func TestStickerTemplateLargerThanAPromptBodyIsDownloadedWhole(t *testing.T) {
	h, admin := stickerHarness(t)

	// Well above the 32 KiB prompt limit, below the sticker template limit.
	body := []byte("# " + strings.Repeat("x", 100*1024) + "\n")
	h.api.files["uploads/prompt.md"] = body
	h.deliver(t, yamlUpload(stickerImportPrompt(h), testAdmin, 0))

	if admin.importCount() != 1 || string(admin.imports[0]) != string(body) {
		t.Fatalf("the whole %d-byte template must reach the import, got %d import(s)", len(body), admin.importCount())
	}
}

func TestStickerTemplateOverTheDownloadBoundIsRejectedOnActualBytes(t *testing.T) {
	h, admin := stickerHarness(t)

	// The fake reports file_size 0: the bound is enforced on bytes read.
	h.api.files["uploads/prompt.md"] = make([]byte, core.MaxAdminUploadBytes+10)
	h.deliver(t, yamlUpload(stickerImportPrompt(h), testAdmin, 0))

	if admin.importCount() != 0 {
		t.Fatalf("an oversized template must never reach the import")
	}
}

func TestOversizedDeclaredFileIsRefusedWithoutADownload(t *testing.T) {
	h, admin := stickerHarness(t)

	before := h.api.callCount("getFile")
	h.deliver(t, yamlUpload(stickerImportPrompt(h), testAdmin, core.MaxAdminUploadBytes+1))

	if got := h.api.callCount("getFile"); got != before {
		t.Fatalf("a document declared over the bound must not even be fetched (getFile calls %d → %d)", before, got)
	}
	if admin.importCount() != 0 {
		t.Fatalf("an oversized template must never reach the import")
	}
	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "exceeds") {
		t.Fatalf("the admin must be told why, got %q", text)
	}
}

func TestStickerImportRequiresTheAdminAndTheCorrelatedPrompt(t *testing.T) {
	h, admin := stickerHarness(t)
	h.api.files["uploads/prompt.md"] = []byte("schema_version: 1\nset: Alpha\nstickers: []\n")

	// A document from a non-admin replying to the prompt is not an import.
	h.deliver(t, yamlUpload(stickerImportPrompt(h), 999, 0))
	if admin.importCount() != 0 {
		t.Fatalf("a non-admin upload must never be imported")
	}

	// An admin document replying to some other bot message is not correlated.
	h.deliver(t, yamlUpload(botPromptMessage(9999, "an unrelated bot message"), testAdmin, 0))
	if admin.importCount() != 0 {
		t.Fatalf("an upload that does not reply to the live prompt must never be imported")
	}

	// The correct admin reply to the live prompt imports.
	h.deliver(t, yamlUpload(stickerImportPrompt(h), testAdmin, 0))
	if admin.importCount() != 1 {
		t.Fatalf("the correlated admin upload must import, got %d", admin.importCount())
	}
}
