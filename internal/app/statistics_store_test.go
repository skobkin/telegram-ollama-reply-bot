package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	psqlite "telegram-ollama-reply-bot/internal/persistence/sqlite"
	"telegram-ollama-reply-bot/internal/statistics"
)

func openStatsStore(t *testing.T, path string) *psqlite.Store {
	t.Helper()
	store, err := psqlite.Open(context.Background(), path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return store
}

func newStatsPersister(store statistics.SnapshotStore, c *statistics.Collector, report func(error)) *statistics.Persister {
	return statistics.NewPersister(statistics.PersisterConfig{
		Store:           store,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Interval:        time.Hour,
		WriteTimeout:    5 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		Report:          report,
	}, c.InteractionDataset(), c.ToolDataset())
}

func recordSampleStatistics(c *statistics.Collector) {
	keys := statistics.InteractionKeys{ChatID: 100, Profile: "talk_low", DecisionModel: "dm", Mode: statistics.ModeLive}
	epoch := c.BeginSubmission(100)
	c.RecordInference(statistics.InferenceAttempt{Epoch: epoch, Keys: keys, Model: "dm", Latency: time.Second, ErrClass: statistics.InferenceSuccess})
	c.RecordDecision(statistics.DecisionObservation{Epoch: epoch, Keys: keys, Outcome: statistics.OutcomeSelectedReply,
		Scores: &statistics.ScoreSet{Base: 0.8}, DecisionLatency: time.Second})
	c.RecordExecutionDispatch(statistics.ExecutionDispatch{Epoch: epoch, Keys: keys, GenerationModel: "gm", Action: statistics.ActionReply})
	c.RecordExecution(statistics.ExecutionObservation{Epoch: epoch, Keys: keys, GenerationModel: "gm", Action: statistics.ActionReply, TextConfirmed: true, Duration: 2 * time.Second})

	// Open at "shutdown": a pending decision and a dispatched, unfinished turn.
	c.BeginSubmission(100)
	open := c.BeginSubmission(100)
	c.RecordExecutionDispatch(statistics.ExecutionDispatch{Epoch: open, Keys: keys, GenerationModel: "gm", Action: statistics.ActionReact})

	c.RecordToolTurn(statistics.ToolTurnObservation{
		Epoch: c.ToolEpoch(), ChatID: 100, GenerationModel: "gm", Advertised: []string{"search_web", "get_current_time"},
		Calls: []statistics.ToolCallObservation{
			{Name: "search_web", Executed: true, Status: "error", ErrorCode: "provider_unavailable", ErrorClass: "transient", Duration: 100 * time.Millisecond},
			{Name: "search_web", Executed: true, Status: "ok", Duration: 200 * time.Millisecond},
		},
	})
}

func snapshotsJSON(t *testing.T, c *statistics.Collector) (interaction, tool string) {
	t.Helper()
	i := c.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
	i.Persistence, i.GeneratedAt = statistics.PersistenceInfo{}, time.Time{}
	tl := c.ToolSnapshot(statistics.GlobalScope())
	tl.Persistence, tl.GeneratedAt = statistics.PersistenceInfo{}, time.Time{}
	ib, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := json.Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}

	return string(ib), string(tb)
}

func TestStatisticsSurviveARestartThroughSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bot.sqlite")

	// First process: collect, then shut down gracefully (final flush).
	first := openStatsStore(t, path)
	c1 := statistics.NewCollector(statistics.Config{RubricVersion: 3})
	p1 := newStatsPersister(statisticsSnapshotStore{store: first}, c1, nil)
	if err := p1.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	p1.Start(ctx)
	recordSampleStatistics(c1)
	if err := p1.Stop(); err != nil {
		t.Fatalf("graceful shutdown must flush: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Second process: restore, with the open operations reconciled.
	second := openStatsStore(t, path)
	c2 := statistics.NewCollector(statistics.Config{RubricVersion: 3})
	p2 := newStatsPersister(statisticsSnapshotStore{store: second}, c2, nil)
	if err := p2.Restore(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}

	snap := c2.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
	if snap.Submission.Submitted != 3 || snap.Submission.InterruptedByRestart != 2 {
		t.Fatalf("submissions must be restored and the open ones interrupted: %+v", snap.Submission)
	}
	if snap.Execution.Totals.TurnsInterrupted != 1 || snap.Execution.Totals.TextsConfirmed != 1 {
		t.Fatalf("execution restored, the unfinished turn interrupted: %+v", snap.Execution.Totals)
	}
	if !snap.Persistence.Restored || snap.Persistence.State != statistics.PersistEnabled {
		t.Fatalf("restoration must be disclosed: %+v", snap.Persistence)
	}
	tools := c2.ToolSnapshot(statistics.GlobalScope())
	var search statistics.ToolRowView
	for _, row := range tools.Tools {
		if row.Tool != nil && *row.Tool == "search_web" {
			search = row
		}
	}
	if search.Counters.Attempts != 2 || search.Counters.RecoveredIdentityTurns != 1 || len(search.ErrorCodes) != 1 {
		t.Fatalf("tool statistics must be restored: %+v", search.Counters)
	}

	// Restart idempotence: flush the reconciled state and restart again.
	p2.Start(ctx)
	if err := p2.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	third := openStatsStore(t, path)
	c3 := statistics.NewCollector(statistics.Config{RubricVersion: 3})
	if err := newStatsPersister(statisticsSnapshotStore{store: third}, c3, nil).Restore(ctx); err != nil {
		t.Fatal(err)
	}
	want, wantTools := snapshotsJSON(t, c2)
	got, gotTools := snapshotsJSON(t, c3)
	if want != got || wantTools != gotTools {
		t.Fatal("a second restart must not change the restored statistics (no double counting)")
	}
}

func TestStatisticsResetSurvivesARestartAndStaleWritesAreRejected(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bot.sqlite")
	store := openStatsStore(t, path)
	c := statistics.NewCollector(statistics.Config{})
	p := newStatsPersister(statisticsSnapshotStore{store: store}, c, nil)
	recordSampleStatistics(c)
	if err := p.FlushNow(ctx, statistics.ToolDatasetName); err != nil {
		t.Fatal(err)
	}
	staleMeta, ok, err := store.LoadStatisticsSnapshotMeta(ctx, statistics.ToolDatasetName)
	if err != nil || !ok {
		t.Fatalf("meta: ok=%v err=%v", ok, err)
	}
	stalePayload, err := store.LoadStatisticsSnapshotPayload(ctx, statistics.ToolDatasetName)
	if err != nil {
		t.Fatal(err)
	}

	generation := c.ResetToolUsage()
	if out := p.EnsureDurable(ctx, statistics.ToolDatasetName, generation); !out.Durable {
		t.Fatalf("reset must be durable: %+v", out)
	}
	// A delayed write captured before the reset is rejected by the database.
	applied, err := store.SaveStatisticsSnapshot(ctx, psqlite.StatisticsSnapshotRow{
		Dataset: statistics.ToolDatasetName, FormatVersion: staleMeta.FormatVersion, Generation: staleMeta.Generation,
		Revision: staleMeta.Revision + 5, Payload: stalePayload, SavedAt: time.Now(),
	})
	if err != nil || applied {
		t.Fatalf("stale write must be rejected: applied=%v err=%v", applied, err)
	}

	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened := openStatsStore(t, path)
	restarted := statistics.NewCollector(statistics.Config{})
	if err := newStatsPersister(statisticsSnapshotStore{store: reopened}, restarted, nil).Restore(ctx); err != nil {
		t.Fatal(err)
	}
	tools := restarted.ToolSnapshot(statistics.GlobalScope())
	if tools.Period.Resets != 1 || tools.Totals.Counters.Attempts != 0 {
		t.Fatalf("a restart must show the reset tool state: %+v", tools.Period)
	}
	// The interaction dataset was not reset: the graceful stop flushed it.
	if got := restarted.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{}).Submission.Submitted; got != 3 {
		t.Fatalf("the independent interaction dataset must be intact, submitted=%d", got)
	}
}

func TestStatisticsNewerFormatRowIsNeverTouchedOrQuarantined(t *testing.T) {
	ctx := context.Background()
	store := openStatsStore(t, filepath.Join(t.TempDir(), "bot.sqlite"))
	row := psqlite.StatisticsSnapshotRow{
		Dataset: statistics.InteractionDatasetName, FormatVersion: 99, Generation: 7, Revision: 7,
		Payload: []byte(`{"from":"the future"}`), SavedAt: time.Now(),
	}
	if _, err := store.SaveStatisticsSnapshot(ctx, row); err != nil {
		t.Fatal(err)
	}

	var reports int
	c := statistics.NewCollector(statistics.Config{})
	p := newStatsPersister(statisticsSnapshotStore{store: store}, c, func(error) { reports++ })
	if err := p.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if p.State(statistics.InteractionDatasetName) != statistics.PersistDisabledNewerFormat || reports != 1 {
		t.Fatalf("state %s reports %d", p.State(statistics.InteractionDatasetName), reports)
	}

	c.BeginSubmission(1) // collection continues in memory
	if err := p.Stop(); err != nil {
		t.Fatalf("a disabled dataset must not fail the shutdown: %v", err)
	}
	payload, err := store.LoadStatisticsSnapshotPayload(ctx, statistics.InteractionDatasetName)
	if err != nil || string(payload) != `{"from":"the future"}` {
		t.Fatalf("the newer-format row must be untouched: %q err=%v", payload, err)
	}
	if _, quarantined, _ := store.LoadStatisticsSnapshotMeta(ctx, statistics.InteractionDatasetName+psqlite.StatisticsQuarantineSuffix); quarantined {
		t.Fatal("a newer-format row must not be quarantined")
	}
	snap := c.InteractionSnapshot(statistics.GlobalScope(), statistics.InteractionFilters{})
	if snap.Persistence.State != statistics.PersistDisabledNewerFormat {
		t.Fatalf("the disabled state must be disclosed: %+v", snap.Persistence)
	}
}

