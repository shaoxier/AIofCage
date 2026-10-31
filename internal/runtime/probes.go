package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const probeRole = "probe"

// Probe is a short-lived helper container of the isolation checks. It runs the sandbox's own agent
// image with the agent's restrictions, never gets the home volume or the project, and carries the
// sandbox label so a leftover one is removed with the sandbox.
type Probe struct {
	// Network is ProbeInternal, ProbeEgress or ProbeNone.
	Network string
	// DNS replaces the upstream resolver of Docker's embedded DNS; empty keeps the VM's.
	DNS string
	// Root runs the command as root, still without any capability.
	Root bool
	// Mounts are extra --mount values (a probe directory or a probe volume).
	Mounts  []string
	Command []string
}

const (
	ProbeInternal = "internal"
	ProbeEgress   = "egress"
	ProbeNone     = "none"
)

func (m *Manager) probeArgs(ctx context.Context, id, name string, probe Probe) ([]string, error) {
	n := names(id)
	// The probe tests the very image the agent runs, not a newer build of it.
	out, err := m.docker.run(ctx, 15*time.Second, nil, "container", "inspect", "--format", "{{.Image}}", n.agent)
	image := strings.TrimSpace(string(out))
	if err != nil || image == "" {
		return nil, errors.New("无法读取 Agent 容器的镜像")
	}
	network := map[string]string{ProbeInternal: n.internal, ProbeEgress: n.egress, ProbeNone: "none"}[probe.Network]
	if network == "" || len(probe.Command) == 0 {
		return nil, fmt.Errorf("探针配置无效（网络 %q）", probe.Network)
	}
	user := agentUser
	if probe.Root {
		user = "0:0"
	}
	args := []string{"--name", name, "--label", labelKey + "=" + id, "--label", roleLabel + "=" + probeRole,
		"--network", network, "--user", user, "--read-only", "--tmpfs", "/tmp:uid=1000,gid=1000,mode=1777,size=8m",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "128m", "--cpus", "0.5", "--pids-limit", "64"}
	if probe.DNS != "" {
		args = append(args, "--dns", probe.DNS)
	}
	for _, mount := range probe.Mounts {
		args = append(args, "--mount", mount)
	}
	return append(append(args, "--entrypoint", probe.Command[0], image), probe.Command[1:]...), nil
}

func probeName(id string) (string, error) {
	var raw [5]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "aiofcage-" + id + "-probe-" + hex.EncodeToString(raw[:]), nil
}

// RunProbe runs a probe to its end and returns what it printed.
func (m *Manager) RunProbe(ctx context.Context, id string, probe Probe) ([]byte, error) {
	name, err := probeName(id)
	if err != nil {
		return nil, err
	}
	args, err := m.probeArgs(ctx, id, name, probe)
	if err != nil {
		return nil, err
	}
	out, err := m.docker.run(ctx, 60*time.Second, nil, append([]string{"run", "--rm"}, args...)...)
	if err != nil {
		// A probe cut short by the timeout may still exist.
		m.RemoveProbe(name)
	}
	return out, err
}

// StartProbe starts a probe in the background and returns its name and, unless it has no network,
// its address. The caller removes it with RemoveProbe.
func (m *Manager) StartProbe(ctx context.Context, id string, probe Probe) (string, string, error) {
	name, err := probeName(id)
	if err != nil {
		return "", "", err
	}
	args, err := m.probeArgs(ctx, id, name, probe)
	if err != nil {
		return "", "", err
	}
	// --rm as well: a background probe exits on its own after a while, so one left behind by a crash
	// disappears.
	if _, err := m.docker.run(ctx, 30*time.Second, nil, append([]string{"run", "--detach", "--rm"}, args...)...); err != nil {
		m.RemoveProbe(name)
		return "", "", err
	}
	if probe.Network == ProbeNone {
		return name, "", nil
	}
	network := names(id).egress
	if probe.Network == ProbeInternal {
		network = names(id).internal
	}
	ip, err := m.containerIP(ctx, name, network)
	if err != nil {
		m.RemoveProbe(name)
		return "", "", errors.New("无法读取探针容器的地址")
	}
	return name, ip, nil
}

