package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/shaoxier/AIofCage/internal/mcp"
	"github.com/shaoxier/AIofCage/internal/runtime"
)

// oauthCheckInterval is how often tokens of OAuth services used by running sandboxes are checked.
const oauthCheckInterval = time.Minute

func openURL(target string) error {
	if !strings.HasPrefix(target, "https://") {
		return errors.New("只打开 https 地址")
	}
	return exec.Command("/usr/bin/open", target).Run()
}

// AuthorizeMCPService runs the OAuth login in the user's browser and keeps the credentials in the
// Keychain. Running sandboxes that use the service get the new access token at once.
func (a *App) AuthorizeMCPService(id string) (MCPServiceView, error) {
	service, err := a.store.MCPService(id)
	if err != nil {
		return MCPServiceView{}, err
	}
	if !service.IsOAuth() {
		return MCPServiceView{}, errors.New("这个服务不使用 OAuth 登录")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	// Logging in again may switch accounts: the old account's token must not stay in use.
	relogin := false
	if previous, err := a.vault.Read(ctx, service.KeyRef); err == nil {
		relogin = true
		clear(previous)
	}
	creds, err := a.oauth.Login(ctx, service.URL)
	if err != nil {
		return MCPServiceView{}, err
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return MCPServiceView{}, err
	}
	// The login can take minutes; the credential is saved only if the service still has the
	// address it was obtained for.
	if err := a.saveLogin(ctx, service, data); err != nil {
		return MCPServiceView{}, err
	}
	toolsErr := ""
	if listed, err := a.listServiceTools(ctx, service); err != nil {
		toolsErr = err.Error()
	} else {
		service = listed
	}
	view, err := a.mcpView(service)
	view.ToolsError = toolsErr
	if err != nil {
		return view, err
	}
	return view, a.refreshSandboxesUsing(ctx, service.ID, relogin)
}

func (a *App) saveLogin(ctx context.Context, service mcp.Service, data []byte) error {
	unlock, err := a.store.LockOperation(ctx, runtime.ServiceLock(service.ID), true)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := a.store.MCPService(service.ID)
	if err != nil {
		return err
	}
	if current.URL != service.URL || !current.IsOAuth() {
		return errors.New("登录期间这个服务的地址被修改了，请重新登录授权")
	}
	return a.vault.Put(ctx, service.KeyRef, data)
}

// DisconnectMCPService removes the locally stored OAuth credentials and refreshes every running
// sandbox that used the service. Provider-side grants are not revoked unless the provider exposes
// a revocation endpoint; the local credential is always removed.
func (a *App) DisconnectMCPService(id string) (MCPServiceView, error) {
	service, err := a.store.MCPService(id)
	if err != nil {
		return MCPServiceView{}, err
	}
	if !service.IsOAuth() {
		return MCPServiceView{}, errors.New("只有 OAuth 服务支持断开授权")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	unlock, err := a.store.LockOperation(ctx, runtime.ServiceLock(id), true)
	if err != nil {
		return MCPServiceView{}, err
	}
	err = a.vault.Delete(ctx, service.KeyRef)
	unlock()
	if err != nil {
		return MCPServiceView{}, err
	}
	view, err := a.mcpView(service)
	if err != nil {
		return view, err
	}
	return view, a.refreshSandboxesUsing(ctx, service.ID, true)
}

// refreshTimeout bounds the refresh of one sandbox, including waiting for its lock.
const refreshTimeout = 2 * time.Minute

// refreshSandboxesUsing re-delivers configuration to running sandboxes that use a service.
// mayNarrow says the change may take something away (an edit, a disconnect, another login): such
// sandboxes go offline when the refresh fails, and the monitor delivers again until it succeeds;
// a missed new credential is retried with the OAuth checks. One failure does not stop the others,
// and each sandbox gets its own time: a revoked login must reach every gatekeeper that holds it.
func (a *App) refreshSandboxesUsing(ctx context.Context, serviceID string, mayNarrow bool) error {
	if a.manager == nil {
		return nil
	}
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return err
	}
	refresh := a.manager.RefreshCredentials
	if mayNarrow {
		refresh = a.manager.Refresh
	}
	var errs []error
	for _, sbx := range sandboxes {
		if slices.Contains(sbx.MCP, serviceID) {
			one, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
			err := refresh(one, sbx.ID)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("已保存，但更新运行中的沙箱「%s」失败：%w", sbx.Name, err))
				if mayNarrow {
					a.markStale(sbx.ID, err)
				} else {
					a.monitor.mu.Lock()
					a.monitor.oauthPending[serviceID] = true
					a.monitor.mu.Unlock()
				}
			}
		}
	}
	return errors.Join(errs...)
}

