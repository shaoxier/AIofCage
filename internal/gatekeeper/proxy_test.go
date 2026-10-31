package gatekeeper

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/audit"
	"github.com/shaoxier/AIofCage/internal/policy"
)

const (
	secretA      = "synthetic-secret-provider-a-0123456789"
	secretB      = "synthetic-secret-provider-b-9876543210"
	placeholderA = "cage-placeholder-provider-a-xyz"
	placeholderB = "cage-placeholder-provider-b-xyz"
)

func testToken() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
}

func testConfig(version uint64) Config {
	return Config{
		Policy: policy.Snapshot{Version: version, SessionID: "sbx-test", Rules: []policy.Rule{
			{ID: "model-a", Effect: policy.Allow, Host: "model-a.example", Methods: []string{"POST"}, Path: "/v1/responses", SecretRef: "provider-a"},
			{ID: "model-a-public", Effect: policy.Allow, Host: "model-a.example", Methods: []string{"GET"}, Path: "/v1/public"},
			{ID: "model-b", Effect: policy.Allow, Host: "model-b.example", Methods: []string{"POST"}, Path: "/v1/messages", SecretRef: "provider-b"},
			{ID: "files", Effect: policy.Deny, Host: "model-a.example", Path: "/v1/files/*"},
		}},
		Secrets: []Secret{
			{ID: "provider-a", Placeholder: placeholderA, Header: "Authorization", Prefix: "Bearer ", Value: secretA},
			{ID: "provider-b", Placeholder: placeholderB, Header: "x-api-key", Value: secretB},
		},
	}
}

type fixture struct {
	gate     *Gate
	handlers Handlers
	client   *http.Client
	proxyURL string
	calls    atomic.Int32
	handler  atomic.Value // http.HandlerFunc
}

// newFixture starts a TLS upstream serving model-a.example and model-b.example and routes the
// gatekeeper's (already authorized) connections to it. This dialer override exists only in tests.
func newFixture(t *testing.T, trustUpstream bool) *fixture {
	t.Helper()
	f := &fixture{}
	f.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	upstreamCA, _, err := newCA()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"model-a.example", "model-b.example"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, upstreamCA.Leaf, &key.PublicKey, upstreamCA.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.handler.Load().(http.HandlerFunc)(w, r)
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	f.gate, err = NewGate()
	if err != nil {
		t.Fatal(err)
	}
	f.gate.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "model-a.example:443" && address != "model-b.example:443" {
			return nil, fmt.Errorf("unexpected destination %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	if trustUpstream {
		roots := x509.NewCertPool()
		roots.AddCert(upstreamCA.Leaf)
		f.gate.transport.TLSClientConfig.RootCAs = roots
	}
	f.handlers, err = f.gate.Handlers(testToken())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(f.handlers.Proxy)
	t.Cleanup(proxy.Close)
	f.proxyURL = proxy.URL
	u, _ := url.Parse(proxy.URL)
	gateRoots := x509.NewCertPool()
	gateRoots.AppendCertsFromPEM(f.gate.caPEM)
	transport := &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{RootCAs: gateRoots}}
	f.client = &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(transport.CloseIdleConnections)
	t.Cleanup(f.gate.transport.CloseIdleConnections)
	return f
}

func (f *fixture) setHandler(h http.HandlerFunc) { f.handler.Store(h) }

func (f *fixture) manage(t *testing.T, method, target string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	r := httptest.NewRequest(method, target, reader)
	r.Header.Set("Authorization", "Bearer "+testToken())
	w := httptest.NewRecorder()
	f.handlers.Management.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func (f *fixture) configure(t *testing.T, config Config) {
	t.Helper()
	if status, body := f.manage(t, "PUT", "/config", config); status != 200 {
		t.Fatalf("configure: %d %s", status, body)
	}
}

func (f *fixture) do(t *testing.T, method, target string, headers map[string][]string, body string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data), resp.Header
}

