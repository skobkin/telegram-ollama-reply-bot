package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	psqlite "telegram-ollama-reply-bot/internal/persistence/sqlite"
	"telegram-ollama-reply-bot/internal/stickers"

	tg "github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	"go.yaml.in/yaml/v3"
)

type fakeStickerAPI struct {
	sets map[string]*tg.StickerSet
	err  error
}

func (f *fakeStickerAPI) GetStickerSet(_ context.Context, p *tg.GetStickerSetParams) (*tg.StickerSet, error) {
	if f.err != nil {
		return nil, f.err
	}
	set, ok := f.sets[p.Name]
	if !ok {
		return nil, &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: STICKERSET_INVALID"}
	}

	return set, nil
}

func tgSet(name string, uniques ...string) *tg.StickerSet {
	set := &tg.StickerSet{Name: name, Title: strings.ToUpper(name), StickerType: tg.StickerTypeRegular}
	for _, u := range uniques {
		set.Stickers = append(set.Stickers, tg.Sticker{FileID: "file-" + u, FileUniqueID: u, Type: tg.StickerTypeRegular, Emoji: "🦊"})
	}

	return set
}

type stickerHarness struct {
	t     *testing.T
	path  string
	store *psqlite.Store
	api   *fakeStickerAPI
	svc   *stickers.Service
}

func newStickerHarness(t *testing.T, api *fakeStickerAPI) *stickerHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	h := &stickerHarness{t: t, path: path, api: api, store: openStatsStore(t, path)}
	h.restart()

	return h
}

// restart builds a fresh service over the same database, as a bot restart does.
func (h *stickerHarness) restart() {
	h.t.Helper()
	h.svc = stickers.NewService(stickerCatalogStore{store: h.store}, telegoStickerFetcher{api: h.api}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := h.svc.Load(context.Background()); err != nil {
		h.t.Fatalf("Load: %v", err)
	}
}

// edit exports the set, applies the mutation to the parsed template and
// imports the result: the real admin round trip.
func (h *stickerHarness) edit(set string, mutate func(*stickers.Template)) (stickers.ImportResult, error) {
	h.t.Helper()

	return h.editWith(h.svc, set, mutate)
}

func (h *stickerHarness) editWith(svc *stickers.Service, set string, mutate func(*stickers.Template)) (stickers.ImportResult, error) {
	h.t.Helper()
	_, raw, err := svc.Export(context.Background(), set)
	if err != nil {
		h.t.Fatalf("Export: %v", err)
	}
	tmpl, err := stickers.ParseTemplate(raw)
	if err != nil {
		h.t.Fatalf("exported template must parse: %v", err)
	}
	mutate(tmpl)
	edited, err := yaml.Marshal(tmpl)
	if err != nil {
		h.t.Fatalf("marshal: %v", err)
	}

	return svc.Import(context.Background(), edited)
}

func (h *stickerHarness) mustEdit(set string, mutate func(*stickers.Template)) stickers.ImportResult {
	h.t.Helper()
	res, err := h.edit(set, mutate)
	if err != nil {
		h.t.Fatalf("import: %v", err)
	}

	return res
}

func (h *stickerHarness) ids(set string) map[string]string {
	h.t.Helper()
	_, entries, err := h.store.LoadStickerCatalog(context.Background())
	if err != nil {
		h.t.Fatalf("load catalog: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.SetName == set {
			out[e.FileUniqueID] = e.SemanticID
		}
	}

	return out
}

func sticker(tm *stickers.Template, unique string) *stickers.TemplateSticker {
	for i := range tm.Stickers {
		if tm.Stickers[i].FileUniqueID == unique {
			return &tm.Stickers[i]
		}
	}

	return &stickers.TemplateSticker{}
}

func annotate(tm *stickers.Template, unique, id string) {
	s := sticker(tm, unique)
	s.ID, s.Description = id, "desc "+id
}

func TestStickerCatalogManyUnannotatedStickersCoexistAcrossSets(t *testing.T) {
	h := newStickerHarness(t, &fakeStickerAPI{sets: map[string]*tg.StickerSet{
		"Alpha": tgSet("Alpha", "a1", "a2", "a3", "a4", "a5"),
		"Beta":  tgSet("Beta", "b1", "b2", "b3"),
	}})

	h.mustEdit("Alpha", func(*stickers.Template) {})
	h.mustEdit("Beta", func(*stickers.Template) {})
	if st := h.svc.Status(); st.Sets != 2 || st.Entries != 8 || st.Incomplete != 8 || st.Active != 0 {
		t.Fatalf("unexpected status after blank imports: %+v", st)
	}

	// Annotate one, then reset it again, then annotate another.
	h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "a1", "alpha_one") })
	h.mustEdit("Alpha", func(tm *stickers.Template) { sticker(tm, "a1").Reset = boolPtr(true) })
	h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "a2", "alpha_two") })

	if ids := h.ids("Alpha"); ids["a1"] != "" || ids["a2"] != "alpha_two" {
		t.Fatalf("unexpected ids %v", ids)
	}
	h.restart()
	if h.svc.Snapshot().Len() != 1 {
		t.Fatalf("exactly alpha_two must be active after a restart")
	}
}

