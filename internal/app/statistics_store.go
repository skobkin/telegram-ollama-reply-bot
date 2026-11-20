package app

import (
	"context"

	psqlite "telegram-ollama-reply-bot/internal/persistence/sqlite"
	"telegram-ollama-reply-bot/internal/statistics"
)

// statisticsSnapshotStore adapts the SQLite store to the plain-parameter
// storage interface of the statistics persister, so the statistics package
// never imports the persistence layer.
type statisticsSnapshotStore struct {
	store *psqlite.Store
}

var _ statistics.SnapshotStore = statisticsSnapshotStore{}

func (s statisticsSnapshotStore) LoadSnapshotMeta(ctx context.Context, dataset string) (statistics.SnapshotMeta, bool, error) {
	meta, ok, err := s.store.LoadStatisticsSnapshotMeta(ctx, dataset)
	if err != nil || !ok {
		return statistics.SnapshotMeta{}, ok, err
	}

	return statistics.SnapshotMeta{
		FormatVersion: meta.FormatVersion,
		Pair:          statistics.PersistPair{Generation: meta.Generation, Revision: meta.Revision},
		PayloadBytes:  meta.PayloadBytes,
		SavedAt:       meta.SavedAt,
	}, true, nil
}

func (s statisticsSnapshotStore) LoadSnapshotPayload(ctx context.Context, dataset string) ([]byte, error) {
	return s.store.LoadStatisticsSnapshotPayload(ctx, dataset)
}

func (s statisticsSnapshotStore) SaveSnapshot(ctx context.Context, row statistics.SnapshotRow) (bool, error) {
	return s.store.SaveStatisticsSnapshot(ctx, psqlite.StatisticsSnapshotRow{
		Dataset:       row.Dataset,
		FormatVersion: row.FormatVersion,
		Generation:    row.Pair.Generation,
		Revision:      row.Pair.Revision,
		Payload:       row.Payload,
		SavedAt:       row.SavedAt,
	})
}

func (s statisticsSnapshotStore) QuarantineSnapshot(ctx context.Context, dataset string) error {
	return s.store.QuarantineStatisticsSnapshot(ctx, dataset)
}
