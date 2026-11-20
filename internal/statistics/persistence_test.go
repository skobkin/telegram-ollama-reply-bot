package statistics

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

// ---- fakes ----

type fakeDataset struct {
	mu         sync.Mutex
	name       string
	version    int
	tr         persistTracker
	content    string
	restoreErr error
	info       PersistenceInfo
	restores   int
}

func newFakeDataset(_ string) *fakeDataset {
	return &fakeDataset{name: "fake", version: 1, tr: newPersistTracker()}
}

func (d *fakeDataset) Name() string       { return d.name }
func (d *fakeDataset) FormatVersion() int { return d.version }

func (d *fakeDataset) Capture() (PersistPair, func() ([]byte, error), bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := d.content

	return d.tr.current, func() ([]byte, error) { return []byte(snapshot), nil }, d.tr.dirty()
}

func (d *fakeDataset) Acknowledge(pair PersistPair) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tr.ack(pair)
}

func (d *fakeDataset) Restore(_ int, pair PersistPair, payload []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restores++
	if d.restoreErr != nil {
		return d.restoreErr
	}
	d.tr.install(pair)
	d.content = string(payload)

	return nil
}

func (d *fakeDataset) SetPersistenceInfo(info PersistenceInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.info = info
}

func (d *fakeDataset) mutate(content string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tr.touch()
	d.content = content
}

func (d *fakeDataset) reset() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.content = ""

	return d.tr.reset()
}

func (d *fakeDataset) pairs() (current, saved PersistPair) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.tr.current, d.tr.saved
}

func (d *fakeDataset) dirty() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.tr.dirty()
}

type fakeStore struct {
	mu          sync.Mutex
	rows        map[string]SnapshotRow
	metaErr     error
	saveErr     error
	quarErr     error
	saveBlock   chan struct{} // when non-nil, SaveSnapshot parks on it or ctx
	saveDeaf    bool          // SaveSnapshot ignores ctx while parked (a non-cooperative driver)
	saveEntered chan struct{}
	saves       int
	applied     int
	payloadLoad int
	quarantined []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]SnapshotRow{}, saveEntered: make(chan struct{}, 16)}
}

func (s *fakeStore) LoadSnapshotMeta(_ context.Context, dataset string) (SnapshotMeta, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.metaErr != nil {
		return SnapshotMeta{}, false, s.metaErr
	}
	row, ok := s.rows[dataset]
	if !ok {
		return SnapshotMeta{}, false, nil
	}

	return SnapshotMeta{FormatVersion: row.FormatVersion, Pair: row.Pair, PayloadBytes: int64(len(row.Payload)), SavedAt: row.SavedAt}, true, nil
}

func (s *fakeStore) LoadSnapshotPayload(_ context.Context, dataset string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloadLoad++

	return s.rows[dataset].Payload, nil
}

func (s *fakeStore) SaveSnapshot(ctx context.Context, row SnapshotRow) (bool, error) {
	s.mu.Lock()
	s.saves++
	block, err := s.saveBlock, s.saveErr
	s.mu.Unlock()
	select {
	case s.saveEntered <- struct{}{}:
	default:
	}
	if block != nil {
		if s.saveDeaf {
			<-block
		} else {
			select {
			case <-block:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
	}
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.rows[row.Dataset]; ok && !row.Pair.Greater(existing.Pair) {
		return false, nil
	}
	s.rows[row.Dataset] = row
	s.applied++

	return true, nil
}

func (s *fakeStore) QuarantineSnapshot(_ context.Context, dataset string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quarErr != nil {
		return s.quarErr
	}
	s.rows[dataset+".unreadable"] = s.rows[dataset]
	delete(s.rows, dataset)
	s.quarantined = append(s.quarantined, dataset)

	return nil
}

func (s *fakeStore) row(dataset string) (SnapshotRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[dataset]

	return row, ok
}

func (s *fakeStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saves
}

type reportSink struct {
	mu   sync.Mutex
	errs []error
}

func (r *reportSink) report(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, err)
}

func (r *reportSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.errs)
}

