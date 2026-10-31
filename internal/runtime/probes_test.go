package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Helper containers of the isolation checks get the agent's restrictions, the sandbox's label and
// only the mounts a check asks for: never the project, the home volume or a Docker socket.
func TestProbesRunWithTheAgentsRestrictions(t *testing.T) {
	m, _, log := liveSandbox(t, &liveGate{})
	ctx := context.Background()
	if _, err := m.RunProbe(ctx, "sbx-live0001", Probe{Network: ProbeEgress, DNS: "192.0.2.3",
		Command: []string{"python3", "-c", "print(1)"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.StartProbe(ctx, "sbx-live0001", Probe{Network: ProbeNone, Root: true,
		Mounts: []string{"type=volume,src=v,dst=/sock"}, Command: []string{"python3"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunProbe(ctx, "sbx-live0001", Probe{Network: "host", Command: []string{"sh"}}); err == nil {
		t.Fatal("only the sandbox's own networks are allowed")
	}
	data, _ := os.ReadFile(log)
	var runs []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "run ") {
			runs = append(runs, line)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("want two probe runs, got %q", runs)
	}
	for _, run := range runs {
		for _, want := range []string{"--label io.aiofcage.sandbox=sbx-live0001", "--label io.aiofcage.role=probe", "--read-only",
			"--cap-drop ALL", "--security-opt no-new-privileges", "--pids-limit 64", "--name aiofcage-sbx-live0001-probe-"} {
			if !strings.Contains(run, want) {
				t.Errorf("probe misses %q: %s", want, run)
			}
		}
		for _, banned := range []string{"-home", "/Users/u/CageWorkspace/p", "docker.sock", "--privileged", "--cap-add"} {
			if strings.Contains(run, banned) {
				t.Errorf("probe must not get %q: %s", banned, run)
			}
		}
	}
	if !strings.Contains(runs[0], "--rm") || !strings.Contains(runs[0], "--network aiofcage-sbx-live0001-out") ||
		!strings.Contains(runs[0], "--user 1000:1000") || !strings.Contains(runs[0], "--dns 192.0.2.3") {
		t.Errorf("one-off egress probe: %s", runs[0])
	}
	if !strings.Contains(runs[1], "--detach") || !strings.Contains(runs[1], "--network none") || !strings.Contains(runs[1], "--user 0:0") ||
		!strings.Contains(runs[1], "--mount type=volume,src=v,dst=/sock") {
		t.Errorf("background probe: %s", runs[1])
	}
}

// A probe left behind (the app quit during a check) would keep the sandbox's networks from being
// removed, so removing a sandbox removes probes first.
func TestRemovingASandboxRemovesLeftoverProbesFirst(t *testing.T) {
	m, _, log := liveSandbox(t, &liveGate{})
	if err := m.removeRuntimeObjects(context.Background(), "sbx-live0001", true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	text := string(data)
	probes := strings.Index(text, "label=io.aiofcage.role=probe")
	networks := strings.Index(text, "network rm")
	if probes < 0 || networks < 0 || probes > networks {
		t.Fatalf("probes must be looked for before the networks go:\n%s", text)
	}
}
