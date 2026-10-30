package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/oauth"
	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/sandbox"
)

// MCPServiceView never contains the token, only whether one is saved. For OAuth services
// KeySaved means the login is still usable (a valid access token or a refresh token). Tools are
// what the service offers and DefaultTools what a sandbox is granted unless the user chooses;
// ToolsError explains why the tool list could not be fetched just now.
type MCPServiceView struct {
	mcp.Service
	Tools        []string `json:"tools"`
	DefaultTools []string `json:"default_tools"`
	ToolsError   string   `json:"tools_error,omitempty"`
	KeySaved     bool     `json:"key_saved"`
	UsedBy       []string `json:"used_by"`
}

func (a *App) ListMCPPresets() []mcp.Preset { return mcp.Presets() }

func (a *App) ListMCPServices() ([]MCPServiceView, error) {
	services, err := a.store.MCPServices()
	if err != nil {
		return nil, err
	}
	views := make([]MCPServiceView, 0, len(services))
	for _, s := range services {
		view, err := a.mcpView(s)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (a *App) mcpView(s mcp.Service) (MCPServiceView, error) {
	view := MCPServiceView{Service: s, Tools: s.Tools(), DefaultTools: s.DefaultTools(), UsedBy: []string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var data []byte
	var err error
	if s.Auth == mcp.AuthNone {
		view.KeySaved = true // nothing to save
	} else if !s.IsBuiltin() {
		data, err = a.vault.Read(ctx, s.KeyRef)
		view.KeySaved = err == nil
	}
	if s.IsOAuth() && err == nil {
		creds, err := oauth.Parse(data)
		view.KeySaved = err == nil && creds.Usable(a.now())
	}
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return view, err
	}
	for _, sbx := range sandboxes {
		for _, id := range sbx.MCP {
			if id == s.ID {
				view.UsedBy = append(view.UsedBy, sbx.Name)
			}
		}
	}
	return view, nil
}

type SaveMCPServiceRequest struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Preset string `json:"preset"`
	// Token is write-only: it goes straight to the Keychain. For database services it is the
	// password; a password pasted inside the connection URL is moved there as well.
	Token string `json:"token"`
	// NoAuth marks a custom HTTP service that needs no token; a token saved earlier is removed.
	NoAuth bool `json:"no_auth"`
}

func (a *App) SaveMCPService(request SaveMCPServiceRequest) (MCPServiceView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	existing := request.ID != ""
	var service mcp.Service
	if existing {
		current, err := a.store.MCPService(request.ID)
		if err != nil {
			return MCPServiceView{}, err
		}
		service = current
	} else {
		id, err := a.newMCPID(request.Preset)
		if err != nil {
			return MCPServiceView{}, err
		}
		// The kind is fixed at creation: switching between HTTP and connector services would leave
		// sandboxes configured for the other one.
		service = mcp.Service{ID: id, Kind: mcp.Kind(request.Kind), KeyRef: mcp.KeyRefFor(id), CreatedAt: a.now().UTC()}
		if service.IsBuiltin() {
			service.KeyRef = ""
		}
	}
	previousURL := service.URL
	service.Name, service.URL, service.Preset = request.Name, request.URL, request.Preset
	service.Normalize()
	service.Auth = ""
	if request.NoAuth {
		service.Auth = mcp.AuthNone
	}
	moved := existing && service.URL != previousURL && !service.IsConnector()
	if moved {
		// Another address may be another server: its tools must be listed again before any is granted.
		service.Catalog, service.CatalogAt = nil, nil
	}
	token := strings.TrimSpace(request.Token)
	missing := "请填写访问令牌"
	if service.IsConnector() && !service.IsBuiltin() {
		clean, password, err := mcp.SplitDatabaseURL(service.Kind, service.URL)
		if err != nil {
			return MCPServiceView{}, err
		}
		service.URL, missing = clean, "请填写数据库密码"
		// Passwords may legitimately start or end with spaces; only tokens are trimmed.
		if token = request.Token; strings.TrimSpace(token) == "" {
			token = password
		}
	}
	if err := service.Validate(); err != nil {
		return MCPServiceView{}, err
	}
	switch {
	case service.IsBuiltin():
		if request.Token != "" {
			return MCPServiceView{}, errors.New("内置案例无需凭据")
		}
	case service.IsOAuth():
		// Credentials come only from AuthorizeMCPService; a pasted token is ignored.
	case service.Auth == mcp.AuthNone:
		if existing {
			if err := a.vault.Delete(ctx, service.KeyRef); err != nil {
				return MCPServiceView{}, err
			}
		}
	case token != "":
		if err := a.vault.Put(ctx, service.KeyRef, []byte(token)); err != nil {
			return MCPServiceView{}, err
		}
	case !existing:
		return MCPServiceView{}, errors.New(missing)
	default:
		if saved, err := a.vault.Read(ctx, service.KeyRef); err != nil {
			return MCPServiceView{}, errors.New(missing)
		} else {
			clear(saved)
		}
	}
	if err := a.store.SaveMCPService(service); err != nil {
		return MCPServiceView{}, err
	}
	if moved {
		if err := a.dropGrants(service.ID); err != nil {
			return MCPServiceView{}, err
		}
	}
	toolsErr := ""
	if !service.IsConnector() && !service.IsOAuth() {
		// The user chooses tools per sandbox, so list them now; OAuth services list after login.
		if listed, err := a.listServiceTools(ctx, service); err != nil {
			toolsErr = err.Error()
		} else {
			service = listed
		}
	}
	view, err := a.mcpView(service)
	view.ToolsError = toolsErr
	if err != nil {
		return view, err
	}
	if existing {
		return view, a.refreshSandboxesUsing(ctx, service.ID)
	}
	return view, nil
}

// dropGrants removes the tool grants sandboxes hold for a service whose address changed: they
// name tools of the old server, even when the new one has tools of the same name. Each sandbox
// then gets the new server's read-only default once its tools are listed.
func (a *App) dropGrants(serviceID string) error {
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return err
	}
	for _, sbx := range sandboxes {
		if _, granted := sbx.MCPPermissions[serviceID]; !granted {
			continue
		}
		if err := a.store.UpdateSandbox(sbx.ID, func(s *sandbox.Sandbox) error {
			delete(s.MCPPermissions, serviceID)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// listServiceTools fetches a remote service's tools from the Mac with the credential the app
// holds and saves them as the service's tool list. Grants in sandboxes are not changed.
func (a *App) listServiceTools(ctx context.Context, service mcp.Service) (mcp.Service, error) {
	upstream, err := service.Upstream()
	if err != nil {
		return service, err
	}
	token := ""
	switch {
	case service.Auth == mcp.AuthNone:
	case service.IsOAuth():
		creds, _, err := a.oauth.AccessToken(ctx, a.vault, service.KeyRef, time.Minute)
		if err != nil {
			return service, err
		}
		token = creds.AccessToken
	default:
		data, err := a.vault.Read(ctx, service.KeyRef)
		if err != nil {
			return service, errors.New("钥匙串中没有这个服务的令牌")
		}
		token = string(data)
		clear(data)
	}
	tools, err := mcp.ListTools(ctx, a.client, upstream, token)
	if err != nil {
		return service, toolListError(service, err)
	}
	now := a.now().UTC()
	service.Catalog, service.CatalogAt = tools, &now
	return service, a.store.SaveMCPService(service)
}

// toolListError says in plain words why a remote service's tools could not be listed.
func toolListError(service mcp.Service, err error) error {
	var status *mcp.StatusError
	var rpc *mcp.RPCError
	switch {
	case errors.As(err, &status) && (status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden):
		if service.Auth == mcp.AuthNone {
			return errors.New("服务要求令牌或登录，不能按“无需令牌”使用")
		}
		return errors.New("服务可达，但令牌无效或没有权限")
	case errors.As(err, &status):
		return fmt.Errorf("服务返回了 HTTP %d，没有拿到工具列表", status.Status)
	case errors.As(err, &rpc), errors.Is(err, mcp.ErrInvalidAnswer):
		return err
	}
	host, _, _ := service.Endpoint()
	return fmt.Errorf("无法连接到 %s（%s）", host, networkReason(err))
}

// RefreshMCPTools lists a remote service's tools again, e.g. after the service added tools. New
// tools stay closed in every sandbox until the user grants them.
func (a *App) RefreshMCPTools(id string) (MCPServiceView, error) {
	service, err := a.store.MCPService(id)
	if err != nil {
		return MCPServiceView{}, err
	}
	if service.IsConnector() {
		return MCPServiceView{}, errors.New("这个服务的工具由 AIofCage 固定，无需刷新")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	listed, err := a.listServiceTools(ctx, service)
	if err != nil {
		return MCPServiceView{}, err
	}
	return a.mcpView(listed)
}

// newMCPID reuses the preset ID (e.g. "github") when free, so agents see a readable server name.
func (a *App) newMCPID(preset string) (string, error) {
	services, err := a.store.MCPServices()
	if err != nil {
		return "", err
	}
	taken := map[string]bool{"filesystem": true}
	for _, s := range services {
		taken[s.ID] = true
	}
	if preset != "" && !strings.HasPrefix(preset, "custom") && mcp.ValidID(preset) && !taken[preset] {
		return preset, nil
	}
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id := fmt.Sprintf("mcp-%x", b)
		if !taken[id] {
			return id, nil
		}
	}
}

func (a *App) DeleteMCPService(id string) error {
	service, err := a.store.MCPService(id)
	if err != nil {
		return err
	}
	view, err := a.mcpView(service)
	if err != nil {
		return err
	}
	if len(view.UsedBy) > 0 {
		return fmt.Errorf("还有沙箱在用这个 MCP 服务（%s），请先在沙箱里停用它", strings.Join(view.UsedBy, "、"))
	}
	if err := a.store.DeleteMCPService(id); err != nil {
		return err
	}
	if service.IsBuiltin() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.vault.Delete(ctx, service.KeyRef)
}

// TestMCPService sends an MCP initialize request from the desktop process.
func (a *App) TestMCPService(id string) (TestResult, error) {
	service, err := a.store.MCPService(id)
	if err != nil {
		return TestResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if service.IsBuiltin() {
		if a.manager == nil {
			return TestResult{Message: "请先安装并启动运行环境"}, nil
		}
		checkCtx, stop := context.WithTimeout(context.Background(), 90*time.Second)
		defer stop()
		ok, message, err := a.manager.CheckConnector(checkCtx, service, "")
		if err != nil {
			return TestResult{Message: "案例连接检查未完成：" + err.Error()}, nil
		}
		if ok {
			message = "连接成功，合成业务案例的只读工具已响应"
		}
		return TestResult{OK: ok, Message: message}, nil
	}
	if !service.IsConnector() {
		// A remote service is tested the way an agent uses it, and its tool list is updated.
		listCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		started := a.now()
		listed, err := a.listServiceTools(listCtx, service)
		result := TestResult{LatencyMS: a.now().Sub(started).Milliseconds()}
		if err != nil {
			result.Message = err.Error()
			return result, nil
		}
		result.OK = true
		result.Message = fmt.Sprintf("连接正常：服务提供 %d 个工具，其中 %d 个标注为只读", len(listed.Catalog), len(listed.DefaultTools()))
		return result, nil
	}
	token, err := a.vault.Read(ctx, service.KeyRef)
	if err != nil {
		return TestResult{Message: "钥匙串中没有这个服务的令牌"}, nil
	}
	defer clear(token)
	return a.testDatabase(service, string(token)), nil
}

// SetSandboxMCP chooses the MCP services a sandbox may use; running sandboxes update at once.
func (a *App) SetSandboxMCP(id string, ids []string, filesystem bool) error {
	return a.SetSandboxMCPPermissions(id, ids, filesystem, nil)
}

// SetSandboxMCPPermissions updates enabled MCP services and connector tool grants.
func (a *App) SetSandboxMCPPermissions(id string, ids []string, filesystem bool, permissions map[string][]string) error {
	if err := a.runtimeReady(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return a.manager.SetMCPPermissions(ctx, id, ids, filesystem, permissions)
}

// testDatabase logs in through a throwaway connector and runs a read-only query, which checks the
// password and the path from the VM. Before the connector image exists (it is built when the
// first sandbox with a database starts), or without the VM, it falls back to a port check.
func (a *App) testDatabase(service mcp.Service, password string) TestResult {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fallback := "（完整的登录检查需要虚拟机，且第一次启动带数据库的沙箱后才可用）"
	if a.manager != nil {
		started := a.now()
		ok, message, err := a.manager.CheckConnector(ctx, service, password)
		result := TestResult{LatencyMS: a.now().Sub(started).Milliseconds()}
		switch {
		case errors.Is(err, runtime.ErrConnectorNotBuilt):
			fallback = "（完整的登录检查会在第一次启动带数据库的沙箱后可用）"
		case err != nil:
			fallback = "（无法运行登录检查：" + err.Error() + "）"
		case ok:
			result.OK, result.Message = true, "登录成功：连接器用保存的密码连上了数据库，只读查询正常"
			return result
		default:
			result.Message = "连接器登录数据库失败：" + databaseReason(message)
			return result
		}
	}
	result := a.testDatabasePort(ctx, service)
	if result.OK {
		result.Message = "数据库端口可以连通，但还没验证密码" + fallback
	}
	return result
}

// databaseReason turns common driver errors into plain language and keeps the original.
func databaseReason(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "password authentication failed") || strings.Contains(lower, "authentication failed"):
		return "用户名或密码不对（" + message + "）"
	case strings.Contains(lower, "does not exist"):
		return "数据库或用户不存在（" + message + "）"
	case strings.Contains(lower, "econnrefused"):
		return "连不上数据库端口，请确认数据库正在运行"
	case strings.Contains(lower, "enotfound") || strings.Contains(lower, "eai_again"):
		return "找不到数据库主机，请检查连接串里的主机名"
	case strings.Contains(lower, "timeout") || strings.Contains(message, "超时"):
		return "连接超时"
	}
	return message
}

// testDatabasePort checks from the Mac that the database port is reachable.
func (a *App) testDatabasePort(ctx context.Context, service mcp.Service) TestResult {
	u, err := url.Parse(service.URL)
	if err != nil {
		return TestResult{Message: "连接串无效"}
	}
	port := u.Port()
	if spec, ok := mcp.ConnectorFor(service.Kind); ok && port == "" {
		port = spec.DefaultPort
	}
	address := net.JoinHostPort(u.Hostname(), port)
	started := a.now()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	result := TestResult{LatencyMS: a.now().Sub(started).Milliseconds()}
	if err != nil {
		result.Message = "连不上 " + address + "（" + networkReason(err) + "）"
		return result
	}
	conn.Close()
	result.OK = true
	return result
}
