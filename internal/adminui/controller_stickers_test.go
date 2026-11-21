package adminui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"telegram-ollama-reply-bot/internal/stickers"
)

type fakeStickerAdmin struct {
	mu           sync.Mutex
	sets         []stickers.SetSummary
	exportErr    error
	importErr    error
	enableErr    error
	removeErr    error
	exports      []string
	archiveErr   error
	archives     []string
	archiveHook  func()
	imports      [][]byte
	enableCalls  []string
	removeCalls  []string
	importResult stickers.ImportResult
}

func (f *fakeStickerAdmin) Export(_ context.Context, ref string) (string, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exports = append(f.exports, ref)
	if f.exportErr != nil {
		return "", nil, f.exportErr
	}

	return "stickers-" + ref + ".yaml", []byte("schema_version: 1\nset: " + ref + "\n"), nil
}

func (f *fakeStickerAdmin) ExportArchive(_ context.Context, ref string) (string, []byte, error) {
	f.mu.Lock()
	f.archives = append(f.archives, ref)
	err, hook := f.archiveErr, f.archiveHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return "", nil, err
	}

	return ref + ".zip", []byte("PK-fake"), nil
}

func (f *fakeStickerAdmin) Import(_ context.Context, raw []byte) (stickers.ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, raw)
	if f.importErr != nil {
		return stickers.ImportResult{}, f.importErr
	}

	return f.importResult, nil
}

func (f *fakeStickerAdmin) Sets() []stickers.SetSummary {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]stickers.SetSummary(nil), f.sets...)
}

func (f *fakeStickerAdmin) Status() stickers.Status {
	st := stickers.Status{}
	for _, s := range f.Sets() {
		st.Sets++
		st.Entries += s.Total
		st.Active += s.Active
		st.Incomplete += s.Incomplete
	}

	return st
}

func (f *fakeStickerAdmin) SetEnabled(_ context.Context, name string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enableCalls = append(f.enableCalls, fmt.Sprintf("%s=%v", name, enabled))
	if f.enableErr != nil {
		return f.enableErr
	}
	for i := range f.sets {
		if f.sets[i].Name == name {
			f.sets[i].Enabled = enabled
		}
	}

	return nil
}

func (f *fakeStickerAdmin) RemoveSet(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, name)
	if f.removeErr != nil {
		return f.removeErr
	}
	kept := f.sets[:0:0]
	for _, s := range f.sets {
		if s.Name != name {
			kept = append(kept, s)
		}
	}
	f.sets = kept

	return nil
}

func twoSets() []stickers.SetSummary {
	return []stickers.SetSummary{
		{Name: "Alpha", Title: "Alpha pack", Enabled: true, Total: 5, Active: 3, Incomplete: 2},
		{Name: "Beta", Title: "Beta pack", Enabled: true, Total: 2, Active: 2},
	}
}

func stickerController(t *testing.T, admin *fakeStickerAdmin) (*Controller, *Session) {
	t.Helper()
	c, _ := newTestController(t, WithStickerAdmin(admin))

	return c, mustOpen(t, c)
}

func reply(t *testing.T, c *Controller, promptMsgID, replyMsgID int, in ReplyInput) Outcome {
	t.Helper()
	armPending(t, c, promptMsgID)
	in.ReplyToMsgID, in.TextAvailable, in.ReplyMsgID = promptMsgID, true, replyMsgID
	outcome, err := c.HandleReply(context.Background(), testAdminID, in)
	if err != nil {
		t.Fatalf("HandleReply: %v", err)
	}

	return outcome
}

func TestDashboardShowsStickersOnlyWhenACatalogIsWired(t *testing.T) {
	c, _ := newTestController(t)
	plain := mustOpen(t, c)
	_ = plain
	outcome, _ := c.OpenDashboard(context.Background(), testAdminID)
	if strings.Contains(screenButtons(outcome.Render.Built), "Stickers") {
		t.Fatalf("no catalog wired: the entry must be hidden")
	}

	c, _ = stickerController(t, &fakeStickerAdmin{})
	outcome, _ = c.OpenDashboard(context.Background(), testAdminID)
	if !strings.Contains(screenButtons(outcome.Render.Built), "Stickers") {
		t.Fatalf("catalog wired: the entry must show")
	}
}

