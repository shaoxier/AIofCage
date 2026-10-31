// 供应商页：参考 CC Switch，按 Agent 分组管理模型供应商；密钥只写入钥匙串，界面从不读取。
import { useEffect, useRef, useState } from "preact/hooks";
import { html, call, AGENTS, agentName, PROTOCOLS, COMPATIBLE, AUTHS, formatTokens, ConfirmDialog, Icon, agentIcon, PageTitle } from "./core.js";
import { PricingLookup } from "./pricing.js";
import { ModelPicker } from "./model-picker.js";

export function ProvidersPage({ providers, presets, summary, reload, notify, tutorial }) {
  const [agent, setAgent] = useState(tutorial?.state.agent ?? "codex");
  useEffect(() => { if (tutorial) setAgent(tutorial.state.agent); }, [tutorial?.state.agent]);
  const [dialog, setDialog] = useState(null); // { mode: "add" | "edit", provider? }
  const list = providers.filter((p) => p.agent === agent);
  return html`
    <section class="page">
      <div class="section-heading">
        <${PageTitle} icon="layers" title="模型供应商" description="按智能体管理供应商，在各个沙箱中独立选择。" />
        <button class="primary" data-guide="add-provider" onClick=${() => setDialog({ mode: "add" })} disabled=${tutorial?.saving}><${Icon} name="plus" />添加供应商</button>
      </div>
      <div class="tabs" role="tablist">
        ${AGENTS.map(
          (item) => html`<button role="tab" aria-selected=${item.id === agent} tabindex=${item.id === agent ? 0 : -1} class=${item.id === agent ? "tab active" : "tab"}
            disabled=${tutorial?.saving} onClick=${() => tutorial ? tutorial.selectAgent(item.id) : setAgent(item.id)}><${Icon} name=${agentIcon(item.id)} size=${19} />${item.name}<span class="count">${providers.filter((p) => p.agent === item.id).length}</span></button>`
        )}
      </div>
      ${list.length === 0
        ? html`<div class="panel empty">
            <span class="empty-icon"><${Icon} name="layers" size=${40} /></span>
            <h3>还没有 ${agentName(agent)} 的供应商</h3>
            <p>从预设里选一个，填入 API Key 即可。</p>
            <button class="primary" onClick=${() => setDialog({ mode: "add" })}>从预设添加</button>
          </div>`
        : html`<div class="card-grid">${list.map(
            (p) => html`<${ProviderCard} key=${p.id} provider=${p} usage=${summary?.providers?.[p.id]} onEdit=${() => setDialog({ mode: "edit", provider: p })}
              reload=${reload} notify=${notify} onTest=${tutorial?.providerTested} guided=${tutorial?.state.provider_id === p.id} />`
          )}</div>`}
      ${dialog &&
      html`<${ProviderDialog} agent=${agent} presets=${presets} initial=${dialog.provider}
        onClose=${() => setDialog(null)} onSaved=${async (saved) => {
          setDialog(null);
          setAgent(saved.agent);
          notify(`已保存「${saved.name}」`);
          await reload();
          if (tutorial) await tutorial.selectProvider(saved);
        }} />`}
    </section>`;
}