func testPersister(store SnapshotStore, sink *reportSink, ds ...Dataset) *Persister {
	cfg := PersisterConfig{
		Store:           store,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Interval:        time.Hour, // tests drive flushes explicitly
		WriteTimeout:    time.Second,
		ShutdownTimeout: 200 * time.Millisecond,
	}
	if sink != nil {
		cfg.Report = sink.report
	}

	return NewPersister(cfg, ds...)
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ---- dirty tracking ----

func TestTrackerResetIsDirtyEvenWithEqualRevision(t *testing.T) {
	tr := newPersistTracker()
	for i := 0; i < 3; i++ {
		tr.touch()
	}
	tr.ack(tr.current)
	if tr.dirty() {
		t.Fatal("freshly acknowledged tracker must be clean")
	}

	tr.reset()
	if !tr.dirty() {
		t.Fatal("a reset must be dirty even though its revision (0) differs only by generation")
	}
	tr.ack(tr.current)
	for i := 0; i < 3; i++ {
		tr.touch()
	}
	// Same revision (3) as the old saved pair, different generation.
	if tr.current.Revision != 3 || !tr.dirty() {
		t.Fatalf("pair %+v must be dirty against saved %+v", tr.current, tr.saved)
	}
	// A reset whose revision equals the saved revision: saved (1,3), then reset+3 touches = (2,3).
	tr.ack(tr.current)
	tr.reset()
	for i := 0; i < 3; i++ {
		tr.touch()
	}
	if tr.current.Revision != tr.saved.Revision || !tr.dirty() {
		t.Fatalf("equal revision in a newer generation must be dirty: current %+v saved %+v", tr.current, tr.saved)
	}
}

// ---- flush ----

func TestFlushWritesOnlyWhenDirty(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	p := testPersister(store, nil, ds)
	ctx := context.Background()

	if err := p.FlushNow(ctx, "fake"); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if store.saveCount() != 0 {
		t.Fatal("a clean dataset must not be written")
	}

	ds.mutate("a")
	if err := p.FlushNow(ctx, "fake"); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := p.FlushNow(ctx, "fake"); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if store.saveCount() != 1 {
		t.Fatalf("expected exactly one write, got %d", store.saveCount())
	}
	row, _ := store.row("fake")
	if string(row.Payload) != "a" || row.Pair != (PersistPair{0, 1}) {
		t.Fatalf("unexpected row %+v", row)
	}
}

func TestMutationDuringWriteStaysDirtyAndOnlyCapturedPairIsAcked(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.saveBlock = make(chan struct{})
	p := testPersister(store, nil, ds)

	ds.mutate("v1")
	done := make(chan error, 1)
	go func() { done <- p.FlushNow(context.Background(), "fake") }()
	waitFor(t, store.saveEntered, "write to start")

	ds.mutate("v2") // lands while the write is in flight
	close(store.saveBlock)
	if err := <-done; err != nil {
		t.Fatalf("flush: %v", err)
	}

	current, saved := ds.pairs()
	if saved != (PersistPair{0, 1}) || current != (PersistPair{0, 2}) {
		t.Fatalf("only the captured pair may be acked: current %+v saved %+v", current, saved)
	}
	if !ds.dirty() {
		t.Fatal("the mutation made during the write must stay dirty")
	}
}

func TestResetDuringWriteStaysDirtyAndStaleWriteIsRejected(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	p := testPersister(store, nil, ds)

	// Persisted generation 0, revision 5.
	for i := 0; i < 5; i++ {
		ds.mutate("old")
	}
	if err := p.FlushNow(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}

	// The reset's generation is persisted first...
	g := ds.reset()
	if err := p.FlushNow(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	// ...then a delayed write of the old generation must not restore counters.
	applied, err := store.SaveSnapshot(context.Background(), SnapshotRow{Dataset: "fake", FormatVersion: 1, Pair: PersistPair{0, 6}, Payload: []byte("stale")})
	if err != nil || applied {
		t.Fatalf("stale pair must be rejected by the store: applied=%v err=%v", applied, err)
	}
	row, _ := store.row("fake")
	if row.Pair.Generation != g || string(row.Payload) != "" {
		t.Fatalf("row must hold the reset generation, got %+v", row)
	}
}

func TestStoreRefusalIsNotAcknowledged(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.rows["fake"] = SnapshotRow{Dataset: "fake", FormatVersion: 1, Pair: PersistPair{0, 99}, Payload: []byte("newer")}
	p := testPersister(store, nil, ds)

	ds.mutate("mine")
	if err := p.FlushNow(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if !ds.dirty() {
		t.Fatal("an unapplied write must not be acknowledged")
	}
}

func TestSaveFailureKeepsDataDirtyRetriesAndReportsOncePerStreak(t *testing.T) {
	store, ds, sink := newFakeStore(), newFakeDataset("fake"), &reportSink{}
	p := testPersister(store, sink, ds)
	store.saveErr = errors.New("disk full")
	ds.mutate("a")

	for i := 0; i < 3; i++ {
		if err := p.FlushNow(context.Background(), "fake"); err == nil {
			t.Fatal("expected a write error")
		}
	}
	if !ds.dirty() || p.State("fake") != PersistWriteFailing {
		t.Fatalf("data must stay dirty and state write_failing, got %s", p.State("fake"))
	}
	if sink.count() != 1 {
		t.Fatalf("only the first failure of a streak is reported, got %d", sink.count())
	}
	if ds.info.State != PersistWriteFailing {
		t.Fatalf("the dataset must disclose the failing state, got %s", ds.info.State)
	}

	store.mu.Lock()
	store.saveErr = nil
	store.mu.Unlock()
	if err := p.FlushNow(context.Background(), "fake"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if ds.dirty() || p.State("fake") != PersistEnabled {
		t.Fatalf("recovery must clean the dataset, state %s", p.State("fake"))
	}
	row, _ := store.row("fake")
	if string(row.Payload) != "a" {
		t.Fatalf("the full state must be written on retry, got %q", row.Payload)
	}
}

func TestOversizedWriteIsRefusedAndKeepsPreviousRow(t *testing.T) {
	store, ds, sink := newFakeStore(), newFakeDataset("fake"), &reportSink{}
	p := NewPersister(PersisterConfig{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxPayloadBytes: 8, Report: sink.report}, ds)

	ds.mutate("ok")
	if err := p.FlushNow(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	ds.mutate("this payload is far too large")
	if err := p.FlushNow(context.Background(), "fake"); err == nil {
		t.Fatal("an oversized snapshot must not be written")
	}
	row, _ := store.row("fake")
	if string(row.Payload) != "ok" {
		t.Fatalf("previous row must stay intact, got %q", row.Payload)
	}
	if p.State("fake") != PersistWriteFailing || !ds.dirty() {
		t.Fatalf("oversized write is a write failure that keeps the data dirty (state %s)", p.State("fake"))
	}
}

func TestPeriodicLoopFlushesOnWakeAndStopJoins(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	p := testPersister(store, nil, ds)
	p.Start(context.Background())

	ds.mutate("a")
	p.Wake()
	waitFor(t, store.saveEntered, "loop flush")

	if err := p.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-p.loopDone:
	default:
		t.Fatal("Stop must join the loop goroutine")
	}
}

// ---- shutdown ----

func TestStopFlushesAfterRunContextCancelledAndIsIdempotent(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	p := testPersister(store, nil, ds)
	runCtx, cancel := context.WithCancel(context.Background())
	p.Start(runCtx)

	ds.mutate("final")
	cancel() // the run context is already cancelled at shutdown

	if err := p.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	row, ok := store.row("fake")
	if !ok || string(row.Payload) != "final" {
		t.Fatalf("the final flush must run on a fresh context, got %+v", row)
	}
	saves := store.saveCount()
	if err := p.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if store.saveCount() != saves {
		t.Fatal("a second Stop must not flush again")
	}
}

func TestWritesAfterStopAreRejectedWithoutTouchingTheStore(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	p := testPersister(store, nil, ds)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}

	ds.mutate("late")
	if err := p.FlushNow(context.Background(), "fake"); !errors.Is(err, ErrPersisterClosed) {
		t.Fatalf("expected ErrPersisterClosed, got %v", err)
	}
	if store.saveCount() != 0 {
		t.Fatal("the store must not be touched after Stop")
	}
}

func TestStopWaitsForAdmittedWriterReportsOverrunAndWaitersSeeClosed(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.saveBlock = make(chan struct{}) // released only after the overrun is observed
	p := testPersister(store, nil, ds)
	overrun := make(chan struct{})
	p.onShutdownOverrun = func() { close(overrun) }

	ds.mutate("a")
	first := make(chan error, 1)
	go func() { first <- p.FlushNow(context.Background(), "fake") }()
	waitFor(t, store.saveEntered, "blocked write")

	waiter := make(chan error, 1)
	go func() { waiter <- p.FlushNow(context.Background(), "fake") }()

	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop() }()

	// The shutdown budget expires while the admitted writer still owns the
	// store: Stop must keep waiting instead of returning.
	waitFor(t, overrun, "shutdown budget overrun")
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while a writer was still active: %v", err)
	default:
	}

	close(store.saveBlock) // the blocked write finally returns
	if err := <-first; err != nil {
		t.Fatalf("the admitted writer must complete: %v", err)
	}
	err := <-stopped
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("Stop must report the deadline exhaustion accurately, got %v", err)
	}
	if err := <-waiter; !errors.Is(err, ErrPersisterClosed) {
		t.Fatalf("a flush that was waiting for the writer must see the closed persister, got %v", err)
	}
	if row, ok := store.row("fake"); !ok || string(row.Payload) != "a" {
		t.Fatalf("the admitted writer's data must be persisted, got %+v", row)
	}
	if err := p.Stop(); err == nil {
		t.Fatal("a second Stop must return the same result")
	}
}