func TestStickerVerbsAreInertWithoutACatalog(t *testing.T) {
	c, _ := newTestController(t)
	session := mustOpen(t, c)

	for _, verb := range []string{VerbStickers, VerbStickerExportNew, VerbStickerImport} {
		outcome := tap(t, c, session, verb)
		if outcome.Answer != AnswerUnknown || outcome.Render != nil {
			t.Fatalf("%s must be inert without a catalog: %+v", verb, outcome)
		}
	}
}

func TestStickersScreenListsSetsAndOpensCards(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)

	outcome := tap(t, c, session, VerbStickers)
	if outcome.Render == nil || outcome.Render.Screen.Kind != ScreenStickers {
		t.Fatalf("expected the stickers screen: %+v", outcome.Render)
	}
	text := outcome.Render.Built.Text
	for _, want := range []string{"Alpha pack", "3/5 active", "2 incomplete", "Beta pack", "Only active stickers are visible to the model"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stickers screen lacks %q:\n%s", want, text)
		}
	}
	buttons := screenButtons(outcome.Render.Built)
	for _, want := range []string{"Alpha", "Beta", "Export YAML…", "Import YAML", "‹ Back"} {
		if !strings.Contains(buttons, want) {
			t.Fatalf("stickers screen lacks button %q:\n%s", want, buttons)
		}
	}

	outcome = tap(t, c, session, VerbStickerSet, "1")
	if outcome.Render.Screen.Kind != ScreenStickerSet || outcome.Render.Screen.StickerSet != "Beta" {
		t.Fatalf("index 1 must open Beta: %+v", outcome.Render.Screen)
	}
	if !strings.Contains(outcome.Render.Built.Text, "Status: enabled") {
		t.Fatalf("set card must show the status:\n%s", outcome.Render.Built.Text)
	}

	if out := tap(t, c, session, VerbBack); out.Render.Screen.Kind != ScreenStickers {
		t.Fatalf("back must return to the list")
	}
	if out := tap(t, c, session, VerbStickerSet, "9"); out.Answer != AnswerUnknown {
		t.Fatalf("an out-of-range index must be rejected: %+v", out)
	}
}

func TestStickerVerbsRejectWrongScreens(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)

	for _, verb := range []string{VerbStickerSet, VerbStickerExportNew, VerbStickerExportCur, VerbStickerImport, VerbStickerToggle, VerbStickerRemove} {
		args := []string(nil)
		if verb == VerbStickerSet {
			args = []string{"0"}
		}
		if out := tap(t, c, session, verb, args...); out.Answer != AnswerUnknown {
			t.Fatalf("%s from the dashboard must be rejected: %+v", verb, out)
		}
	}
	if len(admin.exports)+len(admin.enableCalls)+len(admin.removeCalls) != 0 {
		t.Fatalf("rejected verbs must not reach the catalog")
	}
}

func TestStickerPagingKeepsButtonIndexesGlobal(t *testing.T) {
	admin := &fakeStickerAdmin{}
	for i := 0; i < 10; i++ {
		admin.sets = append(admin.sets, stickers.SetSummary{Name: fmt.Sprintf("Set%02d", i), Title: fmt.Sprintf("Set %02d", i), Enabled: true, Total: 1})
	}
	c, session := stickerController(t, admin)

	out := tap(t, c, session, VerbStickers)
	if !strings.Contains(out.Render.Built.Text, "Page 1/2") || strings.Contains(screenButtons(out.Render.Built), "Set08") {
		t.Fatalf("page 1 holds 8 sets:\n%s", out.Render.Built.Text)
	}
	out = tap(t, c, session, VerbStickers, "2")
	if !strings.Contains(screenButtons(out.Render.Built), "Set08") || out.Render.Screen.Page != 2 {
		t.Fatalf("page 2 must list the remaining sets:\n%s", out.Render.Built.Text)
	}
	out = tap(t, c, session, VerbStickerSet, "9")
	if out.Render.Screen.StickerSet != "Set09" {
		t.Fatalf("the button index is global, got %q", out.Render.Screen.StickerSet)
	}
}

