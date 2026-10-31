package gatekeeper

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"

	"github.com/shaoxier/AIofCage/internal/audit"
	"github.com/shaoxier/AIofCage/internal/policy"
	"github.com/shaoxier/AIofCage/internal/provider"
)

const (
	maxRequestBody     = 32 << 20
	maxErrorBody       = 64 << 10
	warnRatioNumerator = 4 // warn at 4/5 = 80% of a limit
	warnRatioDivisor   = 5
)

// Gate is the gatekeeper: an HTTPS MITM proxy that authorizes every request, replaces sandbox
// placeholders with real credentials, enforces per-sandbox quota and records redacted events.
type Gate struct {
	runID     string
	ca        tls.Certificate
	caPEM     []byte
	proxy     *goproxy.ProxyHttpServer
	transport *http.Transport
	mcpClient *http.Client
	// remoteMCP reaches remote MCP services with the proxy's transport: public addresses only,
	// verified TLS, redirects not followed.
	remoteMCP *http.Client
	events    *eventLog
	now       func() time.Time

	mu      sync.RWMutex
	active  *compiled
	stats   stats
	warned  map[string]bool
	version uint64
}

type stats struct {
	Requests      uint64                 `json:"requests"`
	Denied        uint64                 `json:"denied"`
	QuotaRequests uint64                 `json:"quota_requests"`
	QuotaTokens   uint64                 `json:"quota_tokens"`
	Secrets       map[string]*secretStat `json:"secrets"`
}

type secretStat struct {
	Requests     uint64 `json:"requests"`
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

// connectTarget is stored in the CONNECT context and copied by goproxy into every request
// context of the tunnel.
type connectTarget struct{ host string }

// requestState replaces connectTarget for each request inside the tunnel.
type requestState struct {
	pricing                    *provider.Pricing
	model, agent, providerName string
	modelRequest               bool
	target                     connectTarget
	started                    time.Time
	method                     string
	path                       string
	ruleID                     string
	secretID                   string
	secret                     string
	version                    uint64
	generated                  bool
	// transportFailure is the reason when the gatekeeper answered for an unreachable upstream.
	transportFailure string
	bodyIn           *countingReader
}

func NewGate() (*Gate, error) {
	ca, caPEM, err := newCA()
	if err != nil {
		return nil, err
	}
	runID, err := randomID()
	if err != nil {
		return nil, err
	}
	g := &Gate{runID: runID, ca: ca, caPEM: caPEM, now: time.Now, warned: map[string]bool{},
		stats: stats{Secrets: map[string]*secretStat{}}}
	g.events = newEventLog(runID, func() time.Time { return g.now() })
	g.mcpClient = newMCPClient()
	g.transport = &http.Transport{
		Proxy: nil, DialContext: publicDial, ForceAttemptHTTP2: false,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 4,
	}
	g.remoteMCP = &http.Client{Transport: g.transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p := goproxy.NewProxyHttpServer()
	p.Logger = log.New(io.Discard, "", 0) // goproxy errors may contain URLs with query strings.
	p.Tr = g.transport
	p.ConnectDial = nil
	p.ConnectDialWithReq = nil
	p.NonproxyHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusForbidden, "not_a_proxy_request")
	})
	mitm := &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: goproxy.TLSConfigFromCA(&g.ca)}
	roundTrip := goproxy.RoundTripperFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Response, error) {
		resp, err := g.transport.RoundTrip(req)
		if err != nil {
			// goproxy drops the client connection on transport errors; answer with a redacted 502.
			reason := "upstream_failed"
			if errors.Is(err, errDestinationDenied) {
				reason = "destination_denied"
			}
			if state, ok := ctx.UserData.(*requestState); ok {
				state.transportFailure = reason
			}
			resp = textResponse(req, http.StatusBadGateway, reason)
		}
		return resp, nil
	})
	p.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		return g.connect(host, ctx, mitm, roundTrip)
	})
	p.OnRequest().DoFunc(g.request)
	p.OnResponse().DoFunc(g.response)
	g.proxy = p
	return g, nil
}

func randomID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run-" + strconv.FormatInt(time.Now().Unix(), 36) + "-" + encodeBase32(b[:]), nil
}

func encodeBase32(b []byte) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var out strings.Builder
	var buffer, bits uint
	for _, value := range b {
		buffer = buffer<<8 | uint(value)
		bits += 8
		for bits >= 5 {
			out.WriteByte(alphabet[(buffer>>(bits-5))&31])
			bits -= 5
		}
	}
	if bits > 0 {
		out.WriteByte(alphabet[(buffer<<(5-bits))&31])
	}
	return out.String()
}

