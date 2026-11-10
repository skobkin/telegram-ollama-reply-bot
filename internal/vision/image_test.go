package vision

import (
	"errors"
	"testing"
)

func TestDetectMIME_AcceptsSupportedSignatures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"jpeg", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("rest of jpeg")...), "image/jpeg"},
		{"png", append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("IHDR...")...), "image/png"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), []byte("VP8 data")...), "image/webp"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := DetectMIME(tc.data)
			if err != nil {
				t.Fatalf("detect: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}

// TestDetectMIME_RejectsUnsupportedTypes documents that detection is
// signature-based: a valid signature with a corrupt body still passes (the
// recognizer reports the failure later as a typed error), while unknown
// formats are rejected here.
func TestDetectMIME_RejectsUnsupportedTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data []byte
	}{
		{"gif", []byte("GIF89a....")},
		{"bmp", []byte("BM\x00\x00....")},
		{"svg", []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>")},
		{"garbage", []byte("not an image at all")},
		{"empty", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DetectMIME(tc.data)
			if !errors.Is(err, ErrUnsupportedImageType) {
				t.Fatalf("expected ErrUnsupportedImageType, got %v", err)
			}
		})
	}
}

// TestDetectMIME_CorruptAfterHeaderStillPasses pins the documented accuracy
// statement: sniffing validates the format signature, not the image body.
func TestDetectMIME_CorruptAfterHeaderStillPasses(t *testing.T) {
	t.Parallel()

	got, err := DetectMIME([]byte{0xFF, 0xD8, 0xFF, 0x00, 0x00, 0x00})
	if err != nil {
		t.Fatalf("signature detection must not validate the body: %v", err)
	}
	if got != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %s", got)
	}
}
