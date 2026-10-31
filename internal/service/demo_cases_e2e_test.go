package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/democases"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
)

// 显式运行：一次性沙箱中验证协议、工具边界与关闭；可另启用真实 DeepSeek 生成报告。
func TestDemoCasesEndToEnd(t *testing.T) {
	if os.Getenv("AIOFCAGE_DEMO_E2E") != "1" {
		t.Skip("设置 AIOFCAGE_DEMO_E2E=1 验证真实案例 MCP")
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := vault.NewKeychain()
	m, err := runtime.NewManager(st, keys)
	if err != nil {
		t.Fatal(err)
	}
	a := NewApp(st, keys, m, nil, nil)
	m.Progress = func(_ string, update runtime.Update) { t.Log(update.Step, update.Detail) }
	providerID := ""
	if os.Getenv("AIOFCAGE_DEMO_AGENT_E2E") == "1" {
		dir, _ := store.DefaultDir()
		source, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		providers, err := source.Providers()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range providers {
			if p.Agent == session.DSH {
				if err := st.SaveProvider(p); err != nil {
					t.Fatal(err)
				}
				providerID = p.ID
				break
			}
		}
		if providerID == "" {
			t.Fatal("没有 DeepSeek 供应商")
		}
	}
	for _, c := range democases.List() {
		t.Run(c.ID, func(t *testing.T) {
			result, err := a.CreateDemoSandbox(CreateDemoRequest{CaseID: c.ID, Agent: session.DSH, ProviderID: providerID})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				a.CloseAllTerminals()
				if err := a.DeleteSandbox(result.ID, false); err != nil {
					t.Error(err)
				}
				_ = os.RemoveAll(result.ProjectPath)
			})
			s, _ := st.Sandbox(result.ID)
			if s.DemoCase != c.ID || s.ProviderFor(session.DSH) != providerID || m.Status(context.Background(), s.ID).State != "not_created" {
				t.Fatal("案例未按选择创建为新沙箱")
			}
			if data, err := os.ReadFile(filepath.Join(s.ProjectPath, "TASK.md")); err != nil || !strings.Contains(string(data), c.Task) {
				t.Fatal("案例任务未写入")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			if err := m.Start(ctx, s.ID); err != nil {
				t.Fatal(err)
			}
			id := s.MCP[0]
			script := `import json, os, urllib.request, urllib.error, sys
u='http://gatekeeper:8890/mcp/'+sys.argv[1]
headers={'Content-Type':'application/json','Accept':'application/json, text/event-stream','Authorization':'Bearer '+os.environ[sys.argv[2]]}
opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
def call(method,params=None,rid=1,verb='POST'):
 data=json.dumps({'jsonrpc':'2.0','id':rid,'method':method,'params':params or {}}).encode() if verb=='POST' else None
 r=opener.open(urllib.request.Request(u,data=data,headers=headers,method=verb),timeout=20)
 if r.headers.get('Mcp-Session-Id'): headers['Mcp-Session-Id']=r.headers['Mcp-Session-Id']
 raw=r.read()
 return json.loads(raw) if raw else {}
assert 'serverInfo' in call('initialize',{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'review','version':'1'}})['result']
assert len(call('tools/list')['result']['tools'])==3
overview=call('tools/call',{'name':'case_overview','arguments':{}})['result']['structuredContent']
assert overview['synthetic'] is True
for dataset in overview['datasets']:
 result=call('tools/call',{'name':'read_data','arguments':{'dataset':dataset}})['result']['structuredContent']
 assert result['matched']>0
if sys.argv[1].endswith('sales'):
 result=call('tools/call',{'name':'read_data','arguments':{'dataset':'orders','contains':'2026-10-20'}})['result']['structuredContent']
 assert result['matched']==10 and all(x['order_date']=='2026-10-20' for x in result['records'])
 result=call('tools/call',{'name':'summarize_data','arguments':{'dataset':'orders','group_by':'channel'}})['result']['structuredContent']
 campaign=next(x for x in result['groups'] if x['key']=='campaign')
 assert campaign['rows']==4 and campaign['sums']['paid_cny']==3000 and campaign['sums']['refund_cny']==2000
denied=call('tools/call',{'name':'delete_data','arguments':{}})
assert denied['error']['code']==-32001 and 'result' not in denied
assert call('',verb='DELETE')=={}
print('MCP initialize/list/read/metrics/deny/close passed')`
			var out bytes.Buffer
			if err := m.Exec(ctx, s.ID, false, nil, &out, &out, "python3", "-c", script, id, mcp.EnvVar(id)); err != nil {
				t.Fatalf("MCP协议失败: %v %s", err, out.String())
			}
			t.Log(strings.TrimSpace(out.String()))
			check, err := a.TestMCPService(id)
			if err != nil || !check.OK {
				t.Fatalf("连接测试失败: %v %+v", err, check)
			}
			if c.ID == "sales" && providerID != "" {
				runDemoAgent(t, a, m, s.ID, s.ProjectPath, c.Task)
			}
		})
	}
}

func runDemoAgent(t *testing.T, a *App, m *runtime.Manager, id, path, task string) {
	t.Helper()
	events := make(chan map[string]string, 512)
	a.SetEmitter(func(name string, value any) {
		if name == "conversation:event" {
			data, _ := json.Marshal(value)
			var event map[string]string
			if json.Unmarshal(data, &event) == nil {
				events <- event
			}
		}
	})
	info, err := a.StartConversation("t-demoreview", id, session.DSH)
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseConversation(info.ID)
	if err := a.SendConversationMessage(info.ID, task, nil, ConversationOptions{Model: info.Model}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	mcpCalls := 0
	problem := ""
loop:
	for {
		select {
		case event := <-events:
			switch event["type"] {
			case "tool":
				if strings.Contains(event["text"], "mcp__") {
					mcpCalls++
				}
			case "error":
				problem = event["text"]
			case "done", "stopped":
				break loop
			}
		case <-deadline.C:
			_ = a.StopConversation(info.ID)
			t.Fatal("真实 DeepSeek 案例超时")
		}
	}
	if problem != "" {
		t.Fatal(problem)
	}
	data, err := os.ReadFile(filepath.Join(path, "report.md"))
	if err != nil || !strings.Contains(string(data), "O-10") || !strings.Contains(string(data), "75") || !strings.Contains(string(data), "合成") || mcpCalls < 2 {
		t.Fatalf("报告或真实 MCP 调用不足: file=%v calls=%d", err, mcpCalls)
	}
	t.Logf("真实 DeepSeek 报告已生成并核对退款指标、记录ID和合成数据声明，MCP工具调用%d次", mcpCalls)
}
