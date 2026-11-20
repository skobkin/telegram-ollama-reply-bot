package stickers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Errors surfaced to the administrator. Fetcher and store implementations
// map their failures onto these so callers can give actionable feedback
// without ever seeing raw transport errors.
var (
	// ErrSetNotFound: Telegram does not know the set.
	ErrSetNotFound = errors.New("sticker set not found")
	// ErrUnsupportedSet: the set is not a regular sticker set (masks and
	// custom emoji sets are out of scope).
	ErrUnsupportedSet = errors.New("only regular sticker sets are supported")
	// ErrFetchUnavailable: Telegram could not be reached or answered
	// unexpectedly.
	ErrFetchUnavailable = errors.New("telegram is unavailable")
	// ErrBusy: another catalog operation is already running.
	ErrBusy = errors.New("another sticker catalog operation is already running")
	// ErrConflict: the persisted set changed since it was read.
	ErrConflict = errors.New("sticker set changed concurrently; retry")
	// ErrUnknownSet: the catalog has no set with that name.
	ErrUnknownSet = errors.New("no such sticker set in the catalog")
)

const (
	fetchTimeout = 15 * time.Second
	// maxConcurrentExports bounds simultaneous Telegram fetches for exports.
	maxConcurrentExports = 2
)

// Store persists catalog changes. Implementations map their own errors to
// ErrConflict for a stale revision.
type Store interface {
	// LoadCatalog returns the complete persisted catalog.
	LoadCatalog(ctx context.Context) (State, error)
	// ReplaceSet commits one validated import plan in a single transaction.
	ReplaceSet(ctx context.Context, plan *ImportPlan) error
	SetEnabled(ctx context.Context, name string, enabled bool, expectedRevision int64, at time.Time) error
	DeleteSet(ctx context.Context, name string, expectedRevision int64) error
}

// SetFetcher reads the authoritative current state of a regular sticker set
// from Telegram. Failures are mapped to ErrSetNotFound, ErrUnsupportedSet or
// ErrFetchUnavailable.
type SetFetcher interface {
	FetchSet(ctx context.Context, name string) (RemoteSet, error)
}

// Service owns the catalog: it loads it at startup, serves the immutable
// search index, exports templates (read-only) and applies administrator
// changes atomically — validate the whole prospective catalog and build its
// index first, commit in one transaction, and only then publish.
type Service struct {
	store   Store
	fetcher SetFetcher
	logger  *slog.Logger
	now     func() time.Time

	// opMu serializes every mutation (import, enable/disable, remove).
	opMu sync.Mutex
	// exportSlots bounds concurrent export fetches.
	exportSlots chan struct{}

	mu    sync.RWMutex
	state State // the complete catalog; immutable once published
	index atomic.Pointer[Index]
}

// NewService builds a service with an empty catalog. Call Load before use.
func NewService(store Store, fetcher SetFetcher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		store: store, fetcher: fetcher, logger: logger, now: time.Now,
		exportSlots: make(chan struct{}, maxConcurrentExports),
	}
	s.index.Store(BuildIndex(State{}))

	return s
}

// Load restores the complete persisted catalog, validates it with the same
// rules as imports and publishes its index. It is all or nothing: any
// invalid row or database error fails the restore (the bot must not start
// with a partial or unvalidated catalog, and later imports must merge
// against everything stored). Errors name identities and rules only.
func (s *Service) Load(ctx context.Context) error {
	state, err := s.store.LoadCatalog(ctx)
	if err != nil {
		return fmt.Errorf("restore sticker catalog: %w", err)
	}
	if err := ValidateState(state); err != nil {
		return fmt.Errorf("restore sticker catalog: %w", err)
	}
	idx := BuildIndex(state)

	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.index.Store(idx)
	s.logger.Info("sticker catalog loaded", "sets", len(state.Sets), "entries", len(state.Entries), "active", idx.Len())

	return nil
}

// Snapshot returns the current immutable search index. It is never nil.
func (s *Service) Snapshot() *Index { return s.index.Load() }

func (s *Service) current() State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.state
}

func (s *Service) fetch(ctx context.Context, name string) (RemoteSet, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	return s.fetcher.FetchSet(ctx, name)
}

// Export fetches the current set and renders its template. It never writes
// to the store or changes the catalog.
func (s *Service) Export(ctx context.Context, reference string) (filename string, data []byte, err error) {
	name, err := ParseSetReference(reference)
	if err != nil {
		return "", nil, err
	}
	select {
	case s.exportSlots <- struct{}{}:
		defer func() { <-s.exportSlots }()
	default:
		return "", nil, ErrBusy
	}

	remote, err := s.fetch(ctx, name)
	if err != nil {
		return "", nil, err
	}
	data, err = BuildTemplate(s.current(), remote)
	if err != nil {
		return "", nil, err
	}
	s.logger.Info("sticker template exported", "set", remote.Name, "stickers", len(remote.Stickers), "bytes", len(data))

	return TemplateFilename(remote.Name), data, nil
}

// ImportResult describes a committed import.
type ImportResult struct {
	Set    string
	Report Report
}