function ProviderCard({ provider, usage, onEdit, reload, notify, onTest, guided }) {
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState(null);
  const test = async () => {
    setTesting(true);
    setResult(null);
    try {
      const tested = await call("TestProvider", provider.id);
      setResult(tested);
      onTest?.(provider, tested);
    } catch (error) {
      setResult({ ok: false, message: error.message });
      onTest?.(provider, { ok: false, message: error.message });
    } finally {
      setTesting(false);
    }
  };
  const [confirming, setConfirming] = useState(false);
  const [removing, setRemoving] = useState(false);
  const remove = async () => {
    setRemoving(true);
    try {
      await call("DeleteProvider", provider.id);
      notify(`已删除「${provider.name}」`);
      setConfirming(false);
      await reload();
    } catch (error) {
      setConfirming(false);
      notify(error.message, true);
    } finally {
      setRemoving(false);
    }
  };
  return html`
    <article class="panel provider-card">
      <div class="card-top">
        <div class="card-identity"><span class="card-icon"><${Icon} name=${agentIcon(provider.agent)} size=${22} /></span><h3>${provider.name}</h3></div>
        ${provider.key_saved
          ? html`<span class="pill ok">密钥已保存</span>`
          : html`<span class="pill warn">缺少密钥</span>`}
      </div>
      <p class="mono">${provider.base_url}</p>
      <dl class="facts">
        <div><dt>模型</dt><dd class="mono">${provider.model}</dd></div>
        ${provider.agent === "dsh" && html`<div><dt>输入</dt><dd>${provider.image_input ? "文本、图片" : "文本"}</dd></div>`}
        <div><dt>协议</dt><dd>${PROTOCOLS[provider.protocol] ?? provider.protocol}</dd></div>
        <div><dt>使用中</dt><dd>${provider.used_by.length ? provider.used_by.join("、") : "暂无沙箱"}</dd></div>
        <div><dt>累计用量</dt><dd title="包括已删除沙箱的用量；只统计经过门卫的模型请求">${usage
          ? `${usage.requests} 次请求 · 输入 ${formatTokens(usage.input_tokens)} / 输出 ${formatTokens(usage.output_tokens)} token`
          : "暂无"}</dd></div>
      </dl>
      ${result &&
      html`<p class=${result.ok ? "test-result ok" : "test-result bad"} role="status">
        ${result.ok ? "✓" : "✕"} ${result.message}${result.latency_ms ? ` · ${result.latency_ms} ms` : ""}
      </p>`}
      <div class="card-actions">
        <button data-guide=${guided ? "provider-test" : undefined} onClick=${test} disabled=${testing}><${Icon} name="activity" size=${17} />${testing ? "测试中…" : "测试连接"}</button>
        <button data-guide=${guided ? "provider-edit" : undefined} onClick=${onEdit}><${Icon} name="edit" size=${17} />编辑</button>
        <button onClick=${async () => { try { await call("DuplicateProvider", provider.id); await reload(); notify("已复制供应商，请编辑名称和模型"); } catch (e) { notify(e.message, true); } }}><${Icon} name="copy" size=${17} />复制</button>
        <button class="danger" onClick=${() => setConfirming(true)} disabled=${provider.used_by.length > 0}
          title=${provider.used_by.length ? "仍有沙箱在用，先给它们换一个供应商" : ""}><${Icon} name="trash" size=${17} />删除</button>
      </div>
    ${confirming && html`<${ConfirmDialog} title=${`删除供应商「${provider.name}」？`} busy=${removing}
        message=${provider.key_managed ? "钥匙串里保存的密钥也会删除。" : "钥匙串里的条目不是 Ai of Cage 创建的，会保留。"}
        onConfirm=${remove} onClose=${() => setConfirming(false)} />`}
    </article>`;
}

