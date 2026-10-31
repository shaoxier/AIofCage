package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// The checks below compare what the agent can do with helper containers that must see the same
// thing (positive samples). Helpers run the agent's image with the agent's restrictions and are
// removed afterwards; they never get the project, the home volume or the VM's Docker socket.

func probeNonce() string {
	var raw [6]byte
	rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// waitForProbe waits until a background probe printed marker, e.g. that it is listening.
func (a *App) waitForProbe(ctx context.Context, name, marker string) bool {
	for attempt := 0; attempt < 50; attempt++ {
		if out, err := a.manager.ProbeOutput(ctx, name); err == nil && bytes.Contains(out, []byte(marker)) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false
}

// probeRunner runs a check's script where the agent runs; tests swap in a deliberately broken setup
// to see the check report the problem.
type probeRunner func(ctx context.Context, script string, args ...string) ([]byte, error)

func (a *App) inAgent(sbx sandbox.Sandbox) probeRunner {
	return func(ctx context.Context, script string, args ...string) ([]byte, error) {
		var out bytes.Buffer
		err := a.manager.Exec(ctx, sbx.ID, false, nil, &out, &out, append([]string{"python3", "-c", script}, args...)...)
		return out.Bytes(), err
	}
}

// ---- DNS ----

// dnsCanaryPython answers every query with NXDOMAIN and prints the names that carry this run's
// nonce. It also tells whether the VM resolves public names, so a failed lookup in the agent means
// something.
const dnsCanaryPython = `import socket,sys,json,threading
nonce=sys.argv[1]
def say(**value): print(json.dumps(value),flush=True)
def public():
    try: socket.getaddrinfo("example.com",443);say(vm_resolves=True)
    except OSError: say(vm_resolves=False)
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(("0.0.0.0",53))
say(ready=True)
threading.Thread(target=public,daemon=True).start()
while True:
    data,addr=s.recvfrom(512)
    labels=[];i=12
    while i<len(data) and data[i] and len(labels)<16:
        n=data[i];labels.append(data[i+1:i+1+n].decode("ascii","replace"));i+=1+n
    name=".".join(labels).lower()
    if nonce in name: say(name=name)
    if i+5<=len(data): s.sendto(data[:2]+b"\x81\x83"+data[4:6]+b"\x00"*6+data[12:i+5],addr)
`

// dnsClientPython sends queries named <role>-<path>-<nonce>.aiofcage.invalid. The agent also tries
// public resolvers, TCP 53, DoT and IPv6, and resolves a real public name.
const dnsClientPython = `import socket,sys,json,errno,time
role,nonce,canary=sys.argv[1:4]
LOCAL={errno.ENETUNREACH,errno.EHOSTUNREACH,errno.EACCES,errno.EPERM,errno.EADDRNOTAVAIL,errno.EAFNOSUPPORT}
def packet(name):
    return b"\x12\x34\x01\x00\x00\x01"+b"\x00"*6+b"".join(bytes([len(p)])+p.encode() for p in name.split("."))+b"\x00\x00\x01\x00\x01"
def failure(e):
    if e.errno in LOCAL: return "blocked"
    if isinstance(e,socket.timeout): return "timeout"
    return errno.errorcode.get(e.errno,type(e).__name__)
def udp(name,server,family=socket.AF_INET,tries=1):
    result="timeout"
    for attempt in range(tries):
        if attempt: time.sleep(1)
        try:
            with socket.socket(family,socket.SOCK_DGRAM) as s:
                s.settimeout(2);s.sendto(packet(name),(server,53));s.recvfrom(512)
            return "answered"
        except OSError as e: result=failure(e)
    return result
def tcp(host,port,family=socket.AF_INET):
    try:
        with socket.socket(family,socket.SOCK_STREAM) as s:
            s.settimeout(3);s.connect((host,port))
        return "connected"
    except OSError as e: return failure(e)
def resolve(name):
    try: socket.getaddrinfo(name,443);return "resolved"
    except OSError: return "failed"
def label(path): return role+"-"+path+"-"+nonce+".aiofcage.invalid"
out={"direct":udp(label("direct"),canary,tries=5 if role=="control" else 1),"embedded":resolve(label("embedded"))}
if role=="agent":
    out.update({"udp-223.5.5.5":udp(label("ali"),"223.5.5.5"),"udp-8.8.8.8":udp(label("google"),"8.8.8.8"),
        "tcp-53":tcp("223.5.5.5",53),"tcp-853":tcp("223.5.5.5",853),
        "udp-ipv6":udp(label("v6"),"2400:3200::1",socket.AF_INET6),"public-name":resolve("example.com")})
print(json.dumps(out))
`

// dnsRun is what one DNS check observed.
type dnsRun struct {
	Nonce    string
	Canary   []byte // the canary's output
	Control  []byte // an egress-side helper whose resolver is the canary
	Negative []byte // an internal-side helper whose resolver is the canary
	Agent    []byte
	Err      error // the first failure running a helper
}

func (a *App) checkDNS(ctx context.Context, sbx sandbox.Sandbox, check Attack, agent probeRunner) AttackResult {
	run := dnsRun{Nonce: probeNonce()}
	canary, ip, err := a.manager.StartProbe(ctx, sbx.ID, runtime.Probe{Network: runtime.ProbeEgress,
		Command: []string{"python3", "-u", "-c", dnsCanaryPython, run.Nonce}})
	if err != nil {
		return classifyCheck(check, nil, err)
	}
	defer a.manager.RemoveProbe(canary)
	if !a.waitForProbe(ctx, canary, `"ready"`) {
		return classifyCheck(check, nil, errors.New("DNS 金丝雀没有启动"))
	}
	client := func(role string) []string { return []string{"python3", "-c", dnsClientPython, role, run.Nonce, ip} }
	var errs []error
	run.Control, err = a.manager.RunProbe(ctx, sbx.ID, runtime.Probe{Network: runtime.ProbeEgress, DNS: ip, Command: client("control")})
	errs = append(errs, err)
	run.Negative, err = a.manager.RunProbe(ctx, sbx.ID, runtime.Probe{Network: runtime.ProbeInternal, DNS: ip, Command: client("negative")})
	errs = append(errs, err)
	run.Agent, err = agent(ctx, dnsClientPython, "agent", run.Nonce, ip)
	errs = append(errs, err)
	run.Err = errors.Join(errs...)
	// The canary prints each query as it arrives; give the last one a moment.
	time.Sleep(300 * time.Millisecond)
	run.Canary, err = a.manager.ProbeOutput(ctx, canary)
	if err != nil {
		return classifyCheck(check, nil, err)
	}
	return judgeDNS(check, run)
}

var dnsWords = map[string]string{"blocked": "本地拒绝", "timeout": "超时", "answered": "有应答", "connected": "已连接",
	"resolved": "解析成功", "failed": "解析失败"}

func dnsWord(value string) string {
	if word, ok := dnsWords[value]; ok {
		return word
	}
	if value == "" {
		return "未取得"
	}
	return value
}

func judgeDNS(check Attack, run dnsRun) AttackResult {
	r := AttackResult{Attack: check, Status: checkInconclusive,
		Note: "金丝雀是临时放在出网网络上的 DNS 服务，代表“虚拟机以外”。只发送 aiofcage.invalid 下的随机测试名，另外解析一次 example.com；未覆盖 DNS 以外的隧道（例如经允许的 HTTPS 域名）。"}
	recorded := map[string]bool{}
	vm := "未取得"
	for _, line := range bytes.Split(run.Canary, []byte("\n")) {
		var entry struct {
			Name       string `json:"name"`
			VMResolves *bool  `json:"vm_resolves"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if entry.Name != "" {
			recorded[entry.Name] = true
		}
		if entry.VMResolves != nil {
			vm = map[bool]string{true: "能", false: "不能（Agent 解析不了公网域名不说明问题，只看金丝雀）"}[*entry.VMResolves]
		}
	}
	seen := func(role, path string) bool { return recorded[role+"-"+path+"-"+run.Nonce+".aiofcage.invalid"] }
	r.Controls = []CheckControl{
		{Name: "出网侧对照容器直接发给金丝雀", OK: seen("control", "direct")},
		{Name: "出网侧对照容器经 Docker 内嵌 DNS 转发给金丝雀", OK: seen("control", "embedded")},
	}
	var leaked []string
	for name := range recorded {
		if strings.HasPrefix(name, "agent-") || strings.HasPrefix(name, "negative-") {
			leaked = append(leaked, "金丝雀收到了 "+name)
		}
	}
	parse := func(out []byte) map[string]string {
		var value map[string]string
		if json.Unmarshal(bytes.TrimSpace(out), &value) != nil {
			return nil
		}
		return value
	}
	agent, negative := parse(run.Agent), parse(run.Negative)
	public := []struct{ key, label string }{{"udp-223.5.5.5", "UDP 53 → 223.5.5.5"}, {"udp-8.8.8.8", "UDP 53 → 8.8.8.8"},
		{"tcp-53", "TCP 53"}, {"tcp-853", "DoT 853"}, {"udp-ipv6", "IPv6 UDP 53"}}
	settled := agent != nil && negative != nil
	var paths []string
	if agent != nil {
		paths = append(paths, "直连金丝雀 "+dnsWord(agent["direct"]), "内嵌 DNS "+dnsWord(agent["embedded"]))
		for _, path := range public {
			value := agent[path.key]
			paths = append(paths, path.label+" "+dnsWord(value))
			if value == "answered" || value == "connected" {
				leaked = append(leaked, path.label+" "+dnsWord(value))
			} else if value != "blocked" {
				settled = false
			}
		}
		paths = append(paths, "解析 example.com "+dnsWord(agent["public-name"]))
		if agent["public-name"] == "resolved" {
			leaked = append(leaked, "Agent 解析到了公网域名")
		} else if agent["public-name"] != "failed" {
			settled = false
		}
	}
	agentSeen, negativeSeen := 0, 0
	for name := range recorded {
		if strings.HasPrefix(name, "agent-") {
			agentSeen++
		} else if strings.HasPrefix(name, "negative-") {
			negativeSeen++
		}
	}
	lines := []string{fmt.Sprintf("金丝雀收到：Agent %d 条，内部网络反例 %d 条", agentSeen, negativeSeen),
		"Agent：" + strings.Join(paths, "，")}
	if negative != nil {
		lines = append(lines, "反例（内部网络、DNS 指向金丝雀）：直连 "+dnsWord(negative["direct"])+"，内嵌 DNS "+dnsWord(negative["embedded"]))
	}
	lines = append(lines, "虚拟机解析公网域名："+vm)
	r.Evidence = strings.Join(lines, "\n")
	switch {
	case len(leaked) > 0:
		r.Status = checkProblem
		r.Evidence = strings.Join(leaked, "；") + "\n" + r.Evidence
	case run.Err != nil:
		r.Evidence = "检查容器执行失败：" + run.Err.Error() + "\n" + r.Evidence
	case settled:
		r.Status = checkPassed
	}
	return r
}

// ---- Docker socket ----

// fakeDockerPython serves a fake Docker API on a Unix socket: GET /_ping answers OK.
const fakeDockerPython = `import socket,os,sys,threading
def serve(path):
    s=socket.socket(socket.AF_UNIX);s.bind(path);os.chmod(path,0o666);s.listen(8)
    def loop():
        while True:
            c,_=s.accept()
            try: c.recv(1024);c.sendall(b"HTTP/1.1 200 OK\r\nApi-Version: 1.47\r\nContent-Length: 2\r\n\r\nOK")
            except OSError: pass
            finally: c.close()
    threading.Thread(target=loop,daemon=True).start()
`

// dockerScanPython looks for a reachable container runtime: DOCKER_HOST, mounts and socket files
// (pinging each with GET /_ping), and the Docker TCP ports on loopback, the gatekeeper and the
// network's gateway. As the agent, it first starts its own fake API under /tmp, which it must find.
const dockerScanPython = fakeDockerPython + `import stat,json,re,errno
role,nonce,project=sys.argv[1:4]
KNOWN=re.compile(r"(docker|containerd|podman|buildkit|crio|guestagent)[^/]*\.sock$")
PATHS=["/var/run/docker.sock","/run/docker.sock","/run/containerd/containerd.sock","/run/podman/podman.sock",
    "/run/buildkit/buildkitd.sock","/run/lima-guestagent.sock",os.path.expanduser("~/.docker/run/docker.sock")]
SKIP={"/proc","/sys","/dev","/usr",project}
SKIP_NAMES={"node_modules",".npm",".cache",".git"}
fake=""
if role=="agent":
    folder="/tmp/aiofcage-probe-"+nonce;os.mkdir(folder,0o700);fake=folder+"/docker.sock";serve(fake)
def ping(connect):
    try:
        with connect() as s:
            s.sendall(b"GET /_ping HTTP/1.0\r\nHost: docker\r\n\r\n");data=s.recv(512)
    except OSError as e:
        return "timeout" if isinstance(e,socket.timeout) else errno.errorcode.get(e.errno,type(e).__name__)
    head=data.split(b"\r\n",1)[0]
    return "docker" if b" 200" in head and (data.rstrip().endswith(b"OK") or b"api-version" in data.lower()) else "other"
def unix(path):
    def connect():
        s=socket.socket(socket.AF_UNIX);s.settimeout(2)
        try: s.connect(path)
        except OSError: s.close();raise
        return s
    return connect
sockets=set();visited=0;truncated=False
def walk(path,depth):
    global visited,truncated
    try: entries=list(os.scandir(path))
    except OSError: return
    for e in entries:
        visited+=1
        if visited>50000: truncated=True;return
        try:
            if e.is_symlink(): continue
            if e.is_dir(follow_symlinks=False):
                if e.path not in SKIP and e.name not in SKIP_NAMES and depth<5: walk(e.path,depth+1)
            elif stat.S_ISSOCK(e.stat(follow_symlinks=False).st_mode): sockets.add(e.path)
        except OSError: pass
walk("/",1)
for p in PATHS:
    try:
        if stat.S_ISSOCK(os.stat(p).st_mode): sockets.add(os.path.realpath(p))
    except OSError: pass
findings=[];answers=0;control=""
for key in ("DOCKER_HOST","CONTAINER_HOST"):
    if os.environ.get(key): findings.append("环境变量 "+key)
for line in open("/proc/self/mountinfo"):
    point=line.split()[4].replace("\\040"," ")
    try: is_socket=stat.S_ISSOCK(os.stat(point).st_mode)
    except OSError: is_socket=False
    if (KNOWN.search(point) or is_socket) and point!=fake: findings.append("挂载 "+point)
for path in sorted(sockets):
    answer=ping(unix(path))
    if path==fake: control=answer;continue
    answers+=answer=="docker"
    if answer=="docker" or KNOWN.search(path): findings.append("socket "+path+("（Docker 接口应答）" if answer=="docker" else ""))
hosts={"127.0.0.1"}
try: hosts.add(socket.gethostbyname("gatekeeper"))
except OSError: pass
for row in open("/proc/net/route").read().splitlines()[1:]:
    cols=row.split()
    if len(cols)>7 and cols[1]!="00000000":
        # Little-endian hex of the address; Docker gives the gateway the first address of the subnet.
        net=int(cols[1],16)&int(cols[7],16)
        hosts.add(socket.inet_ntoa((net|0x01000000).to_bytes(4,"little")))
tcp={}
for host in sorted(hosts):
    for port in (2375,2376):
        tcp[host+":"+str(port)]=answer=ping(lambda: socket.create_connection((host,port),timeout=2))
        answers+=answer=="docker"
        if answer=="docker": findings.append("TCP "+host+":"+str(port)+"（Docker 接口应答）")
if fake: os.unlink(fake);os.rmdir(folder)
print(json.dumps(dict(findings=findings,answers=answers,control=control,sockets=len(sockets)-bool(fake),visited=visited,
    truncated=truncated,tcp=tcp),ensure_ascii=False))
`

type dockerScan struct {
	Findings  []string          `json:"findings"`
	Answers   int               `json:"answers"`
	Control   string            `json:"control"`
	Sockets   int               `json:"sockets"`
	Visited   int               `json:"visited"`
	Truncated bool              `json:"truncated"`
	TCP       map[string]string `json:"tcp"`
}

func (a *App) checkDockerSocket(ctx context.Context, sbx sandbox.Sandbox, check Attack, agent probeRunner) AttackResult {
	mounted, mountedErr := a.withFakeDocker(ctx, sbx, func(path string) ([]byte, error) {
		return a.manager.RunProbe(ctx, sbx.ID, runtime.Probe{Network: runtime.ProbeNone,
			Mounts:  []string{"type=bind,src=" + path + ",dst=/var/run/docker.sock"},
			Command: []string{"python3", "-c", dockerScanPython, "control", "", ""}})
	})
	out, err := agent(ctx, dockerScanPython, "agent", probeNonce(), sbx.ProjectPath)
	return judgeDockerSocket(check, mounted, mountedErr, out, err)
}

// withFakeDocker serves a fake Docker API from a scratch volume while run uses it: the positive
// sample bind-mounts the socket at path where the real one usually is, the way it gets into
// containers by mistake.
func (a *App) withFakeDocker(ctx context.Context, sbx sandbox.Sandbox, run func(path string) ([]byte, error)) ([]byte, error) {
	volume, path, err := a.manager.ProbeVolume(ctx, sbx.ID)
	if err != nil {
		return nil, err
	}
	defer a.manager.RemoveProbeVolume(volume)
	server, _, err := a.manager.StartProbe(ctx, sbx.ID, runtime.Probe{Network: runtime.ProbeNone, Root: true,
		Mounts:  []string{"type=volume,src=" + volume + ",dst=/sock"},
		Command: []string{"python3", "-u", "-c", fakeDockerPython + "serve('/sock/docker.sock');print('ready',flush=True);threading.Event().wait()"}})
	if err != nil {
		return nil, err
	}
	defer a.manager.RemoveProbe(server) // before the volume it uses
	if !a.waitForProbe(ctx, server, "ready") {
		return nil, errors.New("假 Docker 接口没有启动")
	}
	return run(path + "/docker.sock")
}

func judgeDockerSocket(check Attack, mounted []byte, mountedErr error, agentOut []byte, agentErr error) AttackResult {
	r := AttackResult{Attack: check, Status: checkInconclusive,
		Note: "查看环境变量、挂载、根目录下 5 层以内的 socket 文件（不进项目、/usr 和 node_modules 等目录），对每个 socket 和 2375/2376 端口发 GET /_ping。不检查容器逃逸。"}
	var control dockerScan
	controlOK := mountedErr == nil && json.Unmarshal(bytes.TrimSpace(mounted), &control) == nil && control.Answers > 0 && len(control.Findings) > 0
	detail := ""
	if mountedErr != nil {
		detail = mountedErr.Error()
	} else if controlOK {
		detail = strings.Join(control.Findings, "，")
	}
	var agent dockerScan
	agentParsed := agentErr == nil && json.Unmarshal(bytes.TrimSpace(agentOut), &agent) == nil
	r.Controls = []CheckControl{
		{Name: "Agent 容器里临时放的假 Docker 接口被检出", OK: agentParsed && agent.Control == "docker"},
		{Name: "挂了假 Docker 接口的对照容器被检出", OK: controlOK, Detail: detail},
	}
	if !agentParsed {
		if agentErr != nil {
			r.Evidence = "检查命令执行失败：" + agentErr.Error()
		} else {
			r.Evidence = "未取得完整检查结果"
		}
		return r
	}
	found := "未发现 Docker 接口"
	if len(agent.Findings) > 0 {
		found = "发现：" + strings.Join(agent.Findings, "，")
	}
	var hosts []string
	for endpoint := range agent.TCP {
		if host, _, ok := strings.Cut(endpoint, ":"); ok && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)
	r.Evidence = fmt.Sprintf("%s；socket 文件 %d 个（不含测试用的假接口）；查看了 %d 个文件和目录；%s 的 2375/2376 端口没有 Docker 应答",
		found, agent.Sockets, agent.Visited, strings.Join(hosts, "、"))
	switch {
	case len(agent.Findings) > 0:
		r.Status = checkProblem
	case agent.Truncated:
		r.Evidence += "；条目太多，扫描没有做完"
	default:
		r.Status = checkPassed
	}
	return r
}

// ---- Project boundary ----

const boundaryPython = `import os,sys,json
project,folder,marker=sys.argv[1:4]
parts=project.strip("/").split("/")
extra=[]
for i in range(1,len(parts)):
    parent="/"+"/".join(parts[:i])
    try: names=os.listdir(parent)
    except OSError: continue
    extra+=[parent+"/"+name for name in sorted(names) if name!=parts[i]]
print(json.dumps(dict(project=os.path.isdir(project),probe=os.path.exists(folder) or os.path.exists(marker),extra=extra[:20]),ensure_ascii=False))
`

func (a *App) checkProjectBoundary(ctx context.Context, sbx sandbox.Sandbox, check Attack, agent probeRunner) AttackResult {
	workspace, err := filepath.EvalSymlinks(sandbox.Workspace())
	if err != nil {
		return classifyCheck(check, nil, errors.New("找不到工作区 ~/CageWorkspace"))
	}
	nonce := probeNonce()
	// A sibling of the project: shared with the VM, but not with this sandbox.
	folder := filepath.Join(workspace, ".aiofcage-probe-"+nonce)
	if strings.ContainsAny(folder, ",\"\n") {
		return classifyCheck(check, nil, errors.New("工作区路径含有逗号或引号，无法挂给对照容器"))
	}
	if err := os.Mkdir(folder, 0o755); err != nil {
		return classifyCheck(check, nil, err)
	}
	defer os.RemoveAll(folder)
	marker := filepath.Join(folder, "marker.txt")
	if err := os.WriteFile(marker, []byte(nonce), 0o644); err != nil {
		return classifyCheck(check, nil, err)
	}
	// Positive sample: the VM sees the folder (the share may take a moment to show a new one).
	vmSees := false
	var controlErr error
	for attempt := 0; attempt < 3 && !vmSees; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		var out []byte
		out, controlErr = a.manager.RunProbe(ctx, sbx.ID, runtime.Probe{Network: runtime.ProbeNone,
			Mounts: []string{"type=bind,src=" + folder + ",dst=/probe,readonly"}, Command: []string{"cat", "/probe/marker.txt"}})
		vmSees = controlErr == nil && strings.TrimSpace(string(out)) == nonce
	}
	out, err := agent(ctx, boundaryPython, sbx.ProjectPath, folder, marker)
	return judgeBoundary(check, vmSees, controlErr, out, err)
}

func judgeBoundary(check Attack, vmSees bool, controlErr error, out []byte, err error) AttackResult {
	r := AttackResult{Attack: check, Status: checkInconclusive,
		Note: "宿主在 ~/CageWorkspace 下临时建一个与项目并列的测试目录，检查后删除；同时列出项目各级上级目录里能看到的条目。只覆盖共享目录内的边界。"}
	control := CheckControl{Name: "虚拟机里能看到测试目录", OK: vmSees}
	if controlErr != nil && !vmSees {
		control.Detail = controlErr.Error()
	}
	r.Controls = []CheckControl{control}
	if err != nil {
		r.Evidence = "检查命令执行失败：" + err.Error()
		return r
	}
	var value struct {
		Project bool     `json:"project"`
		Probe   bool     `json:"probe"`
		Extra   []string `json:"extra"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &value) != nil {
		r.Evidence = "未取得完整检查结果"
		return r
	}
	yes := map[bool]string{true: "是", false: "否"}
	r.Evidence = "Agent 能看到项目目录：" + yes[value.Project] + "；能看到项目旁的测试目录：" + yes[value.Probe] + "；上级目录里的其他条目："
	if len(value.Extra) == 0 {
		r.Evidence += "无"
	} else {
		r.Evidence += strings.Join(value.Extra, "，")
	}
	switch {
	case value.Probe || len(value.Extra) > 0:
		r.Status = checkProblem
	case value.Project:
		r.Status = checkPassed
	}
	return r
}
