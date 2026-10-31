package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/store"
)

// Environment manages the dedicated Colima VM. It never touches the user's default profile or
// switches the default Docker context.
type Environment struct {
	colima  runner
	limactl string
	docker  runner
	store   *store.Store
}

type VMInfo struct {
	CPUs   int    `json:"cpus"`
	Memory uint64 `json:"memory"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Arch   string `json:"arch"`
	Disk   int64  `json:"disk"`
}

var errDockerContextMissing = errors.New("Docker 连接尚未配置，启动沙箱时会自动创建")

func NewEnvironment(stores ...*store.Store) (*Environment, error) {
	colima, err := findTool("colima")
	if err != nil {
		return nil, err
	}
	limactl, err := findTool("limactl")
	if err != nil {
		return nil, err
	}
	docker, err := findTool("docker")
	if err != nil {
		return nil, err
	}
	e := &Environment{
		colima:  runner{path: colima},
		limactl: limactl,
		docker:  runner{path: docker, args: []string{"--context", DockerContext}},
	}
	if len(stores) > 0 {
		e.store = stores[0]
	}
	return e, nil
}

func (e *Environment) vm(ctx context.Context) (*VMInfo, error) {
	out, err := e.colima.run(ctx, 30*time.Second, nil, "list", "--json")
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		var info VMInfo
		if json.Unmarshal(scanner.Bytes(), &info) == nil && info.Name == ColimaProfile {
			return &info, nil
		}
	}
	return nil, nil
}

// Ensure creates or starts the dedicated VM and then verifies its sharing boundary. detail, if
// set, receives what the VM start is doing.
func (e *Environment) Ensure(ctx context.Context, detail func(string)) error {
	if e.store != nil {
		unlock, err := e.store.LockOperation(ctx, "environment", false)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if goruntime.GOOS != "darwin" || goruntime.GOARCH != "arm64" {
		return errors.New("仅支持 Apple Silicon 的 macOS")
	}
	workspace := sandbox.Workspace()
	if info, err := os.Lstat(workspace); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("~/CageWorkspace 不能是符号链接")
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return fmt.Errorf("无法创建工作区：%w", err)
	}
	if err := e.startVM(ctx, workspace, detail); err != nil {
		return err
	}
	return e.verify(ctx, true)
}

// startVM creates or starts the VM when it is not running. Only one start runs at a time, across
// processes, so two sandboxes starting together do not create the VM twice. A start, once begun,
// is not interrupted: stopping colima while it downloads or boots the VM can leave it broken, so a
// stop waits for the start to finish and takes effect after it.
func (e *Environment) startVM(ctx context.Context, workspace string, detail func(string)) error {
	if e.store != nil {
		unlock, err := e.store.LockOperation(ctx, "vm-start", true)
		if err != nil {
			return err
		}
		defer unlock()
	}
	report := func(line string) {
		if detail != nil {
			detail(line)
		}
	}
	// The lock dies with the process that held it, while colima may still be starting the VM for a
	// process that has quit: wait for that start rather than begin another one.
	if err := e.awaitEarlierStart(ctx, report); err != nil {
		return err
	}
	vm, err := e.vm(ctx)
	if err != nil {
		return err
	}
	resources, err := e.resources(vm)
	if err != nil {
		return err
	}
	var args []string
	timeout := 10 * time.Minute
	switch {
	case vm == nil:
		// Same flags as the validation package: no user template, no SSH config or agent
		// forwarding, no context switch, only the workspace shared.
		args = []string{"start", ColimaProfile,
			"--arch", "aarch64", "--vm-type", "vz", "--mount-type", "virtiofs",
			"--cpu", strconv.Itoa(resources.CPUs), "--memory", strconv.Itoa(resources.MemoryGiB), "--disk", strconv.Itoa(resources.DiskGiB), "--mount", workspace + ":w",
			"--activate=false", "--ssh-config=false", "--ssh-agent=false", "--template=false",
			"--kubernetes=false", "--runtime", "docker", "--binfmt=false"}
		timeout = 20 * time.Minute
	case vm.Status != "Running":
		args = []string{"start", ColimaProfile, "--activate=false", "--ssh-config=false",
			"--cpu", strconv.Itoa(resources.CPUs), "--memory", strconv.Itoa(resources.MemoryGiB), "--disk", strconv.Itoa(resources.DiskGiB)}
	default:
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err // stopped before colima started: nothing to wait for
	}
	report("正在启动专用虚拟机")
	done := make(chan error, 1)
	go func() {
		// Output goes to a file: should the app quit meanwhile, colima finishes the start instead
		// of dying of SIGPIPE half-way. Its pid lets a later start wait for it.
		recorded := func(pid int) { _ = os.WriteFile(e.startFile("vm-start.pid"), []byte(strconv.Itoa(pid)), 0o600) }
		done <- e.colima.runLogged(context.WithoutCancel(ctx), timeout, e.startFile("vm-start.log"), recorded, func(line string) { report(vmLine(line)) }, args...)
		_ = os.Remove(e.startFile("vm-start.pid"))
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			report("操作超时，等虚拟机启动完成后再结束（中途打断可能损坏虚拟机）")
		} else {
			report("已收到停止请求，等虚拟机启动完成后再停止（中途打断可能损坏虚拟机）")
		}
		if err = <-done; err == nil {
			err = ctx.Err()
		}
	}
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("启动专用虚拟机失败：%w", err)
	}
	return err
}

// startFile names a file about VM starts (its log, the pid of a running start) in the app's data
// directory, which every process using the dedicated VM shares.
func (e *Environment) startFile(name string) string {
	dir := os.TempDir()
	if e.store != nil {
		dir = e.store.Dir()
	}
	return filepath.Join(dir, name)
}

// awaitEarlierStart waits while a colima start recorded in vm-start.pid still runs. Only a live
// process whose command line is a colima start of the dedicated VM counts; it is never signalled.
func (e *Environment) awaitEarlierStart(ctx context.Context, report func(string)) error {
	raw, err := os.ReadFile(e.startFile("vm-start.pid"))
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return nil
	}
	starting := func() bool {
		out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		command := string(out)
		return err == nil && strings.Contains(command, "colima") && strings.Contains(command, " start ") && strings.Contains(command, ColimaProfile)
	}
	if !starting() {
		return nil
	}
	report("上一次启动的虚拟机还在启动中，等它完成")
	follow := &logFollower{path: e.startFile("vm-start.log"), onLine: func(line string) { report(vmLine(line)) }}
	if info, err := os.Stat(follow.path); err == nil {
		follow.offset = info.Size()
	}
	for starting() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
			follow.read(false)
		}
	}
	return nil
}

var (
	colimaMessage   = regexp.MustCompile(`msg="((?:[^"\\]|\\.)*)"`)
	limaRequirement = regexp.MustCompile(`essential requirement (\d+) of (\d+)`)
)

