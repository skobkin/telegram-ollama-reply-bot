package stickers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu         sync.Mutex
	state      State
	loadErr    error
	replaceErr error
	enableErr  error
	deleteErr  error
	calls      []string
	// onReplace runs inside ReplaceSet, before it returns, to observe what is
	// visible to readers while the commit is in flight.
	onReplace func()
}

func (f *fakeStore) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

func (f *fakeStore) LoadCatalog(context.Context) (State, error) {
	f.record("load")

	return f.state, f.loadErr
}

func (f *fakeStore) ReplaceSet(context.Context, *ImportPlan) error {
	f.record("replace")
	if f.onReplace != nil {
		f.onReplace()
	}

	return f.replaceErr
}

func (f *fakeStore) SetEnabled(context.Context, string, bool, int64, time.Time) error {
	f.record("enable")

	return f.enableErr
}

func (f *fakeStore) DeleteSet(context.Context, string, int64) error {
	f.record("delete")

	return f.deleteErr
}

type fakeFetcher struct {
	mu      sync.Mutex
	sets    map[string]RemoteSet
	err     error
	fetches int
	// entered signals each fetch start; release (when set) parks fetches.
	entered chan struct{}
	release chan struct{}
}

func (f *fakeFetcher) FetchSet(ctx context.Context, name string) (RemoteSet, error) {
	f.mu.Lock()
	f.fetches++
	set, ok := f.sets[name]
	err := f.err
	entered, release := f.entered, f.release
	f.mu.Unlock()

	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return RemoteSet{}, ctx.Err()
		}
	}
	if err != nil {
		return RemoteSet{}, err
	}
	if !ok {
		return RemoteSet{}, ErrSetNotFound
	}

	return set, nil
}

func newTestService(t *testing.T, store *fakeStore, fetcher *fakeFetcher) *Service {
	t.Helper()
	svc := NewService(store, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return testNow }
	if err := svc.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	store.calls = nil

	return svc
}

func kitsuFetcher() *fakeFetcher {
	return &fakeFetcher{sets: map[string]RemoteSet{"Kitsu": remoteSet("Kitsu", "a", "b", "c")}}
}

func importYAML(t *testing.T, mutate func(*Template)) []byte {
	t.Helper()
	tmpl := tmplOf("Kitsu", TemplateSticker{FileUniqueID: "a"}, TemplateSticker{FileUniqueID: "b"}, TemplateSticker{FileUniqueID: "c"})
	tmpl.CatalogVersion = AnnotationVersion(baseCatalog(t), "Kitsu") // exported from baseCatalog
	mutate(tmpl)
	raw, err := marshalForTest(tmpl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return raw
}

func TestLoadPublishesCompleteCatalogAndIndex(t *testing.T) {
	store := &fakeStore{state: baseCatalog(t)}
	svc := newTestService(t, store, kitsuFetcher())

	if svc.Snapshot().Len() != 2 {
		t.Fatalf("expected 2 active stickers, got %d", svc.Snapshot().Len())
	}
	if st := svc.Status(); st.Sets != 1 || st.Entries != 3 || st.Active != 2 || st.Incomplete != 1 {
		t.Fatalf("unexpected status %+v", st)
	}
}

func TestLoadFailsOnStoreErrorOrInvalidRowsWithoutLeaking(t *testing.T) {
	bad := baseCatalog(t)
	bad.Entries[0].SemanticID = "Not Valid"
	bad.Entries[0].Description = "TOPSECRET"

	for name, store := range map[string]*fakeStore{
		"store error":  {loadErr: errors.New("disk on fire")},
		"invalid data": {state: bad},
	} {
		t.Run(name, func(t *testing.T) {
			svc := NewService(store, kitsuFetcher(), nil)
			err := svc.Load(context.Background())
			if err == nil {
				t.Fatalf("Load must fail")
			}
			if strings.Contains(err.Error(), "TOPSECRET") {
				t.Fatalf("restore error leaked annotation text: %v", err)
			}
			if svc.Snapshot().Len() != 0 {
				t.Fatalf("nothing may be advertised after a failed restore")
			}
		})
	}
}

func TestExportIsReadOnlyAndDoesNotChangeTheCatalog(t *testing.T) {
	store := &fakeStore{state: baseCatalog(t)}
	svc := newTestService(t, store, kitsuFetcher())
	before := svc.Snapshot()

	name, data, err := svc.Export(context.Background(), "https://t.me/addstickers/Kitsu")
	if err != nil || name != "stickers-Kitsu.yaml" || !strings.Contains(string(data), "id_a") {
		t.Fatalf("export: %q %v\n%s", name, err, data)
	}
	if store.callCount() != 0 {
		t.Fatalf("export must not touch the store, calls: %v", store.calls)
	}
	if svc.Snapshot() != before {
		t.Fatalf("export must not republish the index")
	}
}

func TestExportErrorsAreActionable(t *testing.T) {
	store := &fakeStore{}
	svc := newTestService(t, store, kitsuFetcher())

	if _, _, err := svc.Export(context.Background(), "not a set!"); err == nil {
		t.Fatalf("a malformed reference must be rejected")
	}
	if _, _, err := svc.Export(context.Background(), "Missing"); !errors.Is(err, ErrSetNotFound) {
		t.Fatalf("expected ErrSetNotFound, got %v", err)
	}
	for _, want := range []error{ErrUnsupportedSet, ErrFetchUnavailable} {
		f := kitsuFetcher()
		f.err = want
		svc := newTestService(t, &fakeStore{}, f)
		if _, _, err := svc.Export(context.Background(), "Kitsu"); !errors.Is(err, want) {
			t.Fatalf("expected %v, got %v", want, err)
		}
	}
}

func TestExportConcurrencyIsBounded(t *testing.T) {
	f := kitsuFetcher()
	f.entered, f.release = make(chan struct{}, 8), make(chan struct{})
	svc := newTestService(t, &fakeStore{}, f)

	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentExports; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = svc.Export(context.Background(), "Kitsu")
		}()
	}
	for i := 0; i < maxConcurrentExports; i++ {
		<-f.entered // parked inside the fetcher, holding an export slot
	}

	if _, _, err := svc.Export(context.Background(), "Kitsu"); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy over the export bound, got %v", err)
	}
	close(f.release)
	wg.Wait()
	if _, _, err := svc.Export(context.Background(), "Kitsu"); err != nil {
		t.Fatalf("slots must be released: %v", err)
	}
}

