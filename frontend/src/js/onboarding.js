import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { html, call, agentName, Icon } from "./core.js";
import { useTerminals, updateTerminalDraft } from "./terminal.js";
import { TUTORIAL_STEPS, TUTORIAL_EXAMPLE, tutorialResources, tutorialStep, tutorialRoute, tutorialHasReply, fillTutorialDraft } from "./onboarding-state.js";
import { OperationProgress } from "./progress.js";

export function useOnboarding({ providers, sandboxes, dataReady, navigate, reload }) {
  const [state, setState] = useState(null);
  const [welcome, setWelcome] = useState(false);
  const [finished, setFinished] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [testResult, setTestResult] = useState(null);
  const current = useRef(null);
  const queue = useRef(Promise.resolve());
  const load = useCallback(async () => {
    setError("");
    try {
      const saved = await call("GetOnboardingState");
      current.current = saved; setState(saved);
      if (saved.status === "new") setWelcome(true);
    } catch (e) { setError(`无法读取教程进度：${e.message}`); }
  }, []);
  useEffect(() => { load(); }, []);

  // Serial writes prevent a late response from replacing a newer selection or exit action.
  const save = useCallback((patch) => {
    setSaving(true); setError("");
    const task = queue.current.then(async () => {
      if (!current.current) throw new Error("请先重新读取教程进度");
      const saved = await call("SaveOnboardingState", { ...current.current, ...patch });
      current.current = saved; setState(saved);
      return saved;
    }).catch((e) => { setError(`无法保存教程进度：${e.message}`); return null; });
    queue.current = task;
    task.finally(() => { if (queue.current === task) setSaving(false); });
    return task;
  }, []);

  const resources = tutorialResources(state, providers, sandboxes);
  const step = tutorialStep(state, resources);
  const active = state?.status === "active" && !dismissed;
  const go = () => {
    const route = tutorialRoute(step, resources.sandbox?.id);
    navigate(route.page, route.detail);
  };
  useEffect(() => {
    if (active && dataReady) go();
  }, [active, step, state?.sandbox_id, dataReady]);

  const begin = async (restart = false) => {
    const saved = await save({ status: "active", step: restart ? 0 : step, ...(restart ? { conversation_verified: false } : {}) });
    if (saved) {
      setWelcome(false); setFinished(false); setDismissed(false);
      const destination = tutorialRoute(tutorialStep(saved, tutorialResources(saved, providers, sandboxes)), saved.sandbox_id);
      navigate(destination.page, destination.detail);
    }
  };
  const pause = async () => {
    await save({ status: "paused", step });
    // A write failure must not trap the user inside a welcome dialog or tutorial.
    setWelcome(false); setDismissed(true);
  };
  return {
    state, step, active, welcome, finished, saving, error, dataReady, resources, providers, testResult, save, load, go, begin, pause, reload,
    open: () => { setWelcome(true); setFinished(false); if (!current.current) load(); },
    closeWelcome: async () => { if (state?.status === "new") await pause(); else setWelcome(false); },
    closeFinished: () => setFinished(false),
    complete: async (verified) => {
      if (await save({ status: "completed", step: 4, conversation_verified: verified })) setFinished(true);
    },
    selectAgent: (agent) => {
      if (agent !== current.current?.agent) { setTestResult(null); return save({ agent, provider_id: "", sandbox_id: "", step: 1, conversation_verified: false }); }
    },
    selectProvider: (provider) => {
      setTestResult(null);
      return save({ agent: provider.agent, provider_id: provider.id, sandbox_id: "", step: 1, conversation_verified: false });
    },
    providerTested: (provider, result) => setTestResult({ id: provider.id, model: provider.model, base_url: provider.base_url, ...result }),
    sandboxCreated: (sandbox) => save({ sandbox_id: sandbox.id, step: 3, conversation_verified: false }),
    navigate,
  };
}

