package gatekeeper

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
)

var redacted = []byte("[REDACTED]")

// withheldBody replaces an upstream error body the gatekeeper cannot decode, so a credential
// echoed inside it never reaches the agent in a form that skipped redaction.
const withheldBody = `{"error":{"type":"aiofcage_withheld","message":"AIofCage gatekeeper: the upstream error body used an encoding that cannot be checked for credentials, so it was withheld"}}`

// readErrorBody returns an upstream error body that is safe to show the agent. It is decoded
// before redaction, because a credential echoed inside a gzip body would otherwise pass through
// unchanged and the agent could decompress it itself. The second result reports whether the body
// was withheld instead.
func readErrorBody(resp *http.Response, secret string) ([]byte, bool) {
	var reader io.Reader = resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip", "x-gzip":
		decoded, err := gzip.NewReader(resp.Body)
		if err != nil {
			return []byte(withheldBody), true
		}
		defer decoded.Close()
		reader = decoded
	case "deflate":
		decoded, err := zlib.NewReader(resp.Body)
		if err != nil {
			return []byte(withheldBody), true
		}
		defer decoded.Close()
		reader = decoded
	default:
		// br, zstd and stacked encodings cannot be decoded with the standard library.
		return []byte(withheldBody), true
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxErrorBody))
	if err != nil && err != io.ErrUnexpectedEOF {
		return []byte(withheldBody), true
	}
	return redactSecret(body, secret), false
}

// redactSecret masks the credential and the partial forms upstreams echo in error messages: a
// masked key such as "sk-...a1b2" and the key's SHA-256, which LiteLLM-based gateways report.
func redactSecret(body []byte, secret string) []byte {
	if secret == "" {
		return body
	}
	body = bytes.ReplaceAll(body, []byte(secret), redacted)
	if len(secret) >= 16 {
		tail := secret[len(secret)-4:]
		for _, mark := range []string{"...", "…", "***"} {
			body = bytes.ReplaceAll(body, []byte(mark+tail), append([]byte(mark), redacted...))
		}
	}
	sum := sha256.Sum256([]byte(secret))
	return bytes.ReplaceAll(body, []byte(hex.EncodeToString(sum[:])), redacted)
}