func TestImportPublishesOnlyAfterCommit(t *testing.T) {
	store := &fakeStore{state: baseCatalog(t)}
	svc := newTestService(t, store, kitsuFetcher())
	before := svc.Snapshot()

	var duringCommit *Index
	store.onReplace = func() { duringCommit = svc.Snapshot() }

	res, err := svc.Import(context.Background(), importYAML(t, func(tm *Template) {
		tm.Stickers[2].ID, tm.Stickers[2].Description = "id_c", "Fox C"
	}))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if duringCommit != before {
		t.Fatalf("the new index must not be visible while the commit is in flight")
	}
	if res.Set != "Kitsu" || res.Report.Updated != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
	if _, ok := svc.Snapshot().Resolve("id_c"); !ok || svc.Snapshot() == before {
		t.Fatalf("the committed index must be published")
	}
}

func TestImportCommitFailureLeavesCatalogAndIndexUntouched(t *testing.T) {
	for name, storeErr := range map[string]error{"write failure": errors.New("disk full"), "conflict": ErrConflict} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{state: baseCatalog(t), replaceErr: storeErr}
			svc := newTestService(t, store, kitsuFetcher())
			before, stateBefore := svc.Snapshot(), svc.current()

			_, err := svc.Import(context.Background(), importYAML(t, func(tm *Template) { tm.Stickers[0].Reset = boolp(true) }))
			if err == nil {
				t.Fatalf("expected the failed commit to surface")
			}
			if errors.Is(storeErr, ErrConflict) && !errors.Is(err, ErrConflict) {
				t.Fatalf("a revision conflict must map to ErrConflict, got %v", err)
			}
			if svc.Snapshot() != before {
				t.Fatalf("a failed commit must not change the index")
			}
			if _, ok := svc.Snapshot().Resolve("id_a"); !ok {
				t.Fatalf("the old snapshot must keep serving id_a")
			}
			if got := svc.current(); len(got.Entries) != len(stateBefore.Entries) || got.Entries[0].SemanticID != "id_a" {
				t.Fatalf("a failed commit must not change the in-memory catalog: %+v", got.Entries[0])
			}
		})
	}
}

