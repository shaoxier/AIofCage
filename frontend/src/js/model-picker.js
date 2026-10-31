import { useState } from "preact/hooks";
import { html, Icon } from "./core.js";
import { matchModels } from "./model-list.js";

// ModelPicker 是可以搜索的模型下拉：输入框可以直接填写，下拉列出供应商返回的模型，
// 按输入筛选；onRefresh 存在时带一个“获取模型列表”按钮。供应商表单和终端共用。
export function ModelPicker({ id, value, models, onChange, onRefresh, loading, disabled, required, label = "模型" }) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const options = matchModels(models, value);
  const listId = `${id}-options`;
  const shown = open && options.length > 0;
  const choose = (model) => { onChange(model); setOpen(false); };
  const onKeyDown = (event) => {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (!open) { setOpen(true); setActive(0); return; }
      const step = event.key === "ArrowDown" ? 1 : -1;
      setActive((current) => (current + step + options.length) % Math.max(options.length, 1));
    } else if (event.key === "Enter" && shown) {
      event.preventDefault();
      choose(options[Math.min(active, options.length - 1)]);
    } else if (event.key === "Escape" && open) {
      event.preventDefault();
      setOpen(false);
    }
  };
  return html`<div class="model-picker">
    <div class="model-picker-field">
      <input id=${id} class="mono" role="combobox" aria-label=${label} aria-autocomplete="list" aria-expanded=${shown}
        aria-controls=${listId} aria-activedescendant=${shown ? `${listId}-${active}` : undefined} autocomplete="off" spellcheck="false"
        required=${required} disabled=${disabled} value=${value}
        onInput=${(event) => { onChange(event.target.value); setOpen(true); setActive(0); }}
        onFocus=${() => models?.length && setOpen(true)} onBlur=${() => setOpen(false)} onKeyDown=${onKeyDown} />
      <button type="button" class="icon-button" aria-label="展开模型列表" title=${models?.length ? `${models.length} 个模型` : "还没有模型列表"}
        disabled=${disabled || !models?.length} onMouseDown=${(event) => event.preventDefault()}
        onClick=${() => { setOpen(!open); setActive(0); document.getElementById(id)?.focus(); }}><${Icon} name="chevron" size=${17} /></button>
      ${onRefresh && html`<button type="button" class="icon-button" aria-label="获取模型列表" title="获取模型列表"
        disabled=${disabled || loading} onClick=${onRefresh}><${Icon} name="refresh" size=${17} /></button>`}
    </div>
    ${shown && html`<ul id=${listId} class="model-options" role="listbox" aria-label="可选模型">
      ${options.map((model, index) => html`<li id=${`${listId}-${index}`} role="option" aria-selected=${index === active}
        class=${index === active ? "active" : ""} onMouseDown=${(event) => { event.preventDefault(); choose(model); }}>${model}</li>`)}
    </ul>`}
  </div>`;
}