// ProbeOutput returns what a background probe printed so far.
func (m *Manager) ProbeOutput(ctx context.Context, name string) ([]byte, error) {
	return m.docker.run(ctx, 15*time.Second, nil, "logs", name)
}

// RemoveProbe removes a probe container, also when the check that started it was cancelled.
func (m *Manager) RemoveProbe(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m.docker.run(ctx, 30*time.Second, nil, "rm", "--force", name)
}

// ProbeVolume creates a scratch volume for a probe and returns its name and its path in the VM.
func (m *Manager) ProbeVolume(ctx context.Context, id string) (string, string, error) {
	name, err := probeName(id)
	if err != nil {
		return "", "", err
	}
	if _, err := m.docker.run(ctx, 15*time.Second, nil, "volume", "create", "--label", labelKey+"="+id,
		"--label", roleLabel+"="+probeRole, name); err != nil {
		return "", "", err
	}
	out, err := m.docker.run(ctx, 15*time.Second, nil, "volume", "inspect", "--format", "{{.Mountpoint}}", name)
	path := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(path, "/") {
		m.RemoveProbeVolume(name)
		return "", "", errors.New("无法读取探针卷的位置")
	}
	return name, path, nil
}

func (m *Manager) RemoveProbeVolume(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m.docker.run(ctx, 30*time.Second, nil, "volume", "rm", "--force", name)
}

// AgentNetwork returns the networks the agent container is attached to and the DNS servers it was
// given: the isolation relies on the internal network only and on 127.0.0.1, where nothing answers.
func (m *Manager) AgentNetwork(ctx context.Context, id string) ([]string, []string, error) {
	out, err := m.docker.run(ctx, 15*time.Second, nil, "container", "inspect", "--format",
		"{{json .NetworkSettings.Networks}}\t{{json .HostConfig.Dns}}", names(id).agent)
	if err != nil {
		return nil, nil, err
	}
	networksJSON, dnsJSON, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	var networks map[string]json.RawMessage
	var dns []string
	if json.Unmarshal([]byte(networksJSON), &networks) != nil || json.Unmarshal([]byte(dnsJSON), &dns) != nil {
		return nil, nil, errors.New("无法读取 Agent 容器的网络设置")
	}
	attached := make([]string, 0, len(networks))
	for name := range networks {
		attached = append(attached, name)
	}
	slices.Sort(attached)
	return attached, dns, nil
}

// InternalNetwork is the name of a sandbox's internal network, the only one its agent may use.
func InternalNetwork(id string) string { return names(id).internal }

// RemoveProbes removes probes and probe volumes a check left behind, e.g. when the app quit in
// the middle of one.
func (m *Manager) RemoveProbes(ctx context.Context, id string) error { return m.removeProbes(ctx, id) }

// removeProbes removes probes and probe volumes a check left behind; a probe still attached to a
// network would keep the network from being removed.
func (m *Manager) removeProbes(ctx context.Context, id string) error {
	filters := []string{"--filter", "label=" + labelKey + "=" + id, "--filter", "label=" + roleLabel + "=" + probeRole}
	var errs []error
	if out, err := m.docker.run(ctx, 15*time.Second, nil, append([]string{"ps", "--all", "--quiet"}, filters...)...); err != nil {
		errs = append(errs, err)
	} else if ids := strings.Fields(string(out)); len(ids) > 0 {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, append([]string{"rm", "--force"}, ids...)...); err != nil {
			errs = append(errs, err)
		}
	}
	if out, err := m.docker.run(ctx, 15*time.Second, nil, append([]string{"volume", "ls", "--quiet"}, filters...)...); err != nil {
		errs = append(errs, err)
	} else if volumes := strings.Fields(string(out)); len(volumes) > 0 {
		if _, err := m.docker.run(ctx, 30*time.Second, nil, append([]string{"volume", "rm", "--force"}, volumes...)...); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
