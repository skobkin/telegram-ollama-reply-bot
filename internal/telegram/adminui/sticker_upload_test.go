package adminui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	core "telegram-ollama-reply-bot/internal/adminui"
	"telegram-ollama-reply-bot/internal/stickers"

	tg "github.com/mymmrac/telego"
)

type recordingStickerAdmin struct {
	mu         sync.Mutex
	imports    [][]byte
	archiveErr error
	archives   []string
}

func (r *recordingStickerAdmin) ExportArchive(_ context.Context, ref string) (string, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.archives = append(r.archives, ref)
	if r.archiveErr != nil {
		return "", nil, r.archiveErr
	}

	return ref + ".zip", []byte("PK-zip"), nil
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

func (h *harness) requestOrder() []string {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	var order []string
	for _, request := range h.api.requests {
		order = append(order, request.Method)
	}

	return order
}

func zipCard(t *testing.T) (*harness, *recordingStickerAdmin) {
	t.Helper()
	admin := &recordingStickerAdmin{}
	h := newHarnessFull(t, []int64{testAdmin}, []core.ControllerOption{core.WithStickerAdmin(admin)})
	h.open(t)
	h.tap(t, core.VerbStickers)
	h.tap(t, core.VerbStickerSet, "0")

	return h, admin
}

func TestZipExportRendersProgressThenDeliversTheArchive(t *testing.T) {
	h, admin := zipCard(t)
	before := len(h.requestOrder())

	h.tap(t, core.VerbStickerZipCur)

	if len(admin.archives) != 1 || admin.archives[0] != "Alpha" {
		t.Fatalf("the archive must be built for the card's set: %v", admin.archives)
	}
	order := h.requestOrder()[before:]
	edit, doc := -1, -1
	for i, m := range order {
		switch m {
		case "editMessageText":
			if edit < 0 {
				edit = i
			}
		case "sendDocument":
			doc = i
		}
	}
	if edit < 0 || doc < 0 || edit > doc {
		t.Fatalf("the Preparing render must precede the document: %v", order)
	}
	if !strings.Contains(h.api.requestsOf("editMessageText")[len(h.api.requestsOf("editMessageText"))-1].Body, "Preparing the ZIP of Alpha") {
		t.Fatalf("the screen must announce the ZIP")
	}
	if body := h.api.requestsOf("sendDocument")[0].Body; !strings.Contains(body, "Alpha.zip") {
		t.Fatalf("the document must carry the archive name: %q", body)
	}
}

func TestZipExportFailureSendsAMessageAndNoDocument(t *testing.T) {
	h, admin := zipCard(t)
	admin.archiveErr = &stickers.DownloadError{FileUniqueID: "AgADx", Reason: stickers.ReasonUnavailable}

	h.tap(t, core.VerbStickerZipCur)

	if h.api.callCount("sendDocument") != 0 {
		t.Fatal("a failed export must not deliver any document")
	}
	if text := h.api.lastSentText("sendMessage"); !strings.Contains(text, "AgADx") || !strings.Contains(text, "No archive was sent") {
		t.Fatalf("the admin must get an actionable failure, got %q", text)
	}
}

func TestZipExportRequiresTheAdmin(t *testing.T) {
	h, admin := zipCard(t)
	session := h.manager.Session(testAdmin)
	_, _, msgID, _ := session.DetachedSnapshot()

	h.deliver(t, callbackUpdate(999, msgID, core.EncodeAction(core.VerbStickerZipCur, session.Seq)))

	if len(admin.archives) != 0 || h.api.callCount("sendDocument") != 0 {
		t.Fatal("a non-admin callback must never export")
	}
}

func messagesAfter(h *harness, before int) []string {
	var texts []string
	for _, request := range h.api.requestsOf("sendMessage")[before:] {
		texts = append(texts, request.Body)
	}

	return texts
}

func TestZipDeliverySuccessSendsNoFailureMessage(t *testing.T) {
	h, admin := zipCard(t)
	messages := h.api.callCount("sendMessage")

	h.tap(t, core.VerbStickerZipCur)

	if h.api.callCount("sendDocument") != 1 || len(admin.archives) != 1 {
		t.Fatalf("one archive must be delivered once")
	}
	if extra := messagesAfter(h, messages); len(extra) != 0 {
		t.Fatalf("a delivered archive must not trigger any message: %v", extra)
	}
}

func TestZipDeliveryRejectedByTelegramIsReported(t *testing.T) {
	h, _ := zipCard(t)
	h.api.failMethods["sendDocument"] = errors.New("Request Entity Too Large https://api.telegram.org/botSECRET-TOKEN/sendDocument")
	h.api.failCodes = map[string]int{"sendDocument": 413}
	messages := h.api.callCount("sendMessage")

	h.tap(t, core.VerbStickerZipCur)

	extra := messagesAfter(h, messages)
	if len(extra) != 1 || !strings.Contains(extra[0], "Telegram rejected the upload (error 413)") || !strings.Contains(extra[0], "Nothing was sent") {
		t.Fatalf("a rejection must be reported as definite: %v", extra)
	}
	if strings.Contains(extra[0], "SECRET-TOKEN") || strings.Contains(h.logs.String(), "SECRET-TOKEN") {
		t.Fatal("the token leaked into a message or the logs")
	}
	if h.api.callCount("sendDocument") != 1 {
		t.Fatal("a rejected upload must not be retried")
	}
}

func TestZipDeliveryTransportFailureIsReportedAsUnconfirmed(t *testing.T) {
	h, _ := zipCard(t)
	h.api.dropConn = map[string]bool{"sendDocument": true}
	messages := h.api.callCount("sendMessage")

	h.tap(t, core.VerbStickerZipCur)

	extra := messagesAfter(h, messages)
	if len(extra) != 1 || !strings.Contains(extra[0], "could not be confirmed") || !strings.Contains(extra[0], "Check this chat") || !strings.Contains(extra[0], "not resent") {
		t.Fatalf("an ambiguous failure must say delivery is unconfirmed: %v", extra)
	}
	for _, banned := range []string{"http", "127.0.0.1", "EOF", "token"} {
		if strings.Contains(extra[0], banned) {
			t.Fatalf("raw transport detail %q leaked: %s", banned, extra[0])
		}
	}
	if h.api.callCount("sendDocument") != 1 {
		t.Fatal("an ambiguous upload must never be resent automatically")
	}
}

func TestUnrelatedDocumentFailuresStayQuiet(t *testing.T) {
	hs, _ := zipCard(t)
	hs.api.failMethods["sendDocument"] = errors.New("boom")
	messages := hs.api.callCount("sendMessage")

	hs.tap(t, core.VerbStickerExportCur) // the YAML export: a plain document delivery

	if extra := messagesAfter(hs, messages); len(extra) != 0 {
		t.Fatalf("non-ZIP document failures keep their log-only behavior: %v", extra)
	}
	if !strings.Contains(hs.logs.String(), "admin ui document send failed") {
		t.Fatal("the YAML delivery failure must still be logged")
	}
}

func TestZipExportsDoNotOverlapAcrossDelivery(t *testing.T) {
	h, admin := zipCard(t)
	entered, release := h.api.armGate("sendDocument")
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unpark) // a failing assertion must not leave the upload parked

	first := make(chan struct{})
	go func() {
		h.tap(t, core.VerbStickerZipCur)
		close(first)
	}()
	<-entered // the first archive is built and its upload is in flight

	messages := h.api.callCount("sendMessage")
	h.tap(t, core.VerbStickerZipCur) // a second request while the first is uploading
	if got := len(admin.archives); got != 1 {
		t.Fatalf("no second export may start while the first is delivering, builds = %d", got)
	}
	extra := messagesAfter(h, messages)
	if len(extra) != 1 || !strings.Contains(extra[0], "Another ZIP export") {
		t.Fatalf("the second request must be refused with a clear message: %v", extra)
	}

	unpark()
	<-first
	if h.api.callCount("sendDocument") != 1 {
		t.Fatalf("exactly the first archive was delivered, got %d", h.api.callCount("sendDocument"))
	}

	h.tap(t, core.VerbStickerZipCur) // the gate is free again
	if len(admin.archives) != 2 || h.api.callCount("sendDocument") != 2 {
		t.Fatalf("a later export must run after delivery completes: builds %d sends %d", len(admin.archives), h.api.callCount("sendDocument"))
	}
}
