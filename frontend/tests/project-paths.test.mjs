import test from "node:test";
import assert from "node:assert/strict";
import { insertAt, insertable, shellQuote } from "../src/js/project-paths.js";

test("在光标处插入路径，前后补空格", () => {
  assert.deepEqual(insertAt("", "src/a.go"), { text: "src/a.go ", cursor: 9 });
  assert.deepEqual(insertAt("看看", "src/a.go"), { text: "看看 src/a.go ", cursor: 12 });
  assert.deepEqual(insertAt("看看 和 b", "src/a.go", 3), { text: "看看 src/a.go 和 b", cursor: 12 });
  assert.deepEqual(insertAt("改 X 吧", "a.go", 2, 3), { text: "改 a.go 吧", cursor: 6 });
});

test("Shell 路径需要时加引号，以“-”开头的不会被当成选项", () => {
  assert.equal(shellQuote("src/main.go"), "src/main.go");
  assert.equal(shellQuote("my docs/计划.md"), "'my docs/计划.md'");
  assert.equal(shellQuote("it's.txt"), `'it'\\''s.txt'`);
  assert.equal(shellQuote("-rf"), "./-rf");
  assert.equal(shellQuote("$(rm -rf ~)"), "'$(rm -rf ~)'");
});

test("含控制字符的路径不插入，免得终端把它当成按键", () => {
  assert.equal(insertable("src/a.go"), true);
  assert.equal(insertable("a\nrm -rf ~"), false);
  assert.equal(insertable("a\u0003"), false);
  assert.equal(insertable(""), false);
});