func (f *fixture) eventsAfter(t *testing.T, seq uint64) []audit.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.gate.events.waitFor(ctx, seq+1); err != nil {
		t.Fatalf("waiting for events after %d: %v", seq, err)
	}
	events, _, _ := f.gate.events.since(seq)
	return events
}

func bearer(value string) map[string][]string {
	return map[string][]string{"Authorization": {"Bearer " + value}}
}

func TestInjectionReplacesOnlyTheCredentialHeader(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1") // must be ignored by the gatekeeper
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretA {
			t.Error("real key was not injected")
		}
		for _, key := range []string{"Proxy-Authorization", "X-Forwarded-For", "Forwarded", "Cookie"} {
			if r.Header.Get(key) != "" {
				t.Errorf("%s was forwarded", key)
			}
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"input":"`+placeholderA+`"}` {
			t.Error("request body must not be rewritten")
		}
		io.WriteString(w, `{"usage":{"input_tokens":12,"output_tokens":30}}`)
	})
	start := f.gate.events.last()
	status, body, _ := f.do(t, "POST", "https://model-a.example/v1/responses?beta=true", map[string][]string{
		"Authorization":       {"Bearer " + placeholderA},
		"Proxy-Authorization": {"Basic abc"}, "X-Forwarded-For": {"1.2.3.4"}, "Forwarded": {"for=x"}, "Cookie": {"a=b"},
	}, `{"input":"`+placeholderA+`"}`)
	if status != 200 || !strings.Contains(body, "input_tokens") {
		t.Fatalf("got %d %s", status, body)
	}
	events := f.eventsAfter(t, start)
	last := events[len(events)-1]
	if last.Decision != audit.DecisionInject || last.SecretRef != "provider-a" || last.Status != 200 ||
		last.InputTokens != 12 || last.OutputTokens != 30 || last.UsageKind != "reported" || last.Path != "/v1/responses" {
		t.Fatalf("unexpected event %+v", last)
	}
	if last.SessionID != "sbx-test" || last.BytesIn == 0 || last.BytesOut == 0 {
		t.Fatalf("missing metadata %+v", last)
	}
}

func TestAnthropicStyleHeaderInjection(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != secretB || r.Header.Get("Authorization") != "" {
			t.Error("x-api-key was not injected correctly")
		}
		io.WriteString(w, "ok")
	})
	status, _, _ := f.do(t, "POST", "https://model-b.example/v1/messages", map[string][]string{"X-Api-Key": {placeholderB}}, "{}")
	if status != 200 {
		t.Fatalf("got %d", status)
	}
}

func TestDenyMatrixNeverReachesUpstream(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	cases := []struct {
		name    string
		method  string
		target  string
		headers map[string][]string
		status  int
		reason  string
	}{
		{"wrong method", "GET", "https://model-a.example/v1/responses", bearer(placeholderA), 403, "default_deny"},
		{"unknown path", "POST", "https://model-a.example/v1/other", bearer(placeholderA), 403, "default_deny"},
		{"deny wins", "POST", "https://model-a.example/v1/files/x", bearer(placeholderA), 403, "rule_denied"},
		{"missing credential", "POST", "https://model-a.example/v1/responses", nil, 403, "credential_missing"},
		{"foreign key", "POST", "https://model-a.example/v1/responses", bearer("attacker-own-key"), 403, "credential_foreign"},
		{"duplicate auth", "POST", "https://model-a.example/v1/responses",
			map[string][]string{"Authorization": {"Bearer " + placeholderA, "Bearer " + placeholderA}}, 403, "credential_mismatch"},
		{"extra x-api-key", "POST", "https://model-a.example/v1/responses",
			map[string][]string{"Authorization": {"Bearer " + placeholderA}, "X-Api-Key": {"attacker"}}, 403, "credential_foreign"},
		{"other provider placeholder", "POST", "https://model-a.example/v1/responses", bearer(placeholderB), 403, "credential_foreign"},
		{"foreign key on public path", "GET", "https://model-a.example/v1/public", bearer("attacker-own-key"), 403, "credential_foreign"},
		{"nominated header", "POST", "https://model-a.example/v1/responses",
			map[string][]string{"Authorization": {"Bearer " + placeholderA}, "Connection": {"Authorization"}}, 403, "connection_header_denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, header := f.do(t, c.method, c.target, c.headers, "{}")
			if status != c.status || header.Get("X-Aiofcage-Reason") != c.reason {
				t.Fatalf("got %d %q (%s)", status, header.Get("X-Aiofcage-Reason"), body)
			}
			if strings.Contains(body, secretA) || strings.Contains(body, placeholderA) {
				t.Fatal("response leaked a credential")
			}
		})
	}
	if f.calls.Load() != 0 {
		t.Fatalf("denied requests reached upstream %d times", f.calls.Load())
	}
}

func TestPublicPathOnSecretHostStripsPlaceholder(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("placeholder must not be forwarded on a public path")
		}
		io.WriteString(w, "public")
	})
	if status, body, _ := f.do(t, "GET", "https://model-a.example/v1/public", bearer(placeholderA), ""); status != 200 || body != "public" {
		t.Fatalf("got %d %s", status, body)
	}
}

func TestConnectDeniedForUnknownHostAndIPLiteral(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	for _, target := range []string{"evil.example:443", "169.254.169.254:443", "127.0.0.1:8899", "model-a.example:8443"} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(f.proxyURL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if !strings.Contains(line, "403") {
			t.Fatalf("CONNECT %s: got %q", target, line)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("denied tunnel reached upstream")
	}
}

func TestPlainHTTPAndUnconfiguredAreRejected(t *testing.T) {
	f := newFixture(t, true)
	proxy, _ := url.Parse(f.proxyURL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}, Timeout: 3 * time.Second}
	resp, err := client.Get("http://model-a.example/v1/public")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured plain HTTP: %d", resp.StatusCode)
	}
	f.configure(t, testConfig(1))
	resp, err = client.Get("http://model-a.example/v1/public")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Aiofcage-Reason") != "plain_http_denied" {
		t.Fatalf("plain HTTP: %d %s", resp.StatusCode, resp.Header.Get("X-Aiofcage-Reason"))
	}
}

func TestUntrustedUpstreamCertificateFailsClosed(t *testing.T) {
	f := newFixture(t, false)
	f.configure(t, testConfig(1))
	status, body, header := f.do(t, "POST", "https://model-a.example/v1/responses", bearer(placeholderA), "{}")
	if status != http.StatusBadGateway || header.Get("X-Aiofcage-Reason") != "upstream_failed" || strings.Contains(body, secretA) {
		t.Fatalf("got %d %s", status, body)
	}
	if f.calls.Load() != 0 {
		t.Fatal("request reached an untrusted upstream")
	}
}

func TestUpstreamErrorBodyIsRedacted(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"invalid key %s"}`, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	})
	status, body, _ := f.do(t, "POST", "https://model-a.example/v1/responses", bearer(placeholderA), "{}")
	if status != 401 || strings.Contains(body, secretA) || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("got %d %s", status, body)
	}
}