func (g *Gate) current() *compiled {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.active
}

func (g *Gate) sessionID() string {
	if active := g.current(); active != nil {
		return active.config.Policy.SessionID
	}
	return ""
}

func (g *Gate) connect(host string, ctx *goproxy.ProxyCtx, mitm *goproxy.ConnectAction,
	roundTrip goproxy.RoundTripper) (*goproxy.ConnectAction, string) {
	active := g.current()
	name, port, err := net.SplitHostPort(host)
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if active == nil {
		ctx.Resp = textResponse(ctx.Req, http.StatusServiceUnavailable, "not_configured")
		return goproxy.RejectConnect, host
	}
	event := audit.Event{Action: audit.ActionConnect, Decision: audit.DecisionDeny, Host: name, Method: http.MethodConnect,
		SessionID: active.config.Policy.SessionID, PolicyVersion: active.config.Policy.Version}
	reason := ""
	if err != nil || port != "443" {
		reason = "port_denied"
	} else if _, ipErr := netip.ParseAddr(name); ipErr == nil {
		reason = "ip_literal_denied"
	} else if decision := active.config.Policy.AllowsConnect(name, policy.DefaultPort); !decision.Allowed {
		reason, event.RuleID = decision.ReasonCode, decision.RuleID
	}
	if reason != "" {
		if _, ipErr := netip.ParseAddr(name); ipErr != nil && !policy.ValidHost(name, false) {
			event.Host = "" // never echo arbitrary attacker-chosen strings into the event log
		}
		event.ReasonCode, event.Status = reason, http.StatusForbidden
		recorded := g.deny(event)
		ctx.Resp = blocked(ctx.Req, http.StatusForbidden, reason, event.Host, recorded.Sequence)
		return goproxy.RejectConnect, host
	}
	ctx.UserData = connectTarget{host: name}
	ctx.RoundTripper = roundTrip
	return mitm, host
}

func (g *Gate) request(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	active := g.current()
	state := &requestState{started: g.now(), method: req.Method}
	if target, ok := ctx.UserData.(connectTarget); ok {
		state.target = target
	}
	ctx.UserData = state
	if active == nil {
		state.generated = true
		return req, textResponse(req, http.StatusServiceUnavailable, "not_configured")
	}
	state.version = active.config.Policy.Version
	event := audit.Event{Action: audit.ActionRequest, Decision: audit.DecisionDeny, Method: req.Method,
		Host: state.target.host, SessionID: active.config.Policy.SessionID, PolicyVersion: state.version}
	deny := func(status int, reason string) (*http.Request, *http.Response) {
		state.generated = true
		event.ReasonCode, event.Status = reason, status
		recorded := g.deny(event)
		return req, blocked(req, status, reason, state.target.host, recorded.Sequence)
	}
	if state.target.host == "" {
		event.Host = ""
		return deny(http.StatusForbidden, "plain_http_denied")
	}
	u := req.URL
	if u == nil || u.Scheme != "https" || strings.ToLower(u.Hostname()) != state.target.host ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil || u.Fragment != "" || !sameHost(req.Host, state.target.host) {
		return deny(http.StatusForbidden, "target_mismatch")
	}
	requestPath, ok := policyPath(u)
	if !ok {
		return deny(http.StatusForbidden, "target_mismatch")
	}
	state.path = requestPath
	event.Path = truncate(requestPath, 256)
	for _, value := range req.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" && !strings.EqualFold(token, "close") && !strings.EqualFold(token, "keep-alive") {
				// Connection could nominate credential headers for removal and confuse our checks.
				return deny(http.StatusForbidden, "connection_header_denied")
			}
		}
	}
	if len(req.Trailer) > 0 || len(req.Header.Values("Trailer")) > 0 {
		// Trailers arrive after the body, past the credential checks, and are forwarded upstream.
		return deny(http.StatusForbidden, "trailer_denied")
	}
	dropOwnHeaders(req.Header)
	decision := active.config.Policy.Evaluate(policy.Request{Host: state.target.host, Port: policy.DefaultPort,
		Method: req.Method, Path: requestPath})
	event.RuleID = decision.RuleID
	if !decision.Allowed {
		return deny(http.StatusForbidden, decision.ReasonCode)
	}
	decision = chooseMatch(active, decision, req.Header)
	event.RuleID, state.ruleID = decision.RuleID, decision.RuleID
	if reason := checkCredentials(active, state.target.host, decision.SecretRef, req.Header); reason != "" {
		return deny(http.StatusForbidden, reason)
	}
	if decision.Anonymous && carriesCredential(req.Header) {
		// An open download source must not become a way to use some account inside the sandbox.
		return deny(http.StatusForbidden, "credential_not_allowed")
	}
	if req.ContentLength > maxRequestBody {
		return deny(http.StatusRequestEntityTooLarge, "body_too_large")
	}
	for key := range req.Header {
		lower := strings.ToLower(key)
		if lower == "proxy-authorization" || lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") {
			req.Header.Del(key)
		}
	}
	if decision.SecretRef != "" {
		secret := active.secrets[decision.SecretRef]
		event.SecretRef = secret.ID
		state.model, state.agent, state.providerName = secret.Model, secret.Agent, secret.Name
		state.pricing = secret.Pricing
		state.modelRequest = secret.Agent != "" && req.Method == http.MethodPost && !strings.HasSuffix(requestPath, "/count_tokens")
		if status, reason := g.reserve(active, secret.ID, event); reason != "" {
			state.generated = true
			return req, blocked(req, status, reason, "", 0)
		}
		req.Header.Del("Cookie")
		for _, header := range credentialHeaders {
			req.Header.Del(header)
		}
		req.Header.Set(secret.Header, secret.Prefix+secret.Value)
		state.secretID, state.secret = secret.ID, secret.Value
	} else if _, bound := active.secretHosts[state.target.host]; bound {
		// A public path on a host that also has injected credentials: never forward placeholders.
		for _, header := range credentialHeaders {
			req.Header.Del(header)
		}
	}
	if req.Body != nil && req.Body != http.NoBody {
		state.bodyIn = &countingReader{ReadCloser: req.Body, limit: maxRequestBody}
		req.Body = state.bodyIn
	}
	return req, nil
}

