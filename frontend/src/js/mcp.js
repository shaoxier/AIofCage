// MCP 页：令牌、OAuth 授权和数据库密码只保存在钥匙串，沙箱里只有占位符。
// Agent 一律经门卫的 MCP 入口访问服务，门卫逐条检查，只放行沙箱里勾选的工具。远程服务的令牌
// 由门卫转发时加上（OAuth 令牌由应用在浏览器登录后获取并自动续期）；数据库由沙箱外的可信连接器代持密码。
import { useState } from "preact/hooks";
import { html, call, ConfirmDialog, Icon, PageTitle } from "./core.js";
import { DemoCases } from "./demo-cases.js";

export function MCPPage({ services, presets, providers, reload, notify, onCreated }) {
  const [dialog, setDialog] = useState(null);
  return html`
    <section class="page">
      <div class="section-heading">
        <${PageTitle} icon="plug" title="工具连接" description="管理 MCP 服务，再到沙箱中选择启用。凭据保存在 macOS 钥匙串。" />
        <button class="primary" onClick=${() => setDialog({})}><${Icon} name="plus" />添加 MCP 服务</button>
      </div>
      <div class="card-grid">
        <article class="panel provider-card builtin">
          <div class="card-top"><div class="card-identity"><span class="card-icon"><${Icon} name="folder" size=${22} /></span><h3>项目文件（filesystem）</h3></div><span class="pill ok">内置</span></div>
          <p>读取和修改当前项目文件，创建沙箱时勾选启用。仅访问当前项目目录，无需凭据。</p>
        </article>
        ${services.map((s) => html`<${ServiceCard} key=${s.id} service=${s} onEdit=${() => setDialog({ service: s })} reload=${reload} notify=${notify} />`)}
      </div>
      <p class="caption">在沙箱的“概览”中选择启用工具连接，并逐个勾选允许的工具。支持 Codex、Claude Code 与 DeepSeek Harness。</p>
      <${DemoCases} providers=${providers} reload=${reload} notify=${notify} onCreated=${onCreated} />
      ${dialog && html`<${ServiceDialog} presets=${presets} initial=${dialog.service} onClose=${() => { setDialog(null); reload(); }}
        onSaved=${async (saved) => {
          setDialog(null);
          notify(saved.tools_error ? `已保存「${saved.name}」，但还没拿到工具列表：${saved.tools_error}` : `已保存「${saved.name}」`, Boolean(saved.tools_error));
          await reload();
        }} />`}
    </section>`;
}