func TestCompressedUpstreamErrorBodyIsDecodedAndRedacted(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	sum := sha256.Sum256([]byte(secretA))
	hash, tail := hex.EncodeToString(sum[:]), secretA[len(secretA)-4:]
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusUnauthorized)
		zw := gzip.NewWriter(w)
		fmt.Fprintf(zw, `{"error":"invalid key %s, received sk-...%s, key hash %s"}`,
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), tail, hash)
		zw.Close()
	})
	// Agents send Accept-Encoding themselves, so the gatekeeper's transport does not decode for us.
	headers := bearer(placeholderA)
	headers["Accept-Encoding"] = []string{"gzip"}
	status, body, header := f.do(t, "POST", "https://model-a.example/v1/responses", headers, "{}")
	if status != 401 || header.Get("Content-Encoding") != "" || strings.Contains(body, secretA) ||
		strings.Contains(body, "..."+tail) || strings.Contains(body, hash) || strings.Count(body, "[REDACTED]") != 3 {
		t.Fatalf("got %d %q %s", status, header.Get("Content-Encoding"), body)
	}
}

func TestUndecodableUpstreamErrorBodyIsWithheld(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("opaque " + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	})
	headers := bearer(placeholderA)
	headers["Accept-Encoding"] = []string{"br"}
	status, body, header := f.do(t, "POST", "https://model-a.example/v1/responses", headers, "{}")
	if status != 400 || header.Get("Content-Encoding") != "" || strings.Contains(body, secretA) || !strings.Contains(body, "aiofcage_withheld") {
		t.Fatalf("got %d %q %s", status, header.Get("Content-Encoding"), body)
	}
}

