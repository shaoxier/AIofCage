package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// agentSettingsEntries 是 runtime.Manager 上经过 aiofcage-run 的入口：它们会先加载 Agent 可写的
// ~/.aiofcage/env，再在项目目录里运行命令。
var agentSettingsEntries = map[string]bool{"Exec": true, "ExecArgs": true, "TerminalCommand": true, "ConversationCommand": true}

// 应用自己在 Agent 容器里运行的命令不能经过 aiofcage-run：Agent 可以改写那里加载的设置，让停止会话、
// 会话组件检查或隔离检查失效（见 runtime/helpers.go）。只有启动 CLI、终端和检查 Agent 环境变量的那项
// 隔离检查需要这些设置。检查的是对 X.manager 上这些方法的引用（调用和方法值）；先把 manager 存进别的
// 变量再调用、以及 runtime 包里的辅助命令不在这里检查，后者各有命令行测试。
func TestOnlyAgentCommandsLoadTheAgentSettings(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var uses []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var stealEnv []*ast.BlockStmt
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if branch, ok := node.(*ast.IfStmt); ok && types.ExprString(branch.Cond) == `check.ID == "steal-env"` {
					stealEnv = append(stealEnv, branch.Body)
				}
				return true
			})
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				method, ok := node.(*ast.SelectorExpr)
				if !ok || !agentSettingsEntries[method.Sel.Name] {
					return true
				}
				if owner, ok := method.X.(*ast.SelectorExpr); ok && owner.Sel.Name == "manager" {
					use := name + " " + fn.Name.Name + " " + method.Sel.Name
					for _, body := range stealEnv {
						if body.Pos() <= method.Pos() && method.End() <= body.End() {
							use += "（仅凭据配置）"
						}
					}
					uses = append(uses, use)
				}
				return true
			})
		}
	}
	slices.Sort(uses)
	want := []string{
		"agent_control.go executeAttempt ConversationCommand",
		"attacks.go runAttack Exec（仅凭据配置）",
		"conversations.go SendConversationMessage ConversationCommand",
		"sandboxes.go OpenTerminal ExecArgs",
		"terminal.go StartTerminal TerminalCommand",
	}
	if !slices.Equal(uses, want) {
		t.Fatalf("经过 aiofcage-run 的入口用在了 %q；应用自己的辅助命令应改用 manager.ExecHelper", uses)
	}
}

// 隔离检查用镜像自带的 Python，不受 PATH、PYTHON* 变量、site-packages 和工作目录影响。
func TestCheckScriptsUseTheImagesPython(t *testing.T) {
	if got := pythonCommand("print(1)", "a"); !slices.Equal(got, []string{"/usr/bin/python3", "-I", "-S", "-c", "print(1)", "a"}) {
		t.Fatalf("pythonCommand = %q", got)
	}
}
