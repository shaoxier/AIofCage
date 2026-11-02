package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 应用自己在 Agent 容器里运行的命令不能经过 aiofcage-run（manager.Exec）：Agent 可以改写它加载的
// ~/.aiofcage/env，让停止会话、会话组件检查或隔离检查失效（见 runtime/helpers.go）。只有检查 Agent
// 环境变量的那项模拟攻击需要它；启动 CLI 和终端走 ConversationCommand、TerminalCommand 与 ExecArgs。
func TestOnlyTheEnvironmentCheckLoadsTheAgentSettings(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
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
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if method, ok := call.Fun.(*ast.SelectorExpr); ok && method.Sel.Name == "Exec" {
					if owner, ok := method.X.(*ast.SelectorExpr); ok && owner.Sel.Name == "manager" {
						callers = append(callers, name+" "+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if !slices.Equal(callers, []string{"attacks.go runAttack"}) {
		t.Fatalf("manager.Exec is called from %q; the app's own helpers use manager.ExecHelper", callers)
	}
}

// 隔离检查用镜像自带的 Python，不受 PATH、PYTHON* 变量、site-packages 和工作目录影响。
func TestCheckScriptsUseTheImagesPython(t *testing.T) {
	if got := pythonCommand("print(1)", "a"); !slices.Equal(got, []string{"/usr/bin/python3", "-I", "-S", "-c", "print(1)", "a"}) {
		t.Fatalf("pythonCommand = %q", got)
	}
}