func TestExportByNameSendsTheTemplateAsADocument(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)

	out := tap(t, c, session, VerbStickerExportNew)
	if out.Input == nil || !strings.HasPrefix(out.Input.Text, AdminInputMarker) {
		t.Fatalf("export must ask for the set: %+v", out.Input)
	}

	out = reply(t, c, 80, 500, ReplyInput{Text: "  https://t.me/addstickers/Gamma \n"})
	if out.Document == nil || out.Document.Filename != "stickers-https://t.me/addstickers/Gamma.yaml" {
		t.Fatalf("the template must be delivered as a document: %+v", out.Document)
	}
	if got := admin.exports; len(got) != 1 || got[0] != "https://t.me/addstickers/Gamma" {
		t.Fatalf("the trimmed reference must reach the catalog: %v", got)
	}
	if !strings.Contains(out.Render.Built.Text, "Template sent below") || len(out.DeleteMsgIDs) != 2 {
		t.Fatalf("success refreshes the screen and cleans up the prompt: %+v", out)
	}
}

func TestExportFailuresRetryWithActionableMessages(t *testing.T) {
	cases := map[error]string{
		stickers.ErrSetNotFound:      "does not know this sticker set",
		stickers.ErrUnsupportedSet:   "regular sticker sets",
		stickers.ErrFetchUnavailable: "unavailable",
		stickers.ErrBusy:             "Another sticker operation",
		errors.New("bad reference"):  "bad reference",
	}
	for cause, want := range cases {
		t.Run(want, func(t *testing.T) {
			admin := &fakeStickerAdmin{sets: twoSets(), exportErr: cause}
			c, session := stickerController(t, admin)
			tap(t, c, session, VerbStickers)
			tap(t, c, session, VerbStickerExportNew)

			out := reply(t, c, 81, 501, ReplyInput{Text: "Gamma"})
			if out.Input == nil || !strings.Contains(out.Input.Text, want) || out.Document != nil {
				t.Fatalf("expected a retry prompt mentioning %q: %+v", want, out)
			}
		})
	}

	// A document instead of a name is an input error and never reaches the catalog.
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	tap(t, c, session, VerbStickerExportNew)
	out := reply(t, c, 82, 502, ReplyInput{Document: &DocumentInput{Filename: "x.yaml", Content: []byte("x")}})
	if out.Input == nil || len(admin.exports) != 0 {
		t.Fatalf("a document reply must be rejected before any fetch: %+v", out)
	}
}

