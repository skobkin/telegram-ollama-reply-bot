package app

import (
	"context"
	"errors"
	"time"

	psqlite "telegram-ollama-reply-bot/internal/persistence/sqlite"
	"telegram-ollama-reply-bot/internal/stickers"
)

// stickerCatalogStore adapts the SQLite store to the stickers package's
// storage interface, mapping persistence rows to domain types so the
// stickers package never imports the persistence layer.
type stickerCatalogStore struct {
	store *psqlite.Store
}

var _ stickers.Store = stickerCatalogStore{}

func (s stickerCatalogStore) LoadCatalog(ctx context.Context) (stickers.State, error) {
	sets, entries, err := s.store.LoadStickerCatalog(ctx)
	if err != nil {
		return stickers.State{}, err
	}

	state := stickers.State{
		Sets:    make([]stickers.Set, 0, len(sets)),
		Entries: make([]stickers.Entry, 0, len(entries)),
	}
	for _, row := range sets {
		state.Sets = append(state.Sets, stickers.Set{
			Name: row.Name, Title: row.Title, Enabled: row.Enabled, Revision: row.Revision,
			SyncedAt: row.SyncedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	for _, row := range entries {
		state.Entries = append(state.Entries, stickers.Entry{
			SetName: row.SetName, FileUniqueID: row.FileUniqueID, FileID: row.FileID, SemanticID: row.SemanticID,
			Description: row.Description, Tags: row.Tags, Emoji: row.Emoji, Format: stickers.Format(row.Format),
			Position: row.Position, Available: row.Available, Enabled: row.Enabled,
			SyncedAt: row.SyncedAt, UpdatedAt: row.UpdatedAt,
		})
	}

	return state, nil
}

func (s stickerCatalogStore) ReplaceSet(ctx context.Context, plan *stickers.ImportPlan) error {
	write := psqlite.StickerSetWrite{
		Set: psqlite.StickerSetRow{
			Name: plan.Set.Name, Title: plan.Set.Title, Enabled: plan.Set.Enabled,
			SyncedAt: plan.Set.SyncedAt, CreatedAt: plan.Set.CreatedAt, UpdatedAt: plan.Set.UpdatedAt,
		},
		ExpectedRevision: plan.ExpectedRevision,
		Entries:          make([]psqlite.StickerEntryRow, 0, len(plan.SetEntries)),
		Forgotten:        plan.Forgotten,
		Release:          plan.Release,
	}
	for _, e := range plan.SetEntries {
		write.Entries = append(write.Entries, psqlite.StickerEntryRow{
			SetName: e.SetName, FileUniqueID: e.FileUniqueID, FileID: e.FileID, SemanticID: e.SemanticID,
			Description: e.Description, Tags: e.Tags, Emoji: e.Emoji, Format: string(e.Format),
			Position: e.Position, Available: e.Available, Enabled: e.Enabled,
			SyncedAt: e.SyncedAt, UpdatedAt: e.UpdatedAt,
		})
	}

	return mapStickerConflict(s.store.ReplaceStickerSet(ctx, write))
}

func (s stickerCatalogStore) SetEnabled(ctx context.Context, name string, enabled bool, expectedRevision int64, at time.Time) error {
	return mapStickerConflict(s.store.SetStickerSetEnabled(ctx, name, enabled, expectedRevision, at))
}

func (s stickerCatalogStore) DeleteSet(ctx context.Context, name string, expectedRevision int64) error {
	return mapStickerConflict(s.store.DeleteStickerSet(ctx, name, expectedRevision))
}

func mapStickerConflict(err error) error {
	if errors.Is(err, psqlite.ErrStickerSetConflict) {
		return stickers.ErrConflict
	}

	return err
}
