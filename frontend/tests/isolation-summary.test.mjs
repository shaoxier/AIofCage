import test from "node:test";
import assert from "node:assert/strict";
import { checkCounts, checkCountsText, checkStatus } from "../src/js/isolation-summary.js";

const results = [{ status: "passed" }, { status: "passed" }, { status: "skipped" }, { status: "inconclusive" }, { status: "weird" }];

test("按状态分别计数，跳过和无法验证不算通过", () => {
  assert.equal(checkCountsText(results), "通过 2 · 发现问题 0 · 无法验证 2 · 跳过 1");
  assert.equal(checkCounts(results).reduce((sum, item) => sum + item.count, 0), results.length);
});

test("未知状态按无法验证算", () => {
  assert.equal(checkStatus({ status: "healthy" }), "inconclusive");
  assert.equal(checkStatus(undefined), "inconclusive");
  assert.equal(checkCountsText([]), "通过 0 · 发现问题 0 · 无法验证 0 · 跳过 0");
});
