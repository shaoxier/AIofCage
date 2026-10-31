package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// Each check must report a problem when the agent's side is set up wrong. Helper containers play an
// agent that got one isolation layer wrong; the checks' own samples and helpers run as usual.
func TestIsolationChecksCatchBrokenSandboxes(t *testing.T) {
	app, sbx := dedicatedApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	runIn := func(probe runtime.Probe) probeRunner {
		return func(ctx context.Context, script string, args ...string) ([]byte, error) {
			probe.Command = pythonCommand(script, args...)
			return app.manager.RunProbe(ctx, sbx.ID, probe)
		}
	}
	workspace, err := filepath.EvalSymlinks(sandbox.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	withSocket := func(ctx context.Context, script string, args ...string) ([]byte, error) {
		return app.withFakeDocker(ctx, sbx, func(path string) ([]byte, error) {
			return runIn(runtime.Probe{Network: runtime.ProbeNone,
				Mounts: []string{"type=bind,src=" + path + ",dst=/var/run/docker.sock"}})(ctx, script, args...)
		})
	}
	for _, tt := range []struct {
		name  string
		check func(Attack, probeRunner) AttackResult
		agent probeRunner
	}{
		{"agent on the egress network", func(c Attack, r probeRunner) AttackResult { return app.checkDNS(ctx, sbx, c, r) },
			runIn(runtime.Probe{Network: runtime.ProbeEgress})},
		{"Docker socket mounted", func(c Attack, r probeRunner) AttackResult { return app.checkDockerSocket(ctx, sbx, c, r) }, withSocket},
		{"agent on a network the VM is on", func(c Attack, r probeRunner) AttackResult { return app.checkVMPorts(ctx, sbx, c, r) },
			runIn(runtime.Probe{Network: runtime.ProbeEgress})},
		{"whole workspace shared", func(c Attack, r probeRunner) AttackResult { return app.checkProjectBoundary(ctx, sbx, c, r) },
			runIn(runtime.Probe{Network: runtime.ProbeNone, Mounts: []string{"type=bind,src=" + workspace + ",dst=" + workspace + ",readonly"}})},
		{"workspace mounted as the project", func(c Attack, r probeRunner) AttackResult { return app.checkProjectBoundary(ctx, sbx, c, r) },
			runIn(runtime.Probe{Network: runtime.ProbeNone, Mounts: []string{"type=bind,src=" + workspace + ",dst=" + sbx.ProjectPath + ",readonly"}})},
		{"workspace mounted elsewhere too", func(c Attack, r probeRunner) AttackResult { return app.checkProjectBoundary(ctx, sbx, c, r) },
			runIn(runtime.Probe{Network: runtime.ProbeNone, Mounts: []string{"type=bind,src=" + sbx.ProjectPath + ",dst=" + sbx.ProjectPath + ",readonly",
				"type=bind,src=" + workspace + ",dst=/workspace,readonly"}})},
	} {
		got := settle(tt.check(Attack{ID: tt.name}, tt.agent))
		t.Logf("%s: %s %s", tt.name, got.Status, got.Evidence)
		if got.Status != checkProblem {
			t.Errorf("%s: want %s, got %s %+v", tt.name, checkProblem, got.Status, got.Controls)
		}
	}
}
