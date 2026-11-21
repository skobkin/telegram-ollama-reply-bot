package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"telegram-ollama-reply-bot/internal/stickers"

	tg "github.com/mymmrac/telego"
)

type fakeFileAPI struct {
	base string
	file *tg.File
	err  error
}

func (f fakeFileAPI) GetFile(context.Context, *tg.GetFileParams) (*tg.File, error) {
	return f.file, f.err
}

func (f fakeFileAPI) FileDownloadURL(path string) string {
	return f.base + "/file/botSECRET-TOKEN/" + path
}

func fileServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, telegoStickerFiles, *tg.File) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	file := &tg.File{FileID: "f", FilePath: "stickers/a.webp"}

	return srv, telegoStickerFiles{api: fakeFileAPI{base: srv.URL, file: file}, client: newStickerFileClient()}, file
}

func TestStickerFileDownloadSuccess(t *testing.T) {
	_, dl, file := fileServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "stickers/a.webp") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("webp-bytes"))
	})
	file.FileSize = 10

	data, err := dl.DownloadFile(context.Background(), "f", 64)
	if err != nil || string(data) != "webp-bytes" {
		t.Fatalf("data %q err %v", data, err)
	}
}

func TestStickerFileReportedSizeOverCapMakesNoDownload(t *testing.T) {
	var hits atomic.Int32
	_, dl, file := fileServer(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	file.FileSize = 65

	if _, err := dl.DownloadFile(context.Background(), "f", 64); !errors.Is(err, stickers.ErrFileTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("downloaded despite a reported oversize")
	}
}

func TestStickerFileActualBodyOverCapIsRejected(t *testing.T) {
	_, dl, _ := fileServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{1}, 65)) // FileSize understated (0)
	})

	if _, err := dl.DownloadFile(context.Background(), "f", 64); !errors.Is(err, stickers.ErrFileTooLarge) {
		t.Fatalf("err = %v", err)
	}
	// Exactly at the cap is fine.
	_, dl, _ = fileServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bytes.Repeat([]byte{1}, 64)) })
	if data, err := dl.DownloadFile(context.Background(), "f", 64); err != nil || len(data) != 64 {
		t.Fatalf("at-cap download: %d %v", len(data), err)
	}
}

func TestStickerFileErrorsNeverLeakTokenOrURL(t *testing.T) {
	cases := map[string]func(*testing.T) error{
		"non-200": func(t *testing.T) error {
			_, dl, _ := fileServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
			_, err := dl.DownloadFile(context.Background(), "f", 64)

			return err
		},
		"getFile fails": func(*testing.T) error {
			dl := telegoStickerFiles{api: fakeFileAPI{base: "http://x", err: errors.New("Post https://api.telegram.org/botSECRET-TOKEN/getFile: boom")}, client: newStickerFileClient()}
			_, err := dl.DownloadFile(context.Background(), "f", 64)

			return err
		},
		"connection refused": func(t *testing.T) error {
			srv, dl, _ := fileServer(t, func(http.ResponseWriter, *http.Request) {})
			srv.Close()
			_, err := dl.DownloadFile(context.Background(), "f", 64)

			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			err := run(t)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, banned := range []string{"SECRET-TOKEN", "/file/bot", "http://", "https://", "127.0.0.1"} {
				if strings.Contains(err.Error(), banned) {
					t.Fatalf("error leaks %q: %v", banned, err)
				}
			}
		})
	}
}

func TestStickerFileRedirectIsNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	_, dl, _ := fileServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusFound)
	})

	_, err := dl.DownloadFile(context.Background(), "f", 64)
	if err == nil || !strings.Contains(err.Error(), "http status 302") {
		t.Fatalf("err = %v", err)
	}
	if targetHits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestStickerFileCancellation(t *testing.T) {
	_, dl, _ := fileServer(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dl.DownloadFile(ctx, "f", 64); err == nil {
		t.Fatal("cancelled context still downloaded")
	}
}
