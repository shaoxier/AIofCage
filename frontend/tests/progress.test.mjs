import test from "node:test";
import assert from "node:assert/strict";
import { activityRunning, formatDuration, outcomeText, stoppingText } from "../src/js/progress-state.js";

const started = "2026-10-30T12:00:00Z";

test("用时按秒和分写出", () => {
  assert.equal(formatDuration(0), "0 秒");
  assert.equal(formatDuration(12_400), "12 秒");
  assert.equal(formatDuration(185_000), "3 分 05 秒");
  assert.equal(formatDuration(-5), "0 秒");
});

test("停止请求在虚拟机启动这一步要等它完成", () => {
  const vm = { steps: [{ text: "检查专用虚拟机", started_at: started }], stopping: true };
  assert.match(stoppingText(vm), /等虚拟机启动完成后再停/);
  const build = { steps: [{ text: "检查专用虚拟机" }, { text: "正在构建镜像 aiofcage/agent:x（首次需要几分钟）" }], stopping: true };
  assert.equal(stoppingText(build), "正在停止…");
});

test("结束的操作说明结果，被停止的不算成功", () => {
  const base = { label: "启动", started_at: started, ended_at: "2026-10-30T12:02:05Z" };
  assert.equal(activityRunning({ ...base, outcome: "" }), true);
  assert.equal(activityRunning({ ...base, outcome: "stopped" }), false);
  assert.equal(outcomeText({ ...base, outcome: "done" }), "启动已完成，用时 2 分 05 秒");
  assert.equal(outcomeText({ ...base, outcome: "stopped", message: "已停止启动；沙箱没有运行" }), "已停止启动；沙箱没有运行（用时 2 分 05 秒）");
  assert.equal(outcomeText({ ...base, outcome: "failed", message: "构建镜像失败：docker build 失败：exit code: 1" }), "启动失败：构建镜像失败：docker build 失败：exit code: 1");
});
