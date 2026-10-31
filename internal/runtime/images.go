package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"text/template"
	"time"

	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/session"
)

//go:embed assets
var assets embed.FS

const (
	gatekeeperBinary = "assets/gatekeeper/gatekeeper-linux-arm64"
	connectorBinary  = "assets/connector/connector-linux-arm64"
)

// AgentSpec pins the npm package installed for an agent. Versions are fixed so that images are
// reproducible and match what was verified.
type AgentSpec struct {
	Agent   session.Agent
	Name    string
	Package string
	Version string
	Command string
	// Scripts allows the package's install scripts. Claude Code's postinstall only copies its
	// native binary out of the platform optional dependency (no network); others skip scripts.
	Scripts bool
}

var agentCatalog = []AgentSpec{
	{session.Codex, "Codex", "@openai/codex", "0.164.0", "codex", false},
	{session.ClaudeCode, "Claude Code", "@anthropic-ai/claude-code", "2.1.309", "claude", true},
	{session.DSH, "DeepSeek Harness", "@deepseek-ai/dsh", "0.2.1-rc.2", "dsh", false},
}

func Spec(agent session.Agent) (AgentSpec, bool) {
	for _, spec := range agentCatalog {
		if spec.Agent == agent {
			return spec, true
		}
	}
	return AgentSpec{}, false
}

// Catalog lists the agents that can be installed in a sandbox.
func Catalog() []AgentSpec { return append([]AgentSpec(nil), agentCatalog...) }

type buildContext struct {
	tag   string
	files map[string][]byte
}

func digest(files map[string][]byte, names ...string) string {
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(files[name]))
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func gatekeeperContext() (buildContext, error) {
	binary, err := assets.ReadFile(gatekeeperBinary)
	if err != nil {
		return buildContext{}, errors.New("门卫二进制没有打包进程序，请先运行 make gatekeeper-linux 再构建")
	}
	dockerfile, err := assets.ReadFile("assets/gatekeeper/Dockerfile")
	if err != nil {
		return buildContext{}, err
	}
	files := map[string][]byte{"Dockerfile": dockerfile, "gatekeeper": binary}
	return buildContext{tag: "aiofcage/gatekeeper:" + digest(files, "Dockerfile", "gatekeeper"), files: files}, nil
}

// connectorContext builds one image with every catalog MCP server, shared by all connectors.
func connectorContext() (buildContext, error) {
	binary, err := assets.ReadFile(connectorBinary)
	if err != nil {
		return buildContext{}, errors.New("连接器二进制没有打包进程序，请先运行 make connector-linux 再构建")
	}
	text, err := assets.ReadFile("assets/connector/Dockerfile.tmpl")
	if err != nil {
		return buildContext{}, err
	}
	var dockerfile bytes.Buffer
	if err := template.Must(template.New("connector").Parse(string(text))).Execute(&dockerfile,
		struct{ Connectors []mcp.ConnectorSpec }{mcp.Connectors()}); err != nil {
		return buildContext{}, err
	}
	files := map[string][]byte{"Dockerfile": dockerfile.Bytes(), "connector": binary}
	inputs := []string{"Dockerfile", "connector"}
	for _, name := range []string{"package.json", "package-lock.json", "policy.mjs", "server.mjs", "policy.test.mjs"} {
		data, err := assets.ReadFile("assets/connector/postgres/" + name)
		if err != nil {
			return buildContext{}, err
		}
		key := "postgres-" + name
		files[key] = data
		inputs = append(inputs, key)
	}
	return buildContext{tag: "aiofcage/connector:" + digest(files, inputs...), files: files}, nil
}

func agentContext(agents []session.Agent) (buildContext, error) {
	return agentContextVersions(agents, nil)
}

