import test from "node:test";
import assert from "node:assert/strict";
import { applyConversationEvent, tabTitle, recoveryPanel } from "../src/js/conversation-state.js";

test("用户、回复、工具与完整 final 分开保留", () => {
  const session = { messages: [] };
  applyConversationEvent(session, { type: "user", text: "问题\n第二行", images: 1 });
  applyConversationEvent(session, { type: "text", text: "截断回复" });
  applyConversationEvent(session, { type: "tool", text: "query" });
  applyConversationEvent(session, { type: "final", text: "完整回复" });
  applyConversationEvent(session, { type: "done" });
  assert.equal(session.messages[0].role, "user");
  assert.equal(session.messages[0].text, "问题\n第二行");
  assert.equal(session.messages[1].text, "完整回复");
  assert.deepEqual(session.messages[1].tools, ["query"]);
  assert.equal(session.busy, false);
});

test("续聊不拼入上轮回复，停止保留部分输出", () => {
  const session = { messages: [] };
  for (const text of ["第一问", "第二问"]) {
    applyConversationEvent(session, { type: "user", text });
    applyConversationEvent(session, { type: "delta", text: "部分" });
    applyConversationEvent(session, { type: "stopped" });
  }
  assert.equal(session.messages.length, 4);
  assert.equal(session.messages[3].text, "部分");
  assert.equal(session.status, "已停止");
});

test("同名标签按最小的空闲序号编号", () => {
  assert.equal(tabTitle("Codex", []), "Codex 1");
  assert.equal(tabTitle("Codex", ["Codex 1", "Shell 1"]), "Codex 2");
  assert.equal(tabTitle("Codex", ["Codex 2"]), "Codex 1");
  assert.equal(tabTitle("Shell", ["Codex 1", "Codex 2"]), "Shell 1");
});

test("恢复面板只列出真能恢复、还没打开的会话，其余作为诊断", () => {
  const records = [
    { id: "a", sandbox_id: "s1", ready: true },
    { id: "b", sandbox_id: "s1", ready: false, reason: "供应商配置已变化" },
    { id: "c", sandbox_id: "s1", ready: true },
    { id: "d", sandbox_id: "s2", ready: true },
  ];
  const { ready, stale } = recoveryPanel(records, "s1", ["c"]);
  assert.deepEqual(ready.map((item) => item.id), ["a"]);
  assert.deepEqual(stale.map((item) => item.id), ["b"]);
  assert.deepEqual(recoveryPanel(undefined, "s1", []), { ready: [], stale: [] });
});