func TestRedirectIsNotFollowedOrRevealed(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		// The upstream holds the key and could spell it into the target's host name.
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		http.Redirect(w, &http.Request{}, "https://"+key+".evil.example/collect", http.StatusFound)
	})
	status, body, header := f.do(t, "POST", "https://model-a.example/v1/responses", bearer(placeholderA), "{}")
	if status != 403 || header.Get("Location") != "" || strings.Contains(body, "evil.example") {
		t.Fatalf("got %d %s location=%q", status, body, header.Get("Location"))
	}
	refused := 0
	for _, event := range f.eventsAfter(t, 0) {
		if event.ReasonCode == "redirect_denied" {
			refused++
			if event.RedirectHost != "" {
				t.Fatalf("the log must not name the target of a redirect that may carry the key: %q", event.RedirectHost)
			}
		}
	}
	if refused != 1 {
		t.Fatalf("the refused redirect is logged once: %d", refused)
	}
}

// Only the gatekeeper marks answers as its own. An upstream sending its headers is still checked,
// and an agent's X-Aiofcage-* headers never reach the upstream, which might echo them.
func TestUpstreamCannotPassForTheGatekeeper(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	var forwarded []string
	answer := "redirect"
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-aiofcage-") {
				forwarded = append(forwarded, name)
			}
		}
		w.Header().Set("X-Aiofcage-Generated", "upstream_failed")
		w.Header().Set("X-Aiofcage-Reason", "forged")
		if answer == "redirect" {
			http.Redirect(w, r, "https://evil.example/collect", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "invalid key %s", strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	})
	headers := bearer(placeholderA)
	headers["X-Aiofcage-Generated"] = []string{"upstream_failed"}
	status, body, header := f.do(t, "POST", "https://model-a.example/v1/responses", headers, "{}")
	if status != 403 || header.Get("Location") != "" || header.Get("X-Aiofcage-Reason") != "redirect_denied" {
		t.Fatalf("a redirect stays refused whatever the upstream claims: %d %v %s", status, header, body)
	}
	answer = "error"
	status, body, header = f.do(t, "POST", "https://model-a.example/v1/responses", headers, "{}")
	if status != 401 || strings.Contains(body, secretA) || header.Get("X-Aiofcage-Reason") != "" || header.Get("X-Aiofcage-Generated") != "" {
		t.Fatalf("an error body stays redacted and the upstream's own headers are dropped: %d %v %s", status, header, body)
	}
	if len(forwarded) != 0 {
		t.Fatalf("the agent's X-Aiofcage-* headers reached the upstream: %v", forwarded)
	}
	for _, event := range f.eventsAfter(t, 0) {
		if event.ReasonCode == "upstream_failed" || event.ReasonCode == "forged" {
			t.Fatalf("the upstream chose the logged reason: %+v", event)
		}
	}
}

