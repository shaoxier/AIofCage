package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/session"
)

// Networks as Docker 29 shows them: the internal one without the VM has no gateway.
const (
	keptOffNetwork = `{"Internal":true,"Options":{"com.docker.network.bridge.inhibit_ipv4":"true"},"IPAM":{"Config":[{"Subnet":"192.168.192.0/20"}]}}`
	oldNetwork     = `{"Internal":true,"Options":{},"IPAM":{"Config":[{"Subnet":"172.18.0.0/16","Gateway":"172.18.0.1"}]}}`
	egressNetwork  = `{"Internal":false,"Options":{},"IPAM":{"Config":[{"Subnet":"192.168.208.0/20","Gateway":"192.168.208.1"}]}}`
)

// statefulDocker keeps networks and containers as files in a directory and logs every command.
func statefulDocker(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
state='` + dir + `'
printf '%s\n' "$*" >> "$state/log"
for last; do :; done
case "$1 $2" in
"network inspect") cat "$state/net-$last" 2>/dev/null && exit 0; echo "Error response from daemon: network $last not found" >&2; exit 1;;
"network create")
  case "$*" in
  *"--internal --opt com.docker.network.bridge.inhibit_ipv4=true"*) echo '` + keptOffNetwork + `' > "$state/net-$last";;
  *--internal*) echo '` + oldNetwork + `' > "$state/net-$last";;
  *) echo '` + egressNetwork + `' > "$state/net-$last";;
  esac; exit 0;;
"network rm") rm "$state/net-$last"; exit $?;;
"container inspect") cat "$state/ctr-$last" 2>/dev/null && exit 0; echo "Error response from daemon: No such container: $last" >&2; exit 1;;
esac
case "$1" in
create)
  name= spec= previous=
  for arg; do
    [ "$previous" = --name ] && name=$arg
    case "$arg" in io.aiofcage.spec=*) spec=${arg#io.aiofcage.spec=};; esac
    previous=$arg
  done
  case "$name" in *-gate) image=gate-img;; *) image=agent-img;; esac
  echo "false $image $spec" > "$state/ctr-$name";;
