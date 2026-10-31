package service

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestFailedControlKeepsACheckFromPassing(t *testing.T) {
	controls := []CheckControl{{Name: "a", OK: true}, {Name: "b", OK: false}}
	if got := settle(AttackResult{Status: checkPassed, Controls: controls}); got.Status != checkInconclusive || !strings.Contains(got.Note, "b") {
		t.Fatalf("a failed control must leave the check inconclusive: %+v", got)
	}
	if got := settle(AttackResult{Status: checkProblem, Controls: controls}); got.Status != checkProblem {
		t.Fatalf("a problem stays a problem: %+v", got)
	}
	if got := settle(AttackResult{Status: checkPassed, Controls: controls[:1]}); got.Status != checkPassed {
		t.Fatalf("%+v", got)
	}
}

func TestProbeScriptsAreValidPython(t *testing.T) {
	for name, script := range map[string]string{"canary": dnsCanaryPython, "dns": dnsClientPython, "docker": dockerScanPython,
		"boundary": boundaryPython, "database": dbControlPython,
		"server": fakeDockerPython + "serve('/sock/docker.sock')"} {
		cmd := exec.Command("python3", "-c", "import ast,sys;ast.parse(sys.stdin.read())")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
}

func canaryLog(nonce string, names ...string) []byte {
	lines := []string{`{"ready": true}`, `{"vm_resolves": true}`}
	for _, name := range names {
		lines = append(lines, `{"name": "`+name+"-"+nonce+`.aiofcage.invalid"}`)
	}
	return []byte(strings.Join(lines, "\n"))
}

func dnsAgent(change map[string]string) []byte {
	value := map[string]string{"direct": "blocked", "embedded": "failed", "udp-223.5.5.5": "blocked", "udp-8.8.8.8": "blocked",
		"tcp-53": "blocked", "tcp-853": "blocked", "udp-ipv6": "blocked", "public-name": "failed"}
	for k, v := range change {
		value[k] = v
	}
	out, _ := json.Marshal(value)
	return out
}

func TestDNSVerdict(t *testing.T) {
	negative := []byte(`{"direct":"blocked","embedded":"failed"}`)
	good := dnsRun{Nonce: "n1", Canary: canaryLog("n1", "control-direct", "control-embedded"), Negative: negative, Agent: dnsAgent(nil)}
	for _, tt := range []struct {
		name string
		edit func(*dnsRun)
		want string
	}{
		{"isolated", func(*dnsRun) {}, checkPassed},
		{"embedded forwarding not observed", func(r *dnsRun) { r.Canary = canaryLog("n1", "control-direct") }, checkInconclusive},
		{"canary never answered", func(r *dnsRun) { r.Canary = canaryLog("n1") }, checkInconclusive},
		{"another run's names do not count", func(r *dnsRun) { r.Canary = canaryLog("n2", "control-direct", "control-embedded") }, checkInconclusive},
		{"agent query reached the canary", func(r *dnsRun) {
			r.Canary = canaryLog("n1", "control-direct", "control-embedded", "agent-direct")
		}, checkProblem},
		{"internal network forwarded", func(r *dnsRun) {
			r.Canary = canaryLog("n1", "control-direct", "control-embedded", "negative-embedded")
		}, checkProblem},
		{"agent resolved a public name", func(r *dnsRun) { r.Agent = dnsAgent(map[string]string{"public-name": "resolved"}) }, checkProblem},
		{"agent reached a public resolver", func(r *dnsRun) { r.Agent = dnsAgent(map[string]string{"udp-8.8.8.8": "answered"}) }, checkProblem},
		{"a timeout proves nothing", func(r *dnsRun) { r.Agent = dnsAgent(map[string]string{"tcp-853": "timeout"}) }, checkInconclusive},
		{"agent output missing", func(r *dnsRun) { r.Agent = nil }, checkInconclusive},
		{"helper failed", func(r *dnsRun) { r.Err = errors.New("docker run failed") }, checkInconclusive},
		{"a leak counts even when a helper failed", func(r *dnsRun) {
			r.Err = errors.New("docker run failed")
			r.Canary = canaryLog("n1", "agent-direct")
		}, checkProblem},
	} {
		run := good
		tt.edit(&run)
		if got := settle(judgeDNS(Attack{ID: "dns-exfil"}, run)); got.Status != tt.want {
			t.Errorf("%s: %s %s %+v", tt.name, got.Status, got.Evidence, got.Controls)
		}
	}
}

func TestDockerSocketVerdict(t *testing.T) {
	mounted := []byte(`{"findings":["挂载 /run/docker.sock","socket /run/docker.sock（Docker 接口应答）"],"answers":1}`)
	clean := []byte(`{"findings":[],"answers":0,"control":"docker","sockets":0,"visited":900,"tcp":{"127.0.0.1:2375":"ECONNREFUSED"}}`)
	for _, tt := range []struct {
		name       string
		mounted    []byte
		mountedErr error
		agent      []byte
		agentErr   error
		want       string
	}{
		{"clean", mounted, nil, clean, nil, checkPassed},
		{"agent finds a socket", mounted, nil, []byte(`{"findings":["环境变量 DOCKER_HOST"],"control":"docker"}`), nil, checkProblem},
		{"agent's own fake not found", mounted, nil, []byte(`{"findings":[],"control":"ENOENT"}`), nil, checkInconclusive},
		{"mounted fake not found", []byte(`{"findings":[],"answers":0}`), nil, clean, nil, checkInconclusive},
		{"mounted sample failed", nil, errors.New("volume"), clean, nil, checkInconclusive},
		{"scan cut short", mounted, nil, []byte(`{"findings":[],"control":"docker","truncated":true}`), nil, checkInconclusive},
		{"exec failed", mounted, nil, clean, errors.New("exec"), checkInconclusive},
	} {
		got := settle(judgeDockerSocket(Attack{ID: "docker-socket"}, tt.mounted, tt.mountedErr, tt.agent, tt.agentErr))
		if got.Status != tt.want {
			t.Errorf("%s: %s %s %+v", tt.name, got.Status, got.Evidence, got.Controls)
		}
	}
}

func TestProjectBoundaryVerdict(t *testing.T) {
	for _, tt := range []struct {
		name   string
		vmSees bool
		out    string
		err    error
		want   string
	}{
		{"isolated", true, `{"project":true,"probe":false,"extra":[]}`, nil, checkPassed},
		{"sibling folder visible", true, `{"project":true,"probe":true,"extra":[]}`, nil, checkProblem},
		{"another project visible", true, `{"project":true,"probe":false,"extra":["/Users/u/CageWorkspace/other"]}`, nil, checkProblem},
		{"VM cannot see the folder", false, `{"project":true,"probe":false,"extra":[]}`, nil, checkInconclusive},
		{"project missing", true, `{"project":false,"probe":false,"extra":[]}`, nil, checkInconclusive},
		{"exec failed", true, ``, errors.New("exec"), checkInconclusive},
	} {
		if got := settle(judgeBoundary(Attack{}, tt.vmSees, nil, []byte(tt.out), tt.err)); got.Status != tt.want {
			t.Errorf("%s: %s %s", tt.name, got.Status, got.Evidence)
		}
	}
}

// The remote MCP check is strict: any answer the gatekeeper did not give means the request got
// past it, even an upstream 401.
func TestDirectRemoteMCPCountsAnyUpstreamAnswer(t *testing.T) {
	for _, tt := range []struct {
		headers string
		strict  bool
		want    string
	}{
		{"HTTP/1.1 403 Forbidden\r\nX-Aiofcage-Reason: default_deny\r\n", true, checkPassed},
		{"HTTP/1.1 403 Forbidden\r\nX-Aiofcage-Reason: path_denied\r\n", true, checkPassed},
		{"HTTP/1.1 200 Connection established\r\n\r\nHTTP/2 401\r\n", true, checkProblem},
		{"HTTP/1.1 200 Connection established\r\n\r\nHTTP/2 401\r\n", false, checkInconclusive},
		{"", true, checkInconclusive},
	} {
		headers, _ := json.Marshal(tt.headers)
		strict := "False"
		if tt.strict {
			strict = "True"
		}
		script := checkPython + "\nclass Response:\n    stdout=" + string(headers) + "\n    returncode=0\nsubprocess.run=lambda *args,**kwargs: Response()\ncurl_check('https://mcp.example/mcp',[],None,strict=" + strict + ")\n"
		out, err := exec.Command("python3", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("python: %v %s", err, out)
		}
		if got := classifyCheck(Attack{}, out, nil); got.Status != tt.want {
			t.Errorf("%q strict=%v: %+v", tt.headers, tt.strict, got)
		}
	}
}