// Response trailers arrive after the body, past every check, so none reach the agent: not on a
// successful answer and not on a redacted error.
func TestResponseTrailersAreDropped(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	status := http.StatusOK
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Aiofcage-Reason, X-Upstream")
		w.WriteHeader(status)
		io.WriteString(w, "body")
		w.Header().Set("X-Aiofcage-Reason", "credential_foreign")
		w.Header().Set("X-Upstream", "after the body")
	})
	for _, code := range []int{http.StatusOK, http.StatusUnauthorized} {
		status = code
		req, err := http.NewRequest("POST", "https://model-a.example/v1/responses", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+placeholderA)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != code || len(resp.Trailer) != 0 {
			t.Fatalf("%d: trailers reached the agent: %v", code, resp.Trailer)
		}
	}
}

// Trailers arrive after the body and Go forwards them upstream, past the credential checks.
func TestRequestTrailersAreRefused(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	req, err := http.NewRequest("POST", "https://model-a.example/v1/responses", io.NopCloser(strings.NewReader("{}")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+placeholderA)
	req.ContentLength = -1
	req.Trailer = http.Header{"Authorization": {"Bearer synthetic-foreign-token-0123456789"}}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get("X-Aiofcage-Reason") != "trailer_denied" || f.calls.Load() != 0 {
		t.Fatalf("got %d %q, upstream calls %d", resp.StatusCode, resp.Header.Get("X-Aiofcage-Reason"), f.calls.Load())
	}
}

func TestStreamingUsageAndFirstChunkLatency(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	release := make(chan struct{})
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":7}}}\n\n")
	})
	req, _ := http.NewRequest("POST", "https://model-a.example/v1/responses", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+placeholderA)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, "response.created") {
			t.Fatalf("unexpected first line %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE chunk was buffered by the gatekeeper")
	}
	start := f.gate.events.last()
	close(release)
	io.ReadAll(reader)
	events := f.eventsAfter(t, start)
	last := events[len(events)-1]
	if last.InputTokens != 100 || last.OutputTokens != 7 || last.UsageKind != "reported" {
		t.Fatalf("stream usage not recorded: %+v", last)
	}
}

func TestQuotaBlocksAndWarns(t *testing.T) {
	f := newFixture(t, true)
	config := testConfig(1)
	config.Limits = Limits{MaxRequests: 5, BaselineRequests: 3}
	f.configure(t, config)
	start := f.gate.events.last()
	results := []int{}
	for i := 0; i < 3; i++ {
		status, _, _ := f.do(t, "POST", "https://model-a.example/v1/responses", bearer(placeholderA), "{}")
		results = append(results, status)
	}
	if fmt.Sprint(results) != "[200 200 429]" {
		t.Fatalf("quota results %v", results)
	}
	if f.calls.Load() != 2 {
		t.Fatalf("blocked request reached upstream: %d calls", f.calls.Load())
	}
	var warned, blocked bool
	for _, event := range f.eventsAfter(t, start) {
		warned = warned || (event.Action == audit.ActionQuotaWarning && event.ReasonCode == "request_quota_80")
		blocked = blocked || (event.Action == audit.ActionQuotaBlocked && event.Status == 429)
	}
	if !warned || !blocked {
		t.Fatalf("warning=%v blocked=%v", warned, blocked)
	}
	status, body := f.manage(t, "GET", "/usage", nil)
	var usage usageResponse
	if status != 200 || json.Unmarshal(body, &usage) != nil || usage.QuotaRequests != 5 || usage.Secrets["provider-a"].Requests != 2 {
		t.Fatalf("usage %d %s", status, body)
	}
	if bytes.Contains(body, []byte(secretA)) || bytes.Contains(body, []byte(placeholderA)) {
		t.Fatal("usage leaked a credential")
	}
}