func TestImportRejectsBadUploadsBeforeTheCatalogAndAcceptsAGoodOne(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets(), importResult: stickers.ImportResult{
		Set: "Alpha", Report: stickers.Report{Added: 2, Updated: 3, Renamed: 1, Reset: 1, Unchanged: 4, Incomplete: 2, NowUnavailable: 1, PreservedBlank: 2},
	}}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)

	big := make([]byte, stickers.MaxUploadBytes+1)
	bad := map[string]*DocumentInput{
		"no document":   nil,
		"wrong ext":     {Filename: "t.pdf", Content: []byte("x")},
		"oversize flag": {Filename: "t.yaml", Oversized: true},
		"oversize":      {Filename: "t.yaml", Content: big},
		"not utf8":      {Filename: "t.yaml", Content: []byte{0xff, 0xfe}},
	}
	msg := 90
	for name, doc := range bad {
		tap(t, c, session, VerbStickerImport)
		out := reply(t, c, msg, msg+1000, ReplyInput{Document: doc, Text: ""})
		msg++
		if out.Input == nil {
			t.Fatalf("%s must retry", name)
		}
	}
	if len(admin.imports) != 0 {
		t.Fatalf("rejected uploads must never reach the catalog: %d imports", len(admin.imports))
	}

	tap(t, c, session, VerbStickerImport)
	payload := []byte("schema_version: 1\nset: Alpha\nstickers: []\n")
	out := reply(t, c, 99, 1099, ReplyInput{Document: &DocumentInput{Filename: "Stickers-Alpha.YML", Content: payload}})
	if len(admin.imports) != 1 || string(admin.imports[0]) != string(payload) {
		t.Fatalf("the exact bytes must be imported: %v", admin.imports)
	}
	text := out.Render.Built.Text
	for _, want := range []string{"Imported Alpha", "2 added", "3 updated (1 renamed)", "1 reset", "4 unchanged", "2 sticker(s) still lack", "1 no longer in Telegram", "2 kept saved values"} {
		if !strings.Contains(text, want) {
			t.Fatalf("import notice lacks %q:\n%s", want, text)
		}
	}
	if len(out.DeleteMsgIDs) != 2 || out.Input != nil {
		t.Fatalf("success cleans up and does not re-prompt: %+v", out)
	}
}

func TestImportFailuresAreActionableAndCancelAfterThreeAttempts(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets(), importErr: &stickers.ValidationError{Issues: []string{"sticker Alpha/u1: semantic id must match ^[a-z]"}, Total: 1}}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	tap(t, c, session, VerbStickerImport)

	doc := &DocumentInput{Filename: "t.yaml", Content: []byte("x: 1")}
	out := reply(t, c, 70, 700, ReplyInput{Document: doc})
	if out.Input == nil || !strings.Contains(out.Input.Text, "Alpha/u1") || !strings.Contains(out.Input.Text, "Nothing was imported") {
		t.Fatalf("validation issues must be shown with a retry: %+v", out.Input)
	}

	admin.importErr = &stickers.ValidationError{Issues: []string{"sticker u9 is no longer in the Telegram set"}, Total: 1, Stale: true}
	out = reply(t, c, 71, 701, ReplyInput{Document: doc})
	if out.Input == nil || !strings.Contains(out.Input.Text, "Export the set again") {
		t.Fatalf("a stale template must advise re-exporting: %+v", out.Input)
	}

	admin.importErr = stickers.ErrConflict
	out = reply(t, c, 72, 702, ReplyInput{Document: doc})
	if out.Input != nil || out.Message == nil || !strings.Contains(out.Message.Text, "cancelled") {
		t.Fatalf("the third failure cancels the input: %+v", out)
	}
	if got := c.manager.Session(testAdminID).Pending; got != nil {
		t.Fatalf("the pending must be dropped after cancellation")
	}
}

func TestImportFromASetCardWorksAndRefreshesTheCard(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets(), importResult: stickers.ImportResult{Set: "Alpha"}}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	tap(t, c, session, VerbStickerSet, "0")

	if out := tap(t, c, session, VerbStickerImport); out.Input == nil {
		t.Fatalf("a card must offer import")
	}
	out := reply(t, c, 60, 600, ReplyInput{Document: &DocumentInput{Filename: "a.yaml", Content: []byte("x: 1")}})
	if out.Render == nil || out.Render.Screen.Kind != ScreenStickerSet || !strings.Contains(out.Render.Built.Text, "Imported Alpha: nothing to change") {
		t.Fatalf("the card must refresh with the result: %+v", out.Render)
	}
}