// vmLine turns a line of colima output into a short description of what the VM start is doing.
func vmLine(line string) string {
	message := line
	if match := colimaMessage.FindStringSubmatch(line); match != nil {
		message = strings.ReplaceAll(match[1], `\"`, `"`)
	}
	switch lower := strings.ToLower(message); {
	case strings.Contains(lower, "downloading"):
		return "正在下载虚拟机系统镜像（首次约 300 MB，网络慢时要等较久）"
	case limaRequirement.MatchString(message):
		match := limaRequirement.FindStringSubmatch(message)
		return fmt.Sprintf("等待虚拟机就绪（%s/%s）", match[1], match[2])
	case strings.Contains(lower, "creating and starting"):
		return "正在创建并启动虚拟机"
	case strings.Contains(lower, "provisioning"):
		return "正在配置虚拟机里的 Docker"
	case lower == "done":
		return "虚拟机已启动"
	}
	return clip(message, 160)
}

// Verify checks the actual VM configuration and mounts rather than trusting our own flags.
func (e *Environment) Verify(ctx context.Context) error {
	return e.verify(ctx, false)
}

func (e *Environment) verify(ctx context.Context, createContext bool) error {
	vm, err := e.vm(ctx)
	if err != nil {
		return err
	}
	if vm == nil || vm.Status != "Running" {
		return errors.New("专用虚拟机 aiofcage 未运行")
	}
	if vm.Arch != "aarch64" && vm.Arch != "arm64" {
		return errors.New("专用虚拟机必须是 arm64")
	}
	workspace, err := filepath.EvalSymlinks(sandbox.Workspace())
	if err != nil {
		return errors.New("工作区不存在")
	}
	home, _ := os.UserHomeDir()
	cmd := exec.CommandContext(ctx, e.limactl, "list", "--all-fields", "--json", "colima-"+ColimaProfile)
	cmd.Env = append(childEnv(), "LIMA_HOME="+filepath.Join(home, ".colima", "_lima"))
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // stopped, not broken
		}
		return errors.New("无法读取虚拟机配置")
	}
	var lima struct {
		Config struct {
			VMType    string `json:"vmType"`
			MountType string `json:"mountType"`
			Mounts    []struct {
				Location string `json:"location"`
				Writable bool   `json:"writable"`
			} `json:"mounts"`
			SSH struct {
				ForwardAgent bool `json:"forwardAgent"`
			} `json:"ssh"`
		} `json:"config"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &lima) != nil {
		return errors.New("虚拟机配置格式无法识别，拒绝继续")
	}
	mounts := lima.Config.Mounts
	if len(mounts) != 1 || !mounts[0].Writable || expand(mounts[0].Location, home) != workspace {
		return errors.New("虚拟机共享目录不是唯一的 ~/CageWorkspace，拒绝继续")
	}
	if lima.Config.VMType != "vz" || lima.Config.MountType != "virtiofs" || lima.Config.SSH.ForwardAgent {
		return errors.New("虚拟机配置不符合 vz + virtiofs、且不转发 SSH agent 的要求")
	}
	procMounts, err := e.colima.run(ctx, 30*time.Second, nil, "ssh", "--profile", ColimaProfile, "--", "cat", "/proc/mounts")
	if err != nil {
		return err
	}
	var shared []string
	for _, line := range strings.Split(string(procMounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && (fields[2] == "virtiofs" || fields[2] == "9p" || fields[2] == "fuse.sshfs") {
			shared = append(shared, strings.ReplaceAll(fields[1], `\040`, " "))
		}
	}
	if len(shared) != 1 || shared[0] != workspace {
		return errors.New("虚拟机中实际挂载的共享目录与白名单不一致")
	}
	return e.verifyDockerContext(ctx, home, createContext)
}

// Only Ensure may register a missing connection, after the VM sharing boundary is verified.
// Never switch the user's active context or overwrite an existing connection.
func (e *Environment) verifyDockerContext(ctx context.Context, home string, create bool) error {
	// Docker context ls synthesizes the selected name even if its metadata is missing.
	// Use the built-in default for metadata commands; the target is always explicit.
	docker := runner{path: e.docker.path, args: []string{"--context", "default"}}
	expected := "unix://" + filepath.Join(home, ".colima", ColimaProfile, "docker.sock")
	out, err := docker.run(ctx, 15*time.Second, nil, "context", "inspect", DockerContext)
	if err != nil {
		names, listErr := docker.run(ctx, 15*time.Second, nil, "context", "ls", "--format", "{{.Name}}")
		if listErr != nil {
			return fmt.Errorf("无法读取 Docker 连接列表：%w", listErr)
		}
		for _, name := range strings.Fields(string(names)) {
			if name == DockerContext {
				return fmt.Errorf("无法读取 Docker context %s：%w", DockerContext, err)
			}
		}
		if !create {
			return errDockerContextMissing
		}
		_, createErr := docker.run(ctx, 15*time.Second, nil, "context", "create", DockerContext,
			"--docker", "host="+expected)
		// Re-read even after a creation error: another process may have created it.
		out, err = docker.run(ctx, 15*time.Second, nil, "context", "inspect", DockerContext)
		if err != nil {
			if createErr != nil {
				return fmt.Errorf("创建 Docker context %s 失败：%w", DockerContext, createErr)
			}
			return fmt.Errorf("无法验证 Docker context %s：%w", DockerContext, err)
		}
	}
	var contexts []struct {
		Endpoints struct {
			Docker struct {
				Host string `json:"Host"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(out, &contexts) != nil || len(contexts) != 1 || contexts[0].Endpoints.Docker.Host != expected {
		return errors.New("同名 Docker context 没有指向专用虚拟机")
	}
	return nil
}

