package statistics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// MaxSnapshotPayloadBytes bounds one persisted dataset payload on BOTH read
// and write: the application never saves a snapshot it would quarantine on
// its next start. It sits above the documented strict worst case of every
// dataset (see the persistence spec), which a test enforces.
const MaxSnapshotPayloadBytes = 32 << 20

const (
	defaultPersistInterval = 30 * time.Second
	defaultWriteTimeout    = 10 * time.Second
	defaultShutdownTimeout = 5 * time.Second
)

var (
	// ErrPersisterClosed is returned by every write path after Stop.
	ErrPersisterClosed = errors.New("statistics persister is closed")
	// ErrPersistenceDisabled is returned when a dataset's persistence is
	// disabled for this process (newer on-disk format, failed quarantine).
	ErrPersistenceDisabled = errors.New("statistics persistence is disabled for this dataset")
	// ErrMalformedSnapshot marks a persisted payload that cannot be decoded or
	// fails validation. Dataset.Restore wraps it; the persister quarantines.
	ErrMalformedSnapshot = errors.New("malformed statistics snapshot")
	// ErrUnknownDataset is returned for a dataset name the persister does not own.
	ErrUnknownDataset = errors.New("unknown statistics dataset")
)

// PersistState is the persistence state of one dataset, disclosed in
// snapshots, exports and the admin view.
type PersistState string

const (
	// PersistNotConfigured: no persister is attached; statistics are RAM-only.
	PersistNotConfigured PersistState = "not_configured"
	// PersistEnabled: saves succeed (or none were needed yet).
	PersistEnabled PersistState = "enabled"
	// PersistWriteFailing: the latest save failed; it is retried every tick.
	PersistWriteFailing PersistState = "write_failing"
	// PersistDisabledNewerFormat: the stored row has a newer format version
	// (application downgrade). It is left untouched and never written.
	PersistDisabledNewerFormat PersistState = "disabled_newer_format"
	// PersistDisabledQuarantineFailed: an unreadable row could not be set
	// aside, so the original is never replaced.
	PersistDisabledQuarantineFailed PersistState = "disabled_quarantine_failed"
)

// Disabled reports whether the state is a permanent per-process disable.
func (s PersistState) Disabled() bool {
	return s == PersistDisabledNewerFormat || s == PersistDisabledQuarantineFailed
}

// PersistenceInfo is the persistence disclosure carried by snapshots.
type PersistenceInfo struct {
	State       PersistState `json:"state"`
	Restored    bool         `json:"restored"`
	LastSavedAt *time.Time   `json:"last_saved_at"`
}

// PersistPair identifies one dataset state: the reset generation and the
// mutation revision within it. Dirty tracking and acknowledgements always
// use the COMPLETE pair, never the revision alone.
type PersistPair struct {
	Generation uint64
	Revision   uint64
}

// Greater reports strict lexicographic ordering (generation, then revision).
func (p PersistPair) Greater(other PersistPair) bool {
	if p.Generation != other.Generation {
		return p.Generation > other.Generation
	}

	return p.Revision > other.Revision
}

// persistTracker is the shared dirty-tracking primitive of a persistent
// dataset. It is NOT synchronized: the owning dataset guards it with its own
// lock.
type persistTracker struct {
	current PersistPair
	saved   PersistPair
	info    PersistenceInfo
}

func newPersistTracker() persistTracker {
	return persistTracker{info: PersistenceInfo{State: PersistNotConfigured}}
}

// touch records one applied mutation.
func (t *persistTracker) touch() { t.current.Revision++ }

// reset starts a new generation. The pair always differs from any saved pair
// of an older generation, so a reset is always dirty, even when its revision
// equals the previously saved one.
func (t *persistTracker) reset() uint64 {
	t.current.Generation++
	t.current.Revision = 0

	return t.current.Generation
}

func (t *persistTracker) dirty() bool { return t.current != t.saved }

// ack acknowledges exactly the pair that was written. Mutations or resets
// that landed during the write leave current != saved, so they stay dirty.
func (t *persistTracker) ack(pair PersistPair) { t.saved = pair }