export function Onboarding({ tutorial: t, page, detail }) {
  const [environment, setEnvironment] = useState(null);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState("");
  const card = useRef(null);
  const sessions = useTerminals(t.state?.sandbox_id);
  const { provider, sandbox, choices, running } = t.resources;
  const route = tutorialRoute(t.step, sandbox?.id);
  const inPlace = page === route.page && (route.detail ? detail?.id === route.detail.id && detail?.tab === route.detail.tab : !detail);
  const verified = Boolean(t.state?.conversation_verified || t.state && tutorialHasReply(sessions, t.state));
  const session = sessions.findLast((s) => s.agent === t.state?.agent && s.info?.provider_id === provider?.id && !s.exited);
  const opening = sessions.some((s) => s.agent === t.state?.agent && !s.info && !s.exited);
  const check = async () => {
    setChecking(true); setCheckError(""); setEnvironment(null);
    try { setEnvironment(await call("CheckOnboardingEnvironment")); }
    catch (e) { setCheckError(e.message); }
    finally { setChecking(false); }
  };
  useEffect(() => { if (t.active && t.step === 0) check(); }, [t.active, t.step]);
  useEffect(() => { if (!t.welcome) card.current?.scrollIntoView({ block: "start" }); }, [t.active, t.step, t.finished, t.welcome]);
  // Remember actual success while the tutorial is active, even if the session is later closed.
  useEffect(() => {
    if (t.active && t.step === 4 && verified && !t.state.conversation_verified && !t.saving && !t.error) {
      t.save({ conversation_verified: true });
      card.current?.scrollIntoView({ block: "start" });
    }
  }, [t.active, t.step, verified, t.state?.conversation_verified, t.saving, t.error]);

  useEffect(() => {
    if (!t.active || !inPlace || !t.dataReady) return;
    const target = ["environment", provider ? (provider.key_saved ? "provider-test" : "provider-edit") : "add-provider", sandbox ? "tutorial-next" : "create-sandbox", running ? "tutorial-next" : "sandbox-start", session ? "message-input" : "new-conversation"][t.step];
    let highlighted;
    const update = () => {
      const next = target === "message-input" ? document.getElementById(`terminal-message-${session.id}`) : document.querySelector(`[data-guide="${target}"]`);
      if (next === highlighted) return;
      highlighted?.classList.remove("tutorial-target");
      highlighted = next;
      highlighted?.classList.add("tutorial-target");
    };
    update();
    const observer = new MutationObserver(update);
    observer.observe(document.getElementById("main-content"), { childList: true, subtree: true });
    return () => { observer.disconnect(); highlighted?.classList.remove("tutorial-target"); };
  }, [t.active, t.step, inPlace, running, session?.id, provider?.id, provider?.key_saved, sandbox?.id, t.dataReady]);

  const nextAllowed = [environment?.can_continue && !checking, provider?.key_saved, Boolean(sandbox), running, verified][t.step];
  const dismissWelcome = () => t.closeWelcome();
  return html`
    ${t.error && html`<div class="feedback error" role="alert"><span>${t.error}</span>${!t.state && html`<button onClick=${t.load}>重新读取</button>`}</div>`}
    ${t.welcome && html`<div class="overlay">
      <div class="dialog tutorial-welcome" role="dialog" aria-modal="true" aria-label="使用教程">
        <div class="dialog-heading"><h2>${t.state?.status === "new" ? "欢迎使用 Ai of Cage" : "使用教程"}</h2>
          <button aria-label="关闭" disabled=${t.saving} onClick=${dismissWelcome}><${Icon} name="x" /></button></div>
        <p>跟着这 5 步，配置供应商、创建沙箱，然后开始和智能体对话。</p>
        <ol class="tutorial-welcome-steps">${TUTORIAL_STEPS.map((title) => html`<li>${title}</li>`)}</ol>
        <p class="caption">需要准备模型供应商的 API Key；可以随时退出，稍后继续。</p>
        ${t.state?.step > 0 && html`<p>上次进度：${TUTORIAL_STEPS[t.step]}</p>`}
        ${t.error && html`<p class="form-error" role="alert">${t.error}</p>${!t.state && html`<button onClick=${t.load}>重新读取</button>`}`}
        ${!t.dataReady && html`<p class="caption">正在读取已有配置。</p><button onClick=${t.reload}>重新读取配置</button>`}
        <div class="dialog-actions">
          <button data-dismiss disabled=${t.saving} onClick=${dismissWelcome}>稍后再说</button>
          ${t.state && (t.state.step > 0 || t.state.status === "completed") && html`<button disabled=${t.saving || !t.dataReady} onClick=${() => t.begin(true)}>重新学习</button>`}
          <button class="primary" disabled=${!t.state || t.saving || !t.dataReady} onClick=${() => t.begin()}>${t.state?.step > 0 ? "继续教程" : "开始设置"}</button>
        </div>
      </div>
    </div>`}
    ${t.finished && html`<section class="panel tutorial-card" ref=${card} aria-label="教程完成">
      <div class="section-heading"><div><h2>${t.state.conversation_verified ? "体验完成" : "配置完成，尚未验证对话"}</h2>
        <p>之后可从左侧「使用教程」再次进入。</p></div><button aria-label="关闭教程结果" onClick=${t.closeFinished}><${Icon} name="x" /></button></div>
      <div class="tutorial-actions"><button onClick=${() => { t.closeFinished(); t.navigate("usage", null); }}>查看用量与费用</button>
        <button onClick=${() => { t.closeFinished(); t.navigate("logs", null); }}>查看请求日志</button></div>
    </section>`}
    ${t.active && html`<section class="panel tutorial-card" ref=${card} aria-label="使用教程" aria-busy=${t.saving}>
      <div class="tutorial-heading"><div><span class="caption">使用教程 · ${t.step + 1} / 5</span><h2>${TUTORIAL_STEPS[t.step]}</h2></div>
        <button disabled=${t.saving} onClick=${t.pause}>退出教程</button></div>
      <ol class="tutorial-steps" aria-label="教程步骤">${TUTORIAL_STEPS.map((title, i) => html`<li class=${i === t.step ? "current" : ""} aria-current=${i === t.step ? "step" : undefined}><span>${i + 1}</span>${title}</li>`)}</ol>
      ${!t.dataReady ? html`<p role="status">尚未读到已有配置，请稍候或重新读取。</p><button onClick=${t.reload}>重新读取配置</button>` : !inPlace ? html`<p>已保留教程进度，可以回到当前步骤继续。</p><button class="primary" onClick=${t.go}>返回当前步骤</button>` : html`
        ${t.step === 0 && html`<p>检查运行所需的依赖，首次启动沙箱时会自动准备虚拟机。</p>
          <ul class="tutorial-checks" aria-live="polite">${environment?.checks.map((c) => html`<li><strong>${c.name}</strong><span class=${["missing", "error", "unsupported"].includes(c.status) && c.id !== "vm" ? "form-error" : ""}>${c.detail}</span></li>`)}</ul>
          ${environment?.install_command && html`<p class="caption">在终端中执行以下命令（需要先安装 Homebrew）：</p><code class="tutorial-command">${environment.install_command}</code>`}
          ${checkError && html`<p class="form-error" role="alert">${checkError}</p>`}
          <button onClick=${check} disabled=${checking}>${checking ? "正在检测…" : "重新检测"}</button>`}
        ${t.step === 1 && html`<p>选择智能体后添加供应商，或选用已有配置；保存后可点击「测试连接」。</p>
          <label for="tutorial-provider">本次使用的供应商 · ${agentName(t.state.agent)}</label>
          <select id="tutorial-provider" disabled=${t.saving} value=${provider?.id ?? ""} onChange=${(e) => { const p = t.resources.provider?.id === e.target.value ? null : t.providers.find((p) => p.id === e.target.value); if (p) t.selectProvider(p); }}>
            <option value="" disabled>选择已有供应商，或在下方添加</option>
            ${t.providers.filter((p) => p.agent === t.state.agent).map((p) => html`<option value=${p.id}>${p.name}${p.key_saved ? "" : "（缺少密钥）"}</option>`)}
          </select>
          ${provider && html`<p class="caption" role="status">${!provider.key_saved ? "请编辑供应商，补充 API Key。" : t.testResult?.id === provider.id && t.testResult.model === provider.model && t.testResult.base_url === provider.base_url ? t.testResult.message : "配置已保存，尚未测试连接。"} 发送消息后才能验证模型对话。</p>`}`}
        ${t.step === 2 && html`<p>${sandbox ? `已选用沙箱「${sandbox.name}」，点击继续。` : `点击「创建沙箱」，默认新建空项目，并选好 ${agentName(t.state.agent)} 和「${provider?.name}」。`}</p>
          ${choices.length > 0 && html`<label for="tutorial-sandbox">也可以使用已有沙箱</label><select id="tutorial-sandbox" disabled=${t.saving} value=${sandbox?.id ?? ""} onChange=${(e) => t.save({ sandbox_id: e.target.value, conversation_verified: false })}>
            <option value="">创建新的沙箱</option>${choices.map((s) => html`<option value=${s.id}>${s.name}</option>`)}</select>`}`}
        ${t.step === 3 && html`<p>${running ? "沙箱已启动，可以打开智能体了。" : sandbox?.config_pending ? "已保存的配置尚未生效，请在下方重启以应用。" : "点击下方「启动」，等待运行环境和智能体准备完成。"}</p>
          ${sandbox && html`<${OperationProgress} sandboxId=${sandbox.id} activity=${sandbox.activity} />`}`}
        ${t.step === 4 && html`<p>${verified ? "已收到智能体的完整回复，可以完成教程。" : "打开智能体，发送一句话试试。消息会产生模型用量，也可以稍后体验。"}</p>
          <div class="tutorial-example"><code>${TUTORIAL_EXAMPLE}</code>
            <button disabled=${t.saving || !running || opening || Boolean(session?.busy || session?.draft.busy || session?.draft.reading)} onClick=${() => {
              t.navigate("sandboxes", { id: sandbox.id, tab: "terminal", open: { agent: t.state.agent, nonce: Date.now(), reuse: true } });
              if (fillTutorialDraft(session)) updateTerminalDraft();
            }}>${opening ? "正在打开…" : session ? "填入示例" : `打开 ${agentName(t.state.agent)}`}</button></div>
          ${session?.draft.text && session.draft.text !== TUTORIAL_EXAMPLE && html`<p class="caption">已保留输入框中的草稿，可以直接发送。</p>`}`}
        <div class="tutorial-actions">
          ${t.step > 0 && html`<button disabled=${t.saving} onClick=${() => t.save({ step: t.step - 1 })}>上一步</button>`}
          ${t.step === 4 && !verified && html`<button disabled=${t.saving} onClick=${() => t.complete(false)}>稍后体验</button>`}
          <button class="primary" data-guide="tutorial-next" disabled=${t.saving || !nextAllowed} onClick=${() => t.step === 4 ? t.complete(true) : t.save({ step: t.step + 1 })}>${t.saving ? "保存进度…" : t.step === 4 ? "完成教程" : "继续"}</button>
        </div>`}
    </section>`}`;
}
