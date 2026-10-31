import test from "node:test";
import assert from "node:assert/strict";
import { matchModels } from "../src/js/model-list.js";

const models = ["claude-sonnet-4-5", "deepseek-chat", "gpt-6-sol", "gpt-6.1-sol", "my-gpt-proxy"];

test("按输入筛选模型，开头匹配的排在前面", () => {
  assert.deepEqual(matchModels(models, "GPT"), ["gpt-6-sol", "gpt-6.1-sol", "my-gpt-proxy"]);
  assert.deepEqual(matchModels(models, "sol"), ["gpt-6-sol", "gpt-6.1-sol"]);
  assert.deepEqual(matchModels(models, "qwen"), []);
});

test("没有输入或输入正好是某个模型时列出全部，方便换模型", () => {
  assert.deepEqual(matchModels(models, ""), models);
  assert.deepEqual(matchModels(models, "gpt-6-sol"), models);
  assert.deepEqual(matchModels(undefined, "x"), []);
  assert.equal(matchModels(models, "", 2).length, 2);
});
