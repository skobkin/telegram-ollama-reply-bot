package app

import (
	"context"
	"errors"
	"strings"

	"telegram-ollama-reply-bot/internal/stickers"
	"telegram-ollama-reply-bot/internal/support/safeerr"

	tg "github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

// stickerSetAPI is the one Bot API method the catalog needs. getStickerSet is
// read-only.
type stickerSetAPI interface {
	GetStickerSet(ctx context.Context, params *tg.GetStickerSetParams) (*tg.StickerSet, error)
}

// errStickerSetRequest names getStickerSet failures; the raw error is
// dropped by safeerr because SDK errors can embed the request URL (and with
// it the bot token).
var errStickerSetRequest = errors.New("telegram getStickerSet request failed")

// telegoStickerFetcher maps getStickerSet onto the domain's RemoteSet and its
// error vocabulary.
type telegoStickerFetcher struct {
	api stickerSetAPI
}

var _ stickers.SetFetcher = telegoStickerFetcher{}

func (f telegoStickerFetcher) FetchSet(ctx context.Context, name string) (stickers.RemoteSet, error) {
	set, err := f.api.GetStickerSet(ctx, &tg.GetStickerSetParams{Name: name})
	if err != nil {
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && apiErr.ErrorCode == 400 && isStickerSetNotFound(apiErr.Description) {
			return stickers.RemoteSet{}, stickers.ErrSetNotFound
		}

		return stickers.RemoteSet{}, errors.Join(stickers.ErrFetchUnavailable, safeerr.WrapTransport(errStickerSetRequest, err))
	}
	if set == nil {
		return stickers.RemoteSet{}, errors.Join(stickers.ErrFetchUnavailable, errStickerSetRequest)
	}

	return mapStickerSet(set)
}

func isStickerSetNotFound(description string) bool {
	d := strings.ToUpper(description)

	return strings.Contains(d, "STICKERSET_INVALID") || strings.Contains(d, "STICKER SET NAME")
}

// mapStickerSet accepts only regular sets of regular stickers. The format
// lives on each sticker in the Bot API, so a set may mix formats.
func mapStickerSet(set *tg.StickerSet) (stickers.RemoteSet, error) {
	if set.StickerType != tg.StickerTypeRegular {
		return stickers.RemoteSet{}, stickers.ErrUnsupportedSet
	}

	out := stickers.RemoteSet{Name: set.Name, Title: set.Title, Stickers: make([]stickers.RemoteSticker, 0, len(set.Stickers))}
	for _, s := range set.Stickers {
		if s.Type != "" && s.Type != tg.StickerTypeRegular {
			return stickers.RemoteSet{}, stickers.ErrUnsupportedSet
		}
		format := stickers.FormatStatic
		switch {
		case s.IsVideo:
			format = stickers.FormatVideo
		case s.IsAnimated:
			format = stickers.FormatAnimated
		}
		out.Stickers = append(out.Stickers, stickers.RemoteSticker{
			FileUniqueID: s.FileUniqueID, FileID: s.FileID, Emoji: s.Emoji, Format: format,
		})
	}

	return out, nil
}