func agentContextVersions(agents []session.Agent, versions map[session.Agent]string) (buildContext, error) {
	var specs []AgentSpec
	for _, spec := range agentCatalog { // catalog order keeps shared layers cacheable
		for _, agent := range agents {
			if agent == spec.Agent {
				if version := versions[agent]; version != "" {
					if !ValidAgentVersion(version) {
						return buildContext{}, errors.New("智能体版本无效")
					}
					spec.Version = version
				}
				specs = append(specs, spec)
			}
		}
	}
	text, err := assets.ReadFile("assets/agent/Dockerfile.tmpl")
	if err != nil {
		return buildContext{}, err
	}
	var dockerfile bytes.Buffer
	if err := template.Must(template.New("agent").Parse(string(text))).Execute(&dockerfile, struct{ Agents []AgentSpec }{specs}); err != nil {
		return buildContext{}, err
	}
	files := map[string][]byte{"Dockerfile": dockerfile.Bytes()}
	for _, name := range []string{"aiofcage-init.py", "aiofcage-run", "bashrc", "agent-turn.py"} {
		data, err := assets.ReadFile("assets/agent/" + name)
		if err != nil {
			return buildContext{}, err
		}
		files[name] = data
	}
	return buildContext{tag: "aiofcage/agent:" + digest(files, "Dockerfile", "aiofcage-init.py", "aiofcage-run", "bashrc", "agent-turn.py"), files: files}, nil
}

// ensureImage builds the image inside the dedicated VM unless an image with the same content
// digest already exists. Progress goes to the sandbox id, if any.
func (m *Manager) ensureImage(ctx context.Context, build buildContext, id string) error {
	if _, err := m.docker.run(ctx, 15*time.Second, nil, "image", "inspect", build.tag); err == nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "aiofcage-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for name, data := range build.files {
		mode := fs.FileMode(0o644)
		if name == "gatekeeper" || name == "connector" || name == "aiofcage-run" || name == "aiofcage-init.py" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
			return err
		}
	}
	m.progress(id, "正在构建镜像 "+build.tag+"（首次需要几分钟）")
	var steps buildSteps
	if err := m.docker.runLines(ctx, 30*time.Minute, func(line string) {
		if detail, changed := steps.feed(line); changed {
			m.detail(id, detail)
		}
	}, "build", "--progress=plain", "--platform", "linux/arm64",
		"--label", "io.aiofcage.managed=true", "--tag", build.tag, dir); err != nil {
		return fmt.Errorf("构建镜像失败：%w", err)
	}
	return nil
}

var (
	// "#7 [3/9] RUN npm install …", or "#7 [builder 3/9] …" in a multi-stage build.
	buildStepLine = regexp.MustCompile(`^#(\d+) \[(?:[^\]]*\s)?(\d+)/(\d+)\] (.+)$`)
	// "#7 12.345 added 120 packages": output of a running step.
	buildOutputLine = regexp.MustCompile(`^#(\d+) \d+\.\d+ (.+)$`)
	// "#5 sha256:4f4f… 12.58MB / 49.55MB 3.2s": a base image layer being downloaded.
	buildPullLine = regexp.MustCompile(`^#(\d+) sha256:[0-9a-f]+ (\S+ / \S+)`)
)

// buildSteps follows `docker build --progress=plain` output and says which step of the build is
// running and what it printed last.
type buildSteps struct {
	names   map[string]string // vertex number → "第 k/N 步：…"
	current string
	latest  string
	shown   string
}

func (b *buildSteps) feed(line string) (string, bool) {
	if b.names == nil {
		b.names = map[string]string{}
	}
	if match := buildStepLine.FindStringSubmatch(line); match != nil {
		b.names[match[1]] = fmt.Sprintf("第 %s/%s 步：%s", match[2], match[3], clip(match[4], 140))
		b.current, b.latest = b.names[match[1]], ""
	} else if match := buildPullLine.FindStringSubmatch(line); match != nil && b.names[match[1]] != "" {
		b.current, b.latest = b.names[match[1]], "下载基础镜像 "+match[2]
	} else if match := buildOutputLine.FindStringSubmatch(line); match != nil && b.names[match[1]] != "" {
		b.current, b.latest = b.names[match[1]], clip(match[2], 120)
	}
	detail := b.current
	if b.latest != "" {
		detail += " · " + b.latest
	}
	if detail == b.shown {
		return detail, false
	}
	b.shown = detail
	return detail, detail != ""
}
