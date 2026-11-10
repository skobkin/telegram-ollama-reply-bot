package vision

import (
	"fmt"
	"net/http"
)

// supportedMIME lists the image formats accepted for recognition: the three
// types the MiniMax VLM API supports, which the OpenAI-compatible multimodal
// path also sends as data URLs.
var supportedMIME = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
	"image/webp": {},
}

// DetectMIME detects the image format from the leading byte signature of data
// and returns one of image/jpeg, image/png, or image/webp.
//
// This is signature detection, not image validation: data whose header matches
// but whose body is corrupt still passes detection and fails later at the
// recognizer as a typed error. The MIME type is derived from the actual bytes
// only — never from Telegram metadata, filenames, or content-type headers —
// and unknown formats are rejected rather than defaulted.
func DetectMIME(data []byte) (string, error) {
	detected := http.DetectContentType(data)
	if _, ok := supportedMIME[detected]; !ok {
		return "", fmt.Errorf("%w: detected %s; supported types are image/jpeg, image/png, image/webp", ErrUnsupportedImageType, detected)
	}

	return detected, nil
}
