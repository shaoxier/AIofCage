package service

import (
	"encoding/json"
	"errors"
	"fmt"
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
		"tcp-53": "blocked", "tcp-853": "blocked", "udp-ipv6": "blocked", "public-name": "failed",
		"udp-gateway": "timeout", "tcp-gateway": "ECONNREFUSED", "udp-gatekeeper": "ECONNREFUSED", "tcp-gatekeeper": "ECONNREFUSED"}
	for k, v := range change {
		value[k] = v
	}
	out, _ := json.Marshal(value)
	return out
}

func TestDNSVerdict(t *testing.T) {
	negative := []byte(`{"direct":"blocked","embedded":"failed"}`)
	good := dnsRun{Nonce: "n1", Canary: canaryLog("n1", "control-direct", "control-embedded"), Negative: negative, Agent: dnsAgent(nil),
		Internal: "net-in", Networks: []string{"net-in"}, DNSServers: []string{"127.0.0.1"}}
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
		{"the VM itself resolves nothing", func(r *dnsRun) {
			r.Canary = []byte(strings.Replace(string(r.Canary), `{"vm_resolves": true}`, `{"vm_resolves": false}`, 1))
		}, checkInconclusive},
		{"no word from the VM lookup", func(r *dnsRun) {
			r.Canary = []byte(strings.Replace(string(r.Canary), `{"vm_resolves": true}`, ``, 1))
		}, checkInconclusive},
		{"agent got an answer the canary did not log", func(r *dnsRun) { r.Agent = dnsAgent(map[string]string{"direct": "answered"}) }, checkProblem},
		{"agent resolved the test name", func(r *dnsRun) { r.Agent = dnsAgent(map[string]string{"embedded": "resolved"}) }, checkProblem},
		{"internal network resolved the test name", func(r *dnsRun) { r.Negative = []byte(`{"direct":"blocked","embedded":"resolved"}`) }, checkProblem},
		{"a resolver listens on the gateway", func(r *dnsRun) { r.Agent = dnsAgent(map[string]string{"tcp-gateway": "connected"}) }, checkProblem},
		{"the gatekeeper answers DNS", func(r *dnsRun) { r.Agent = dnsAgent(map[string]string{"udp-gatekeeper": "answered"}) }, checkProblem},
		{"agent on another network too", func(r *dnsRun) { r.Networks = []string{"bridge", "net-in"} }, checkProblem},
		{"agent with a real DNS upstream", func(r *dnsRun) { r.DNSServers = []string{"8.8.8.8"} }, checkProblem},
		{"Docker settings unreadable", func(r *dnsRun) { r.Networks, r.DNSServers, r.ConfigErr = nil, nil, errors.New("inspect") }, checkInconclusive},
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
		{"isolated", true, `{"project":true,"mounted":true,"rooted":true}`, nil, checkPassed},
		{"sibling folder visible", true, `{"project":true,"mounted":true,"rooted":true,"probe":["/w/.aiofcage-probe-1/marker.txt"]}`, nil, checkProblem},
		{"another project visible", true, `{"project":true,"mounted":true,"rooted":true,"extra":["/Users/u/CageWorkspace/other"]}`, nil, checkProblem},
		{"workspace mounted elsewhere too", true, `{"project":true,"mounted":true,"rooted":true,"mounts":["/workspace（/）"]}`, nil, checkProblem},
		{"wrong folder mounted as the project", true, `{"project":true,"mounted":true,"rooted":false}`, nil, checkProblem},
		{"project not a mount of its own", true, `{"project":true,"mounted":false,"rooted":false}`, nil, checkInconclusive},
		{"VM cannot see the folder", false, `{"project":true,"mounted":true,"rooted":true}`, nil, checkInconclusive},
		{"project missing", true, `{"project":false,"mounted":true,"rooted":true}`, nil, checkInconclusive},
		{"exec failed", true, ``, errors.New("exec"), checkInconclusive},
	} {
		if got := settle(judgeBoundary(Attack{}, tt.vmSees, nil, []byte(tt.out), tt.err)); got.Status != tt.want {
			t.Errorf("%s: %s %s", tt.name, got.Status, got.Evidence)
		}
	}
}