func TestStopJoinsLoopStuckInWriteAndFlushesAfterwards(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.saveBlock = make(chan struct{})
	store.saveDeaf = true // the loop's write does not react to cancellation
	p := testPersister(store, nil, ds)
	overrun := make(chan struct{})
	p.onShutdownOverrun = func() { close(overrun) }
	p.Start(context.Background())

	ds.mutate("a")
	p.Wake()
	waitFor(t, store.saveEntered, "loop write")

	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop() }()
	waitFor(t, overrun, "overrun while the loop is stuck")
	select {
	case <-stopped:
		t.Fatal("Stop must not return before the loop is joined")
	default:
	}

	close(store.saveBlock)
	if err := <-stopped; err == nil {
		t.Fatal("the overrun must be reported")
	}
	select {
	case <-p.loopDone:
	default:
		t.Fatal("the loop must have been joined")
	}
	if ds.dirty() {
		t.Fatal("the final flush must persist the data after the workers ended")
	}
}

func TestFlushNowWaitingForWriterRespectsItsContext(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.saveBlock = make(chan struct{})
	defer close(store.saveBlock)
	p := testPersister(store, nil, ds)

	ds.mutate("a")
	go func() { _ = p.FlushNow(context.Background(), "fake") }()
	waitFor(t, store.saveEntered, "blocked write")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.FlushNow(ctx, "fake"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// ---- restore ----

func TestRestoreInstallsPersistedPair(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.rows["fake"] = SnapshotRow{Dataset: "fake", FormatVersion: 1, Pair: PersistPair{3, 7}, Payload: []byte("saved"), SavedAt: time.Unix(100, 0)}
	p := testPersister(store, nil, ds)

	if err := p.Restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	current, saved := ds.pairs()
	if current != (PersistPair{3, 7}) || saved != current || ds.dirty() {
		t.Fatalf("restored dataset must be clean at the persisted pair: %+v / %+v", current, saved)
	}
	if !ds.info.Restored || ds.info.LastSavedAt == nil {
		t.Fatalf("restore must be disclosed: %+v", ds.info)
	}
}

func TestRestoreLoadErrorFailsStartup(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.metaErr = errors.New("io failure")
	p := testPersister(store, nil, ds)
	if err := p.Restore(context.Background()); err == nil {
		t.Fatal("a load I/O error must fail Restore: starting empty could overwrite real data")
	}
}

func TestNewerFormatIsNeverQuarantinedOrWritten(t *testing.T) {
	store, ds, sink := newFakeStore(), newFakeDataset("fake"), &reportSink{}
	// An oversized payload of a newer format must also stay untouched.
	store.rows["fake"] = SnapshotRow{Dataset: "fake", FormatVersion: 9, Pair: PersistPair{1, 1}, Payload: make([]byte, 64)}
	p := NewPersister(PersisterConfig{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxPayloadBytes: 8, Report: sink.report}, ds)

	if err := p.Restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if p.State("fake") != PersistDisabledNewerFormat || sink.count() != 1 {
		t.Fatalf("state %s, reports %d", p.State("fake"), sink.count())
	}
	if len(store.quarantined) != 0 || store.payloadLoad != 0 {
		t.Fatal("a newer-format row must be neither quarantined nor loaded")
	}

	ds.mutate("new")
	if err := p.FlushNow(context.Background(), "fake"); !errors.Is(err, ErrPersistenceDisabled) {
		t.Fatalf("expected ErrPersistenceDisabled, got %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("stop must not fail because of a disabled dataset: %v", err)
	}
	if store.saveCount() != 0 {
		t.Fatal("a newer-format dataset must never be written")
	}
	if row, _ := store.row("fake"); row.FormatVersion != 9 {
		t.Fatal("the newer-format row must be untouched")
	}
}

func TestMalformedSnapshotIsQuarantinedAndDatasetStartsFresh(t *testing.T) {
	store, ds, sink := newFakeStore(), newFakeDataset("fake"), &reportSink{}
	store.rows["fake"] = SnapshotRow{Dataset: "fake", FormatVersion: 1, Pair: PersistPair{2, 2}, Payload: []byte("bad")}
	ds.restoreErr = ErrMalformedSnapshot
	p := testPersister(store, sink, ds)

	if err := p.Restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, ok := store.row("fake"); ok {
		t.Fatal("the unreadable row must have been moved away")
	}
	if _, ok := store.row("fake.unreadable"); !ok {
		t.Fatal("the unreadable row must be quarantined")
	}
	if p.State("fake") != PersistEnabled || sink.count() != 1 {
		t.Fatalf("persistence stays enabled after quarantine: %s, reports %d", p.State("fake"), sink.count())
	}

	// A fresh dataset can now be saved even though its pair restarts low.
	ds.mutate("fresh")
	if err := p.FlushNow(context.Background(), "fake"); err != nil {
		t.Fatal(err)
	}
	if row, ok := store.row("fake"); !ok || string(row.Payload) != "fresh" {
		t.Fatalf("fresh state must be saved, got %+v", row)
	}
}

func TestOversizedSupportedSnapshotIsQuarantinedWithoutLoadingTheBlob(t *testing.T) {
	store, ds := newFakeStore(), newFakeDataset("fake")
	store.rows["fake"] = SnapshotRow{Dataset: "fake", FormatVersion: 1, Pair: PersistPair{1, 1}, Payload: make([]byte, 64)}
	p := NewPersister(PersisterConfig{Store: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxPayloadBytes: 8}, ds)

	if err := p.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.payloadLoad != 0 || len(store.quarantined) != 1 || ds.restores != 0 {
		t.Fatalf("payloadLoad=%d quarantined=%v restores=%d", store.payloadLoad, store.quarantined, ds.restores)
	}
}

func TestQuarantineFailureNeverOverwritesTheOriginal(t *testing.T) {
	store, ds, sink := newFakeStore(), newFakeDataset("fake"), &reportSink{}
	store.rows["fake"] = SnapshotRow{Dataset: "fake", FormatVersion: 1, Pair: PersistPair{2, 2}, Payload: []byte("bad")}
	store.quarErr = errors.New("quarantine failed")
	ds.restoreErr = ErrMalformedSnapshot
	p := testPersister(store, sink, ds)

	if err := p.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.State("fake") != PersistDisabledQuarantineFailed {
		t.Fatalf("state %s", p.State("fake"))
	}
	ds.mutate("fresh")
	if err := p.FlushNow(context.Background(), "fake"); !errors.Is(err, ErrPersistenceDisabled) {
		t.Fatalf("got %v", err)
	}
	if row, _ := store.row("fake"); string(row.Payload) != "bad" {
		t.Fatal("the original row must never be replaced")
	}
}

// ---- reset acknowledgement ----

func TestEnsureDurableAcknowledgementCases(t *testing.T) {
	ctx := context.Background()

	t.Run("flush writes the generation", func(t *testing.T) {
		store, ds := newFakeStore(), newFakeDataset("fake")
		p := testPersister(store, nil, ds)
		ds.mutate("a")
		g := ds.reset()
		out := p.EnsureDurable(ctx, "fake", g)
		if !out.Durable || out.State != PersistEnabled {
			t.Fatalf("got %+v", out)
		}
	})

	t.Run("a periodic flush already persisted the generation", func(t *testing.T) {
		store, ds := newFakeStore(), newFakeDataset("fake")
		p := testPersister(store, nil, ds)
		g := ds.reset()
		if err := p.FlushNow(ctx, "fake"); err != nil { // the "periodic" flush
			t.Fatal(err)
		}
		saves := store.saveCount()
		out := p.EnsureDurable(ctx, "fake", g) // nothing left to write
		if !out.Durable || store.saveCount() != saves {
			t.Fatalf("an already-persisted generation is a durable reset without a new write: %+v", out)
		}
	})

	t.Run("write failure is temporary", func(t *testing.T) {
		store, ds := newFakeStore(), newFakeDataset("fake")
		store.saveErr = errors.New("boom")
		p := testPersister(store, nil, ds)
		g := ds.reset()
		out := p.EnsureDurable(ctx, "fake", g)
		if out.Durable || out.State != PersistWriteFailing || out.Err == nil {
			t.Fatalf("got %+v", out)
		}
	})

	t.Run("disabled persistence promises no retry", func(t *testing.T) {
		store, ds := newFakeStore(), newFakeDataset("fake")
		store.rows["fake"] = SnapshotRow{Dataset: "fake", FormatVersion: 7, Pair: PersistPair{1, 1}}
		p := testPersister(store, nil, ds)
		if err := p.Restore(ctx); err != nil {
			t.Fatal(err)
		}
		g := ds.reset()
		out := p.EnsureDurable(ctx, "fake", g)
		if out.Durable || !out.State.Disabled() {
			t.Fatalf("got %+v", out)
		}
	})
}