rm) rm -f "$state/ctr-$last";;
esac
exit 0
`
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func networkSandbox(t *testing.T) sandbox.Sandbox {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	project := filepath.Join(home, "CageWorkspace", "proj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return sandbox.Sandbox{ID: "sbx-net00001", Name: "net", ProjectPath: project, Bindings: []sandbox.Binding{{Agent: session.Codex}}}
}

func ensureWith(t *testing.T, docker string, sbx sandbox.Sandbox) string {
	t.Helper()
	m := &Manager{docker: runner{path: docker}, gates: map[string]*gateClient{}, locks: map[string]*sync.Mutex{}, connectorTokens: map[string]string{}}
	if err := m.ensureObjects(context.Background(), sbx, "gate-img", "agent-img"); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(filepath.Dir(docker), "log"))
	return string(log)
}

// in reports whether the log has these lines in this order.
func in(log string, lines ...string) bool {
	at := 0
	for _, line := range lines {
		i := strings.Index(log[at:], line)
		if i < 0 {
			return false
		}
		at += i + len(line)
	}
	return true
}

func TestNewInternalNetworkHasNoVMAddress(t *testing.T) {
	docker, _ := statefulDocker(t)
	log := ensureWith(t, docker, networkSandbox(t))
	if !in(log, "network create --label io.aiofcage.sandbox=sbx-net00001 --internal --opt com.docker.network.bridge.inhibit_ipv4=true aiofcage-sbx-net00001-in\n",
		"network create --label io.aiofcage.sandbox=sbx-net00001 aiofcage-sbx-net00001-out\n",
		"--network aiofcage-sbx-net00001-out --ip 192.168.208.2 ",
		"--proxy-listen 192.168.192.2:8888 --mcp-listen 192.168.192.2:8890\n",
		"network connect --ip 192.168.192.2 --alias gatekeeper aiofcage-sbx-net00001-in aiofcage-sbx-net00001-gate\n",
		"create --name aiofcage-sbx-net00001-agent ") {
		t.Fatalf("unexpected commands:\n%s", log)
	}
}

// A sandbox made before the change gets a new internal network on its next start, with new
// containers; its home volume stays.
func TestOldInternalNetworkIsReplaced(t *testing.T) {
	docker, state := statefulDocker(t)
	sbx := networkSandbox(t)
	for name, content := range map[string]string{"net-aiofcage-sbx-net00001-in": oldNetwork, "net-aiofcage-sbx-net00001-out": egressNetwork,
		"ctr-aiofcage-sbx-net00001-gate": "false gate-img " + specVersion, "ctr-aiofcage-sbx-net00001-agent": "false agent-img " + specVersion} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := ensureWith(t, docker, sbx)
	if !in(log, "label=io.aiofcage.role=probe", "rm --force aiofcage-sbx-net00001-agent\n", "rm --force aiofcage-sbx-net00001-gate\n",
		"network rm aiofcage-sbx-net00001-in\n",
		"network create --label io.aiofcage.sandbox=sbx-net00001 --internal --opt com.docker.network.bridge.inhibit_ipv4=true aiofcage-sbx-net00001-in\n",
		"label=io.aiofcage.role=connector", // connectors on the egress network are made again too
		"create --name aiofcage-sbx-net00001-gate ",
		"network connect --ip 192.168.192.2 --alias gatekeeper aiofcage-sbx-net00001-in aiofcage-sbx-net00001-gate\n",
		"create --name aiofcage-sbx-net00001-agent ") {
		t.Fatalf("unexpected commands:\n%s", log)
	}
	if strings.Contains(log, "volume rm") || strings.Contains(log, "volume create") || strings.Contains(log, "network rm aiofcage-sbx-net00001-out") ||
		strings.Contains(log, "network create --label io.aiofcage.sandbox=sbx-net00001 aiofcage-sbx-net00001-out") {
		t.Fatalf("only the internal network and the containers on it are replaced:\n%s", log)
	}
}

func TestInternalNetworkWithoutVMIsKept(t *testing.T) {
	docker, state := statefulDocker(t)
	for name, content := range map[string]string{"net-aiofcage-sbx-net00001-in": keptOffNetwork, "net-aiofcage-sbx-net00001-out": egressNetwork,
		"ctr-aiofcage-sbx-net00001-gate": "true gate-img " + specVersion, "ctr-aiofcage-sbx-net00001-agent": "false agent-img " + specVersion} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := ensureWith(t, docker, networkSandbox(t))
	for _, command := range []string{"network rm", "network create", "rm --force", "create --name"} {
		if strings.Contains(log, command) {
			t.Fatalf("an up-to-date sandbox is left alone, got %q:\n%s", command, log)
		}
	}
}

func TestSandboxNetworkReportsTheVMsAddresses(t *testing.T) {
	docker, state := statefulDocker(t)
	for name, content := range map[string]string{"net-aiofcage-sbx-net00001-in": keptOffNetwork, "net-aiofcage-sbx-net00001-out": egressNetwork} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{docker: runner{path: docker}}
	internal, err := m.SandboxNetwork(context.Background(), "sbx-net00001", ProbeInternal)
	if err != nil || internal.Name != "aiofcage-sbx-net00001-in" || len(internal.Gateways) != 0 {
		t.Fatalf("%+v %v", internal, err)
	}
	egress, err := m.SandboxNetwork(context.Background(), "sbx-net00001", ProbeEgress)
	if err != nil || strings.Join(egress.Gateways, ",") != "192.168.208.1" {
		t.Fatalf("%+v %v", egress, err)
	}
	if _, err := m.SandboxNetwork(context.Background(), "sbx-net00001", ProbeNone); err == nil {
		t.Fatal("only the internal and the egress network can be read")
	}
}