func boolPtr(b bool) *bool { return &b }

func TestStickerCatalogResetAndReuseIDInOneImport(t *testing.T) {
	h := newStickerHarness(t, &fakeStickerAPI{sets: map[string]*tg.StickerSet{"Alpha": tgSet("Alpha", "a", "b")}})
	h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "a", "shared_id") })

	res := h.mustEdit("Alpha", func(tm *stickers.Template) {
		sticker(tm, "a").Reset = boolPtr(true) // populated repeat-export entry, only the flag flips
		annotate(tm, "b", "shared_id")
	})
	if res.Report.Reset != 1 {
		t.Fatalf("unexpected report: %+v", res.Report)
	}
	if ids := h.ids("Alpha"); ids["a"] != "" || ids["b"] != "shared_id" {
		t.Fatalf("the id must move from a to b: %v", ids)
	}
	if _, ok := h.svc.Snapshot().Resolve("shared_id"); !ok {
		t.Fatalf("shared_id must resolve after the move")
	}
}

func TestStickerCatalogSwapIDsInOneImport(t *testing.T) {
	h := newStickerHarness(t, &fakeStickerAPI{sets: map[string]*tg.StickerSet{"Alpha": tgSet("Alpha", "a", "b")}})
	h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "a", "id_one"); annotate(tm, "b", "id_two") })

	res := h.mustEdit("Alpha", func(tm *stickers.Template) {
		a, b := sticker(tm, "a"), sticker(tm, "b")
		a.ID, b.ID = b.ID, a.ID
	})
	if res.Report.Renamed != 2 {
		t.Fatalf("unexpected report: %+v", res.Report)
	}
	if ids := h.ids("Alpha"); ids["a"] != "id_two" || ids["b"] != "id_one" {
		t.Fatalf("ids must be swapped: %v", ids)
	}
}

func TestStickerCatalogReuseOfAForgottenMissingStickersID(t *testing.T) {
	api := &fakeStickerAPI{sets: map[string]*tg.StickerSet{"Alpha": tgSet("Alpha", "a", "gone")}}
	h := newStickerHarness(t, api)
	h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "gone", "legacy_id") })

	// "gone" disappears from Telegram: retained as unavailable with its annotation.
	api.sets["Alpha"] = tgSet("Alpha", "a")
	res := h.mustEdit("Alpha", func(tm *stickers.Template) {
		if len(tm.Missing) != 1 || tm.Missing[0].ID != "legacy_id" {
			t.Fatalf("the vanished sticker must be disclosed under missing: %+v", tm.Missing)
		}
	})
	if res.Report.NowUnavailable != 1 {
		t.Fatalf("unexpected report: %+v", res.Report)
	}
	if h.ids("Alpha")["gone"] != "legacy_id" {
		t.Fatalf("the annotation of a missing sticker must be retained")
	}

	res = h.mustEdit("Alpha", func(tm *stickers.Template) {
		tm.Missing[0].Forget = boolPtr(true)
		annotate(tm, "a", "legacy_id")
	})
	if res.Report.Forgotten != 1 {
		t.Fatalf("unexpected report: %+v", res.Report)
	}
	if ids := h.ids("Alpha"); len(ids) != 1 || ids["a"] != "legacy_id" {
		t.Fatalf("legacy_id must be reassigned and the forgotten row deleted: %v", ids)
	}
}