// install adopts a restored pair as both current and saved.
func (t *persistTracker) install(pair PersistPair) {
	t.current = pair
	t.saved = pair
}

// Dataset is one persistent statistics dataset as seen by the Persister. The
// implementations own their locking. Capture MUST copy the state under the
// dataset lock and return an encoder that runs after the lock is released.
type Dataset interface {
	// Name is the stable storage key of the dataset.
	Name() string
	// FormatVersion is the highest payload format version this build writes
	// and understands.
	FormatVersion() int
	// Capture returns the current pair, an encoder over an immutable copy,
	// and whether the dataset is dirty (pair != last acknowledged pair).
	Capture() (pair PersistPair, encode func() ([]byte, error), dirty bool)
	// Acknowledge marks pair as durably saved.
	Acknowledge(pair PersistPair)
	// Restore atomically decodes, validates and installs a persisted payload.
	// A failure wraps ErrMalformedSnapshot and leaves the dataset untouched.
	Restore(version int, pair PersistPair, payload []byte) error
	// SetPersistenceInfo publishes the persistence disclosure.
	SetPersistenceInfo(PersistenceInfo)
}

// SnapshotMeta is the blob-free header of a persisted dataset row.
type SnapshotMeta struct {
	FormatVersion int
	Pair          PersistPair
	PayloadBytes  int64
	SavedAt       time.Time
}

// SnapshotRow is one persisted dataset row.
type SnapshotRow struct {
	Dataset       string
	FormatVersion int
	Pair          PersistPair
	Payload       []byte
	SavedAt       time.Time
}

// SnapshotStore is the storage the Persister needs, expressed in plain
// parameters so this package never imports the SQLite layer.
type SnapshotStore interface {
	LoadSnapshotMeta(ctx context.Context, dataset string) (meta SnapshotMeta, ok bool, err error)
	LoadSnapshotPayload(ctx context.Context, dataset string) ([]byte, error)
	// SaveSnapshot is a conditional upsert: applied is false when the store
	// already holds an equal or newer pair.
	SaveSnapshot(ctx context.Context, row SnapshotRow) (applied bool, err error)
	QuarantineSnapshot(ctx context.Context, dataset string) error
}

// PersisterConfig carries the construction-time dependencies. The durations
// are code constants by default (not runtime settings, not deployment
// configuration); tests override them.
type PersisterConfig struct {
	Store           SnapshotStore
	Logger          *slog.Logger
	Interval        time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
	// MaxPayloadBytes overrides MaxSnapshotPayloadBytes (tests).
	MaxPayloadBytes int64
	// Report receives actionable persistence errors (the app wires Sentry).
	// Optional.
	Report func(error)
	// Now overrides the wall clock. Nil means time.Now.
	Now func() time.Time
}

type persistEntry struct {
	ds       Dataset
	state    PersistState
	restored bool
	durable  PersistPair // last pair known to be in the store (guarded by Persister.mu)
	savedAt  time.Time

	failStreak int
}

// Persister saves and restores persistent datasets as whole-row aggregate
// snapshots. Writes are batched on a ticker, serialized by a context-aware
// writer semaphore, and never run under a dataset lock.
type Persister struct {
	cfg PersisterConfig
	log *slog.Logger

	sem chan struct{}

	mu      sync.Mutex // guards entries' mutable fields and closed
	entries []*persistEntry
	byName  map[string]*persistEntry
	closed  bool

	loopCancel context.CancelFunc
	loopDone   chan struct{}
	wake       chan struct{}

	stopOnce sync.Once
	stopErr  error

	// onShutdownOverrun is a test hook: it runs when Stop's wait for a worker
	// outlasts the shutdown budget.
	onShutdownOverrun func()
}