func TestImportValidationFailureMakesNoStoreCall(t *testing.T) {
	store := &fakeStore{state: baseCatalog(t)}
	svc := newTestService(t, store, kitsuFetcher())

	_, err := svc.Import(context.Background(), importYAML(t, func(tm *Template) {
		tm.Stickers[0].ID = "Bad ID"
		tm.Stickers[0].Description = "x"
	}))
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	if store.callCount() != 0 {
		t.Fatalf("validation must precede any database call: %v", store.calls)
	}
}

func TestImportRejectsUnparsableInputBeforeAnyNetworkCall(t *testing.T) {
	f := kitsuFetcher()
	svc := newTestService(t, &fakeStore{}, f)
	if _, err := svc.Import(context.Background(), []byte("not: [valid")); err == nil {
		t.Fatalf("expected a parse error")
	}
	if f.fetches != 0 {
		t.Fatalf("a malformed upload must not reach Telegram")
	}
}

func TestSecondConcurrentImportGetsBusy(t *testing.T) {
	f := kitsuFetcher()
	f.entered, f.release = make(chan struct{}, 2), make(chan struct{})
	store := &fakeStore{}
	svc := newTestService(t, store, f)
	raw := importYAML(t, func(*Template) {})

	done := make(chan error, 1)
	go func() {
		_, err := svc.Import(context.Background(), raw)
		done <- err
	}()
	<-f.entered // the first import now holds the operation lock inside the fetcher

	if _, err := svc.Import(context.Background(), raw); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	if err := svc.SetEnabled(context.Background(), "Kitsu", false); !errors.Is(err, ErrBusy) {
		t.Fatalf("other mutations are serialized too, got %v", err)
	}
	close(f.release)
	if err := <-done; err != nil {
		t.Fatalf("first import: %v", err)
	}
}

func TestSetEnabledAndRemoveSet(t *testing.T) {
	store := &fakeStore{state: baseCatalog(t)}
	svc := newTestService(t, store, kitsuFetcher())

	if err := svc.SetEnabled(context.Background(), "Nope", false); !errors.Is(err, ErrUnknownSet) {
		t.Fatalf("expected ErrUnknownSet, got %v", err)
	}
	if err := svc.SetEnabled(context.Background(), "Kitsu", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if svc.Snapshot().Len() != 0 {
		t.Fatalf("a disabled set must leave the index")
	}
	if got := svc.current().Sets[0]; got.Enabled || got.Revision != 2 {
		t.Fatalf("set state after disable: %+v", got)
	}
	if err := svc.SetEnabled(context.Background(), "Kitsu", true); err != nil || svc.Snapshot().Len() != 2 {
		t.Fatalf("re-enable must restore the stickers: %v %d", err, svc.Snapshot().Len())
	}

	if err := svc.RemoveSet(context.Background(), "Kitsu"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if svc.Snapshot().Len() != 0 || len(svc.current().Entries) != 0 {
		t.Fatalf("a removed set must be gone from memory")
	}
}

func TestSetEnabledAndRemoveFailuresKeepTheOldSnapshot(t *testing.T) {
	store := &fakeStore{state: baseCatalog(t), enableErr: errors.New("boom"), deleteErr: ErrConflict}
	svc := newTestService(t, store, kitsuFetcher())
	before := svc.Snapshot()

	if err := svc.SetEnabled(context.Background(), "Kitsu", false); err == nil {
		t.Fatalf("expected failure")
	}
	if err := svc.RemoveSet(context.Background(), "Kitsu"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if svc.Snapshot() != before || svc.Snapshot().Len() != 2 {
		t.Fatalf("failed writes must not change the published index")
	}
}

func TestSetsSummaryCountsEntryStates(t *testing.T) {
	st := baseCatalog(t)
	off := annotated("d", "id_d", "Fox D")
	off.Enabled = false
	gone := annotated("e", "id_e", "Fox E")
	gone.Available = false
	st.Entries = append(st.Entries, withSet(off), withSet(gone))
	svc := newTestService(t, &fakeStore{state: st}, kitsuFetcher())

	sums := svc.Sets()
	if len(sums) != 1 {
		t.Fatalf("unexpected sets: %+v", sums)
	}
	if s := sums[0]; s.Total != 5 || s.Active != 2 || s.Incomplete != 1 || s.Disabled != 1 || s.Unavailable != 1 {
		t.Fatalf("unexpected summary: %+v", s)
	}
}

func withSet(e Entry) Entry {
	e.SetName, e.FileID, e.Format, e.Position = "Kitsu", "f-"+e.FileUniqueID, FormatStatic, 10

	return e
}