func TestTokenQuota(t *testing.T) {
	f := newFixture(t, true)
	config := testConfig(1)
	config.Limits = Limits{MaxTokens: 50}
	f.configure(t, config)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"usage":{"prompt_tokens":40,"completion_tokens":20}}`)
	})
	if status, _, _ := f.do(t, "POST", "https://model-a.example/v1/responses", bearer(placeholderA), "{}"); status != 200 {
		t.Fatalf("first request %d", status)
	}
	quotaTokens := func() uint64 {
		f.gate.mu.RLock()
		defer f.gate.mu.RUnlock()
		return f.gate.stats.QuotaTokens
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for quotaTokens() == 0 && ctx.Err() == nil {
		f.gate.events.waitFor(ctx, f.gate.events.last()+1)
	}
	if quotaTokens() != 60 {
		t.Fatalf("token usage not counted: %d", quotaTokens())
	}
	if status, _, header := f.do(t, "POST", "https://model-a.example/v1/responses", bearer(placeholderA), "{}"); status != 429 ||
		header.Get("X-Aiofcage-Reason") != "token_quota_exceeded" {
		t.Fatalf("token quota not enforced: %d", status)
	}
}

func TestConfigValidationAndVersioning(t *testing.T) {
	f := newFixture(t, true)
	bad := testConfig(1)
	bad.Secrets[0].Placeholder = "not-a-placeholder"
	status, body := f.manage(t, "PUT", "/config", bad)
	if status != 400 || bytes.Contains(body, []byte(secretA)) || bytes.Contains(body, []byte("not-a-placeholder")) {
		t.Fatalf("bad placeholder: %d %s", status, body)
	}
	missing := testConfig(1)
	missing.Secrets = missing.Secrets[:1]
	if status, body = f.manage(t, "PUT", "/config", missing); status != 400 || !bytes.Contains(body, []byte("provider-b")) {
		t.Fatalf("missing secret ref: %d %s", status, body)
	}
	f.configure(t, testConfig(2))
	if status, _ = f.manage(t, "PUT", "/config", testConfig(2)); status != 409 {
		t.Fatalf("stale version accepted: %d", status)
	}
	status, body = f.manage(t, "GET", "/health", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"ready":true`)) || bytes.Contains(body, []byte(secretA)) {
		t.Fatalf("health %d %s", status, body)
	}
	unknown := []byte(`{"policy":{"version":3,"rules":[]},"secrets":[],"limits":{},"extra":1}`)
	r := httptest.NewRequest("PUT", "/config", bytes.NewReader(unknown))
	r.Header.Set("Authorization", "Bearer "+testToken())
	w := httptest.NewRecorder()
	f.handlers.Management.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("unknown field accepted: %d", w.Code)
	}
}

func TestEventStreamResumeAndGap(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, testConfig(1))
	f.do(t, "POST", "https://model-a.example/v1/other", bearer(placeholderA), "{}")
	server := httptest.NewServer(f.handlers.Management)
	t.Cleanup(server.Close)
	read := func(lastEventID string, want int) []string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/events", nil)
		req.Header.Set("Authorization", "Bearer "+testToken())
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
			t.Fatalf("content type %s", resp.Header.Get("Content-Type"))
		}
		var ids []string
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() && len(ids) < want {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var event audit.Event
				if err := json.Unmarshal([]byte(line[6:]), &event); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(line, secretA) || strings.Contains(line, placeholderA) {
					t.Fatal("event leaked a credential")
				}
				ids = append(ids, event.Action+"@"+event.ID)
			}
		}
		return ids
	}
	all := read("", 2)
	if len(all) != 2 || !strings.HasPrefix(all[0], audit.ActionPolicyChange) || !strings.HasPrefix(all[1], audit.ActionRequest) {
		t.Fatalf("initial stream %v", all)
	}
	resumed := read(f.gate.runID+":1", 1)
	if len(resumed) != 1 || resumed[0] != all[1] {
		t.Fatalf("resume %v", resumed)
	}
	other := read("run-other:5", 1)
	if len(other) != 1 || !strings.HasPrefix(other[0], audit.ActionGap) {
		t.Fatalf("cursor from another run must produce a gap first: %v", other)
	}
}