func TestSetCardReExportToggleAndRemove(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	tap(t, c, session, VerbStickerSet, "0")

	out := tap(t, c, session, VerbStickerExportCur)
	if out.Document == nil || out.Document.Filename != "stickers-Alpha.yaml" || admin.exports[0] != "Alpha" {
		t.Fatalf("re-export must deliver the set's template: %+v %v", out.Document, admin.exports)
	}

	out = tap(t, c, session, VerbStickerToggle)
	if got := admin.enableCalls; len(got) != 1 || got[0] != "Alpha=false" || !strings.Contains(out.Render.Built.Text, "Set disabled") {
		t.Fatalf("toggle must disable an enabled set: %v\n%s", got, out.Render.Built.Text)
	}
	if !strings.Contains(out.Render.Built.Text, "DISABLED") || !strings.Contains(screenButtons(out.Render.Built), "Enable set") {
		t.Fatalf("the card must reflect the disabled state:\n%s", out.Render.Built.Text)
	}
	tap(t, c, session, VerbStickerToggle)
	if admin.enableCalls[1] != "Alpha=true" {
		t.Fatalf("toggle must re-enable: %v", admin.enableCalls)
	}

	out = tap(t, c, session, VerbStickerRemove)
	if out.Render.Screen.Kind != ScreenConfirm || !strings.Contains(out.Render.Built.Text, "Alpha") || len(admin.removeCalls) != 0 {
		t.Fatalf("removal must wait for confirmation: %+v", out.Render)
	}
	out = tap(t, c, session, VerbConfirmNo)
	if len(admin.removeCalls) != 0 || out.Render.Screen.Kind != ScreenStickerSet {
		t.Fatalf("cancel must not remove: %v", admin.removeCalls)
	}

	tap(t, c, session, VerbStickerRemove)
	out = tap(t, c, session, VerbConfirmYes)
	if got := admin.removeCalls; len(got) != 1 || got[0] != "Alpha" {
		t.Fatalf("confirm must remove: %v", got)
	}
	if out.Render.Screen.Kind != ScreenStickers || strings.Contains(out.Render.Built.Text, "Alpha pack") || !strings.Contains(out.Render.Built.Text, "Beta pack") {
		t.Fatalf("after removal the refreshed list must show the remaining set:\n%s", out.Render.Built.Text)
	}
	if got := out.Render.Screen.StickerSets; len(got) != 1 || got[0] != "Beta" {
		t.Fatalf("the snapshot must be refreshed so button indexes stay valid: %v", got)
	}
}

func TestSetCardFailuresShowNoticesWithoutLeavingTheCard(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets(), enableErr: stickers.ErrBusy, exportErr: stickers.ErrFetchUnavailable}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	tap(t, c, session, VerbStickerSet, "0")

	out := tap(t, c, session, VerbStickerToggle)
	if out.Render.Screen.Kind != ScreenStickerSet || !strings.Contains(out.Render.Built.Text, "Another sticker operation") {
		t.Fatalf("a busy catalog must be reported on the card: %+v", out.Render.Built.Text)
	}
	out = tap(t, c, session, VerbStickerExportCur)
	if out.Document != nil || !strings.Contains(out.Render.Built.Text, "unavailable") {
		t.Fatalf("a failed export must not send a document: %+v", out.Document)
	}

	admin.removeErr = stickers.ErrConflict
	tap(t, c, session, VerbStickerRemove)
	out = tap(t, c, session, VerbConfirmYes)
	if out.Render == nil || !strings.Contains(out.Render.Built.Text, "Failed: The catalog changed") {
		t.Fatalf("a failed removal must surface its reason: %+v", out)
	}
}

func TestSetRemovedBehindTheAdminsBackShowsAGoneCard(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	_ = admin.RemoveSet(context.Background(), "Alpha") // removed elsewhere after the snapshot

	out := tap(t, c, session, VerbStickerSet, "0")
	if !strings.Contains(out.Render.Built.Text, "no longer in the catalog") {
		t.Fatalf("a vanished set must render a gone card:\n%s", out.Render.Built.Text)
	}
	if out = tap(t, c, session, VerbStickerToggle); !strings.Contains(out.Render.Built.Text, "no longer in the catalog") || len(admin.enableCalls) != 0 {
		t.Fatalf("a vanished set cannot be toggled")
	}
}