// chooseMatch picks, among allow rules matching the same request, the one whose secret's
// placeholder the request carries. Without such a credential a public (secret-less) rule wins;
// otherwise the first rule is kept so the credential check reports the problem.
func chooseMatch(active *compiled, decision policy.Decision, header http.Header) policy.Decision {
	if len(decision.Matches) < 2 {
		return decision
	}
	for _, match := range decision.Matches {
		if match.SecretRef == "" {
			continue
		}
		secret := active.secrets[match.SecretRef]
		if values := header.Values(secret.Header); len(values) == 1 && values[0] == secret.Prefix+secret.Placeholder {
			decision.RuleID, decision.SecretRef, decision.Anonymous = match.RuleID, match.SecretRef, match.Anonymous
			return decision
		}
	}
	for _, match := range decision.Matches {
		if match.SecretRef == "" {
			decision.RuleID, decision.SecretRef, decision.Anonymous = match.RuleID, "", match.Anonymous
			return decision
		}
	}
	return decision
}

func sameHost(hostHeader, target string) bool {
	hostHeader = strings.ToLower(hostHeader)
	return hostHeader == target || hostHeader == target+":443"
}

// checkCredentials enforces that a host with injected secrets only ever receives our placeholders.
// This blocks exfiltration with an attacker's own key through an allowed model domain.
func checkCredentials(active *compiled, host, secretRef string, header http.Header) string {
	bound := active.secretHosts[host]
	present := 0
	for _, name := range credentialHeaders {
		values := header.Values(name)
		present += len(values)
		for _, value := range values {
			if len(bound) > 0 && !isPlaceholderFor(bound, name, value) {
				return "credential_foreign"
			}
		}
	}
	if secretRef == "" {
		return ""
	}
	secret := active.secrets[secretRef]
	values := header.Values(secret.Header)
	if len(values) != 1 || values[0] != secret.Prefix+secret.Placeholder || present != 1 {
		if present == 0 {
			return "credential_missing"
		}
		return "credential_mismatch"
	}
	return ""
}

func isPlaceholderFor(secrets []Secret, header, value string) bool {
	for _, secret := range secrets {
		if secret.Header == header && value == secret.Prefix+secret.Placeholder {
			return true
		}
	}
	return false
}