// NewPersister builds a persister over the given datasets.
func NewPersister(cfg PersisterConfig, datasets ...Dataset) *Persister {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultPersistInterval
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.MaxPayloadBytes <= 0 {
		cfg.MaxPayloadBytes = MaxSnapshotPayloadBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	p := &Persister{
		cfg:    cfg,
		log:    logger.With("component", "statistics_persistence"),
		sem:    make(chan struct{}, 1),
		byName: map[string]*persistEntry{},
		wake:   make(chan struct{}, 1),
	}
	for _, ds := range datasets {
		entry := &persistEntry{ds: ds, state: PersistEnabled}
		p.entries = append(p.entries, entry)
		p.byName[ds.Name()] = entry
		ds.SetPersistenceInfo(PersistenceInfo{State: PersistEnabled})
	}

	return p
}

// Restore loads every dataset. It must run synchronously before any
// producer records and before Start. A load I/O error fails the call (and
// therefore startup): continuing empty could overwrite real data.
func (p *Persister) Restore(ctx context.Context) error {
	for _, entry := range p.entries {
		if err := p.restoreEntry(ctx, entry); err != nil {
			return err
		}
	}

	return nil
}

func (p *Persister) restoreEntry(ctx context.Context, e *persistEntry) error {
	name := e.ds.Name()
	meta, ok, err := p.cfg.Store.LoadSnapshotMeta(ctx, name)
	if err != nil {
		return fmt.Errorf("restore statistics dataset %q: %w", name, err)
	}
	if !ok {
		return nil
	}

	// The format version is classified BEFORE any size-based malformed
	// handling: an oversized snapshot from a newer format stays untouched.
	if meta.FormatVersion > e.ds.FormatVersion() {
		p.disable(e, PersistDisabledNewerFormat, fmt.Errorf(
			"statistics dataset %q has format version %d, this build supports up to %d; persistence disabled and the stored snapshot is left untouched",
			name, meta.FormatVersion, e.ds.FormatVersion()))

		return nil
	}

	if meta.PayloadBytes > p.cfg.MaxPayloadBytes {
		return p.quarantine(ctx, e, fmt.Errorf("%w: payload of %d bytes exceeds the %d byte limit", ErrMalformedSnapshot, meta.PayloadBytes, p.cfg.MaxPayloadBytes))
	}

	payload, err := p.cfg.Store.LoadSnapshotPayload(ctx, name)
	if err != nil {
		return fmt.Errorf("restore statistics dataset %q: %w", name, err)
	}
	if err := e.ds.Restore(meta.FormatVersion, meta.Pair, payload); err != nil {
		if errors.Is(err, ErrMalformedSnapshot) {
			return p.quarantine(ctx, e, err)
		}

		return fmt.Errorf("restore statistics dataset %q: %w", name, err)
	}

	p.mu.Lock()
	e.durable = meta.Pair
	e.restored = true
	e.savedAt = meta.SavedAt
	p.mu.Unlock()
	p.publish(e)
	p.log.Info("statistics dataset restored", "dataset", name, "generation", meta.Pair.Generation, "revision", meta.Pair.Revision)

	return nil
}

// quarantine sets an unreadable row aside atomically and lets the dataset
// start fresh. A quarantine failure disables persistence for the dataset so
// the original row is never replaced.
func (p *Persister) quarantine(ctx context.Context, e *persistEntry, cause error) error {
	name := e.ds.Name()
	if err := p.cfg.Store.QuarantineSnapshot(ctx, name); err != nil {
		p.disable(e, PersistDisabledQuarantineFailed, fmt.Errorf("statistics dataset %q is unreadable (%w) and quarantine failed: %w", name, cause, err))

		return nil
	}
	p.report(fmt.Errorf("statistics dataset %q was unreadable and has been quarantined: %w", name, cause))
	p.log.Error("statistics dataset quarantined; starting fresh", "dataset", name, "error", cause)

	return nil
}

func (p *Persister) disable(e *persistEntry, state PersistState, cause error) {
	p.mu.Lock()
	e.state = state
	p.mu.Unlock()
	p.publish(e)
	p.report(cause)
	p.log.Error("statistics persistence disabled", "dataset", e.ds.Name(), "state", string(state), "error", cause)
}

func (p *Persister) report(err error) {
	if p.cfg.Report != nil {
		p.cfg.Report(err)
	}
}

// publish pushes the entry's persistence disclosure to its dataset.
func (p *Persister) publish(e *persistEntry) {
	p.mu.Lock()
	info := PersistenceInfo{State: e.state, Restored: e.restored}
	if !e.savedAt.IsZero() {
		t := e.savedAt
		info.LastSavedAt = &t
	}
	p.mu.Unlock()
	e.ds.SetPersistenceInfo(info)
}

// Start launches the periodic flush loop. It is joined by Stop.
func (p *Persister) Start(ctx context.Context) {
	loopCtx, cancel := context.WithCancel(ctx)
	p.loopCancel = cancel
	p.loopDone = make(chan struct{})

	go func() {
		defer close(p.loopDone)
		ticker := time.NewTicker(p.cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			case <-p.wake:
			}
			p.flushAll(loopCtx)
		}
	}()
}

