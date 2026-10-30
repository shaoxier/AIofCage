// 开发环境：创建沙箱和沙箱详情共用。只放行匿名下载，不放行发布、推送和其他网站；
// 镜像里本来就有 Node、npm、git，这里只决定门卫让哪些下载源通过。
import { useEffect, useState } from "preact/hooks";
import { html, call, Icon } from "./core.js";

let catalog = null;

export function useDevSources() {
  const [sources, setSources] = useState(catalog ?? []);
  useEffect(() => {
    if (catalog) return;
    call("ListDevSources").then((list) => { catalog = list ?? []; setSources(catalog); }).catch(() => {});
  }, []);
  return sources;
}

// 同一组（npm 官方源和国内镜像）只能选一个，用单选；其余用复选。
export function DevSourceChoices({ sources, selected, onChange }) {
  const groups = [...new Set(sources.filter((s) => s.group).map((s) => s.group))];
  const pick = (group, id) => onChange([...selected.filter((x) => sources.find((s) => s.id === x)?.group !== group), ...(id ? [id] : [])]);
  const toggle = (id) => onChange(selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id]);
  return html`<div class="agent-pick dev-sources">
    ${groups.map((group) => {
      const options = sources.filter((s) => s.group === group);
      const current = options.find((s) => selected.includes(s.id))?.id ?? "";
      return html`<div class="dev-group" role="radiogroup" aria-label=${group}>
        <span class="dev-group-name">${group}</span>
        <label class="check"><input type="radio" name=${`dev-${group}`} checked=${current === ""} onChange=${() => pick(group, "")} />不开放</label>
        ${options.map((s) => html`<label class="check" title=${s.note}><input type="radio" name=${`dev-${group}`} checked=${current === s.id}
          onChange=${() => pick(group, s.id)} />${s.name}</label>`)}
      </div>`;
    })}
    ${sources.filter((s) => !s.group).map((s) => html`<label class="check" title=${s.note}>
      <input type="checkbox" checked=${selected.includes(s.id)} onChange=${() => toggle(s.id)} />${s.name}</label>`)}
    <p class="caption">镜像里已有 Node 24、npm、git、curl 和 python3。这里只放行下载：不能发布包、推送代码，也不开放其他网站；${""
      }请求自带账号凭据时会被拦下。</p>
  </div>`;
}

export function DevSourceLine({ sandbox, sources, busy, onEdit }) {
  const enabled = (sandbox.dev_sources ?? []).map((id) => sources.find((s) => s.id === id)?.name ?? id);
  return html`<div class="mcp-line">
    <span class="faint">下载源</span>
    ${enabled.map((name) => html`<span class="binding">${name}</span>`)}
    ${enabled.length === 0 && html`<span class="faint">未开放</span>`}
    <button class="link" onClick=${onEdit} disabled=${Boolean(busy)}>设置</button>
  </div>`;
}

export function DevSourceDialog({ sandbox, sources, onClose, onSave }) {
  const [selected, setSelected] = useState(sandbox.dev_sources ?? []);
  return html`
    <div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
      <div class="dialog" role="dialog" aria-modal="true" aria-label="开发环境">
        <div class="dialog-heading"><h2>「${sandbox.name}」的开发环境</h2>
          <button type="button" class="icon-button" aria-label="关闭" onClick=${onClose}><${Icon} name="x" size=${20} /></button></div>
        <${DevSourceChoices} sources=${sources} selected=${selected} onChange=${setSelected} />
        <p class="caption">运行中的沙箱会立即生效；终端里已经打开的 shell 要重新打开，才会读到新的 npm 源设置。</p>
        <div class="dialog-actions">
          <button onClick=${onClose}>取消</button>
          <button class="primary" onClick=${() => onSave(selected)}>保存</button>
        </div>
      </div>
    </div>`;
}