// Every remote service is tried without credentials. Only a denial the gatekeeper gives before
// anything reaches the service passes; any answer from the service, a redirect it sent or a failed
// attempt to reach it means the proxy let the request through.
func TestDirectRemoteMCPNeedsADenialBeforeTheService(t *testing.T) {
	deny := "HTTP/1.1 403 Forbidden\r\nX-Aiofcage-Reason: default_deny\r\n"
	for _, tt := range []struct {
		name    string
		answers []string
		want    string
	}{
		{"both denied", []string{deny, "HTTP/1.1 403 Forbidden\r\nX-Aiofcage-Reason: rule_denied\r\n"}, checkPassed},
		{"second one reached the service", []string{deny, "HTTP/1.1 200 Connection established\r\n\r\nHTTP/2 401\r\n"}, checkProblem},
		{"service redirected", []string{"HTTP/1.1 200 Connection established\r\n\r\nHTTP/1.1 403 Forbidden\r\nX-Aiofcage-Reason: redirect_denied\r\n"}, checkProblem},
		{"gatekeeper tried to connect", []string{"HTTP/1.1 502 Bad Gateway\r\nX-Aiofcage-Reason: upstream_failed\r\n"}, checkProblem},
		{"a credential refusal is no proof", []string{"HTTP/1.1 403 Forbidden\r\nX-Aiofcage-Reason: credential_foreign\r\n"}, checkInconclusive},
		{"no answer", []string{deny, ""}, checkInconclusive},
	} {
		answers, _ := json.Marshal(tt.answers)
		var urls []string
		for i := range tt.answers {
			urls = append(urls, fmt.Sprintf("https://mcp%d.example/mcp", i))
		}
		targets, _ := json.Marshal(urls)
		script := checkPython + "\nanswers=" + string(answers) + "\nclass Response:\n    def __init__(self,out): self.stdout=out;self.returncode=0\n" +
			"def fake(args,**kwargs):\n    assert not any(a.lower().startswith('authorization') for a in args), args\n    return Response(answers[int(args[-1][11])])\n" +
			"subprocess.run=fake\ndirect_check(" + string(targets) + ")\n"
		out, err := exec.Command("python3", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("python: %v %s", err, out)
		}
		if got := classifyCheck(Attack{}, out, nil); got.Status != tt.want {
			t.Errorf("%s: %+v", tt.name, got)
		}
	}
}

// boundaryRun runs the project-boundary script against a made-up mount table and file tree.
func boundaryRun(t *testing.T, mountinfo string, exists []string) ProjectBoundaryOutput {
	t.Helper()
	shim := `import builtins,io,os
MOUNTINFO=` + pyString(mountinfo) + `
EXISTS=set(` + pyList(exists) + `)
real_open=builtins.open
builtins.open=lambda path,*a,**k: io.StringIO(MOUNTINFO) if path=="/proc/self/mountinfo" else real_open(path,*a,**k)
os.path.exists=lambda p: p in EXISTS
os.path.isdir=lambda p: p in EXISTS
os.listdir=lambda p: sorted({e[len(p)+1:].split("/")[0] for e in EXISTS if e.startswith(p+"/")})
`
	out, err := exec.Command("python3", "-c", shim+boundaryPython, "/W/proj", "/W", "/W/.aiofcage-probe-1/marker.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v %s", err, out)
	}
	var value ProjectBoundaryOutput
	if err := json.Unmarshal(out, &value); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return value
}

type ProjectBoundaryOutput struct {
	Project bool     `json:"project"`
	Mounted bool     `json:"mounted"`
	Rooted  bool     `json:"rooted"`
	Mounts  []string `json:"mounts"`
	Probe   []string `json:"probe"`
	Extra   []string `json:"extra"`
}

func pyString(s string) string { data, _ := json.Marshal(s); return string(data) }
func pyList(items []string) string {
	data, _ := json.Marshal(items)
	return string(data)
}

func TestProjectBoundaryReadsTheMountTable(t *testing.T) {
	base := "1 0 0:30 / / rw - overlay overlay rw\n2 1 0:5 / /proc rw - proc proc rw\n3 1 254:1 /docker/volumes/home/_data /home/agent rw - ext4 /dev/vda1 rw\n"
	project := "4 1 0:41 /proj /W/proj rw - virtiofs mount0 rw\n"
	files := []string{"/W", "/W/proj", "/W/proj/main.go", "/W/.aiofcage-probe-1/marker.txt", "/W/other/secret.txt"}
	inContainer := []string{"/W", "/W/proj", "/W/proj/main.go"} // what Docker shows above the project
	if v := boundaryRun(t, base+project, inContainer); !v.Mounted || !v.Rooted || len(v.Mounts)+len(v.Probe)+len(v.Extra) > 0 {
		t.Fatalf("a correct mount: %+v", v)
	}
	// The whole workspace mounted at the project's path (a wrong bind source).
	whole := "4 1 0:41 / /W/proj rw - virtiofs mount0 rw\n"
	sees := []string{"/W", "/W/proj", "/W/proj/proj/main.go", "/W/proj/.aiofcage-probe-1/marker.txt", "/W/proj/other/secret.txt"}
	if v := boundaryRun(t, base+whole, sees); v.Rooted || len(v.Probe) != 1 || v.Probe[0] != "/W/proj/.aiofcage-probe-1/marker.txt" {
		t.Fatalf("wrong bind source: %+v", v)
	}
	// The project mounted correctly, and the workspace once more elsewhere, with a space in its name.
	extra := project + "5 1 0:41 / /work\\040space rw - virtiofs mount0 rw\n"
	sees = append(append([]string{}, inContainer...), "/work space/.aiofcage-probe-1/marker.txt")
	if v := boundaryRun(t, base+extra, sees); len(v.Mounts) != 1 || v.Mounts[0] != "/work space（/）" ||
		len(v.Probe) != 1 || v.Probe[0] != "/work space/.aiofcage-probe-1/marker.txt" {
		t.Fatalf("extra mount: %+v", v)
	}
	// The workspace shared at its own path: no mount of the project itself, and the siblings show.
	shared := "4 1 0:41 / /W rw - virtiofs mount0 rw\n"
	if v := boundaryRun(t, base+shared, files); v.Mounted || len(v.Mounts) != 1 || len(v.Probe) != 1 || len(v.Extra) == 0 {
		t.Fatalf("whole workspace shared: %+v", v)
	}
}
