package gatekeeper

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
)

func errorResponse(encoding string, body []byte) *http.Response {
	header := http.Header{}
	if encoding != "" {
		header.Set("Content-Encoding", encoding)
	}
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: header, Body: io.NopCloser(bytes.NewReader(body))}
}

func TestReadErrorBodyDecodesBeforeRedacting(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	zw.Write([]byte("bad key " + secretA))
	zw.Close()
	body, withheld := readErrorBody(errorResponse("gzip", compressed.Bytes()), secretA)
	if withheld || string(body) != "bad key [REDACTED]" {
		t.Fatalf("gzip: %q withheld=%v", body, withheld)
	}
	for name, response := range map[string]*http.Response{
		"br":           errorResponse("br", []byte("opaque "+secretA)),
		"corrupt gzip": errorResponse("gzip", []byte("not gzip "+secretA)),
		"stacked":      errorResponse("gzip, br", compressed.Bytes()),
	} {
		body, withheld := readErrorBody(response, secretA)
		if !withheld || strings.Contains(string(body), secretA) {
			t.Fatalf("%s: %q withheld=%v", name, body, withheld)
		}
	}
}

func TestRedactSecretKeepsUnrelatedText(t *testing.T) {
	short := "short-key"
	if got := string(redactSecret([]byte("use short-key, not ...-key"), short)); got != "use [REDACTED], not ...-key" {
		t.Fatalf("short secret: %q", got)
	}
	tail := secretA[len(secretA)-4:]
	if got := string(redactSecret([]byte("got sk-..."+tail+" and ...0000"), secretA)); got != "got sk-...[REDACTED] and ...0000" {
		t.Fatalf("masked echo: %q", got)
	}
}
