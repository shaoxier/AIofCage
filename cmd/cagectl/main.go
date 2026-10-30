// cagectl drives the same sandbox runtime as the desktop app from the command line. It is used for
// development, automated checks and as a fallback during demos.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/shaoxier/AIofCage/internal/audit"
	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/oauth"
	"github.com/shaoxier/AIofCage/internal/provider"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
	"github.com/shaoxier/AIofCage/internal/service"
	"github.com/shaoxier/AIofCage/internal/session"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
)

const usage = `用法：
  cagectl ai serve                       作为本机 stdio MCP，供外部 AI 创建隔离实验沙箱和执行任务
  cagectl env check                      检查专用虚拟机
  cagectl env up                         创建或启动专用虚拟机并校验共享边界

  cagectl provider add --id ID --name 名称 --agent codex|claude-code|dsh --protocol openai-responses|openai-chat|anthropic
                       --base-url URL --model 模型 [--auth bearer|x-api-key] [--keychain-service 条目 | --key-stdin]
  cagectl provider ls
  cagectl provider rm ID

  cagectl mcp add --id ID --name 名称 --url URL --token-stdin   保存远程 MCP 服务（令牌进钥匙串）
  cagectl mcp add --id ID --name 名称 --url URL --no-auth       保存不需要令牌的公开远程 MCP 服务
  cagectl mcp add --kind postgres --id ID --name 名称 --url postgresql://用户@主机:5432/库 --token-stdin
                                                     保存数据库 MCP（密码进钥匙串，由可信连接器代持）
  cagectl mcp add --kind oauth --id ID --name 名称 --url URL      保存 OAuth 类远程 MCP（例如 https://mcp.notion.com/mcp）
  cagectl mcp login ID                                  在浏览器里登录授权，令牌进钥匙串
  cagectl mcp tools ID                                  获取远程 MCP 服务的工具列表（标出服务标注的只读工具）
  cagectl mcp ls
  cagectl mcp rm ID

  cagectl sandbox create --name 名称 --project 路径 --bind codex=供应商ID [--bind ...] [--max-requests N] [--max-tokens N]
                         [--mcp 服务ID ...] [--filesystem-mcp]
  cagectl sandbox ls
  cagectl sandbox start|stop 名称
  cagectl sandbox rm 名称 [--keep-home]
  cagectl sandbox bind 名称 agent=供应商ID
  cagectl sandbox mcp 名称 [--filesystem] [服务ID ...] [--grant 服务ID=工具,工具 ...]
                         未用 --grant 指定的服务保留原授权；新启用的远程服务默认只开放标注只读的工具
  cagectl sandbox exec 名称 [-t] -- 命令...
  cagectl sandbox usage 名称
  cagectl sandbox events 名称
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("缺少子命令")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := store.DefaultDir()
	if err != nil {
		return err
	}
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	keychain := vault.NewKeychain()
	switch args[0] + " " + args[1] {
	case "ai serve":
		return service.RunAgentControl(ctx, os.Stdin, os.Stdout)
	case "env check", "env up":
		env, err := runtime.NewEnvironment(st)
		if err != nil {
			return err
		}
		if args[1] == "up" {
			fmt.Println("正在准备专用虚拟机（首次创建需要下载镜像）…")
			if err := env.Ensure(ctx); err != nil {
				return err
			}
		}
		report := env.Inspect(ctx)
		for _, check := range report.Checks {
			fmt.Printf("%-24s %-12s %s\n", check.Name, check.Status, check.Detail)
		}
		if !report.Ready {
			return errors.New("环境未就绪")
		}
		return nil
	case "provider add":
		return providerAdd(ctx, st, keychain, args[2:])
	case "provider ls":
		items, err := st.Providers()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t名称\tAgent\t协议\t接口\t模型\t钥匙串条目")
		for _, p := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Agent, p.Protocol, p.BaseURL, p.Model, p.KeyRef)
		}
		return w.Flush()
	case "provider rm":
		if len(args) != 3 {
			return errors.New("用法：cagectl provider rm ID")
		}
		p, err := st.Provider(args[2])
		if err != nil {
			return err
		}
		if err := st.DeleteProvider(p.ID); err != nil {
			return err
		}
		// Like the desktop app: only items AIofCage created are deleted, never e.g. sbx-builderday-key.
		if p.KeyRef == provider.KeyRefFor(p.ID) {
			return keychain.Delete(ctx, p.KeyRef)
		}
		return nil
	case "mcp add":
		return mcpAdd(ctx, st, keychain, args[2:])
	case "mcp ls":
		items, err := st.MCPServices()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t名称\t类型\t地址\t钥匙串条目\t工具")
		for _, s := range items {
			kind := string(s.Kind)
			if kind == "" {
				kind = "http"
			}
			keyRef, tools := s.KeyRef, fmt.Sprintf("%d 个，%d 个标注只读", len(s.Tools()), len(s.DefaultTools()))
			if s.Auth == mcp.AuthNone {
				keyRef = "（无需令牌）"
			}
			if s.IsConnector() {
				tools = fmt.Sprintf("%d 个（AIofCage 固定）", len(s.Tools()))
			} else if s.CatalogAt == nil {
				tools = "未获取（cagectl mcp tools " + s.ID + "）"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Name, kind, s.URL, keyRef, tools)
		}
		return w.Flush()
	case "mcp tools":
		if len(args) != 3 {
			return errors.New("用法：cagectl mcp tools ID")
		}
		service, err := st.MCPService(args[2])
		if err != nil {
			return err
		}
		if service.IsConnector() {
			return errors.New("这个服务的工具由 AIofCage 固定：" + strings.Join(service.Tools(), "、"))
		}
		manager, err := runtime.NewManager(st, keychain)
		if err != nil {
			return err
		}
		listed, err := manager.ListTools(ctx, service)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "工具\t服务的标注\t说明")
		for _, tool := range listed.Catalog {
			mark := "未标注只读"
			if tool.ReadOnly {
				mark = "只读"
			} else if tool.Destructive {
				mark = "可能删改"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", tool.Name, mark, firstOf(tool.Title, tool.Description))
		}
		return w.Flush()
	case "mcp login":
		if len(args) != 3 {
			return errors.New("用法：cagectl mcp login ID")
		}
		return mcpLogin(ctx, st, keychain, args[2])
	case "mcp rm":
		if len(args) != 3 {
			return errors.New("用法：cagectl mcp rm ID")
		}
		service, err := st.MCPService(args[2])
		if err != nil {
			return err
		}
		if err := st.DeleteMCPService(service.ID); err != nil {
			return err
		}
		if service.IsBuiltin() {
			return nil
		}
		return keychain.Delete(ctx, service.KeyRef)
	}
	if args[0] != "sandbox" {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("未知命令")
	}
	manager, err := runtime.NewManager(st, keychain)
	if err != nil {
		return err
	}
	manager.Progress = func(_, message string) { fmt.Fprintln(os.Stderr, "·", message) }
	switch args[1] {
	case "create":
		return sandboxCreate(ctx, manager, args[2:])
	case "ls":
		items, err := st.Sandboxes()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "名称\tID\t状态\t项目\tAgent→供应商\t已用请求\t已用 token")
		for _, s := range items {
			var bindings []string
			for _, b := range s.Bindings {
				bindings = append(bindings, string(b.Agent)+"→"+orDash(b.ProviderID))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", s.Name, s.ID, manager.Status(ctx, s.ID).State,
				s.ProjectPath, strings.Join(bindings, ","), s.Usage.Requests, s.Usage.Tokens)
		}
		return w.Flush()
	}
	if len(args) < 3 {
		return errors.New("缺少沙箱名称")
	}
	sbx, err := st.Sandbox(args[2])
	if err != nil {
		return err
	}
	switch args[1] {
	case "start":
		return manager.Start(ctx, sbx.ID)
	case "stop":
		return manager.Stop(ctx, sbx.ID)
	case "rm":
		flags := flag.NewFlagSet("rm", flag.ContinueOnError)
		keepHome := flags.Bool("keep-home", false, "保留 Agent 的 home 数据卷")
		if err := flags.Parse(args[3:]); err != nil {
			return err
		}
		return manager.Delete(ctx, sbx.ID, *keepHome)
	case "bind":
		if len(args) != 4 {
			return errors.New("用法：cagectl sandbox bind 名称 agent=供应商ID")
		}
		agent, providerID, _ := strings.Cut(args[3], "=")
		return manager.Bind(ctx, sbx.ID, session.Agent(agent), providerID)
	case "exec":
		rest := args[3:]
		tty := false
		if len(rest) > 0 && rest[0] == "-t" {
			tty, rest = true, rest[1:]
		}
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		return manager.Exec(ctx, sbx.ID, tty, os.Stdin, os.Stdout, os.Stderr, rest...)
	case "mcp":
		filesystem := false
		var ids []string
		grants := map[string][]string{}
		rest := args[3:]
		for i := 0; i < len(rest); i++ {
			switch {
			case rest[i] == "--filesystem":
				filesystem = true
			case rest[i] == "--grant" && i+1 < len(rest):
				i++
				id, tools, ok := strings.Cut(rest[i], "=")
				if !ok || id == "" {
					return errors.New("--grant 的格式是 服务ID=工具,工具（不开放任何工具时写 服务ID=）")
				}
				grants[id] = []string{}
				for _, tool := range strings.Split(tools, ",") {
					if tool = strings.TrimSpace(tool); tool != "" {
						grants[id] = append(grants[id], tool)
					}
				}
			default:
				ids = append(ids, rest[i])
			}
		}
		return manager.SetMCPPermissions(ctx, sbx.ID, ids, filesystem, grants)
	case "usage":
		usage, err := manager.Usage(ctx, sbx.ID)
		if err != nil {
			return err
		}
		out, _ := json.MarshalIndent(usage, "", "  ")
		fmt.Println(string(out))
		return nil
	case "events":
		return manager.Events(ctx, sbx.ID, "", func(event audit.Event) error {
			target := event.Path
			if event.ServiceID != "" {
				target = "mcp:" + event.ServiceID
				if event.ToolID != "" {
					target += "/" + event.ToolID
				}
			}
			_, err := fmt.Printf("%s #%d %-14s %-8s %-22s %s %s %s %s\n", event.Time.Local().Format("15:04:05"), event.Sequence,
				event.Action, event.Decision, event.ReasonCode, event.Method, event.Host, target, tokens(event))
			return err
		})
	}
	return errors.New("未知的沙箱命令")
}

func providerAdd(ctx context.Context, st *store.Store, keychain *vault.Keychain, args []string) error {
	flags := flag.NewFlagSet("provider add", flag.ContinueOnError)
	var p provider.Provider
	var agent, protocol, auth string
	flags.StringVar(&p.ID, "id", "", "供应商 ID")
	flags.StringVar(&p.Name, "name", "", "显示名称")
	flags.StringVar(&agent, "agent", "", "codex | claude-code | dsh")
	flags.StringVar(&protocol, "protocol", "", "openai-responses | openai-chat | anthropic")
	flags.StringVar(&p.BaseURL, "base-url", "", "接口地址")
	flags.StringVar(&p.Model, "model", "", "默认模型")
	flags.StringVar(&auth, "auth", "bearer", "bearer | x-api-key")
	flags.StringVar(&p.KeyRef, "keychain-service", "", "已存在的钥匙串条目名称")
	keyStdin := flags.Bool("key-stdin", false, "从标准输入读取密钥并保存到钥匙串")
	if err := flags.Parse(args); err != nil {
		return err
	}
	p.Agent, p.Protocol, p.Auth = session.Agent(agent), provider.Protocol(protocol), provider.Auth(auth)
	p.CreatedAt = time.Now().UTC()
	if p.KeyRef == "" {
		p.KeyRef = provider.KeyRefFor(p.ID)
	}
	p.Normalize()
	if err := p.Validate(); err != nil {
		return err
	}
	if *keyStdin {
		fmt.Fprint(os.Stderr, "请输入密钥（不会显示，回车结束）：")
		line, err := bufio.NewReader(io.LimitReader(os.Stdin, 8192)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		fmt.Fprintln(os.Stderr)
		if err := keychain.Put(ctx, p.KeyRef, []byte(strings.TrimSpace(line))); err != nil {
			return err
		}
	} else if _, err := keychain.Read(ctx, p.KeyRef); err != nil {
		return fmt.Errorf("钥匙串条目 %s 不可用：%w", p.KeyRef, err)
	}
	return st.SaveProvider(p)
}

func sandboxCreate(ctx context.Context, manager *runtime.Manager, args []string) error {
	flags := flag.NewFlagSet("sandbox create", flag.ContinueOnError)
	var request runtime.CreateRequest
	var binds multiFlag
	flags.StringVar(&request.Name, "name", "", "沙箱名称")
	flags.StringVar(&request.ProjectPath, "project", "", "项目目录（位于 ~/CageWorkspace 内）")
	flags.Var(&binds, "bind", "agent=供应商ID，可重复")
	flags.Uint64Var(&request.Limits.MaxRequests, "max-requests", 0, "请求数上限（0 为不限）")
	flags.Uint64Var(&request.Limits.MaxTokens, "max-tokens", 0, "token 上限（0 为不限）")
	var mcpIDs multiFlag
	flags.Var(&mcpIDs, "mcp", "启用的远程 MCP 服务 ID，可重复")
	flags.BoolVar(&request.FilesystemMCP, "filesystem-mcp", false, "启用内置的 filesystem MCP（只开放项目目录）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	request.MCP = mcpIDs
	for _, value := range binds {
		agent, providerID, _ := strings.Cut(value, "=")
		request.Bindings = append(request.Bindings, sandbox.Binding{Agent: session.Agent(agent), ProviderID: providerID})
	}
	sbx, err := manager.Create(ctx, request)
	if err != nil {
		return err
	}
	fmt.Printf("已创建沙箱 %s（%s），用 cagectl sandbox start %s 启动\n", sbx.Name, sbx.ID, sbx.Name)
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func tokens(event audit.Event) string {
	if event.InputTokens == 0 && event.OutputTokens == 0 {
		return ""
	}
	return fmt.Sprintf("tokens=%d/%d", event.InputTokens, event.OutputTokens)
}

// mcpLogin runs the OAuth login in the browser; a running sandbox picks the token up on its next
// start or refresh (the desktop app refreshes running sandboxes at once).
func mcpLogin(ctx context.Context, st *store.Store, keychain *vault.Keychain, id string) error {
	service, err := st.MCPService(id)
	if err != nil {
		return err
	}
	if !service.IsOAuth() {
		return errors.New("这个服务不使用 OAuth 登录")
	}
	client := oauth.NewClient(&http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}},
		func(target string) error { return exec.Command("/usr/bin/open", target).Run() })
	fmt.Fprintln(os.Stderr, "正在打开浏览器，请在页面里完成授权…")
	creds, err := client.Login(ctx, service.URL)
	if err != nil {
		return err
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	if err := keychain.Put(ctx, service.KeyRef, data); err != nil {
		return err
	}
	fmt.Println("已授权，令牌保存在钥匙串（会自动续期）")
	return nil
}

func mcpAdd(ctx context.Context, st *store.Store, keychain *vault.Keychain, args []string) error {
	flags := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	var service mcp.Service
	flags.StringVar(&service.ID, "id", "", "服务 ID（也是 Agent 配置里的服务名）")
	flags.StringVar(&service.Name, "name", "", "显示名称")
	flags.StringVar(&service.URL, "url", "", "streamable HTTP 地址；数据库为不含密码的连接串")
	kind := flags.String("kind", "http", "http、oauth 或 postgres")
	tokenStdin := flags.Bool("token-stdin", false, "从标准输入读取令牌或数据库密码并保存到钥匙串")
	noAuth := flags.Bool("no-auth", false, "公开服务，不需要令牌")
	if err := flags.Parse(args); err != nil {
		return err
	}
	service.Kind = mcp.Kind(*kind)
	service.KeyRef, service.CreatedAt = mcp.KeyRefFor(service.ID), time.Now().UTC()
	if *noAuth {
		service.Auth = mcp.AuthNone
	}
	service.Normalize()
	if err := service.Validate(); err != nil {
		return err
	}
	if service.Auth == mcp.AuthNone {
		if *tokenStdin {
			return errors.New("--no-auth 和 --token-stdin 不能同时使用")
		}
		if err := st.SaveMCPService(service); err != nil {
			return err
		}
		fmt.Printf("已保存。用 cagectl mcp tools %s 获取它的工具列表\n", service.ID)
		return nil
	}
	if service.IsOAuth() {
		if err := st.SaveMCPService(service); err != nil {
			return err
		}
		fmt.Printf("已保存。用 cagectl mcp login %s 在浏览器里登录授权\n", service.ID)
		return nil
	}
	if !*tokenStdin {
		return errors.New("请用 --token-stdin 输入令牌或密码")
	}
	fmt.Fprint(os.Stderr, "请输入访问令牌或数据库密码（不会显示，回车结束）：")
	line, err := bufio.NewReader(io.LimitReader(os.Stdin, 8192)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	fmt.Fprintln(os.Stderr)
	secret := strings.TrimRight(line, "\r\n")
	if !service.IsConnector() {
		secret = strings.TrimSpace(secret)
	}
	if strings.TrimSpace(secret) == "" {
		return errors.New("令牌或密码不能为空")
	}
	if err := keychain.Put(ctx, service.KeyRef, []byte(secret)); err != nil {
		return err
	}
	return st.SaveMCPService(service)
}

func firstOf(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
