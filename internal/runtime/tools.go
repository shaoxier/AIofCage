package runtime

import (
	"bufio"
	"bytes"
	"context"
	"debug/macho"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
	"unicode/utf8"
)

var errToolArchitecture = errors.New("需要 ARM 原生工具")

// Finder does not inherit shell startup files. Include user-installed tools explicitly.
func toolSearchDirs() []string {
	dirs := []string{"/opt/homebrew/bin"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	dirs = append(dirs, "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	return append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
}

func findTool(name string) (string, error) {
	return findToolIn(name, toolSearchDirs(), goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64")
}

func findToolIn(name string, dirs []string, nativeARM bool) (string, error) {
	var incompatible error
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			if nativeARM && (name == "colima" || name == "limactl") && intelOnly(candidate) {
				incompatible = fmt.Errorf("%w：%s 是 Intel 版本，请使用 Apple Silicon 版 Homebrew（/opt/homebrew/bin/brew）安装", errToolArchitecture, name)
				continue
			}
			return candidate, nil
		}
	}
	if incompatible != nil {
		return "", incompatible
	}
	return "", fmt.Errorf("未找到 %s，请先安装", name)
}

// Lima refuses Rosetta even for status checks. Accept ARM and universal binaries;
// scripts remain supported for installations that use executable wrappers.
func intelOnly(path string) bool {
	if file, err := macho.Open(path); err == nil {
		defer file.Close()
		return file.Cpu == macho.CpuAmd64 || file.Cpu == macho.Cpu386
	}
	if file, err := macho.OpenFat(path); err == nil {
		defer file.Close()
		for _, arch := range file.Arches {
			if arch.Cpu == macho.CpuArm64 {
				return false
			}
		}
		return true
	}
	return false
}

// childEnv removes proxy and Docker selection variables so every command talks to the dedicated
// instance directly and nothing from the user's shell leaks into the VM.
func childEnv() []string {
	return childEnvFor(os.Environ(), toolSearchDirs(), goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64")
}

func childEnvFor(source, dirs []string, nativeARM bool) []string {
	var env []string
	for _, item := range source {
		key, _, _ := strings.Cut(item, "=")
		lower := strings.ToLower(key)
		if strings.HasSuffix(lower, "_proxy") || lower == "docker_host" || lower == "docker_context" || key == "PATH" {
			continue
		}
		env = append(env, item)
	}
	// Colima invokes limactl by name; it must resolve the same native Lima we detected.
	var selected []string
	for _, name := range []string{"limactl", "colima"} {
		if path, err := findToolIn(name, dirs, nativeARM); err == nil {
			selected = append(selected, filepath.Dir(path))
		}
	}
	var paths []string
	seen := make(map[string]bool)
	for _, dir := range append(selected, dirs...) {
		if filepath.IsAbs(dir) && !seen[dir] {
			paths = append(paths, dir)
			seen[dir] = true
		}
	}
	return append(env, "PATH="+strings.Join(paths, string(os.PathListSeparator)))
}

// commandError keeps only a short, fixed-size summary of stderr. Callers never pass secrets as
// arguments, and stdin is not echoed.
type commandError struct {
	tool   string
	action string
	detail string
}

func (e *commandError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("%s %s 失败", e.tool, e.action)
	}
	return fmt.Sprintf("%s %s 失败：%s", e.tool, e.action, e.detail)
}

func summarize(stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	return clip(strings.TrimSpace(lines[len(lines)-1]), 300)
}

// clip shortens s to at most limit bytes without cutting a character in half.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// scanOutputLines splits command output on newlines and on carriage returns, which progress
// displays use to redraw a line.
func scanOutputLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

type runner struct {
	path string
	args []string // fixed leading arguments, e.g. --context
}

func (r runner) run(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.path, append(append([]string{}, r.args...), args...)...)
	boundCommandLifetime(cmd)
	cmd.Env = childEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		action := ""
		if len(args) > 0 {
			action = args[0]
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return stdout.Bytes(), &commandError{filepath.Base(r.path), action, "超时"}
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return stdout.Bytes(), &commandError{filepath.Base(r.path), action, "已取消"}
		}
		return stdout.Bytes(), &commandError{filepath.Base(r.path), action, summarize(stderr.Bytes())}
	}
	return stdout.Bytes(), nil
}

// runLines runs a command like run, but hands each non-empty line of its output (stdout and stderr
// together) to onLine as it arrives. The error keeps the last line, which for most tools says what
// went wrong.
func (r runner) runLines(ctx context.Context, timeout time.Duration, onLine func(string), args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.path, append(append([]string{}, r.args...), args...)...)
	boundCommandLifetime(cmd)
	cmd.Env = childEnv()
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	last := ""
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		scanner.Split(scanOutputLines)
		for scanner.Scan() {
			if line := strings.TrimSpace(scanner.Text()); line != "" {
				last = line
				if onLine != nil {
					onLine(line)
				}
			}
		}
		io.Copy(io.Discard, reader) // keep the command from blocking after an overlong line
	}()
	err := cmd.Run()
	writer.Close()
	<-scanned
	if err == nil {
		return nil
	}
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &commandError{filepath.Base(r.path), action, "超时"}
	case errors.Is(ctx.Err(), context.Canceled):
		return &commandError{filepath.Base(r.path), action, "已取消"}
	}
	return &commandError{filepath.Base(r.path), action, clip(last, 300)}
}

// stream runs a command with caller-provided stdio, for interactive exec from the CLI.
func (r runner) stream(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, r.path, append(append([]string{}, r.args...), args...)...)
	cmd.Env = childEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}