func TestStickerCatalogCollisionAcrossSetsIsRejectedBeforeAnyWrite(t *testing.T) {
	h := newStickerHarness(t, &fakeStickerAPI{sets: map[string]*tg.StickerSet{"Alpha": tgSet("Alpha", "a"), "Beta": tgSet("Beta", "b")}})
	h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "a", "taken_id") })

	_, err := h.edit("Beta", func(tm *stickers.Template) { annotate(tm, "b", "taken_id") })
	var verr *stickers.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if ids := h.ids("Beta"); len(ids) != 0 {
		t.Fatalf("a rejected import must not create the set: %v", ids)
	}
	if ids := h.ids("Alpha"); ids["a"] != "taken_id" {
		t.Fatalf("existing annotations must be untouched: %v", ids)
	}
}

func TestStickerCatalogStaleServiceConflictsAndLeavesEverythingUnchanged(t *testing.T) {
	h := newStickerHarness(t, &fakeStickerAPI{sets: map[string]*tg.StickerSet{"Alpha": tgSet("Alpha", "a", "b")}})
	h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "a", "first_id") })

	// A second service instance (as if another replica or a restart race)
	// changes the set after the first one loaded it.
	other := stickers.NewService(stickerCatalogStore{store: h.store}, telegoStickerFetcher{api: h.api}, nil)
	if err := other.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := h.editWith(other, "Alpha", func(tm *stickers.Template) { annotate(tm, "b", "second_id") }); err != nil {
		t.Fatalf("other import: %v", err)
	}

	before := h.svc.Snapshot()
	_, err := h.edit("Alpha", func(tm *stickers.Template) { sticker(tm, "a").Reset = boolPtr(true) })
	if !errors.Is(err, stickers.ErrConflict) {
		t.Fatalf("a stale revision must conflict, got %v", err)
	}
	if h.svc.Snapshot() != before {
		t.Fatalf("a conflicting import must not change the published index")
	}
	if ids := h.ids("Alpha"); ids["a"] != "first_id" || ids["b"] != "second_id" {
		t.Fatalf("the database must keep both committed annotations: %v", ids)
	}
}

func TestStickerCatalogLaterImportsKeepEverythingStoredAcrossRestarts(t *testing.T) {
	api := &fakeStickerAPI{sets: map[string]*tg.StickerSet{"Alpha": tgSet("Alpha", "keep", "gone", "blank"), "Beta": tgSet("Beta", "x")}}
	h := newStickerHarness(t, api)
	h.mustEdit("Alpha", func(tm *stickers.Template) {
		annotate(tm, "keep", "keep_id")
		annotate(tm, "gone", "gone_id")
		sticker(tm, "keep").Enabled = boolPtr(false)
	})
	api.sets["Alpha"] = tgSet("Alpha", "keep", "blank")
	h.mustEdit("Alpha", func(*stickers.Template) {})

	h.restart() // disabled, incomplete and unavailable rows must all be in memory again
	h.mustEdit("Beta", func(tm *stickers.Template) { annotate(tm, "x", "beta_id") })
	h.mustEdit("Alpha", func(tm *stickers.Template) {
		if s := sticker(tm, "keep"); s.ID != "keep_id" || s.Enabled == nil || *s.Enabled {
			t.Fatalf("the repeat export must prefill the disabled sticker: %+v", s)
		}
	})

	ids := h.ids("Alpha")
	if ids["keep"] != "keep_id" || ids["gone"] != "gone_id" || ids["blank"] != "" {
		t.Fatalf("a later import must not lose annotations: %v", ids)
	}
	if h.svc.Snapshot().Len() != 1 { // only beta_id: keep is disabled, gone is unavailable
		t.Fatalf("expected only beta_id to be active, got %d", h.svc.Snapshot().Len())
	}
}