func expand(location, home string) string {
	if strings.HasPrefix(location, "~/") {
		location = filepath.Join(home, location[2:])
	}
	resolved, err := filepath.EvalSymlinks(location)
	if err != nil {
		return location
	}
	return resolved
}

// Inspect reports readiness without changing anything.
func (e *Environment) Inspect(ctx context.Context) Report {
	checks := []Check{{ID: "platform", Name: "macOS Apple Silicon", Status: "detected", Detail: goruntime.GOOS + "/" + goruntime.GOARCH}}
	if goruntime.GOOS != "darwin" || goruntime.GOARCH != "arm64" {
		checks[0].Status = "unsupported"
	}
	vm, err := e.vm(ctx)
	switch {
	case err != nil:
		checks = append(checks, Check{ID: "vm", Name: "专用虚拟机", Status: "error", Detail: err.Error()})
	case vm == nil:
		checks = append(checks, Check{ID: "vm", Name: "专用虚拟机", Status: "missing", Detail: "尚未创建，首次启动沙箱时自动创建"})
	case vm.Status != "Running":
		checks = append(checks, Check{ID: "vm", Name: "专用虚拟机", Status: "stopped", Detail: "已创建，启动沙箱时自动启动"})
	default:
		status, detail := "ready", "运行中，只共享 ~/CageWorkspace"
		verifyErr := e.Verify(ctx)
		if verifyErr != nil && !errors.Is(verifyErr, errDockerContextMissing) {
			status, detail = "error", verifyErr.Error()
		}
		checks = append(checks, Check{ID: "vm", Name: "专用虚拟机", Status: status, Detail: detail})
		if errors.Is(verifyErr, errDockerContextMissing) {
			checks = append(checks, Check{ID: "docker_context", Name: "Docker 连接", Status: "missing", Detail: verifyErr.Error()})
		}
	}
	ready := true
	for _, check := range checks {
		if check.Status != "ready" && check.Status != "detected" {
			ready = false
		}
	}
	return Report{Ready: ready, Checks: checks, VM: vm}
}