func TestEventBufferOverflowReportsGap(t *testing.T) {
	log := newEventLog("run-x", time.Now)
	for i := 0; i < eventCapacity+10; i++ {
		log.append(audit.Event{Action: audit.ActionRequest})
	}
	events, gap, _ := log.since(3)
	if !gap || len(events) != eventCapacity || events[0].Sequence != 11 {
		t.Fatalf("gap=%v len=%d first=%d", gap, len(events), events[0].Sequence)
	}
	if _, gap, _ := log.since(10); gap {
		t.Fatal("cursor right before the buffer must not be a gap")
	}
}

func TestPublicIP(t *testing.T) {
	for value, want := range map[string]bool{
		"8.8.8.8": true, "223.5.5.5": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.17.0.1": false, "192.168.5.2": false, "169.254.169.254": false,
		"100.64.0.1": false, "::1": false, "fe80::1": false, "fd42::1": false, "::ffff:127.0.0.1": false, "0.0.0.0": false,
	} {
		if got := publicIP(netip.MustParseAddr(value)); got != want {
			t.Errorf("%s: got %v want %v", value, got, want)
		}
	}
	if _, err := publicDial(context.Background(), "tcp", "127.0.0.1:443"); err == nil {
		t.Error("IP literal destination must be refused")
	}
	if _, err := publicDial(context.Background(), "tcp", "example.com:80"); err == nil {
		t.Error("non-443 destination must be refused")
	}
}

func TestUsageSniffer(t *testing.T) {
	anthropic := newUsageSniffer("text/event-stream")
	for _, chunk := range []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"cache_read_input_tokens\":5,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tok", "ens\":15}}\n\n",
	} {
		anthropic.Write([]byte(chunk))
	}
	if input, output, found := anthropic.Result(); !found || input != 30 || output != 15 {
		t.Fatalf("anthropic stream: %d %d %v", input, output, found)
	}
	chat := newUsageSniffer("application/json")
	chat.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}`))
	if input, output, found := chat.Result(); !found || input != 9 || output != 3 {
		t.Fatalf("chat json: %d %d %v", input, output, found)
	}
	none := newUsageSniffer("text/event-stream")
	none.Write([]byte("data: {\"type\":\"x\"}\n\n"))
	if _, _, found := none.Result(); found {
		t.Fatal("no usage expected")
	}
}

func TestSharedEndpointPicksSecretByPlaceholder(t *testing.T) {
	f := newFixture(t, true)
	config := testConfig(1)
	// A second provider on the same host and path, sending its key in another header.
	config.Policy.Rules = append(config.Policy.Rules, policy.Rule{ID: "model-a-second", Effect: policy.Allow,
		Host: "model-a.example", Methods: []string{"POST"}, Path: "/v1/responses", SecretRef: "provider-c"})
	config.Secrets = append(config.Secrets, Secret{ID: "provider-c", Placeholder: "cage-placeholder-provider-c-xyz",
		Header: "X-Api-Key", Value: "synthetic-secret-provider-c-555"})
	f.configure(t, config)
	var seen []string
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Api-Key"))
		io.WriteString(w, "ok")
	})
	if status, _, _ := f.do(t, "POST", "https://model-a.example/v1/responses", bearer(placeholderA), "{}"); status != 200 {
		t.Fatalf("first provider %d", status)
	}
	if status, _, _ := f.do(t, "POST", "https://model-a.example/v1/responses",
		map[string][]string{"X-Api-Key": {"cage-placeholder-provider-c-xyz"}}, "{}"); status != 200 {
		t.Fatalf("second provider %d", status)
	}
	if len(seen) != 2 || seen[0] != "Bearer "+secretA+"|" || seen[1] != "|synthetic-secret-provider-c-555" {
		t.Fatalf("wrong injection %q", seen)
	}
	if status, _, header := f.do(t, "POST", "https://model-a.example/v1/responses", bearer("attacker-own-key"), "{}"); status != 403 ||
		header.Get("X-Aiofcage-Reason") != "credential_foreign" {
		t.Fatalf("foreign key on shared endpoint: %d %s", status, header.Get("X-Aiofcage-Reason"))
	}
}