// Wake requests an immediate flush pass from the loop (non-blocking).
func (p *Persister) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Persister) flushAll(ctx context.Context) {
	for _, e := range p.entries {
		if ctx.Err() != nil {
			return
		}
		if err := p.acquire(ctx); err != nil {
			return
		}
		_ = p.flushEntry(ctx, e)
		p.release()
	}
}

func (p *Persister) acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Persister) release() { <-p.sem }

// flushEntry writes the dataset when dirty. The caller holds the writer
// semaphore; closed and ctx are rechecked here, AFTER acquisition.
func (p *Persister) flushEntry(ctx context.Context, e *persistEntry) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrPersisterClosed
	}

	return p.flushEntryFinal(ctx, e)
}

// flushEntryFinal is flushEntry without the admission check: Stop closes
// admission first and then performs the final flush itself.
func (p *Persister) flushEntryFinal(ctx context.Context, e *persistEntry) error {
	p.mu.Lock()
	state := e.state
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Disabled() {
		return ErrPersistenceDisabled
	}

	pair, encode, dirty := e.ds.Capture()
	if !dirty {
		return nil
	}
	payload, err := encode()
	if err != nil {
		return p.writeFailed(e, fmt.Errorf("encode statistics snapshot: %w", err))
	}
	if int64(len(payload)) > p.cfg.MaxPayloadBytes {
		return p.writeFailed(e, fmt.Errorf("oversized statistics snapshot: %d bytes exceed the %d byte limit", len(payload), p.cfg.MaxPayloadBytes))
	}

	wctx, cancel := context.WithTimeout(ctx, p.cfg.WriteTimeout)
	defer cancel()
	now := p.cfg.Now()
	applied, err := p.cfg.Store.SaveSnapshot(wctx, SnapshotRow{
		Dataset:       e.ds.Name(),
		FormatVersion: e.ds.FormatVersion(),
		Pair:          pair,
		Payload:       payload,
		SavedAt:       now,
	})
	if err != nil {
		return p.writeFailed(e, fmt.Errorf("save statistics snapshot: %w", err))
	}
	if !applied {
		// The store already holds an equal or newer pair (a stale capture).
		// Not acknowledged: the dataset stays dirty and the next pass writes
		// the current state.
		p.log.Warn("statistics snapshot not applied: the store holds a newer pair", "dataset", e.ds.Name(),
			"generation", pair.Generation, "revision", pair.Revision)

		return nil
	}

	e.ds.Acknowledge(pair)
	p.mu.Lock()
	e.durable = pair
	e.savedAt = now
	recovered := e.state == PersistWriteFailing
	e.state = PersistEnabled
	e.failStreak = 0
	p.mu.Unlock()
	p.publish(e)
	if recovered {
		p.log.Info("statistics persistence recovered", "dataset", e.ds.Name())
	}

	return nil
}

// writeFailed keeps the dataset dirty (nothing is discarded) and marks the
// temporary failure. Only the first failure of a streak is reported.
func (p *Persister) writeFailed(e *persistEntry, err error) error {
	p.mu.Lock()
	e.failStreak++
	first := e.failStreak == 1
	e.state = PersistWriteFailing
	p.mu.Unlock()
	p.publish(e)

	p.log.Warn("statistics snapshot save failed; will retry", "dataset", e.ds.Name(), "error", err)
	if first {
		p.report(err)
	}

	return err
}

