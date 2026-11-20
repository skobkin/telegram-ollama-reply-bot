package stickers

import "time"

// Format is the media format of one sticker. In the Bot API the format lives
// on each sticker, not on the set, so a set may mix formats.
type Format string

const (
	FormatStatic   Format = "static"
	FormatAnimated Format = "animated"
	FormatVideo    Format = "video"
)

// Valid reports whether f is a known format.
func (f Format) Valid() bool {
	switch f {
	case FormatStatic, FormatAnimated, FormatVideo:
		return true
	default:
		return false
	}
}

// RemoteSticker is one regular sticker as currently reported by Telegram.
type RemoteSticker struct {
	FileUniqueID string
	FileID       string
	Emoji        string
	Format       Format
}

// RemoteSet is a regular sticker set as currently reported by Telegram, in
// Telegram's display order.
type RemoteSet struct {
	Name     string
	Title    string
	Stickers []RemoteSticker
}

// Set is one cataloged sticker set.
type Set struct {
	Name      string
	Title     string
	Enabled   bool
	Revision  int64
	SyncedAt  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Entry is one cataloged sticker, identified by (SetName, FileUniqueID).
// FileID is only the current send token; Position is a display aid, never
// identity. SemanticID == "" means unassigned.
type Entry struct {
	SetName      string
	FileUniqueID string
	FileID       string
	SemanticID   string
	Description  string
	Tags         []string
	Emoji        string
	Format       Format
	Position     int
	Available    bool
	Enabled      bool
	SyncedAt     time.Time
	UpdatedAt    time.Time
}

// Annotated reports whether the entry carries any curated data.
func (e Entry) Annotated() bool {
	return e.SemanticID != "" || e.Description != "" || len(e.Tags) > 0
}

// Complete reports whether the entry has everything selection requires: a
// semantic ID and a description. Tags are optional.
func (e Entry) Complete() bool {
	return e.SemanticID != "" && e.Description != ""
}

// Active reports whether the entry may be exposed to the model and sent:
// available, enabled, complete, and its set enabled.
func (e Entry) Active(setEnabled bool) bool {
	return setEnabled && e.Available && e.Enabled && e.Complete()
}

// State is the complete catalog: every set and every entry, disabled,
// incomplete and unavailable ones included. It is treated as immutable once
// built; updates build a prospective State and publish it after commit.
type State struct {
	Sets    []Set
	Entries []Entry
}

// Set returns the named set.
func (s State) Set(name string) (Set, bool) {
	for _, set := range s.Sets {
		if set.Name == name {
			return set, true
		}
	}

	return Set{}, false
}

// EntriesOf returns the entries of one set in stored order.
func (s State) EntriesOf(name string) []Entry {
	var out []Entry
	for _, e := range s.Entries {
		if e.SetName == name {
			out = append(out, e)
		}
	}

	return out
}