func TestStatisticsMalformedRowIsQuarantinedAndCollectionStartsFresh(t *testing.T) {
	ctx := context.Background()
	store := openStatsStore(t, filepath.Join(t.TempDir(), "bot.sqlite"))
	if _, err := store.SaveStatisticsSnapshot(ctx, psqlite.StatisticsSnapshotRow{
		Dataset: statistics.ToolDatasetName, FormatVersion: 1, Generation: 3, Revision: 9,
		Payload: []byte(`{not json`), SavedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	c := statistics.NewCollector(statistics.Config{})
	p := newStatsPersister(statisticsSnapshotStore{store: store}, c, nil)
	if err := p.Restore(ctx); err != nil {
		t.Fatalf("a malformed row must not fail startup: %v", err)
	}
	quarantined, err := store.LoadStatisticsSnapshotPayload(ctx, statistics.ToolDatasetName+psqlite.StatisticsQuarantineSuffix)
	if err != nil || string(quarantined) != `{not json` {
		t.Fatalf("the unreadable payload must be preserved in the quarantine slot: %q err=%v", quarantined, err)
	}
	if _, ok, _ := store.LoadStatisticsSnapshotMeta(ctx, statistics.ToolDatasetName); ok {
		t.Fatal("the original row must have been moved away")
	}

	c.RecordToolTurn(statistics.ToolTurnObservation{Epoch: c.ToolEpoch(), Advertised: []string{"t"}})
	if err := p.FlushNow(ctx, statistics.ToolDatasetName); err != nil {
		t.Fatalf("a fresh dataset can be saved again: %v", err)
	}
}

// failingStatsStore simulates database failures around a real store.
type failingStatsStore struct {
	statisticsSnapshotStore
	failSaves bool
}

func (s *failingStatsStore) SaveSnapshot(ctx context.Context, row statistics.SnapshotRow) (bool, error) {
	if s.failSaves {
		return false, errors.New("database is locked")
	}

	return s.statisticsSnapshotStore.SaveSnapshot(ctx, row)
}

func TestStatisticsDatabaseFailureKeepsDataAndRecovers(t *testing.T) {
	ctx := context.Background()
	store := openStatsStore(t, filepath.Join(t.TempDir(), "bot.sqlite"))
	failing := &failingStatsStore{statisticsSnapshotStore: statisticsSnapshotStore{store: store}, failSaves: true}
	var reports int
	c := statistics.NewCollector(statistics.Config{})
	p := newStatsPersister(failing, c, func(error) { reports++ })

	recordSampleStatistics(c)
	for i := 0; i < 3; i++ {
		if err := p.FlushNow(ctx, statistics.ToolDatasetName); err == nil {
			t.Fatal("expected a save failure")
		}
	}
	if p.State(statistics.ToolDatasetName) != statistics.PersistWriteFailing || reports != 1 {
		t.Fatalf("state %s, reports %d (only the first failure of a streak is reported)", p.State(statistics.ToolDatasetName), reports)
	}
	// Nothing was discarded: the live aggregate is intact and the full state
	// is written once the database recovers.
	if got := c.ToolSnapshot(statistics.GlobalScope()).Totals.Counters.Attempts; got != 2 {
		t.Fatalf("live statistics must be intact, attempts=%d", got)
	}
	failing.failSaves = false
	if err := p.Stop(); err != nil {
		t.Fatalf("the final flush must write the retained state: %v", err)
	}
	restarted := statistics.NewCollector(statistics.Config{})
	if err := newStatsPersister(statisticsSnapshotStore{store: store}, restarted, nil).Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if got := restarted.ToolSnapshot(statistics.GlobalScope()).Totals.Counters.Attempts; got != 2 {
		t.Fatalf("the retained state must have been persisted, attempts=%d", got)
	}
}