function ServiceCard({ service, onEdit, reload, notify }) {
  const [result, setResult] = useState(null);
  const [testing, setTesting] = useState(false);
  const [authorizing, setAuthorizing] = useState(false);
  const [disconnecting, setDisconnecting] = useState(false);
  const authorize = async () => {
    setAuthorizing(true);
    setResult(null);
    try { await call("AuthorizeMCPService", service.id); notify(`「${service.name}」已授权`); await reload(); }
    catch (e) { setResult({ ok: false, message: e.message }); }
    finally { setAuthorizing(false); }
  };
  const disconnect = async () => {
    setDisconnecting(true);
    setResult(null);
    try { await call("DisconnectMCPService", service.id); notify(`已断开「${service.name}」的本机授权`); await reload(); }
    catch (e) { setResult({ ok: false, message: e.message }); }
    finally { setDisconnecting(false); }
  };
  const test = async () => {
    setTesting(true);
    setResult(null);
    try {
      setResult(await call("TestMCPService", service.id));
      if (isRemote(service)) await reload(); // 测试远程服务会顺带更新它的工具列表
    } catch (e) { setResult({ ok: false, message: e.message }); }
    finally { setTesting(false); }
  };
  const [confirming, setConfirming] = useState(false);
  const [removing, setRemoving] = useState(false);
  const remove = async () => {
    setRemoving(true);
    try { await call("DeleteMCPService", service.id); notify(`已删除「${service.name}」`); setConfirming(false); await reload(); }
    catch (e) { setConfirming(false); notify(e.message, true); }
    finally { setRemoving(false); }
  };
  const database = isDatabase(service);
  const oauth = isOAuth(service);
  const secret = database ? "密码" : "令牌";
  const builtin = isBuiltin(service);
  const open = service.auth === "none";
  const status = builtin ? html`<span class="pill ok">内置 · 无需凭据</span>` : open ? html`<span class="pill ok">无需令牌</span>` : oauth
    ? (service.key_saved ? html`<span class="pill ok">已授权</span>` : html`<span class="pill warn">未登录</span>`)
    : (service.key_saved ? html`<span class="pill ok">${secret}已保存</span>` : html`<span class="pill warn">缺少${secret}</span>`);
  const kind = builtin ? "合成业务案例 · 只读工具" : database ? "PostgreSQL · 只读查询"
    : oauth ? "远程 MCP · 浏览器授权" : open ? "远程 MCP · 公开服务" : "远程 MCP · 访问令牌";
  return html`
    <article class="panel provider-card">
      <div class="card-top"><div class="card-identity"><span class="card-icon"><${Icon} name=${database ? "database" : "plug"} size=${22} /></span><h3>${service.name}</h3></div>${status}</div>
      <p class="mono">${service.url}</p>
      <dl class="facts">
        <div><dt>类型</dt><dd>${kind}</dd></div>
        <div><dt>名称</dt><dd class="mono">${service.id}</dd></div>
        <div><dt>工具</dt><dd>${toolSummary(service)}</dd></div>
        <div><dt>使用中</dt><dd>${service.used_by.length ? service.used_by.join("、") : "暂无沙箱"}</dd></div>
      </dl>
      ${result && html`<p class=${result.ok ? "test-result ok" : "test-result bad"} role="status">${result.ok ? "✓" : "✕"} ${result.message}</p>`}
      ${authorizing && html`<p class="test-result" role="status">请在打开的浏览器页面里登录并同意授权…</p>`}
      <div class="card-actions">
        ${oauth && html`<button class=${service.key_saved ? "" : "primary"} onClick=${authorize} disabled=${authorizing || disconnecting}>
          ${authorizing ? "等待授权…" : service.key_saved ? "重新登录" : "登录授权"}</button>`}
        ${oauth && service.key_saved && html`<button onClick=${disconnect} disabled=${disconnecting}>${disconnecting ? "断开中…" : "断开授权"}</button>`}
        <button onClick=${test} disabled=${testing || (oauth && !service.key_saved)}>${testing ? "测试中…" : "测试连接"}</button>
        <button onClick=${onEdit}>编辑</button>
        <button class="danger" onClick=${() => setConfirming(true)} disabled=${service.used_by.length > 0}
          title=${service.used_by.length ? "仍有沙箱在用，先在沙箱里停用它" : ""}>删除</button>
      </div>
      ${confirming && html`<${ConfirmDialog} title=${`删除 MCP 服务「${service.name}」？`} busy=${removing}
        message=${builtin ? "移除这个案例的数据连接，已生成的项目报告会保留。" : `钥匙串里的${database ? "密码" : oauth ? "授权" : "令牌"}也会删除。`}
        onConfirm=${remove} onClose=${() => setConfirming(false)} />`}
    </article>`;
}

// 远程服务的工具来自服务自己列出的清单；“只读”是服务的标注，只用来决定默认开放哪些。
function toolSummary(service) {
  if (!isRemote(service)) return `${service.tools?.length ?? 0} 个（由 AIofCage 固定）`;
  if (!service.catalog_at) return "还没获取，点“测试连接”获取";
  return `${service.tools.length} 个，其中 ${service.default_tools.length} 个标注只读`;
}

