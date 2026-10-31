package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthUsesRealLimitsAndNoInventedCheck(t *testing.T) {
	var row inspectedContainer
	if err := json.Unmarshal([]byte(`{"ID":"id","Name":"/aiofcage-sbx-a-mcp-db","RestartCount":2,"State":{"Status":"exited","Running":false,"ExitCode":143,"OOMKilled":false},"HostConfig":{"Memory":536870912,"NanoCPUs":1000000000,"PidsLimit":128}}`), &row); err != nil {
		t.Fatal(err)
	}
	c := componentFromInspect(row)
	if c.Kind != "mcp" || c.MemoryLimit != 512<<20 || c.CPULimit != 1 || c.Health != "未配置健康检查" || c.OOMKilled || c.ExitCode != 143 {
		t.Fatalf("%+v", c)
	}
	if state := healthState([]ComponentHealth{{Kind: "agent", Running: true}, {Kind: "gate", Running: true}, {Kind: "mcp", Running: false}}); state != statePartial {
		t.Fatal("停止的 MCP 被忽略")
	}
	if state := healthState([]ComponentHealth{{Kind: "agent", State: "unknown"}, {Kind: "gate", State: "missing"}}); state != "unknown" {
		t.Fatal("采集失败被当作停止")
	}
}

func TestHealthSamplingFailuresAreNotZeroOrStopped(t *testing.T) {
	tool := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
case "$1" in
 ps) printf '%s\n' aiofcage-sbx-live-agent aiofcage-sbx-live-gate aiofcage-sbx-off-agent aiofcage-sbx-off-gate ;;
 container) printf '%s\n' '{"Name":"/aiofcage-sbx-live-agent","State":{"Status":"running","Running":true}}' '{"Name":"/aiofcage-sbx-live-gate","State":{"Status":"running","Running":true}}' '{"Name":"/aiofcage-sbx-off-agent","State":{"Status":"exited"}}' '{"Name":"/aiofcage-sbx-off-gate","State":{"Status":"exited"}}' ;;
 stats) printf '%s\n' '{"Name":"aiofcage-sbx-live-agent","CPUPerc":"1.5%","MemUsage":"10MiB / 6GiB","PIDs":"3","NetIO":"2kB / 1kB","BlockIO":"0B / 0B"}' ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{docker: runner{path: tool}}
	h := m.SampleHealth(context.Background(), []string{"sbx-live", "sbx-off"})
	if h["sbx-off"].Stats.Valid || h["sbx-off"].State != stateStopped {
		t.Fatal("停止沙箱被其他沙箱的运行状态污染", h["sbx-off"])
	}
	if h["sbx-live"].Stats.Valid || h["sbx-live"].Stats.Error == "" {
		t.Fatal("部分资源采集失败却显示有效", h["sbx-live"])
	}
	m.docker.path = "/usr/bin/false"
	h = m.SampleHealth(context.Background(), []string{"sbx-live"})
	if h["sbx-live"].State != "unknown" || h["sbx-live"].Error == "" || h["sbx-live"].Stats.Valid {
		t.Fatal("命令失败被误报停止", h)
	}
}

// An isolation check starts and removes helper containers within seconds. They are not part of the
// sandbox: a probe that is still being created must not make the sandbox look broken, and one that
// vanished between list and stats must not fail the whole sample.
func TestHealthIgnoresIsolationProbes(t *testing.T) {
	dir := t.TempDir()
	tool, log := filepath.Join(dir, "docker"), filepath.Join(dir, "commands")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + log + `'
case "$1" in
 ps) printf 'aiofcage-sbx-live-agent\t\naiofcage-sbx-live-gate\t\naiofcage-sbx-live-probe-0123456789\tprobe\n' ;;
 container) printf '%s\n' '{"Name":"/aiofcage-sbx-live-agent","State":{"Status":"running","Running":true}}' '{"Name":"/aiofcage-sbx-live-gate","State":{"Status":"running","Running":true}}' ;;
 stats) printf '%s\n' '{"Name":"aiofcage-sbx-live-agent","CPUPerc":"1%","MemUsage":"10MiB / 6GiB","PIDs":"3","NetIO":"0B / 0B","BlockIO":"0B / 0B"}' '{"Name":"aiofcage-sbx-live-gate","CPUPerc":"1%","MemUsage":"10MiB / 6GiB","PIDs":"3","NetIO":"0B / 0B","BlockIO":"0B / 0B"}' ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{docker: runner{path: tool}}
	h := m.SampleHealth(context.Background(), []string{"sbx-live"})["sbx-live"]
	if h.State != stateRunning || len(h.Components) != 2 || !h.Stats.Valid || h.Error != "" {
		t.Fatalf("a probe changed the sandbox's health: %+v", h)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "probe-0123456789") {
		t.Fatalf("probes must not be inspected or sampled:\n%s", data)
	}
}

func TestStatsMalformedAndContainerCounterReset(t *testing.T) {
	at := time.Now()
	for _, bad := range []struct{ cpu, mem, pids, net string }{
		{"NaN%", "1MiB / 2GiB", "2", "0B / 0B"},
		{"Inf%", "1MiB / 2GiB", "2", "0B / 0B"},
		{"1%", "unknown / 2GiB", "2", "0B / 0B"},
		{"1%", "1MiB / 2GiB", "-2", "0B / 0B"},
		{"1%", "1MiB / 2GiB", "2", "-1kB / 0B"},
	} {
		if s := parseComponentStats("x", bad.cpu, bad.mem, bad.pids, bad.net, "0B / 0B", at); s.Valid || s.Error == "" {
			t.Fatal("异常样本被当成零", bad, s)
		}
	}
	before := parseComponentStats("old", "1%", "1MiB / 2GiB", "2", "2MB / 3MB", "0B / 0B", at)
	after := parseComponentStats("new", "0%", "0B / 2GiB", "1", "0B / 0B", "0B / 0B", at.Add(time.Second))
	if !before.Valid || !after.Valid || after.NetworkIn != 0 || after.NetworkOut != 0 {
		t.Fatal("容器重建计数应原样保留，不制造负速率")
	}
}

func TestDedicatedHealthSnapshot(t *testing.T) {
	ids := strings.Fields(os.Getenv("AIOFCAGE_HEALTH_IDS"))
	if len(ids) == 0 {
		t.Skip("设置 AIOFCAGE_HEALTH_IDS 后只读检查专用 Docker context")
	}
	path, err := findTool("docker")
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{docker: runner{path: path, args: []string{"--context", DockerContext}}}
	for id, h := range m.SampleHealth(context.Background(), ids) {
		if h.Error != "" {
			t.Fatal(h.Error)
		}
		t.Logf("%s state=%s components=%d sample=%dms valid=%v memory=%d", id, h.State, len(h.Components), h.DurationMS, h.Stats.Valid, h.Stats.MemoryBytes)
		for _, c := range h.Components {
			t.Logf("  %s %s memory_limit=%d cpu_limit=%.1f health=%s", c.Kind, c.State, c.MemoryLimit, c.CPULimit, c.Health)
		}
	}
}