// staleRetries is how many times, a minute apart, the monitor delivers the stored settings again
// to a sandbox whose refresh failed. A cause that lasts longer (an older gatekeeper) needs a restart.
const staleRetries = 10

// markStale remembers a sandbox whose last refresh failed, so the monitor delivers its stored
// settings again; it returns err unchanged.
func (a *App) markStale(id string, err error) error {
	if err != nil {
		a.monitor.mu.Lock()
		a.monitor.stale[id] = 0
		a.monitor.mu.Unlock()
	}
	return err
}

// retryStale delivers the stored settings again, with refresh, to sandboxes whose last refresh
// failed. A sandbox that went offline comes back once it succeeds, and a gatekeeper that another
// operation reconfigured meanwhile gets the stored settings. Stopped sandboxes succeed at once.
func (a *App) retryStale(ctx context.Context, refresh func(context.Context, string) error) {
	a.monitor.mu.Lock()
	ids := slices.Sorted(maps.Keys(a.monitor.stale))
	a.monitor.mu.Unlock()
	for _, id := range ids {
		_, err := a.store.Sandbox(id)
		if err == nil {
			one, cancel := context.WithTimeout(ctx, refreshTimeout)
			err = refresh(one, id)
			cancel()
		}
		a.monitor.mu.Lock()
		if a.monitor.stale[id]++; err == nil || a.monitor.stale[id] >= staleRetries {
			delete(a.monitor.stale, id)
		}
		a.monitor.mu.Unlock()
	}
}

// refreshOAuth renews access tokens that running sandboxes will need soon and hands the new ones
// to their gatekeepers, so long agent sessions are not cut off when a token expires.
func (a *App) refreshOAuth(ctx context.Context) {
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return
	}
	checked := map[string]bool{}
	for _, sbx := range sandboxes {
		for _, id := range sbx.MCP {
			if checked[id] {
				continue
			}
			checked[id] = true
			service, err := a.store.MCPService(id)
			if err != nil || !service.IsOAuth() || !a.usedByRunning(ctx, id) {
				continue
			}
			// Renew well before the gatekeeper's copy expires, under the service's lock so an edit of
			// its address cannot interleave with writing the renewed credential.
			unlock, err := a.store.LockOperation(ctx, runtime.ServiceLock(id), true)
			if err != nil {
				continue
			}
			_, refreshed, err := a.oauth.AccessToken(ctx, a.vault, service.KeyRef, 2*runtime.OAuthMargin)
			unlock()
			a.monitor.mu.Lock()
			alerted := a.monitor.oauthAlerted[id]
			a.monitor.oauthAlerted[id] = err != nil
			pending := a.monitor.oauthPending[id]
			a.monitor.mu.Unlock()
			if err != nil {
				if !alerted {
					a.emit("sandbox:alert", map[string]string{"id": sbx.ID,
						"message": fmt.Sprintf("MCP 服务「%s」需要重新登录授权：%s", service.Name, err.Error())})
				}
				continue
			}
			if refreshed || pending {
				// A renewal takes nothing away, so a missed push only leaves the old token in place;
				// it is retried on the next check.
				err := a.refreshSandboxesUsing(ctx, id, false)
				a.monitor.mu.Lock()
				a.monitor.oauthPending[id] = err != nil
				a.monitor.mu.Unlock()
				if err != nil && !pending {
					a.emit("sandbox:alert", map[string]string{"id": sbx.ID, "message": err.Error()})
				}
			}
		}
	}
}

func (a *App) usedByRunning(ctx context.Context, serviceID string) bool {
	sandboxes, err := a.store.Sandboxes()
	if err != nil {
		return false
	}
	for _, sbx := range sandboxes {
		for _, id := range sbx.MCP {
			if id == serviceID && a.manager.Status(ctx, sbx.ID).State == "running" {
				return true
			}
		}
	}
	return false
}
