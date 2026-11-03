import test from "node:test";
import assert from "node:assert/strict";
import { readWorkspace, saveWorkspace, readAppView, saveAppView, orderedRecoveries } from "../src/js/workspace-state.js";

test("沙箱页面和标签分别保存，更新选中会话不覆盖页面", () => {
  const values = new Map();
  globalThis.localStorage = { getItem: (key) => values.get(key), setItem: (key, value) => values.set(key, value) };
  saveWorkspace("a", { tab: "terminal", active: "t2", terminals: [{ id: "t2", agent: "codex", title: "Codex 2" }] });
  saveWorkspace("b", { tab: "usage", active: "t3" });
  saveWorkspace("a", { active: "t1" });
  assert.equal(readWorkspace("a").tab, "terminal");
  assert.equal(readWorkspace("a").active, "t1");
  assert.equal(readWorkspace("b").tab, "usage");
  assert.equal(readWorkspace("b").active, "t3");
  saveAppView("sandboxes", { id: "a", tab: "terminal", open: { agent: "codex", nonce: 1 } });
  assert.deepEqual(readAppView(), { page: "sandboxes", detail: { id: "a", tab: "terminal" } });
  saveAppView("sandboxes", null);
  assert.equal(readAppView().detail, null);
  saveAppView("providers", { id: "a", tab: "terminal" });
  assert.deepEqual(readAppView(), { page: "providers", detail: null });
});

test("无效或禁用的本地存储不阻止打开沙箱", () => {
  for (const raw of ["{", "null", '{"tab":"bad","active":42,"terminals":[null,{"id":"x","agent":"bad"}]}']) {
    globalThis.localStorage = { getItem: () => raw };
    assert.deepEqual(readWorkspace("a"), { tab: null, active: null, terminals: [] });
    assert.deepEqual(readAppView(), { page: "sandboxes", detail: null });
  }
  globalThis.localStorage = { getItem: () => { throw new Error("denied"); }, setItem: () => { throw new Error("denied"); } };
  assert.doesNotThrow(() => saveWorkspace("a", { tab: "terminal" }));
  assert.equal(readWorkspace("a").active, null);
});

test("只自动恢复当前沙箱的有效记录，并保留原来的标签顺序", () => {
  const records = [
    { id: "new", sandbox_id: "a", ready: true },
    { id: "second", sandbox_id: "a", ready: true },
    { id: "first", sandbox_id: "a", ready: true },
    { id: "stale", sandbox_id: "a", ready: false },
    { id: "other", sandbox_id: "b", ready: true },
  ];
  const workspace = { terminals: [{ id: "first" }, { id: "second" }] };
  assert.deepEqual(orderedRecoveries(records, "a", workspace).map((r) => r.id), ["first", "second", "new"]);
  assert.equal(records[0].id, "new");
});
