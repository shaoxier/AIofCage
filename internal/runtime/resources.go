package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	gort "runtime"
	"strconv"
	"strings"
	"time"
)

const resourcesKey = "environment_resources"

type ResourceConfig struct {
	CPUs      int `json:"cpus"`
	MemoryGiB int `json:"memory_gib"`
	DiskGiB   int `json:"disk_gib"`
}

type ResourceSettings struct {
	Desired       ResourceConfig `json:"desired"`
	Actual        *VMInfo        `json:"actual,omitempty"`
	HostCPUs      int            `json:"host_cpus"`
	HostMemoryGiB int            `json:"host_memory_gib"`
	Error         string         `json:"error"`
	Pending       bool           `json:"pending"`
}

func hostMemoryGiB() int {
	if gort.GOOS != "darwin" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	return int(n >> 30)
}

func resourcesFromVM(vm *VMInfo) ResourceConfig {
	if vm == nil {
		return ResourceConfig{CPUs: 4, MemoryGiB: 6, DiskGiB: 40}
	}
	return ResourceConfig{CPUs: vm.CPUs, MemoryGiB: int(vm.Memory >> 30), DiskGiB: int(vm.Disk >> 30)}
}

func validateResources(c ResourceConfig, cpus, memory int, vm *VMInfo) error {
	if c.CPUs < 1 || c.CPUs > cpus {
		return fmt.Errorf("CPU 数量须为 1 至 %d", cpus)
	}
	maxMemory := 128
	if memory > 0 {
		maxMemory = min(maxMemory, memory-2)
	}
	if c.MemoryGiB < 2 || c.MemoryGiB > maxMemory {
		return fmt.Errorf("内存须为 2 至 %d GiB，给 macOS 预留至少 2 GiB", maxMemory)
	}
	if c.DiskGiB < 20 || c.DiskGiB > 1024 {
		return errors.New("磁盘容量须为 20 至 1024 GiB")
	}
	if vm != nil && uint64(c.DiskGiB)<<30 < uint64(vm.Disk) {
		return errors.New("已有虚拟磁盘只能扩大，不能缩小")
	}
	return nil
}

func (e *Environment) resources(vm *VMInfo) (ResourceConfig, error) {
	c := resourcesFromVM(vm)
	if e.store != nil {
		if raw := e.store.Setting(resourcesKey); raw != "" {
			if json.Unmarshal([]byte(raw), &c) != nil {
				return c, errors.New("运行环境资源配置损坏")
			}
			if err := validateResources(c, gort.NumCPU(), hostMemoryGiB(), vm); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}

func (e *Environment) ResourceSettings(ctx context.Context) ResourceSettings {
	result := ResourceSettings{HostCPUs: gort.NumCPU(), HostMemoryGiB: hostMemoryGiB()}
	vm, err := e.vm(ctx)
	if err != nil {
		result.Error = "无法读取运行环境：" + err.Error()
	}
	result.Actual = vm
	result.Desired, err = e.resources(vm)
	if err != nil {
		result.Error = err.Error()
	}
	result.Pending = vm != nil && result.Desired != resourcesFromVM(vm)
	return result
}

func (e *Environment) SaveResources(ctx context.Context, c ResourceConfig) error {
	if e.store == nil {
		return errors.New("运行环境存储不可用")
	}
	vm, err := e.vm(ctx)
	if err != nil {
		return err
	}
	if err := validateResources(c, gort.NumCPU(), hostMemoryGiB(), vm); err != nil {
		return err
	}
	data, _ := json.Marshal(c)
	return e.store.SetSetting(resourcesKey, string(data))
}

// SuspendEnvironment 仅停止没有任何运行容器的专用虚拟机；不会替用户结束任务或其他容器。
func (m *Manager) SuspendEnvironment(ctx context.Context) error {
	m.environmentMu.Lock()
	defer m.environmentMu.Unlock()
	unlockEnvironment, err := m.useEnvironment(ctx, true)
	if err != nil {
		return err
	}
	defer unlockEnvironment()
	vm, err := m.env.vm(ctx)
	if err != nil {
		return err
	}
	if vm == nil || vm.Status != "Running" {
		return nil
	}
	out, err := m.docker.run(ctx, 15*time.Second, nil, "ps", "--quiet")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return errors.New("专用虚拟机中仍有运行容器，请先停止所有沙箱及其他容器")
	}
	if err := m.env.Verify(ctx); err != nil {
		return err
	}
	// Like a start, the stop writes to a file, so quitting the app does not cut it short.
	return m.env.colima.runLogged(ctx, 2*time.Minute, m.env.startFile("vm-stop.log"), nil, nil, "stop", ColimaProfile)
}
