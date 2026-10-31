package gatekeeper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/shaoxier/AIofCage/internal/policy"
)

// blockedStatus replaces the reason phrase of refusals, so even clients that print only the
// status line (git, pip, curl after a refused CONNECT) show who refused the request.
const blockedStatus = "403 Blocked by AIofCage sandbox policy"

// blockedText explains a refusal in plain words, in Chinese for the user and English for the
// agent. %s is the host involved, when there is one.
var blockedText = map[string][2]string{
	"default_deny":             {"%s 不在这个沙箱的允许列表里", "%s is not on this sandbox's allow list"},
	"rule_denied":              {"这个请求命中了沙箱的禁止规则（开发环境只放行下载，不放行发布和推送）", "the request matches a deny rule of this sandbox (development sources allow downloads only, never publishing or pushing)"},
	"credential_not_allowed":   {"开发环境只用于匿名下载，请求里带了自己的凭据", "development sources are for anonymous downloads, and the request carried its own credentials"},
	"redirect_denied":          {"上游要求跳转到 %s，这个地址不在允许范围内", "the upstream redirected to %s, which this sandbox may not reach"},
	"plain_http_denied":        {"沙箱只允许 HTTPS", "only HTTPS is allowed"},
	"port_denied":              {"沙箱只允许 443 端口", "only port 443 is allowed"},
	"ip_literal_denied":        {"沙箱不允许直接访问 IP 地址", "IP addresses cannot be reached directly"},
	"target_mismatch":          {"请求的地址或路径写法不被接受", "the request target or path is not accepted"},
	"credential_foreign":       {"请求带了不属于这个沙箱的凭据", "the request carried a credential that does not belong to this sandbox"},
	"credential_missing":       {"请求缺少这个沙箱的占位凭据", "the request lacks this sandbox's placeholder credential"},
	"credential_mismatch":      {"请求的凭据请求头格式不对", "the credential header is malformed"},
	"connection_header_denied": {"请求带了不允许的 Connection 请求头", "the Connection header is not allowed"},
	"trailer_denied":           {"请求带了 trailer（正文之后再附加的请求头），沙箱不接受", "requests with trailers are not accepted"},
	"body_too_large":           {"请求体太大", "the request body is too large"},
	"request_quota_exceeded":   {"这个沙箱已经用完了请求次数上限", "this sandbox has used up its request limit"},
	"token_quota_exceeded":     {"这个沙箱已经用完了 token 上限", "this sandbox has used up its token limit"},
}

// blocked answers a refused request so that people and agents can tell what happened: the
// status line names the sandbox policy, the body explains the reason in Chinese and English
// (as JSON when the client asks for JSON) and gives the number of the audit event.
func blocked(req *http.Request, status int, reason, host string, event uint64) *http.Response {
	text, ok := blockedText[reason]
	if !ok {
		text = [2]string{"请求被沙箱策略拦截", "the request was refused by the sandbox policy"}
	}
	zh, en := text[0], text[1]
	if strings.Contains(zh, "%s") {
		if host == "" {
			host = "目标地址"
		}
		zh, en = fmt.Sprintf(zh, host), fmt.Sprintf(en, host)
	}
	record, recordEN := "", ""
	if event > 0 {
		record, recordEN = fmt.Sprintf("（请求日志记录 #%d）", event), fmt.Sprintf(" (request log event #%d)", event)
	}
	zh = "AIofCage 沙箱策略拦截了这个请求：" + zh + "。请不要重试、换源或修改代理设置；确实需要时，请让用户在 AIofCage 里为这个沙箱打开对应的开发环境或查看请求日志" + record + "。"
	en = "Blocked by AIofCage sandbox policy (" + reason + "): " + en + ". Do not retry, switch sources or change proxy settings; if this access is needed, ask the user to allow it in AIofCage" + recordEN + "."
	var body []byte
	contentType := "text/plain; charset=utf-8"
	if strings.Contains(strings.ToLower(req.Header.Get("Accept")), "json") {
		contentType = "application/json"
		fields := map[string]any{"error": zh, "message": en, "reason": reason}
		if event > 0 {
			fields["event"] = event
		}
		body, _ = json.Marshal(fields)
	} else {
		body = []byte(zh + "\n" + en + "\n")
	}
	resp := &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: req, Header: http.Header{},
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
	if status == http.StatusForbidden {
		resp.Status = blockedStatus
	}
	resp.Header.Set("Content-Type", contentType)
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("X-Aiofcage-Reason", reason)
	if event > 0 {
		resp.Header.Set("X-Aiofcage-Event", strconv.FormatUint(event, 10))
	}
	resp.Header.Set("Cache-Control", "no-store")
	return resp
}

// scopedPackage is the metadata path of a scoped npm package, which registries require with an
// encoded slash: /@scope%2Fname, optionally followed by more path.
var scopedPackage = regexp.MustCompile(`^/@[a-z0-9~][a-z0-9._~-]*%2[fF][a-z0-9~][a-z0-9._~-]*(/[^%]*)?$`)

// policyPath returns the path the policy evaluates, or false when the request path could be read
// in more than one way. The literal characters ! ( ) * ' [ ] are accepted (Go module paths use "!"
// for capitals; Go marks such paths as raw, but the bytes sent upstream stay as written). Two
// percent-encodings are accepted, evaluated decoded: an encoded "@" anywhere (npm mirrors write
// scopes as %40types), which cannot change how the path splits, and the slash of a scoped npm
// package name. Every other encoding could, so it is refused.
func policyPath(u *url.URL) (string, bool) {
	escaped := u.EscapedPath()
	if !strings.Contains(escaped, "%") {
		return u.Path, escaped == u.Path && policy.ValidPath(u.Path)
	}
	rest := strings.ReplaceAll(escaped, "%40", "@")
	if strings.Contains(rest, "%") && (!scopedPackage.MatchString(rest) || strings.Count(rest, "%") != 1) {
		return "", false
	}
	return u.Path, policy.ValidPath(u.Path)
}

// redirectTarget reports where a redirect leads and whether it may reach the agent: only when
// the request carried no injected credential and the target is an https address on port 443 that
// the policy allows for GET without a credential. The agent's client then follows it through the
// proxy, where the new request is checked again. An unusable or invalid target is never echoed.
func redirectTarget(active *compiled, state *requestState, resp *http.Response) (string, bool) {
	location, err := resp.Location()
	if err != nil {
		return "", false
	}
	host := strings.ToLower(location.Hostname())
	if !policy.ValidHost(host, false) {
		return "", false
	}
	if state.secretID != "" || location.Scheme != "https" || (location.Port() != "" && location.Port() != "443") || location.User != nil {
		return host, false
	}
	path, ok := policyPath(location)
	if !ok {
		return host, false
	}
	decision := active.config.Policy.Evaluate(policy.Request{Host: host, Port: policy.DefaultPort, Method: http.MethodGet, Path: path})
	return host, decision.Allowed && decision.SecretRef == ""
}

// dropOwnHeaders removes X-Aiofcage-* headers, which only the gatekeeper may set: an agent or an
// upstream could otherwise make an answer look like the gatekeeper's own.
func dropOwnHeaders(header http.Header) {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(name), "x-aiofcage-") {
			delete(header, name)
		}
	}
}

// carriesCredential reports whether a request brings its own credential.
func carriesCredential(header http.Header) bool {
	for _, name := range append([]string{"Cookie"}, credentialHeaders...) {
		if len(header.Values(name)) > 0 {
			return true
		}
	}
	return false
}
