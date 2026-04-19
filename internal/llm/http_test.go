package llm

import (
	"io"
	"net/http"
	"strings"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type roundTripDoer func(*http.Request) (*http.Response, error)

func (f roundTripDoer) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonHTTPResponse(body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}