// reserve counts a request against the sandbox quota before it is forwarded.
func (g *Gate) reserve(active *compiled, secretID string, event audit.Event) (int, string) {
	g.mu.Lock()
	limits := active.config.Limits
	blocked := ""
	if limits.MaxRequests > 0 && g.stats.QuotaRequests >= limits.MaxRequests {
		blocked = "request_quota_exceeded"
	} else if limits.MaxTokens > 0 && g.stats.QuotaTokens >= limits.MaxTokens {
		blocked = "token_quota_exceeded"
	}
	if blocked != "" {
		g.stats.Requests++
		g.stats.Denied++
		g.mu.Unlock()
		event.Action, event.ReasonCode, event.Status = audit.ActionQuotaBlocked, blocked, http.StatusTooManyRequests
		g.events.append(event)
		return http.StatusTooManyRequests, blocked
	}
	g.stats.QuotaRequests++
	stat := g.secretStat(secretID)
	stat.Requests++
	warnings := g.quotaWarningsLocked(limits)
	g.mu.Unlock()
	for _, reason := range warnings {
		g.events.append(audit.Event{Action: audit.ActionQuotaWarning, Decision: audit.DecisionNotApplicable,
			ReasonCode: reason, SessionID: event.SessionID, PolicyVersion: event.PolicyVersion})
	}
	return 0, ""
}

func (g *Gate) secretStat(id string) *secretStat {
	stat := g.stats.Secrets[id]
	if stat == nil {
		stat = &secretStat{}
		g.stats.Secrets[id] = stat
	}
	return stat
}

// quotaWarningsLocked returns warnings that crossed 80% for the first time. Caller holds g.mu.
func (g *Gate) quotaWarningsLocked(limits Limits) []string {
	var result []string
	check := func(key string, used, limit uint64) {
		if limit > 0 && used*warnRatioDivisor >= limit*warnRatioNumerator && !g.warned[key] {
			g.warned[key] = true
			result = append(result, key)
		}
	}
	check("request_quota_80", g.stats.QuotaRequests, limits.MaxRequests)
	check("token_quota_80", g.stats.QuotaTokens, limits.MaxTokens)
	return result
}