func corruptRow(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}
}

func TestStickerCatalogRestoreFailsOnInvalidPersistedRows(t *testing.T) {
	for name, statement := range map[string]string{
		"bad tags json":    `UPDATE sticker_entries SET tags = 'not json'`,
		"invalid semantic": `UPDATE sticker_entries SET semantic_id = 'Bad ID' WHERE file_unique_id = 'a'`,
		"oversized text":   `UPDATE sticker_entries SET description = replace(hex(zeroblob(200)), '0', 'я') WHERE file_unique_id = 'a'`,
		"unknown format":   `PRAGMA ignore_check_constraints = ON; UPDATE sticker_entries SET format = 'gif'`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newStickerHarness(t, &fakeStickerAPI{sets: map[string]*tg.StickerSet{"Alpha": tgSet("Alpha", "a")}})
			h.mustEdit("Alpha", func(tm *stickers.Template) { annotate(tm, "a", "good_id") })
			corruptRow(t, h.path, statement)

			fresh := stickers.NewService(stickerCatalogStore{store: h.store}, telegoStickerFetcher{api: h.api}, nil)
			err := fresh.Load(context.Background())
			if err == nil {
				t.Fatalf("restore must fail on %s", name)
			}
			if strings.Contains(err.Error(), "desc good_id") {
				t.Fatalf("restore error leaked annotation text: %v", err)
			}
			if fresh.Snapshot().Len() != 0 {
				t.Fatalf("nothing may be advertised after a failed restore")
			}
		})
	}
}

func TestTelegoStickerFetcherMapsSetsAndErrors(t *testing.T) {
	ctx := context.Background()
	mixed := tgSet("Mixed", "s", "an", "vid")
	mixed.Stickers[1].IsAnimated = true
	mixed.Stickers[2].IsVideo = true
	api := &fakeStickerAPI{sets: map[string]*tg.StickerSet{
		"Mixed": mixed,
		"Masks": {Name: "Masks", StickerType: tg.StickerTypeMask, Stickers: []tg.Sticker{{FileUniqueID: "m"}}},
		"Emoji": {Name: "Emoji", StickerType: tg.StickerTypeCustomEmoji},
		"Odd":   {Name: "Odd", StickerType: tg.StickerTypeRegular, Stickers: []tg.Sticker{{FileUniqueID: "m", Type: tg.StickerTypeMask}}},
	}}
	fetcher := telegoStickerFetcher{api: api}

	set, err := fetcher.FetchSet(ctx, "Mixed")
	if err != nil || len(set.Stickers) != 3 {
		t.Fatalf("FetchSet: %v %+v", err, set)
	}
	if set.Stickers[0].Format != stickers.FormatStatic || set.Stickers[1].Format != stickers.FormatAnimated || set.Stickers[2].Format != stickers.FormatVideo {
		t.Fatalf("formats are per sticker: %+v", set.Stickers)
	}
	for _, name := range []string{"Masks", "Emoji", "Odd"} {
		if _, err := fetcher.FetchSet(ctx, name); !errors.Is(err, stickers.ErrUnsupportedSet) {
			t.Fatalf("%s must be unsupported, got %v", name, err)
		}
	}
	if _, err := fetcher.FetchSet(ctx, "Nowhere"); !errors.Is(err, stickers.ErrSetNotFound) {
		t.Fatalf("expected ErrSetNotFound, got %v", err)
	}

	api.err = errors.New("Post \"https://api.telegram.org/bot123456:SECRET-TOKEN/getStickerSet\": dial tcp: refused")
	_, err = fetcher.FetchSet(ctx, "Mixed")
	if !errors.Is(err, stickers.ErrFetchUnavailable) {
		t.Fatalf("expected ErrFetchUnavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") || strings.Contains(err.Error(), "api.telegram.org") {
		t.Fatalf("the fetch error must not carry the request URL: %v", err)
	}
	api.err = &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}
	if _, err = fetcher.FetchSet(ctx, "Mixed"); !errors.Is(err, stickers.ErrFetchUnavailable) {
		t.Fatalf("a rate limit is an unavailable fetch, got %v", err)
	}
}
