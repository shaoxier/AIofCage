package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ComponentHealth struct {
	Name        string          `json:"name"`
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	State       string          `json:"state"`
	Running     bool            `json:"running"`
	Health      string          `json:"health"`
	ExitCode    int             `json:"exit_code"`
	OOMKilled   bool            `json:"oom_killed"`
	Restarts    int             `json:"restarts"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  time.Time       `json:"finished_at"`
	MemoryLimit uint64          `json:"memory_limit"`
	CPULimit    float64         `json:"cpu_limit"`
	PIDsLimit   int64           `json:"pids_limit"`
	Stats       *ContainerStats `json:"stats,omitempty"`
	Error       string          `json:"error,omitempty"`
}
type SandboxHealth struct {
	ID          string            `json:"id"`
	State       string            `json:"state"`
	CollectedAt time.Time         `json:"collected_at"`
	DurationMS  int64             `json:"duration_ms"`
	Components  []ComponentHealth `json:"components"`
	Stats       ContainerStats    `json:"stats"`
	Error       string            `json:"error,omitempty"`
	GateAPI     bool              `json:"gate_api"`
	GateError   string            `json:"gate_error,omitempty"`
}

type inspectedContainer struct {
	ID           string
	Name         string
	RestartCount int
	State        struct {
		Status     string
		Running    bool
		ExitCode   int
		OOMKilled  bool
		StartedAt  time.Time
		FinishedAt time.Time
		Error      string
		Health     *struct{ Status string }
	}
	HostConfig struct {
		Memory    uint64
		NanoCPUs  int64
		CPUQuota  int64
		CPUPeriod int64
		PidsLimit *int64
	}
}

func componentFromInspect(row inspectedContainer) ComponentHealth {
	c := ComponentHealth{Name: strings.TrimPrefix(row.Name, "/"), ID: row.ID, Running: row.State.Running, State: row.State.Status, Health: "未配置健康检查", ExitCode: row.State.ExitCode, OOMKilled: row.State.OOMKilled, Restarts: row.RestartCount, StartedAt: row.State.StartedAt, FinishedAt: row.State.FinishedAt, Error: row.State.Error, MemoryLimit: row.HostConfig.Memory}
	if row.State.Health != nil {
		c.Health = row.State.Health.Status
	}
	c.CPULimit = float64(row.HostConfig.NanoCPUs) / 1e9
	if c.CPULimit == 0 && row.HostConfig.CPUQuota > 0 && row.HostConfig.CPUPeriod > 0 {
		c.CPULimit = float64(row.HostConfig.CPUQuota) / float64(row.HostConfig.CPUPeriod)
	}
	if row.HostConfig.PidsLimit != nil {
		c.PIDsLimit = *row.HostConfig.PidsLimit
	}
	switch {
	case strings.HasSuffix(c.Name, "-agent"):
		c.Kind = "agent"
	case strings.HasSuffix(c.Name, "-gate"):
		c.Kind = "gate"
	default:
		c.Kind = "mcp"
	}
	return c
}

func healthState(components []ComponentHealth) string {
	var agent, gate ComponentHealth
	for _, c := range components {
		if c.Kind == "agent" {
			agent = c
		}
		if c.Kind == "gate" {
			gate = c
		}
	}
	if agent.State == "unknown" || gate.State == "unknown" {
		return "unknown"
	}
	if agent.Running && gate.Running {
		for _, c := range components {
			if !c.Running {
				return statePartial
			}
		}
		return stateRunning
	}
	if !agent.Running && !gate.Running {
		if agent.State == "missing" && gate.State == "missing" {
			return stateMissing
		}
		return stateStopped
	}
	return statePartial
}

// SampleHealth 使用一次列表、批量 inspect 和批量 stats，覆盖专用实例中的所有沙箱组件。
func (m *Manager) SampleHealth(ctx context.Context, ids []string) map[string]SandboxHealth {
	started := time.Now()
	result := map[string]SandboxHealth{}
	for _, id := range ids {
		result[id] = SandboxHealth{ID: id, State: "unknown", CollectedAt: started, Components: []ComponentHealth{}, Stats: ContainerStats{CollectedAt: started}}
	}
	if len(ids) == 0 {
		return result
	}
	out, listErr := m.docker.run(ctx, 10*time.Second, nil, "ps", "-a", "--filter", "label="+labelKey, "--format", "{{.Names}}\t{{.Label \""+roleLabel+"\"}}")
	owner := map[string]string{}
	found := map[string]bool{}
	var targets []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, role, _ := strings.Cut(line, "\t")
		// Helper containers of an isolation check come and go within seconds; they are not part of
		// the sandbox, and one vanishing between list and stats would fail the whole sample.
		if name == "" || role == probeRole {
			continue
		}
		for _, id := range ids {
			if strings.HasPrefix(name, "aiofcage-"+id+"-") {
				owner[name] = id
				targets = append(targets, name)
				break
			}
		}
	}
	records := map[string]ComponentHealth{}
	var inspectErr error
	if len(targets) > 0 {
		data, err := m.docker.run(ctx, 15*time.Second, nil, append([]string{"container", "inspect", "--format", "{{json .}}"}, targets...)...)
		inspectErr = err
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		for scanner.Scan() {
			var row inspectedContainer
			if err := json.Unmarshal(scanner.Bytes(), &row); err == nil {
				c := componentFromInspect(row)
				records[c.Name] = c
				found[c.Name] = true
			} else {
				inspectErr = fmt.Errorf("容器状态格式无法识别")
			}
		}
		if scanner.Err() != nil {
			inspectErr = scanner.Err()
		}
	}
	for _, name := range targets {
		if !found[name] {
			records[name] = ComponentHealth{Name: name, Kind: "mcp", State: "unknown", Health: "未采集"}
		}
	}
	for _, id := range ids {
		n := names(id)
		expected := []string{n.agent, n.gate}
		if m.store != nil {
			if sbx, err := m.store.Sandbox(id); err == nil {
				if services, err := m.servicesFor(sbx); err == nil {
					for _, svc := range connectorServices(services) {
						expected = append(expected, connectorName(id, svc.ID))
					}
				}
			}
		}
		for _, name := range expected {
			if !found[name] {
				kind := "mcp"
				if name == n.agent {
					kind = "agent"
				}
				if name == n.gate {
					kind = "gate"
				}
				state := "missing"
				if listErr != nil || inspectErr != nil {
					state = "unknown"
				}
				records[name] = ComponentHealth{Name: name, Kind: kind, State: state, Health: "未采集"}
				owner[name] = id
			}
		}
	}
	var running []string
	for name, c := range records {
		if c.Running {
			running = append(running, name)
		}
	}
	samples, statsErr := m.sampleComponents(ctx, running, started)
	for name, c := range records {
		if c.Running {
			if s, ok := samples[name]; ok {
				c.Stats = &s
			} else {
				message := "未收到组件资源样本"
				if statsErr != nil {
					message = statsErr.Error()
				}
				c.Stats = &ContainerStats{CollectedAt: started, Error: message}
			}
		}
		id := owner[name]
		h := result[id]
		h.Components = append(h.Components, c)
		result[id] = h
	}
	for id, h := range result {
		sort.Slice(h.Components, func(i, j int) bool { return h.Components[i].Name < h.Components[j].Name })
		h.State = healthState(h.Components)
		h.Stats.Valid = true
		runningCount := 0
		for _, c := range h.Components {
			if c.State == "unknown" {
				h.Stats.Valid = false
			}
			if c.Running {
				runningCount++
				if c.Stats == nil || !c.Stats.Valid {
					h.Stats.Valid = false
					h.Stats.Error = "部分组件资源采集失败"
					continue
				}
				s := c.Stats
				h.Stats.CPUPercent += s.CPUPercent
				h.Stats.MemoryBytes += s.MemoryBytes
				h.Stats.MemoryLimit += c.MemoryLimit
				h.Stats.PIDs += s.PIDs
				h.Stats.NetworkIn += s.NetworkIn
				h.Stats.NetworkOut += s.NetworkOut
				h.Stats.BlockRead += s.BlockRead
				h.Stats.BlockWrite += s.BlockWrite
			}
		}
		if listErr != nil {
			h.Error = "无法读取容器列表：" + listErr.Error()
			h.Stats.Valid = false
		}
		if inspectErr != nil {
			h.Error = "部分容器状态读取失败：" + inspectErr.Error()
			h.Stats.Valid = false
		}
		if runningCount == 0 {
			h.Stats.Valid = false
			h.Stats.Error = "沙箱未运行"
		}
		h.DurationMS = time.Since(started).Milliseconds()
		result[id] = h
	}
	return result
}

func (m *Manager) sampleComponents(ctx context.Context, names []string, at time.Time) (map[string]ContainerStats, error) {
	result := map[string]ContainerStats{}
	if len(names) == 0 {
		return result, nil
	}
	out, err := m.docker.run(ctx, 20*time.Second, nil, append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, names...)...)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		var row struct{ Name, CPUPerc, MemUsage, PIDs, NetIO, BlockIO string }
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		s := parseComponentStats(row.Name, row.CPUPerc, row.MemUsage, row.PIDs, row.NetIO, row.BlockIO, at)
		result[row.Name] = s
	}
	if scanner.Err() != nil {
		err = scanner.Err()
	}
	return result, err
}

// 累计 I/O 是当前容器计数，容器重建后从零开始；这里不推算跨重启速率。
func parseComponentStats(name, cpuValue, memValue, pidsValue, netValue, blockValue string, at time.Time) ContainerStats {
	cpu, cpuErr := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(cpuValue), "%"), 64)
	pids, pidErr := strconv.Atoi(pidsValue)
	used, limit, memOK := strings.Cut(memValue, "/")
	netIn, netOut, netOK := strings.Cut(netValue, "/")
	blockRead, blockWrite, blockOK := strings.Cut(blockValue, "/")
	values := []string{used, limit, netIn, netOut, blockRead, blockWrite}
	sizes := make([]uint64, len(values))
	valid := cpuErr == nil && !math.IsNaN(cpu) && !math.IsInf(cpu, 0) && cpu >= 0 && pidErr == nil && pids >= 0 && memOK && netOK && blockOK
	for i, value := range values {
		n, ok := parseSizeChecked(value)
		sizes[i] = n
		valid = valid && ok
	}
	s := ContainerStats{CPUPercent: cpu, MemoryBytes: sizes[0], MemoryLimit: sizes[1], PIDs: pids, NetworkIn: sizes[2], NetworkOut: sizes[3], BlockRead: sizes[4], BlockWrite: sizes[5], CollectedAt: at, Valid: valid}
	if !s.Valid {
		s.CPUPercent = 0
		s.Error = fmt.Sprintf("组件 %s 的资源数据不完整", name)
	}
	return s
}

// GateRunning 不受可选 MCP 组件故障影响，确保仍可读取代理用量。
func (h SandboxHealth) GateRunning() bool {
	for _, c := range h.Components {
		if c.Kind == "gate" {
			return c.Running
		}
	}
	return false
}