func (g *Gate) response(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
	state, _ := ctx.UserData.(*requestState)
	if state == nil || state.generated {
		return resp
	}
	active := g.current()
	event := audit.Event{Action: audit.ActionRequest, Host: state.target.host, Method: state.method,
		Path: truncate(state.path, 256), RuleID: state.ruleID, SecretRef: state.secretID, PolicyVersion: state.version}
	if active != nil {
		event.SessionID = active.config.Policy.SessionID
	}
	if resp == nil {
		event.Decision, event.ReasonCode, event.Status = audit.DecisionError, "upstream_failed", http.StatusBadGateway
		g.finish(state, event)
		return textResponse(ctx.Req, http.StatusBadGateway, "upstream_failed")
	}
	if state.transportFailure != "" {
		event.Decision, event.ReasonCode, event.Status = audit.DecisionError, state.transportFailure, resp.StatusCode
		g.finish(state, event)
		return resp
	}
	// X-Aiofcage-* headers reaching the agent always come from the gatekeeper itself. Trailers are
	// not passed on at all: they arrive after the body, past every check here. goproxy writes the
	// trailers it finds when it starts writing, so a nil map stays nil.
	dropOwnHeaders(resp.Header)
	resp.Trailer = nil
	event.Status = resp.StatusCode
	event.Decision = audit.DecisionAllow
	if state.secretID != "" {
		event.Decision = audit.DecisionInject
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
		// The gatekeeper never follows a redirect. It passes one on only when it cannot carry a
		// credential and leads where the sandbox may go anyway. Otherwise the Location never
		// reaches the agent. The log and the refusal name the target host only for requests without
		// an injected credential, whose redirect cannot carry the credential or a derived token.
		target, ok := redirectTarget(active, state, resp)
		if state.secretID != "" {
			target = "" // the upstream could spell the credential into a host name
		}
		event.RedirectHost = target
		if !ok {
			resp.Body.Close()
			event.Decision, event.ReasonCode, event.Status = audit.DecisionDeny, "redirect_denied", http.StatusForbidden
			recorded := g.finish(state, event)
			return blocked(ctx.Req, http.StatusForbidden, "redirect_denied", target, recorded.Sequence)
		}
	}
	if resp.StatusCode >= 400 {
		// Error bodies are small and may echo the credential; decode and redact before returning them.
		body, withheld := readErrorBody(resp, state.secret)
		resp.Body.Close()
		resp.Trailer = nil // reading the body to its end filled them in again
		if withheld {
			resp.Header.Set("Content-Type", "application/json")
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		resp.Header.Del("Content-Encoding")
		event.ReasonCode = "upstream_status"
		event.BytesOut = int64(len(body))
		g.finish(state, event)
		return resp
	}
	event.ReasonCode = "completed"
	observed := &observedBody{ReadCloser: resp.Body, gate: g, state: state, event: event,
		empty: resp.ContentLength == 0 || state.method == http.MethodHead}
	if state.secretID != "" {
		observed.usage = newUsageSniffer(resp.Header.Get("Content-Type"))
	}
	resp.Body = observed
	return resp
}

// finish records the completed request: usage for quota and the redacted event, which it returns
// as recorded.
func (g *Gate) finish(state *requestState, event audit.Event) audit.Event {
	event.ConfiguredModel, event.Agent, event.ProviderName, event.ModelRequest = state.model, state.agent, state.providerName, state.modelRequest
	if event.Model == "" {
		event.Model = state.model
	}
	if state.modelRequest && event.UsageKind == "" {
		event.UsageKind = "unknown"
	}
	if state.modelRequest {
		event.Pricing = state.pricing
		switch {
		case event.UsageKind != "reported":
			event.CostStatus = "usage-unknown"
		case state.pricing == nil:
			event.CostStatus = "price-missing"
		case event.Model != state.model:
			event.CostStatus = "model-mismatch"
		default:
			if cost, ok := state.pricing.Cost(event.InputTokens, event.OutputTokens, event.CacheReadTokens, event.CacheWriteTokens); ok {
				event.EstimatedCost = &cost
				event.CostStatus = "estimated"
			} else {
				event.CostStatus = "usage-invalid"
			}
		}
	}
	if state.bodyIn != nil {
		event.BytesIn = state.bodyIn.count()
	}
	event.DurationMS = g.now().Sub(state.started).Milliseconds()
	g.mu.Lock()
	g.stats.Requests++
	if event.Decision == audit.DecisionDeny {
		g.stats.Denied++
	}
	var warnings []string
	if state.secretID != "" && (event.InputTokens > 0 || event.OutputTokens > 0) {
		stat := g.secretStat(state.secretID)
		stat.InputTokens += uint64(event.InputTokens)
		stat.OutputTokens += uint64(event.OutputTokens)
		g.stats.QuotaTokens += uint64(event.InputTokens + event.OutputTokens)
		if g.active != nil {
			warnings = g.quotaWarningsLocked(g.active.config.Limits)
		}
	}
	g.mu.Unlock()
	recorded := g.events.append(event)
	for _, reason := range warnings {
		g.events.append(audit.Event{Action: audit.ActionQuotaWarning, Decision: audit.DecisionNotApplicable,
			ReasonCode: reason, SessionID: event.SessionID, PolicyVersion: event.PolicyVersion})
	}
	return recorded
}

// deny records a refusal and returns the event as recorded, with its sequence number.
func (g *Gate) deny(event audit.Event) audit.Event {
	g.mu.Lock()
	g.stats.Requests++
	g.stats.Denied++
	g.mu.Unlock()
	return g.events.append(event)
}

// observedBody forwards the upstream response unchanged while counting bytes and sniffing usage.
type observedBody struct {
	io.ReadCloser
	gate  *Gate
	state *requestState
	event audit.Event
	usage *usageSniffer
	bytes int64
	empty bool
	once  sync.Once
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	if b.usage != nil && n > 0 {
		b.usage.Write(p[:n])
	}
	if err == io.EOF {
		b.complete("completed")
	} else if err != nil {
		b.complete("stream_interrupted")
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	if b.empty {
		b.complete("completed")
	}
	b.complete("closed_early")
	return err
}

func (b *observedBody) complete(reason string) {
	b.once.Do(func() {
		event := b.event
		event.BytesOut = b.bytes
		if reason != "completed" {
			event.ReasonCode = reason
		}
		if b.usage != nil {
			input, output, found := b.usage.Result()
			event.InputTokens, event.OutputTokens = input, output
			event.CacheReadTokens, event.CacheWriteTokens = b.usage.cacheRead, b.usage.cacheWrite
			event.Model = b.usage.model
			event.UsageKind = "unknown"
			if found {
				event.UsageKind = "reported"
			}
		}
		b.gate.finish(b.state, event)
	})
}

type countingReader struct {
	io.ReadCloser
	mu    sync.Mutex
	n     int64
	limit int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	r.n += int64(n)
	over := r.n > r.limit
	r.mu.Unlock()
	if over {
		return n, errors.New("request body too large")
	}
	return n, err
}

func (r *countingReader) count() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func textResponse(req *http.Request, status int, reason string) *http.Response {
	body := "AIofCage gatekeeper: request blocked (" + reason + ")\n"
	if status == http.StatusServiceUnavailable || status == http.StatusBadGateway {
		body = "AIofCage gatekeeper: " + reason + "\n"
	}
	resp := goproxy.NewResponse(req, goproxy.ContentTypeText, status, body)
	resp.Header.Set("X-Aiofcage-Reason", reason)
	resp.Header.Set("Cache-Control", "no-store")
	return resp
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return path.Clean(value[:limit])
}
