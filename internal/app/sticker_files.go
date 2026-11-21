package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"telegram-ollama-reply-bot/internal/stickers"
	"telegram-ollama-reply-bot/internal/support/httpclient"
	"telegram-ollama-reply-bot/internal/support/safeerr"

	tg "github.com/mymmrac/telego"
)

// stickerFileAPI is the Bot API surface of a file download: getFile
// (metadata) and the download URL builder. *tg.Bot satisfies it.
type stickerFileAPI interface {
	GetFile(ctx context.Context, params *tg.GetFileParams) (*tg.File, error)
	FileDownloadURL(filepath string) string
}

// Errors name failures without causes: getFile and download URLs embed the
// bot token, so raw transport errors are dropped via safeerr.
var (
	errStickerFileMetadata = errors.New("telegram sticker file metadata request failed")
	errStickerFileDownload = errors.New("telegram sticker file download failed")
)

// newStickerFileClient never follows redirects: a Telegram file URL carries
// the token, and a redirect could forward it elsewhere.
func newStickerFileClient() *http.Client {
	client := httpclient.New()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return client
}

// telegoStickerFiles downloads sticker files by file_id. Only the Bot API's
// own file endpoint is ever contacted: the URL comes from the SDK, never from
// model or user input.
type telegoStickerFiles struct {
	api    stickerFileAPI
	client *http.Client
}

var _ stickers.FileDownloader = telegoStickerFiles{}

func (f telegoStickerFiles) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, error) {
	file, err := f.api.GetFile(ctx, &tg.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, safeerr.WrapTransport(errStickerFileMetadata, err)
	}
	if file == nil || file.FilePath == "" {
		return nil, fmt.Errorf("%w: no file path", errStickerFileMetadata)
	}
	if file.FileSize > maxBytes {
		return nil, stickers.ErrFileTooLarge
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.api.FileDownloadURL(file.FilePath), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request", errStickerFileDownload)
	}
	response, err := f.client.Do(request)
	if err != nil {
		return nil, safeerr.WrapTransport(errStickerFileDownload, err)
	}
	defer response.Body.Close() //nolint:errcheck // read-side cleanup

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: http status %d", errStickerFileDownload, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, safeerr.WrapTransport(errStickerFileDownload, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, stickers.ErrFileTooLarge
	}

	return data, nil
}