// FlushNow writes the dataset synchronously when dirty.
func (p *Persister) FlushNow(ctx context.Context, name string) error {
	e, ok := p.lookup(name)
	if !ok {
		return ErrUnknownDataset
	}
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()

	return p.flushEntry(ctx, e)
}

func (p *Persister) lookup(name string) (*persistEntry, bool) {
	e, ok := p.byName[name]

	return e, ok
}

// State reports the persistence state of a dataset.
func (p *Persister) State(name string) PersistState {
	e, ok := p.lookup(name)
	if !ok {
		return PersistNotConfigured
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	return e.state
}

// DurableOutcome is the result of EnsureDurable.
type DurableOutcome struct {
	// Durable: the dataset's persisted generation reached the target.
	Durable bool
	// State is the dataset's persistence state after the attempt.
	State PersistState
	// Err is the write failure, when one occurred.
	Err error
}

// EnsureDurable makes a reset generation durable. The acknowledgement
// depends on the PERSISTED generation, not on whether this call performed
// the write: a periodic flush may already have persisted the generation.
func (p *Persister) EnsureDurable(ctx context.Context, name string, generation uint64) DurableOutcome {
	e, ok := p.lookup(name)
	if !ok {
		return DurableOutcome{State: PersistNotConfigured, Err: ErrUnknownDataset}
	}

	durable := func() DurableOutcome {
		p.mu.Lock()
		defer p.mu.Unlock()

		return DurableOutcome{Durable: e.durable.Generation >= generation && !e.state.Disabled(), State: e.state}
	}
	if out := durable(); out.Durable || out.State.Disabled() {
		return out
	}

	err := p.FlushNow(ctx, name)
	out := durable()
	if !out.Durable {
		out.Err = err
	}

	return out
}

// Stop shuts persistence down. It is idempotent: the explicit shutdown call
// and the defer guard may both run. Order:
//
//  1. close admission: every ordinary flush (FlushNow, the loop) now sees
//     ErrPersisterClosed once it holds the writer;
//  2. cancel and JOIN the periodic loop;
//  3. wait until any writer admitted before step 1 has finished;
//  4. run the final flush of every dirty dataset under ShutdownTimeout.
//
// Steps 2 and 3 are never abandoned: returning while a worker still owns the
// store would let the caller close the database under it, so Stop waits for
// them (it relies on the driver honouring context cancellation, and starts
// no detached goroutine). ShutdownTimeout is the budget of the final flush
// only; when joining the workers took longer than that, the overrun is
// reported in the returned error rather than hidden.
func (p *Persister) Stop() error {
	p.stopOnce.Do(func() {
		p.markClosed()

		var errs []error
		overrun := time.NewTimer(p.cfg.ShutdownTimeout)
		defer overrun.Stop()
		overran := func(what string) {
			if p.onShutdownOverrun != nil {
				p.onShutdownOverrun()
			}
			errs = append(errs, fmt.Errorf("%s did not finish within the %s shutdown budget; waited for it to complete", what, p.cfg.ShutdownTimeout))
		}

		if p.loopCancel != nil {
			p.loopCancel()
			select {
			case <-p.loopDone:
			case <-overrun.C:
				overran("statistics persistence loop")
				<-p.loopDone
			}
		}
		// Ownership of the writer semaphore: no earlier writer remains.
		select {
		case p.sem <- struct{}{}:
		case <-overrun.C:
			overran("in-flight statistics write")
			p.sem <- struct{}{}
		}

		final, cancel := context.WithTimeout(context.Background(), p.cfg.ShutdownTimeout)
		defer cancel()
		for _, e := range p.entries {
			if err := p.flushEntryFinal(final, e); err != nil && !errors.Is(err, ErrPersistenceDisabled) {
				errs = append(errs, fmt.Errorf("final flush of %q: %w", e.ds.Name(), err))
			}
		}
		p.release()

		p.stopErr = errors.Join(errs...)
	})

	return p.stopErr
}

func (p *Persister) markClosed() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
}
