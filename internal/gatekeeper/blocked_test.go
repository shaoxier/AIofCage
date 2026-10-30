package gatekeeper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/shaoxier/AIofCage/internal/policy"
)

// devConfig opens model-b.example like a development source: anonymous GET downloads only, with
// no injected credential on that host.
func devConfig(version uint64) Config {
	config := testConfig(version)
	var rules []policy.Rule
	for _, rule := range config.Policy.Rules {
		if rule.Host != "model-b.example" {
			rules = append(rules, rule)
		}
	}
	config.Policy.Rules = append(rules,
		policy.Rule{ID: "dev-pkg", Effect: policy.Allow, Host: "model-b.example", Methods: []string{"GET", "HEAD"}, Path: "/*", Anonymous: true},
		policy.Rule{ID: "dev-pkg-write", Effect: policy.Deny, Host: "model-b.example", Methods: []string{"PUT"}, Path: "/*"})
	return config
}

func TestPathsAsPackageClientsWriteThem(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, devConfig(1))
	var seen []string
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.RequestURI)
	})
	for _, c := range []struct {
		target string
		status int
	}{
		{"https://model-b.example/@types%2fnode", 200},
		{"https://model-b.example/@types%2Fnode/latest", 200},
		{"https://model-b.example/github.com/!burnt!sushi/toml/@v/list", 200},
		{"https://model-b.example/pkg/(beta)*'x'", 200},
		{"https://model-b.example/a%2e%2e/b", 403},
		{"https://model-b.example/@types%2fnode%2fx", 403},
		{"https://model-b.example/%40types%2fnode", 200},
		{"https://model-b.example/packages/%40types/node/26.6.6/node-26.6.6.tgz", 200},
		{"https://model-b.example/packages/%40types%2e%2e/x", 403},
		{"https://model-b.example/a%2fb", 403},
	} {
		if status, body, _ := f.do(t, "GET", c.target, nil, ""); status != c.status {
			t.Errorf("%s: %d %s", c.target, status, body)
		}
	}
	want := "/@types%2fnode,/@types%2Fnode/latest,/github.com/!burnt!sushi/toml/@v/list,/pkg/(beta)*'x',/%40types%2fnode,/packages/%40types/node/26.6.6/node-26.6.6.tgz"
	if strings.Join(seen, ",") != want {
		t.Fatalf("the upstream must receive the path exactly as written: %v", seen)
	}
}

func TestAnonymousSourcesRefuseCredentialsReadably(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, devConfig(1))
	before := f.calls.Load()
	status, body, header := f.do(t, "GET", "https://model-b.example/left-pad", map[string][]string{"Authorization": {"Bearer npm_attacker_token"}}, "")
	if status != 403 || header.Get("X-Aiofcage-Reason") != "credential_not_allowed" || header.Get("X-Aiofcage-Event") == "" ||
		!strings.Contains(body, "匿名下载") || !strings.Contains(body, "Blocked by AIofCage sandbox policy") || !strings.Contains(body, "#"+header.Get("X-Aiofcage-Event")) {
		t.Fatalf("own credentials on a download source: %d %v %s", status, header, body)
	}
	status, body, header = f.do(t, "PUT", "https://model-b.example/left-pad", map[string][]string{"Accept": {"application/json"}}, "{}")
	var answer struct {
		Error, Message, Reason string
		Event                  uint64
	}
	if status != 403 || json.Unmarshal([]byte(body), &answer) != nil || answer.Reason != "rule_denied" || answer.Event == 0 ||
		!strings.Contains(answer.Error, "只放行下载") || !strings.Contains(answer.Message, "downloads only") {
		t.Fatalf("publishing, answered as JSON: %d %v %s", status, header, body)
	}
	if f.calls.Load() != before {
		t.Fatal("refused requests must not reach the upstream")
	}
}

func TestRedirectsPassOnlyWhereTheSandboxMayGo(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, devConfig(1))
	location := ""
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if location == "304" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		http.Redirect(w, r, location, http.StatusFound)
	})
	for _, c := range []struct {
		location string
		status   int
		shown    string
	}{
		{"/files/pkg-1.0.0.tgz", 302, ""},
		{"https://model-b.example/files/pkg-1.0.0.tgz", 302, ""},
		{"https://evil.example/collect?d=secret", 403, "evil.example"},
		{"http://model-b.example/files/pkg.tgz", 403, "model-b.example"},
		{"https://model-b.example:8443/files/pkg.tgz", 403, "model-b.example"},
		{"https://model-a.example/v1/responses", 403, "model-a.example"},
		{"304", 304, ""},
	} {
		location = c.location
		status, body, header := f.do(t, "GET", "https://model-b.example/pkg", nil, "")
		if status != c.status {
			t.Errorf("%s: %d %s", c.location, status, body)
			continue
		}
		if status == 302 && header.Get("Location") == "" {
			t.Errorf("%s: an allowed redirect keeps its Location", c.location)
		}
		if status == 403 && (header.Get("Location") != "" || !strings.Contains(body, c.shown) || strings.Contains(body, "collect")) {
			t.Errorf("%s: a refused redirect names only the host: %v %s", c.location, header, body)
		}
	}
	events := f.eventsAfter(t, 0)
	var hosts []string
	for _, event := range events {
		if event.ReasonCode == "redirect_denied" {
			hosts = append(hosts, event.RedirectHost)
		}
	}
	if strings.Join(hosts, ",") != "evil.example,model-b.example,model-b.example,model-a.example" {
		t.Fatalf("the log records where refused redirects pointed: %v", hosts)
	}
}

func TestRefusedTunnelNamesTheSandboxPolicy(t *testing.T) {
	f := newFixture(t, true)
	f.configure(t, devConfig(1))
	u, _ := url.Parse(f.proxyURL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "CONNECT unknown.example:443 HTTP/1.1\r\nHost: unknown.example:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Status != blockedStatus || resp.Header.Get("X-Aiofcage-Event") == "" {
		t.Fatalf("status line %q, headers %v", resp.Status, resp.Header)
	}
}