function ServiceDialog({ presets, initial, onClose, onSaved }) {
  const editing = Boolean(initial);
  const [form, setForm] = useState(() => editing ? { ...initial, token: "", no_auth: initial.auth === "none" } : null);
  const [preset, setPreset] = useState(null);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState("");
  const set = (key) => (e) => setForm({ ...form, [key]: e.target.value });
  // 令牌只属于它当初填写时的域名：换了域名必须重新填写。
  const hostChanged = editing && hostOf(form?.url) !== hostOf(initial.url);
  const keepsToken = editing && initial.auth !== "none" && !hostChanged;
  const submit = async (e) => {
    e.preventDefault();
    setSaving("保存中…");
    setError("");
    try {
      let saved = await call("SaveMCPService", { id: form.id ?? "", name: form.name, kind: form.kind ?? "", url: form.url,
        preset: form.preset ?? "", token: form.no_auth ? "" : form.token, no_auth: Boolean(form.no_auth) });
      setForm({ ...form, id: saved.id, token: "" });
      if (isOAuth(saved) && !saved.key_saved) {
        setSaving("等待浏览器授权…");
        saved = await call("AuthorizeMCPService", saved.id);
      }
      onSaved(saved);
    } catch (err) { setError(err.message); } finally { setSaving(""); }
  };
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="dialog" role="dialog" aria-modal="true" aria-label=${editing ? "编辑 MCP 服务" : "添加 MCP 服务"}>
        <div class="dialog-heading"><h2>${editing ? "编辑 MCP 服务" : form ? "填写 MCP 服务" : "选择 MCP 服务"}</h2>
          <button type="button" class="icon-button" aria-label="关闭" onClick=${onClose}><${Icon} name="x" size=${20} /></button></div>
        ${!form
          ? html`<div class="preset-grid">${presets.map((p) => html`<button class="preset" onClick=${() => { setPreset(p); setForm({ name: p.name, kind: p.kind ?? "", url: p.url, preset: p.id, token: "" }); }}>
              <strong>${p.name}</strong><span class="mono">${p.url || (p.kind === "postgres" ? "填写连接串" : "自己填写地址")}</span>${p.note && html`<small>${p.note}</small>`}</button>`)}</div>`
          : html`<form onSubmit=${submit}>
              <label for="m-name">名称</label>
              <input id="m-name" required maxlength="60" value=${form.name} onInput=${set("name")} />
              ${isBuiltin(form) ? html`<p class="caption">合成业务案例，只开放三个只读工具，无需密钥。数据版本随应用发布。</p>` : isOAuth(form)
                ? html`<label for="m-url">地址（streamable HTTP）</label>
                  <input id="m-url" required class="mono" placeholder="https://" value=${form.url} onInput=${set("url")} />
                  <p class="caption">保存后会打开浏览器，请在服务的官方页面登录并同意授权，不需要复制任何令牌。${""
                    }授权保存在 macOS 钥匙串，由 Ai of Cage 自动续期；沙箱里只有占位符，门卫在请求发出时注入令牌。${""
                    }${preset?.docs ? html` 参考：<span class="mono">${preset.docs}</span>` : ""}</p>
                  ${saving === "等待浏览器授权…" && html`<p class="test-result" role="status">请在打开的浏览器页面里登录并同意授权…</p>`}`
                : isDatabase(form)
                ? html`<label for="m-url">连接串</label>
                  <input id="m-url" required class="mono" spellcheck="false" placeholder="postgresql://用户名@localhost:5432/数据库名"
                    value=${form.url} onInput=${set("url")} />
                  <label for="m-token">数据库密码</label>
                  <input id="m-token" type="password" autocomplete="off" spellcheck="false" class="mono"
                    placeholder=${editing ? "留空则保持原密码不变" : "连接串里已带密码可留空"} value=${form.token} onInput=${set("token")} />
                  <p class="caption">密码直接写入 macOS 钥匙串（连接串里带的密码也会自动移过去），只交给沙箱外的可信连接器；${""
                    }必须使用专用只读账号，管理账号或带写权限的账号无法查询。本机数据库填 localhost 即可。</p>`
                : html`<label for="m-url">地址（streamable HTTP）</label>
                  <input id="m-url" required class="mono" placeholder="https://" value=${form.url} onInput=${set("url")} />
                  ${form.preset !== "github" && html`<label class="check"><input type="checkbox" checked=${Boolean(form.no_auth)}
                    onChange=${(e) => setForm({ ...form, no_auth: e.target.checked })} />这个服务不需要令牌（公开服务，例如文档检索）</label>`}
                  ${!form.no_auth && html`<label for="m-token">访问令牌</label>
                  <input id="m-token" type="password" autocomplete="off" spellcheck="false" class="mono" required=${!keepsToken}
                    placeholder=${keepsToken ? "留空则保持原令牌不变" : hostChanged ? "换了域名，请重新填写令牌（原令牌不会发给新地址）" : "粘贴令牌，例如 GitHub 的 PAT"}
                    value=${form.token} onInput=${set("token")} />`}
                  <p class="caption">${form.no_auth ? "不保存任何凭据。" : "令牌直接写入 macOS 钥匙串，沙箱里只会看到占位符。"}${""
                    }Agent 经门卫访问这个服务，只能调用你在沙箱里勾选的工具；保存后会自动获取工具列表。${preset?.docs ? html` 参考：<span class="mono">${preset.docs}</span>` : ""}</p>`}
              ${error && html`<p class="form-error" role="alert">${error}</p>`}
              <div class="dialog-actions">
                ${!editing && html`<button type="button" onClick=${() => setForm(null)}>返回</button>`}
                <button class="primary" type="submit" disabled=${Boolean(saving)}>
                  ${saving || (isOAuth(form) && !form.key_saved ? "保存并登录" : "保存")}</button>
              </div>
            </form>`}
      </div>
    </div>`;
}

const hostOf = (url) => { try { return new URL(url).hostname.toLowerCase(); } catch { return ""; } };
const isOAuth = (service) => service?.kind === "oauth";
const isBuiltin = (service) => service?.kind === "demo";
const isDatabase = (service) => service?.kind === "postgres";
const isRemote = (service) => !isBuiltin(service) && !isDatabase(service);

// cleanGrants 去掉服务已经不再提供的工具，避免保存时引用不存在的工具。
export function cleanGrants(services, grants = {}) {
  const result = {};
  for (const [id, tools] of Object.entries(grants)) {
    const service = services.find((item) => item.id === id);
    result[id] = service ? tools.filter((tool) => (service.tools ?? []).includes(tool)) : tools;
  }
  return result;
}

// 沙箱里勾选启用哪些 MCP；创建向导和沙箱行共用。每个服务按工具授权：连接器的工具由 AIofCage 固定，
// 远程服务的工具来自服务列出的清单，默认只开放它标注为只读的工具，之后新出现的工具默认不开放。
export function MCPChoices({ services, selected, filesystem, permissions = {}, onChange }) {
  const update = (ids, fs, grants = permissions) => onChange(ids, fs, grants);
  const granted = (service) => (permissions[service.id] ?? service.default_tools ?? service.tools ?? [])
    .filter((tool) => (service.tools ?? []).includes(tool));
  const toggle = (service) => {
    const grants = { ...permissions };
    if (selected.includes(service.id)) {
      delete grants[service.id];
      update(selected.filter((x) => x !== service.id), filesystem, grants);
      return;
    }
    if (service.tools?.length && !Object.prototype.hasOwnProperty.call(grants, service.id)) {
      grants[service.id] = [...(service.default_tools ?? service.tools)];
    }
    update([...selected, service.id], filesystem, grants);
  };
  const setTools = (service, tools) => update(selected, filesystem, { ...permissions, [service.id]: tools });
  const toggleTool = (service, tool) => {
    const current = granted(service);
    setTools(service, current.includes(tool) ? current.filter((item) => item !== tool) : [...current, tool]);
  };
  return html`<div class="agent-pick">
    <label class="check"><input type="checkbox" checked=${filesystem} onChange=${(e) => update(selected, e.target.checked)} />
      项目文件（filesystem，内置，无需密钥）</label>
    ${services.map((s) => html`<div class="mcp-choice" key=${s.id}>
      <label class="check"><input type="checkbox" checked=${selected.includes(s.id)} onChange=${() => toggle(s)} />
        ${s.name}${isDatabase(s) ? "（数据库，只读）" : isBuiltin(s) ? "（合成数据，只读）" : ""}${!s.key_saved && !isBuiltin(s)
          ? html` <span class="pill warn">${isOAuth(s) ? "未登录" : `缺少${isDatabase(s) ? "密码" : "令牌"}`}</span>` : ""}</label>
      ${selected.includes(s.id) && html`<${ToolGrant} service=${s} granted=${granted(s)}
        onToggle=${(tool) => toggleTool(s, tool)} onSet=${(tools) => setTools(s, tools)} />`}
    </div>`)}
  </div>`;
}

// 标签只转述服务自己的标注：标了只读、标了可能删改，或者什么都没标。
const toolTag = (tool) => tool?.read_only ? html`<span class="pill ok">标注只读</span>`
  : tool?.destructive ? html`<span class="pill bad">标注可能删改</span>` : html`<span class="pill warn">未标注只读</span>`;

function ToolGrant({ service, granted, onToggle, onSet }) {
  const tools = service.tools ?? [];
  const remote = isRemote(service);
  if (!tools.length) {
    return remote ? html`<div class="mcp-tool-list"><small>还没获取到这个服务的工具列表，Agent 暂时用不了它的工具。${""
      }到「工具连接」页点“测试连接”即可获取；启动沙箱时也会自动尝试。</small></div>` : "";
  }
  const info = Object.fromEntries((service.catalog ?? []).map((tool) => [tool.name, tool]));
  return html`<div class="mcp-tool-list" aria-label=${`${service.name} 工具权限`}>
    <div class="mcp-tool-head"><small>允许的工具（${granted.length}/${tools.length}）${remote ? "。标签转述服务自己的标注，新工具默认不开放" : ""}</small>
      ${remote && html`<span class="mcp-tool-actions">
        <button type="button" class="link" onClick=${() => onSet([...(service.default_tools ?? [])])}>只开放标注只读的</button>
        <button type="button" class="link" onClick=${() => onSet([])}>全部关闭</button></span>`}</div>
    <div class="mcp-tool-scroll">
      ${tools.map((tool) => html`<label class="check" title=${info[tool]?.description ?? ""}>
        <input type="checkbox" checked=${granted.includes(tool)} onChange=${() => onToggle(tool)} />
        <span class="mono">${tool}</span>${info[tool]?.title ? html`<span class="muted">${info[tool].title}</span>` : ""}
        ${remote && toolTag(info[tool])}</label>`)}
    </div>
  </div>`;
}