func TestStickerCallbacksFitTheLimit(t *testing.T) {
	for verb, args := range map[string][]string{
		VerbStickers: {"99"}, VerbStickerSet: {"99999"}, VerbStickerExportNew: nil, VerbStickerExportCur: nil,
		VerbStickerImport: nil, VerbStickerToggle: nil, VerbStickerRemove: nil,
	} {
		data := EncodeAction(verb, 4294967295, args...)
		if len(data) > MaxCallbackBytes {
			t.Fatalf("%s callback is %d bytes: %q", verb, len(data), data)
		}
		action, err := DecodeAction(data)
		if err != nil || action.Verb != verb || strings.Join(action.Args, ",") != strings.Join(args, ",") {
			t.Fatalf("round trip %s: %+v %v", verb, action, err)
		}
	}
}

func TestZipExportButtonsAreOfferedAlongsideYAML(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)

	out := tap(t, c, session, VerbStickers)
	buttons := screenButtons(out.Render.Built)
	for _, want := range []string{"Export YAML…", "Export ZIP…", "Import YAML"} {
		if !strings.Contains(buttons, want) {
			t.Fatalf("stickers screen lacks %q:\n%s", want, buttons)
		}
	}
	out = tap(t, c, session, VerbStickerSet, "0")
	buttons = screenButtons(out.Render.Built)
	for _, want := range []string{"Export YAML", "Export ZIP", "Import YAML"} {
		if !strings.Contains(buttons, want) {
			t.Fatalf("set card lacks %q:\n%s", want, buttons)
		}
	}
}

func TestZipVerbsRejectWrongScreensAndNeverReachTheCatalog(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)

	for _, verb := range []string{VerbStickerZipNew, VerbStickerZipCur} {
		if out := tap(t, c, session, verb); out.Answer != AnswerUnknown || out.Archive != nil {
			t.Fatalf("%s from the dashboard must be rejected: %+v", verb, out)
		}
	}
	tap(t, c, session, VerbStickers)
	if out := tap(t, c, session, VerbStickerZipCur); out.Answer != AnswerUnknown || out.Archive != nil {
		t.Fatalf("set-card ZIP from the list must be rejected: %+v", out)
	}
	if len(admin.archives) != 0 {
		t.Fatalf("rejected verbs must not export")
	}

	plain, _ := newTestController(t)
	ps := mustOpen(t, plain)
	for _, verb := range []string{VerbStickerZipNew, VerbStickerZipCur} {
		if out := tap(t, plain, ps, verb); out.Answer != AnswerUnknown || out.Render != nil {
			t.Fatalf("%s must be inert without a catalog: %+v", verb, out)
		}
	}
}

func TestZipFromASetCardDefersTheWorkOutsideTheSessionLock(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	tap(t, c, session, VerbStickerSet, "0")

	out := tap(t, c, session, VerbStickerZipCur)
	if out.Archive == nil || out.Archive.Set != "Alpha" {
		t.Fatalf("the verb must only request the job: %+v", out.Archive)
	}
	if len(admin.archives) != 0 || out.Document != nil {
		t.Fatal("no export may run inside the session commit")
	}
	if out.Render == nil || !strings.Contains(out.Render.Built.Text, "Preparing the ZIP of Alpha") {
		t.Fatalf("the screen must say the ZIP is being prepared: %+v", out.Render)
	}

	// Deferred phase: runs with the session free, so the session is usable
	// from inside the (slow) export.
	admin.archiveHook = func() {
		if _, ok := c.manager.ActiveSession(testAdminID); !ok {
			t.Error("session must stay available during the export")
		}
		if got := tap(t, c, session, VerbBack); got.Answer == AnswerMenuGone {
			t.Error("the menu must stay usable during the export")
		}
	}
	done := c.RunStickerArchive(context.Background(), testAdminID, *out.Archive)
	if done.Document == nil || done.Document.Filename != "Alpha.zip" || admin.archives[0] != "Alpha" {
		t.Fatalf("the job must deliver the archive: %+v", done)
	}
}