// Import validates an uploaded template against the authoritative Telegram
// set and the whole prospective catalog, commits it atomically and only then
// publishes the new index. On any failure nothing changes.
func (s *Service) Import(ctx context.Context, raw []byte) (ImportResult, error) {
	tmpl, err := ParseTemplate(raw)
	if err != nil {
		return ImportResult{}, err
	}
	if !s.opMu.TryLock() {
		return ImportResult{}, ErrBusy
	}
	defer s.opMu.Unlock()

	remote, err := s.fetch(ctx, tmpl.Set)
	if err != nil {
		return ImportResult{}, err
	}
	plan, err := PlanImport(s.current(), tmpl, remote, s.now())
	if err != nil {
		return ImportResult{}, err
	}
	idx := BuildIndex(plan.Next) // built before the commit; published after it

	if err := s.store.ReplaceSet(ctx, plan); err != nil {
		return ImportResult{}, s.commitError("import", plan.Set.Name, err)
	}
	s.publish(plan.Next, idx)
	s.logger.Info("sticker import committed", "set", plan.Set.Name, "active", idx.Len(),
		"added", plan.Report.Added, "updated", plan.Report.Updated, "reset", plan.Report.Reset,
		"forgotten", plan.Report.Forgotten, "unavailable", plan.Report.NowUnavailable, "incomplete", plan.Report.Incomplete)

	return ImportResult{Set: plan.Set.Name, Report: plan.Report}, nil
}

func (s *Service) publish(state State, idx *Index) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.index.Store(idx)
}

func (s *Service) commitError(op, set string, err error) error {
	if errors.Is(err, ErrConflict) {
		s.logger.Warn("sticker catalog write conflict", "op", op, "set", set)

		return ErrConflict
	}
	s.logger.Error("sticker catalog write failed", "op", op, "set", set)

	return fmt.Errorf("sticker catalog %s failed: %w", op, err)
}

// SetEnabled enables or disables a whole set. Disabling hides its stickers
// from the model without touching any annotation.
func (s *Service) SetEnabled(ctx context.Context, name string, enabled bool) error {
	if !s.opMu.TryLock() {
		return ErrBusy
	}
	defer s.opMu.Unlock()

	state := s.current()
	set, ok := state.Set(name)
	if !ok {
		return ErrUnknownSet
	}
	now := s.now()
	updated := set
	updated.Enabled, updated.UpdatedAt, updated.Revision = enabled, now, set.Revision+1
	next := replaceSet(state, updated, state.EntriesOf(name))
	if err := ValidateState(next); err != nil {
		return err
	}
	idx := BuildIndex(next)

	if err := s.store.SetEnabled(ctx, name, enabled, set.Revision, now); err != nil {
		return s.commitError("set-enabled", name, err)
	}
	s.publish(next, idx)

	return nil
}

// RemoveSet deletes a set and all its annotations.
func (s *Service) RemoveSet(ctx context.Context, name string) error {
	if !s.opMu.TryLock() {
		return ErrBusy
	}
	defer s.opMu.Unlock()

	state := s.current()
	set, ok := state.Set(name)
	if !ok {
		return ErrUnknownSet
	}
	next := State{}
	for _, other := range state.Sets {
		if other.Name != name {
			next.Sets = append(next.Sets, other)
		}
	}
	for _, e := range state.Entries {
		if e.SetName != name {
			next.Entries = append(next.Entries, e)
		}
	}
	idx := BuildIndex(next)

	if err := s.store.DeleteSet(ctx, name, set.Revision); err != nil {
		return s.commitError("remove", name, err)
	}
	s.publish(next, idx)

	return nil
}

// SetSummary counts one set's entries by state.
type SetSummary struct {
	Name        string
	Title       string
	Enabled     bool
	Total       int
	Active      int // selectable by the model right now
	Incomplete  int // available but lacking an id or a description
	Disabled    int // available and complete but individually disabled
	Unavailable int // retained, no longer in Telegram
}

// Sets summarizes every cataloged set, ordered by name.
func (s *Service) Sets() []SetSummary {
	state := s.current()
	byName := make(map[string]*SetSummary, len(state.Sets))
	out := make([]SetSummary, 0, len(state.Sets))
	for _, set := range state.Sets {
		out = append(out, SetSummary{Name: set.Name, Title: set.Title, Enabled: set.Enabled})
	}
	for i := range out {
		byName[out[i].Name] = &out[i]
	}
	for _, e := range state.Entries {
		sum := byName[e.SetName]
		if sum == nil {
			continue
		}
		sum.Total++
		switch {
		case !e.Available:
			sum.Unavailable++
		case !e.Complete():
			sum.Incomplete++
		case !e.Enabled:
			sum.Disabled++
		case sum.Enabled:
			sum.Active++
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

// Status is a whole-catalog summary.
type Status struct {
	Sets, Entries, Active, Incomplete, Disabled, Unavailable int
}

// Status totals the catalog.
func (s *Service) Status() Status {
	var st Status
	for _, sum := range s.Sets() {
		st.Sets++
		st.Entries += sum.Total
		st.Active += sum.Active
		st.Incomplete += sum.Incomplete
		st.Disabled += sum.Disabled
		st.Unavailable += sum.Unavailable
	}

	return st
}