function ProviderDialog({ agent, presets, initial, onClose, onSaved }) {
  const editing = Boolean(initial);
  const [preset, setPreset] = useState(editing ? presets.find((p) => p.id === initial.preset) ?? null : null);
  const [form, setForm] = useState(() =>
    editing
      ? { ...initial, api_key: "", use_existing: false, price_enabled: Boolean(initial.pricing), price_currency: initial.pricing?.currency || "CNY", price_input: initial.pricing?.input ?? "", price_output: initial.pricing?.output ?? "", price_cache_read: initial.pricing?.cache_read ?? "", price_cache_write: initial.pricing?.cache_write ?? "", price_source: initial.pricing?.source || "", price_as_of: initial.pricing?.as_of || "" }
      : null
  );
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [models, setModels] = useState({ list: [], message: "", ok: false, loading: false });
  const fetches = useRef(0); // 连续获取时只采用最后一次的结果
  // 地址、协议、请求头或密钥变了，旧列表不再对应；还没回来的请求也作废。
  const dropModels = () => { fetches.current++; setModels({ list: [], message: "", ok: false, loading: false }); };
  const choose = (p) => {
    dropModels();
    setPreset(p);
    setForm({
      id: "", name: p.name, agent: p.agent, protocol: p.protocol, base_url: p.base_url, model: p.model,
      auth: p.auth, preset: p.id, api_key: "", use_existing: Boolean(p.existing_key_ref),
    });
  };
  const set = (key) => (event) => {
    const value = event.target.type === "checkbox" ? event.target.checked : event.target.value;
    if (["base_url", "protocol", "auth", "api_key", "use_existing"].includes(key) && value !== form[key]) dropModels();
    setForm({ ...form,
      ...(key.startsWith("price_") ? { price_source: "", price_as_of: "" } : {}),
      // 单价按模型填写，换了模型才清空
      ...(key === "model" && value !== form.model ? { price_enabled: false, price_source: "", price_as_of: "" } : {}),
      [key]: value });
  };
  const request = () => ({
    id: form.id ?? "", name: form.name, agent: form.agent, protocol: form.protocol, base_url: form.base_url,
    model: form.model, auth: form.agent === "dsh" ? (form.protocol === "anthropic" ? "x-api-key" : "bearer") : form.auth, preset: form.preset ?? "",
    api_key: form.use_existing ? "" : form.api_key,
    image_input: form.agent === "dsh" && Boolean(form.image_input),
    pricing: form.price_enabled ? { currency: form.price_currency || "CNY", input: Number(form.price_input), output: Number(form.price_output), cache_read: Number(form.price_cache_read), cache_write: Number(form.price_cache_write), source: form.price_source || "", as_of: form.price_as_of || "" } : null,
    existing_key_ref: form.use_existing && preset?.existing_key_ref ? preset.existing_key_ref : "",
  });
  // 用表单里的地址和密钥获取模型列表，不用先保存；编辑时密钥留空就用已保存的。
  const fetchModels = async () => {
    const turn = ++fetches.current;
    setModels((current) => ({ ...current, loading: true, message: "正在获取模型列表…" }));
    try {
      const result = await call("PreviewProviderModels", request());
      if (turn === fetches.current) setModels({ list: result.models ?? [], message: result.message, ok: result.ok, loading: false });
    } catch (e) {
      if (turn === fetches.current) setModels({ list: [], message: e.message, ok: false, loading: false });
    }
  };
  // 编辑已有供应商时先显示缓存的列表；这期间点了获取或改了地址，就不再采用它。
  useEffect(() => {
    if (!editing) return;
    const turn = fetches.current;
    call("ProviderModels", initial.id, false)
      .then((result) => { if (turn === fetches.current) setModels({ list: result.models ?? [], message: result.ok ? "" : result.message, ok: result.ok, loading: false }); })
      .catch(() => {});
  }, []);
  // 地址固定的预设，填好密钥后自动获取；自定义地址要用户自己点，免得把密钥发给还没填完的域名。
  const fixedAddress = Boolean(preset?.base_url) && form?.base_url === preset.base_url;
  const keyReady = form && (form.use_existing || (form.api_key ?? "").trim().length >= 8);
  useEffect(() => {
    if (!fixedAddress || !keyReady || editing) return undefined;
    const timer = setTimeout(fetchModels, 700);
    return () => clearTimeout(timer);
  }, [fixedAddress, keyReady, form?.preset, form?.api_key, form?.use_existing, form?.protocol, form?.auth]);
  const submit = async (event) => {
    event.preventDefault();
    setSaving(true);
    setError("");
    try {
      const saved = await call("SaveProvider", request());
      setForm({ ...form, api_key: "" });
      onSaved(saved);
    } catch (e) {
      setError(e.message);
    } finally {
      setSaving(false);
    }
  };
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="dialog" role="dialog" aria-modal="true" aria-label=${editing ? "编辑供应商" : "添加供应商"}>
        <div class="dialog-heading">
          <h2>${editing ? "编辑供应商" : form ? `填写 ${agentName(form.agent)} 的供应商信息` : `选择 ${agentName(agent)} 的供应商预设`}</h2>
          <button type="button" class="icon-button" aria-label="关闭" onClick=${onClose}><${Icon} name="x" size=${20} /></button>
        </div>
        ${!form
          ? html`<div class="preset-grid">
              ${presets.filter((p) => p.agent === agent).map(
                (p) => html`<button class="preset" onClick=${() => choose(p)}>
                  <strong>${p.name}</strong>
                  <span class="mono">${p.base_url || "自己填写接口地址"}</span>
                  ${p.note && html`<small>${p.note}</small>`}
                </button>`
              )}
            </div>
            <p class="caption">预设里的模型名可能会过时，以服务商文档为准，添加时可以修改。</p>`
          : html`<form onSubmit=${submit}>
              <label for="p-name">名称</label>
              <input id="p-name" required maxlength="60" value=${form.name} onInput=${set("name")} />
              <label for="p-url">接口地址（Base URL）</label>
              <input id="p-url" required class="mono" placeholder="https://" value=${form.base_url} onInput=${set("base_url")} />
              <label for="p-model">模型</label>
              <${ModelPicker} id="p-model" required value=${form.model} models=${models.list} loading=${models.loading}
                onChange=${(model) => set("model")({ target: { value: model } })} onRefresh=${fetchModels} />
              <p class=${models.message && !models.ok && !models.loading ? "caption form-warning" : "caption"} role="status">${models.message ||
                (fixedAddress ? "填好 API Key 后会自动获取模型列表，也可以直接填写模型名称。" : "填好地址和 API Key 后，点右侧按钮获取模型列表；也可以直接填写模型名称。")}</p>
              ${form.agent === "dsh" && html`<label class="check"><input type="checkbox" checked=${Boolean(form.image_input)} onChange=${set("image_input")} />模型支持图片输入</label>
                <p class="caption">请按模型的实际能力选择。运行中的沙箱重启后生效。</p>`}
              <details class="advanced" open=${editing || !preset || preset.id?.startsWith("custom")}>
                <summary>高级设置（协议、密钥请求头）</summary>
                <p class="caption">选了预设一般不用改。</p>
                <label for="p-protocol">协议</label>
                <select id="p-protocol" value=${form.protocol} onChange=${set("protocol")}>
                  ${COMPATIBLE[form.agent].map((id) => html`<option value=${id}>${PROTOCOLS[id]}</option>`)}
                </select>
                <label for="p-auth">密钥放在哪个请求头</label>
                <select id="p-auth" disabled=${form.agent === "dsh"} value=${form.agent === "dsh" ? (form.protocol === "anthropic" ? "x-api-key" : "bearer") : form.auth} onChange=${set("auth")}>
                  ${Object.entries(AUTHS).map(([id, label]) => html`<option value=${id}>${label}</option>`)}
                </select>
              </details>
              ${preset?.existing_key_ref &&
              html`<label class="check"><input type="checkbox" checked=${form.use_existing} onChange=${set("use_existing")} />
                使用钥匙串中已有的条目 <span class="mono">${preset.existing_key_ref}</span></label>`}
              ${!form.use_existing &&
              html`<label for="p-key">API Key</label>
                <input id="p-key" type="password" autocomplete="off" spellcheck="false" class="mono"
                  required=${!editing} placeholder=${editing ? "留空则保持原密钥不变" : "粘贴 API Key"}
                  value=${form.api_key} onInput=${set("api_key")} />
                <p class="caption">密钥保存在 macOS 钥匙串。更新已使用的密钥前，请先停止使用它的沙箱。</p>`}
              <details class="advanced" open=${Boolean(form.price_enabled)}><summary>费用估算</summary>
                <${PricingLookup} model=${form.model} onApply=${(price) => setForm({ ...form, price_enabled: true, price_currency: price.currency, price_input: price.input, price_output: price.output, price_cache_read: price.cache_read, price_cache_write: price.cache_write, price_source: price.source, price_as_of: price.as_of })} />
                <label class="check"><input type="checkbox" checked=${Boolean(form.price_enabled)} onChange=${set("price_enabled")} />填写模型单价</label>
                ${form.price_enabled && html`<label for="p-currency">币种</label><select id="p-currency" value=${form.price_currency || "CNY"} onChange=${set("price_currency")}><option value="CNY">CNY · 人民币</option><option value="USD">USD · 美元</option></select>
                  <div class="form-row">${[{id:"input",label:"普通输入"},{id:"output",label:"输出"},{id:"cache_read",label:"缓存读取"},{id:"cache_write",label:"缓存写入"}].map((f) => html`<div><label for=${`price-${f.id}`}>${f.label} / 百万 token</label><input id=${`price-${f.id}`} type="number" required min="0" max="1000000" step="any" value=${form[`price_${f.id}`] ?? ""} onInput=${set(`price_${f.id}`)} /></div>`)}</div>
                  <p class="caption">按照当前模型的供应商报价填写。零表示免费；历史请求保留当时单价，运行中沙箱需重启应用新价格。</p>`}
                ${form.price_source && html`<p class="caption mono">${form.price_source} · ${form.price_as_of}</p>`}
              </details>
              ${error && html`<p class="form-error" role="alert">${error}</p>`}
              <div class="dialog-actions">
                ${!editing && html`<button type="button" onClick=${() => setForm(null)}>返回预设</button>`}
                <button class="primary" type="submit" disabled=${saving}>${saving ? "保存中…" : "保存"}</button>
              </div>
            </form>`}
      </div>
    </div>`;
}
