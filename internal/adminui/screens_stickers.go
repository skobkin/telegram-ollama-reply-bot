package adminui

import (
	"fmt"
	"strconv"
	"strings"

	"telegram-ollama-reply-bot/internal/stickers"
)

// StickerSetRef is one set row of the stickers screen: Index addresses the
// session's set snapshot.
type StickerSetRef struct {
	Index   int
	Summary stickers.SetSummary
	// Gone marks a set that left the catalog after the snapshot was taken.
	Gone bool
}

// StickersData feeds the stickers screen.
type StickersData struct {
	Status stickers.Status
	Sets   []StickerSetRef
	Page   int
	Pages  int
}

func setCounts(s stickers.SetSummary) string {
	parts := []string{fmt.Sprintf("%d/%d active", s.Active, s.Total)}
	if s.Incomplete > 0 {
		parts = append(parts, fmt.Sprintf("%d incomplete", s.Incomplete))
	}
	if s.Disabled > 0 {
		parts = append(parts, fmt.Sprintf("%d disabled", s.Disabled))
	}
	if s.Unavailable > 0 {
		parts = append(parts, fmt.Sprintf("%d missing", s.Unavailable))
	}

	return strings.Join(parts, ", ")
}

func (b screenBuilder) stickers(data StickersData) *Screen {
	var text strings.Builder
	text.WriteString("🎟 Stickers\n\n")
	st := data.Status
	fmt.Fprintf(&text, "%d set(s) — %d sticker(s): %d active, %d incomplete, %d disabled, %d missing from Telegram\n",
		st.Sets, st.Entries, st.Active, st.Incomplete, st.Disabled, st.Unavailable)
	text.WriteString("Only active stickers are visible to the model.\n\n")

	if len(data.Sets) == 0 {
		text.WriteString("No sticker sets yet. Use Export YAML… (or Export ZIP… for the images too) to get a template, fill it in, then Import YAML.\n")
	}
	rows := make([]ButtonRow, 0, len(data.Sets)+4)
	for _, ref := range data.Sets {
		state := ""
		if !ref.Summary.Enabled {
			state = " (disabled)"
		}
		if ref.Gone {
			fmt.Fprintf(&text, "%d. %s — no longer in the catalog\n", ref.Index+1, ref.Summary.Name)
		} else {
			fmt.Fprintf(&text, "%d. %s — %s%s\n", ref.Index+1, truncateRunes(ref.Summary.Title, 32), setCounts(ref.Summary), state)
		}
		rows = append(rows, ButtonRow{b.btn(truncateRunes(ref.Summary.Name, 28), VerbStickerSet, strconv.Itoa(ref.Index))})
	}
	if data.Pages > 1 {
		fmt.Fprintf(&text, "\nPage %d/%d\n", data.Page, data.Pages)
		var nav ButtonRow
		if data.Page > 1 {
			nav = append(nav, b.btn("‹ Prev", VerbStickers, strconv.Itoa(data.Page-1)))
		}
		if data.Page < data.Pages {
			nav = append(nav, b.btn("Next ›", VerbStickers, strconv.Itoa(data.Page+1)))
		}
		rows = append(rows, nav)
	}
	rows = append(rows,
		ButtonRow{b.btn("Export YAML…", VerbStickerExportNew), b.btn("Export ZIP…", VerbStickerZipNew)},
		ButtonRow{b.btn("Import YAML", VerbStickerImport)},
		b.footerRow(),
	)

	return &Screen{Text: text.String(), Rows: rows}
}

// StickerSetData feeds the set card.
type StickerSetData struct {
	Name    string
	Summary stickers.SetSummary
	Found   bool
}

func (b screenBuilder) stickerSet(data StickerSetData) *Screen {
	if !data.Found {
		return &Screen{
			Text: "🎟 " + data.Name + "\n\nThis set is no longer in the catalog.\n",
			Rows: []ButtonRow{b.footerRow()},
		}
	}

	s := data.Summary
	var text strings.Builder
	fmt.Fprintf(&text, "🎟 %s\n%s\n\n", truncateRunes(s.Title, 48), s.Name)
	if s.Enabled {
		text.WriteString("Status: enabled\n")
	} else {
		text.WriteString("Status: DISABLED — none of its stickers are visible to the model\n")
	}
	fmt.Fprintf(&text, "Stickers: %d total\n", s.Total)
	fmt.Fprintf(&text, "  active: %d\n  incomplete (no id or description): %d\n  individually disabled: %d\n  missing from Telegram (annotations kept): %d\n",
		s.Active, s.Incomplete, s.Disabled, s.Unavailable)

	toggle := "Disable set"
	if !s.Enabled {
		toggle = "Enable set"
	}

	return &Screen{
		Text: text.String(),
		Rows: []ButtonRow{
			{b.btn("Export YAML", VerbStickerExportCur), b.btn("Export ZIP", VerbStickerZipCur)},
			{b.btn("Import YAML", VerbStickerImport)},
			{b.btn(toggle, VerbStickerToggle), b.danger("Remove set", VerbStickerRemove)},
			b.footerRow(),
		},
	}
}