func TestZipByNameParsesInsideThenRunsAfter(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)

	out := tap(t, c, session, VerbStickerZipNew)
	if out.Input == nil || !strings.Contains(out.Input.Text, "ZIP") {
		t.Fatalf("ZIP export must ask for the set: %+v", out.Input)
	}
	out = reply(t, c, 110, 910, ReplyInput{Text: " https://t.me/addstickers/Gamma "})
	if out.Archive == nil || out.Archive.Set != "Gamma" || len(admin.archives) != 0 {
		t.Fatalf("the reply must request a job for the parsed name: %+v", out.Archive)
	}
	if len(out.DeleteMsgIDs) != 2 || out.Render == nil || !strings.Contains(out.Render.Built.Text, "Preparing the ZIP of Gamma") {
		t.Fatalf("prompt cleanup and notice expected: %+v", out)
	}
	if got := c.manager.Session(testAdminID).Pending; got != nil {
		t.Fatal("the pending must be dropped once the job is accepted")
	}
}

func TestZipByNameRejectsBadInputWithARetry(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, session := stickerController(t, admin)
	tap(t, c, session, VerbStickers)
	tap(t, c, session, VerbStickerZipNew)

	out := reply(t, c, 111, 911, ReplyInput{Text: "not a valid/name!"})
	if out.Input == nil || out.Archive != nil || !strings.Contains(out.Input.Text, "ZIP") {
		t.Fatalf("a malformed reference must retry the ZIP prompt: %+v", out)
	}
	out = reply(t, c, 112, 912, ReplyInput{Document: &DocumentInput{Filename: "x.zip", Content: []byte("x")}})
	if out.Input == nil || out.Archive != nil {
		t.Fatalf("a document reply must be rejected: %+v", out)
	}
	if len(admin.archives) != 0 {
		t.Fatal("rejected input must not export")
	}
}

func TestRunStickerArchiveFailuresAreActionableAndSendNothing(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"download": {&stickers.DownloadError{FileUniqueID: "AgADx", Reason: stickers.ReasonUnavailable}, "AgADx"},
		"timeout":  {stickers.ErrArchiveTimeout, "took too long"},
		"large":    {stickers.ErrArchiveTooLarge, "too large"},
		"busy":     {stickers.ErrBusy, "Another sticker operation"},
		"notfound": {stickers.ErrSetNotFound, "does not know"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			admin := &fakeStickerAdmin{sets: twoSets(), archiveErr: tc.err}
			c, _ := stickerController(t, admin)
			out := c.RunStickerArchive(context.Background(), testAdminID, StickerArchiveJob{Set: "Alpha"})
			if out.Document != nil || out.Message == nil || !strings.Contains(out.Message.Text, tc.want) || !strings.Contains(out.Message.Text, "No archive was sent") {
				t.Fatalf("expected an actionable failure message: %+v", out)
			}
		})
	}
}

func TestRunStickerArchiveRechecksAdmin(t *testing.T) {
	admin := &fakeStickerAdmin{sets: twoSets()}
	c, _ := stickerController(t, admin)
	if out := c.RunStickerArchive(context.Background(), testAdminID+1, StickerArchiveJob{Set: "Alpha"}); out.Document != nil || out.Message != nil || len(admin.archives) != 0 {
		t.Fatalf("a non-admin must get nothing: %+v", out)
	}
}

func TestZipVerbCodecFitsCallbackLimit(t *testing.T) {
	for _, verb := range []string{VerbStickerZipNew, VerbStickerZipCur} {
		data := EncodeAction(verb, 4294967295)
		if len(data) > MaxCallbackBytes {
			t.Fatalf("%s callback too long: %d", verb, len(data))
		}
		if _, err := DecodeAction(data); err != nil {
			t.Fatalf("%s must decode: %v", verb, err)
		}
	}
}
