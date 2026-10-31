import test from "node:test";
import assert from "node:assert/strict";
import { matchModels, modelOptions, withModel } from "../src/js/model-list.js";

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

test("超过上限时说明还有多少个没显示", () => {
  const many = Array.from({ length: 120 }, (_, index) => `model-${String(index).padStart(3, "0")}`);
  assert.deepEqual(modelOptions(many, "").more, 70);
  assert.equal(modelOptions(many, "").options.length, 50);
  assert.deepEqual(modelOptions(many, "model-11"), { options: many.slice(110, 120), more: 0 });
  assert.deepEqual(modelOptions(undefined, ""), { options: [], more: 0 });
});

test("供应商配置的模型不在列表里时放在最前面，方便换回来", () => {
  assert.deepEqual(withModel(["gpt-6-sol"], "gpt-6.1-sol"), ["gpt-6.1-sol", "gpt-6-sol"]);
  assert.deepEqual(withModel(["gpt-6-sol"], "gpt-6-sol"), ["gpt-6-sol"]);
  assert.deepEqual(withModel(undefined, "gpt-6-sol"), ["gpt-6-sol"]);
  assert.deepEqual(withModel(["gpt-6-sol"], ""), ["gpt-6-sol"]);
});
