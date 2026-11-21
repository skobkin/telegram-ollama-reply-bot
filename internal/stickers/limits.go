package stickers

// Hard caps for catalog data and inputs. Every value is exercised at its
// boundary by tests.
const (
	// MaxUploadBytes bounds an uploaded YAML template. It must admit every
	// template the exporter can produce for a maximally annotated catalog:
	// 400 entries (MaxStickersPerSet + MaxMissingPerSet) with a full-length
	// description and 16 full-length tags each. The YAML encoder escapes
	// characters outside the BMP as \UXXXXXXXX (10 bytes per rune), which
	// puts that worst case near 3 MB; TestMaximalTemplateRoundTrips pins it.
	MaxUploadBytes = 4 << 20

	// MaxStickerFileBytes bounds one downloaded sticker file, measured on the
	// actual bytes. Telegram allows 512 KB static, 64 KB animated and 256 KB
	// video stickers; the rest is margin.
	MaxStickerFileBytes = 1 << 20
	// MaxArchiveDownloadBytes bounds the sum of all downloaded sticker files.
	MaxArchiveDownloadBytes = 40 << 20
	// MaxArchiveBytes bounds the ZIP document, safely below the Bot API's
	// 50 MB upload limit.
	MaxArchiveBytes = 45 << 20
	// ArchiveConcurrency bounds simultaneous Telegram file downloads.
	ArchiveConcurrency = 4

	// MaxSets bounds the number of cataloged sticker sets.
	MaxSets = 32
	// MaxStickersPerSet bounds the available stickers of one set. Telegram
	// allows at most 120 stickers in a regular set; the margin absorbs
	// future growth.
	MaxStickersPerSet = 200
	// MaxMissingPerSet bounds the retained (no longer in Telegram) entries
	// of one set.
	MaxMissingPerSet = 200
	// MaxTotalEntries bounds the whole catalog, retained entries included.
	MaxTotalEntries = 5000

	// MaxDescriptionRunes bounds a sticker description.
	MaxDescriptionRunes = 240
	// MaxTags bounds the tags of one sticker.
	MaxTags = 16
	// MaxTagRunes bounds one tag.
	MaxTagRunes = 32
	// MaxTitleRunes bounds a stored set title.
	MaxTitleRunes = 128
	// MaxEmojiRunes bounds the stored Telegram emoji string.
	MaxEmojiRunes = 32
	// MaxTelegramIDBytes bounds a stored file_id / file_unique_id.
	MaxTelegramIDBytes = 256

	// MaxIssues bounds the itemized problems reported for one rejected
	// input; the total is still counted.
	MaxIssues = 20

	// MaxSearchQueryRunes, MaxSearchTags and MaxSearchLimit bound
	// list_stickers arguments.
	MaxSearchQueryRunes = 200
	MaxSearchTags       = 8
	MaxSearchLimit      = 20
	// DefaultSearchLimit applies when the caller gives no limit.
	DefaultSearchLimit = 10
)
