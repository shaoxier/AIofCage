# 终端与供应商调研：第二轮迭代前（只读排查）

> 基线：`main` 4f75106（2026-10-26 合并 #66），与本地缓存的 `origin/main` 相同。只读代码；没有构建、没有启动应用或容器、没有读钥匙串、没有调用模型接口，也没有修改仓库。
>
> 标注说明：
> - **[代码]**：读代码确认；
> - **[推测]**：没有实测的推断；
> - **[外部]**：公开文档或第三方仓库，2026-10-29 读取；
> - **[已有探测]**：引用同目录 `5-qiniu-api-probe.md`、llmapi 的 `/openapi.json`。这些是他人此前在宿主上执行的结果，本次只在本地读取，没有复核。

---

## 一、“恢复会话”

### 1.1 渲染位置 [代码]

- 唯一的恢复入口是终端标签里的“可恢复会话”面板，每条记录一个“恢复”按钮（`frontend/src/js/sandbox-detail.js:120-128`）。
  - 沙箱未运行且没有标签时，面板出现在“沙箱还没有启动”空状态上方（`sandbox-detail.js:146-152`）。
  - 其他情况下，面板在 `.terminal-panel` 顶部、标签栏之上（`sandbox-detail.js:159`）。
- “+ 新建”下拉只列出已安装的智能体和 Shell（`sandbox-detail.js:144`、`165-171`），空状态“开始工作”也一样（`180-183`），两处都没有恢复选项。全仓库没有叫“恢复会话”的按钮，后端只有一条错误文案用到这四个字（`internal/service/conversations.go:609`）。
- 截图如果来自当前 main，用户说的“恢复会话”就是这个面板（标题“可恢复会话”，按钮“恢复”）。以后如果把恢复挪进“+ 新建”菜单，也要遵守同样的显示条件。

### 1.2 显示条件与数据来源 [代码]

**显示条件**

- 面板在 `pendingRecoveries.length > 0` 时渲染（`sandbox-detail.js:121`）。
- `pendingRecoveries` 是全局 `recoverable` 中满足两个条件的记录：`sandbox_id` 等于本沙箱，且当前没有同 ID、未结束的标签（`sandbox-detail.js:120`）。
- **这里没有按 `ready` 过滤。** `ready=false` 的记录同样会让面板出现，只是该行显示原因、没有按钮（`:125-126`）。

**前端数据何时刷新**

- `recoverable` 是 App 级状态（`frontend/src/main.js:37`），只在 `reload()` 里刷新（`main.js:48-60`）。
- 触发刷新的只有三种情况：
  - 页面切换（`main.js:140`）；
  - 沙箱操作结束：`run` 在 finally 里调用 `reload`（`sandboxes.js:27-40`）；
  - 收到 `sandbox:progress` 事件（`main.js:89`）。
- **关闭标签、首条消息产生记录时，都不会刷新。**

**后端来源**

- `ListRecoverableConversations`（`conversations.go:559-594`）读取设置项 `conversation.recovery`（`:450`、`:487-497`）。
- 沙箱已删除的记录会被跳过，但不会被清理（`:566-569`）。
- 供应商绑定变化或供应商不可用时，记录标为 `Ready=false` 并附原因（`:571-589`）。
- **后端不检查原生会话文件是否仍在 Agent home 里，也不检查沙箱是否在运行。**

产品规则见 `docs/product-workspace.md:22`、`docs/architecture-current.md:153`、`docs/features.md:25`。

### 1.3 记录的生命周期 [代码]

| 时机 | 记录变化 | 证据 |
|---|---|---|
| 新建标签（StartConversation） | 无 | conversations.go:66-134 |
| 每轮收到 CLI 的 `session` 事件 | 写入或覆盖（native ID 非空时） | conversations.go:273-283、499-515；agent-turn.py:57-58、67-68、80-81 |
| 用户关闭**未结束**的标签 | 删除 | terminal.js:172 → CloseConversation → conversations.go:369-407（`forget=true`） |
| 用户关闭**已结束**的标签 | **不删除**（前端不调用后端） | terminal.js:172 |
| 停止沙箱、重建、应用退出 | 保留（`forget=false`） | sandboxes.go:126；app.go:64；conversations.go:409-448；terminal.go:241-242 |
| 重置且不保留 home | 删除本沙箱的全部记录 | sandboxes.go:150-152 |
| 删除沙箱 | 删除本沙箱的全部记录 | sandboxes.go:177 |

### 1.4 点击“恢复”后会发生什么 [代码]

调用链：

1. `recover(item)`（`sandbox-detail.js:113-119`）。如果沙箱没在运行，**先启动沙箱**（`:115`）。
2. `openTerminal(sandbox, agent, item)`，标签 ID 沿用记录 ID（`terminal.js:118`）。
3. 后端 `RecoverConversation(id)`（`conversations.go:596-612`）进入 `startConversation(..., recovery)`，检查沙箱在运行、会话组件存在、供应商与记录一致（`:104-106`）。

按记录状态分三种情况：

- **记录已不在后端（前端列表过期）。** 后端返回“找不到可恢复的会话”（`:604-607`）。前端新开一个“已结束”的标签，显示错误，并自动运行 `DiagnoseConversation`（`terminal.js:133-137`）。如果沙箱原本是停止的，会先完整启动沙箱，然后才失败。
- **记录还在，但原生会话已经不存在。**
  - 后端照常返回 `Resumed:true`。前端锁定模型和思考强度，状态显示“已恢复原生会话”（`terminal.js:128-132`）。
  - 直到第一条消息，才会用 `codex exec resume <id>`、`claude --resume <id>` 或 `dsh --session-id <id>` 运行（`agent-turn.py:17-46`）。
  - 失败时只显示一条通用提示加诊断尾部（`conversations.go:298-301`；`conversation_diagnostics.go:126-136`）。各 CLI 的具体报错是 **[推测]**。
  - 这个标签之后无法换回新会话，只能关闭再新建。
- **记录 `ready=false`。** 这一行没有按钮，点不了，但面板标题“可恢复会话”依然显示。

另外，恢复后的会话不显示旧正文（这是设计如此），界面仍是“描述任务，或添加图片开始会话”的欢迎页（`conversation-view.js:50-51`），容易让人以为没恢复成功。

### 1.5 没有可恢复会话时仍出现“恢复”按钮的几种路径

1. **前端列表过期（最可能）[代码]**
   1. 用户在本次运行中发过消息，后端已有记录。
   2. 用户切换页面，`reload()` 把这条记录拉进 `recoverable`。此时标签还开着，被过滤掉。
   3. 用户关闭标签，后端删除记录，但前端列表没有更新。
   4. 过滤条件不再命中，面板立刻出现“恢复”。点击后就是 1.4 的第一种情况。
2. **只有 `ready=false` 的记录**，面板照样显示（`sandbox-detail.js:120-126`）[代码]。
3. **原生会话已丢失但记录还在**，例如 home 卷被外部删除，或重建失败 **[推测]**。按钮可以点，但第一轮一定失败。
4. **用户关闭“已结束”的标签后**，记录按设计保留，面板随即出现（`terminal.js:172`）[代码]。这算不算“主动关闭会话”，需要产品确认。

### 1.6 精确修改点（建议，没有改文件）

1. **`sandbox-detail.js:120`**：过滤条件加上 `item.ready`，至少有一条可恢复记录时才渲染面板（`:121`）。`ready=false` 的记录不进这个面板。如果需要保留清理入口，放到诊断页或“已失效记录”里，不用“恢复”字样。
2. **修复过期列表**，二选一：
   - 在 `main.js` 增加 `refreshRecoverable`（只调用 `ListRecoverableConversations` 再 `setRecoverable`），沿 `main.js:148` → `sandboxes.js:9/83` → `sandbox-detail.js:25/60/86` 传下去。在 `close`（`sandbox-detail.js:140-143`）里调用；首轮出现 `session` 后也可以调用。
   - 或者在 `terminal.js:169-178` 的 `closeTerminal` 里，把关闭的会话 ID 记进一个模块级集合并导出，`pendingRecoveries` 排除这些 ID。
3. **`sandbox-detail.js:113-119`**：启动已停止的沙箱之前，先确认记录还在（重新拉取列表，或新增一个只读的 `RecoveryStatus(id)`），避免为过期记录启动沙箱。另外 `:125` 的 `disabled` 表达式里 `!running && Boolean(busy)` 与前一项重复，可以顺手删掉。
4. **`conversations.go:596-612`**：沙箱运行时，在 `startConversation` 之前用 `manager.Exec` 检查原生会话是否存在。不存在就删除记录，并返回“原生会话已不存在，已移除恢复项”。各 CLI 的存放位置需按固定版本核对 **[推测]**：
   - Codex 0.164.0：`~/.codex/sessions/**/rollout-*-<thread_id>.jsonl`；
   - Claude Code 2.1.309：`~/.claude/projects/<编码后的项目路径>/<session_id>.jsonl`；
   - dsh 0.2.1-rc.2：在 `DSH_HOME=/home/agent/.dsh`（`agentconfig.go:79`）下，具体布局未知。
5. **`conversations.go:559-594`**：顺带清理沙箱已不存在的孤儿记录。
6. **产品确认**：关闭“已结束”的标签是否等于放弃恢复。如果是，`terminal.js:172` 对已结束的会话也调用 `CloseConversation` 即可。后端在内存里找不到会话时，仍会删除记录（`conversations.go:374-379`）。
7. **测试**：把 `pendingRecoveries` 的筛选抽成纯函数（参照 `conversation-state.js`），在 `frontend/tests/` 里补用例，用 `node --test` 运行（`docs/development.md:32`）。后端为“原生会话缺失时删除记录”补单测。

**附带发现：恢复记录写盘过于频繁 [代码]。** `conversations.go:273-283` 每收到一个 `session` 事件就写一次 settings.json（`store.go:269-290`：加锁、读取、全量写回），不判断 native ID 有没有变。`agent-turn.py:80-81` 会转发 Claude Code 每一行带 `session_id` 的事件；如果 stream-json 每行都带这个字段 **[推测]**，流式输出期间会高频写盘。建议只在 ID 变化时保存。

---

## 二、同一 agent 的多个对话

### 2.1 结论 [代码]

**当前桌面路径没有“每个智能体只能开一个对话”的限制。** 同一沙箱可以同时开多个 Codex、Claude Code 或 dsh 标签，而且可以各自同时跑一轮。真正的上限只有两个：

- 全应用最多 12 个结构化会话（`conversations.go:121`；`terminal.go:29`）。Shell 终端另有一套 12 个的上限。
- 每个会话同一时间只跑一轮（`conversations.go:188-190`）。

以上未在应用里实测。

**用户为什么觉得“只能开一个”（[推测]，需要在应用里复现）**

- 同类标签标题都叫“Codex”（`terminal.js:120`），没有序号、时间或忙碌标记，看起来像同一个会话。
- 沙箱列表的“继续工作”在已有标签时只跳到终端，不会新建（`sandboxes.js:172`）。教程入口会复用已有的同供应商标签（`onboarding.js:182`，配合 `sandbox-detail.js:133`）。
- 新建入口只在“+ 新建”下拉里（`sandbox-detail.js:165-171`）。有了标签以后，空状态的大按钮就不见了（`:176-183`）。

**“一沙箱一任务”的限制在本机 MCP 控制路径，而不是桌面路径**

- 每会话最多 2 个并发，每个沙箱只能有 1 个运行中的任务（`agent_control.go:321-329` 的 `activeIn`，`:400-401`）。
- 控制沙箱只能绑定一个智能体（`:359-361`）。
- 控制任务每次都开新的原生会话，payload 里没有 `resume_id`（`:501`）。
- 单个任务的用量是用沙箱全局计数器的前后差值算出来的（`:484`、`:566-572`）。这也是它只允许每沙箱一个任务的原因之一。

### 2.2 逐层核对

| 层 | 现状 | 证据 |
|---|---|---|
| 前端会话表 | 按随机 ID 存进 `Map`，数量不限；草稿、消息、模型都按标签独立 | terminal.js:11、114、116-140；terminal-draft.js:7-9 |
| 新建菜单 | 每个已安装智能体加 Shell，不检查已有标签；`creating` 只防连点 | sandbox-detail.js:106-112、144、169 |
| 后端会话模型 | `items[id]`，只拒绝重复 ID 和总数 ≥12 | conversations.go:113-128 |
| 每轮进程 | 每轮一个 `docker exec … agent-turn.py`；进程标记为 `<标签ID>-<batch>`，停止时只结束本轮 | conversations.go:230-254；runtime/conversation.go:12-17；terminal.go:209-221 |
| 原生会话 | 首轮新建，之后 `resume`，每个标签有自己的 native ID | agent-turn.py:17-46；conversations.go:194-196、224 |
| Agent home | 三个 CLI 共用 `/home/agent`（Docker 卷），没有给每个会话设 `CODEX_HOME` 或 `CLAUDE_CONFIG_DIR` | manager.go:542；agentconfig.go:59-81 |
| 附件 | 存在 `~/.aiofcage/attachments/<batch>`，按批次隔离 | runtime/terminal_images.go:33；terminal-images.py:12-22 |
| 门卫与额度 | 额度按沙箱算；事件带沙箱、Agent、供应商，不带会话 ID | gatekeeper/config.go:53-60；manager.go:632-650；audit/audit.go:11-48 |

### 2.3 支持多个并行对话需要改的地方（建议）

1. **标签可区分。** 在 `terminal.js:120` 生成“Codex 2”或“Codex · 10:42”这样的标题，忙碌时在标签上显示状态（`sandbox-detail.js:161-164`）。恢复列表加上 `updated_at`（`sandbox-detail.js:124`；后端已经返回，见 `conversations.go:472`）。不能用首条消息做标题，那会违反“只存元数据、不存正文”（`product-workspace.md:22`）。
2. **按沙箱限制并发。** 在 `SendConversationMessage`（`conversations.go:186-196`）里统计同一沙箱的忙碌会话，例如最多 2 到 3 个。容器限制是 2 CPU、4 GiB 内存、512 个进程（`manager.go:546`）。每轮至少启动一个 CLI 进程（Claude Code 和 dsh 是 Node）；启用 filesystem MCP 时，每个进程还会再启动一个 `mcp-server-filesystem`（`agentconfig.go:121-123`、`138-140`）。
3. **原生 home 的并发安全（[推测]，需按固定版本实测）。**
   - Codex 的 rollout 文件按线程分开，但 `~/.codex/history.jsonl` 这类共享文件需要确认。
   - Claude Code 会频繁改写 `~/.claude.json`，多个实例同时写是否安全要验证。
   - dsh 的会话存储方式未知。

   如果验证不通过，就给每个会话设独立的 `CODEX_HOME`、`CLAUDE_CONFIG_DIR`、`DSH_HOME`，复制或链接 `aiofcage-init` 生成的配置，并把目录写进恢复记录。
4. **恢复记录写入去重**，见 1.6 末段。
5. **事件与用量归属**（如果要按对话统计）。门卫目前只知道沙箱、Agent 和供应商。两个可选方案 **[推测]**：
   - 每个会话注册独立的占位符，由 `agent-turn.py` 在子进程环境里覆盖 `AIOFCAGE_*_API_KEY`，门卫据此给事件打标签；
   - 在每轮子进程的 `HTTPS_PROXY` 里带上会话标识。

   这两种标识都只能用于展示，不能当作授权依据，因为同一容器内同一 uid 的进程可以互相读取环境变量。
6. **提示用户。** 并行会话共用项目目录和额度。两个智能体同时改同一个文件，或同时运行 `npm install`，会互相覆盖；目前没有锁，也没有回滚。

### 2.4 风险

- **并发写 home**：见上。Shell 或系统终端里运行的 TUI 也共用同一个 home（`terminal.go:56-71`；`sandboxes.go:193-209`）。
- **额度**：并行对话共用沙箱额度。任何一个对话触顶，同沙箱的所有请求都会被拒（`quota.blocked`，`main.js:76-83`）。
- **门卫事件归属**：两个 Codex 标签在日志页和用量页无法区分。控制路径用前后差值算用量，并行时会把别的会话也算进来。
- **资源**：2 CPU、4 GiB、512 进程。多个 CLI 加上它们的子命令，可能触发 OOM 或进程数上限。
- **恢复**：应用退出时，所有发过消息的会话都会被列为可恢复。同类记录需要靠时间来区分。

---

## 三、模型列表 /models

### 3.1 已有代码（Go 与 JS 都 grep 过 `models`）[代码]

**Go**

- 取模型列表的只有两处，各自拼接 `/models`：`TestProvider`（`internal/service/app.go:328-380`）和 `ConversationModels`（`internal/service/conversations.go:614-663`）。
- 门卫规则允许沙箱内访问 `GET {base}/models` 或 `GET {base}/v1/models`（`internal/provider/provider.go:157`、`160`、`163`）。
- `pricing_catalog.go:17` 里的 models.dev 是价格目录，与供应商的模型列表无关。

**JS**

- 只有终端用到模型列表（`conversation-view.js:25-37`；`terminal.js:121`、`130`）。
- 供应商对话框没有任何模型列表调用，模型字段是纯文本框（`providers.js:180-181`）。

### 3.2 “测试连接” [代码]

- **宿主直连，不经过门卫。** `a.client` 显式设置了 `Proxy: nil`（`app.go:57-58`）；代码注释说明请求由可信的桌面进程发出（`app.go:328-329`）。
- **请求的接口**：OpenAI 协议是 `GET https://{host}{base}/models`；Anthropic 协议是 `GET {base}/v1/models`，并带 `anthropic-version: 2023-06-01`（`app.go:345-357`）。认证头按 `Injection()` 决定（`provider.go:168-174`）。
- **只看状态码，不读响应正文**（`app.go:365-379`）：
  - 200：密钥有效；
  - 401、403：密钥无效或没有权限；
  - 404、405：服务可达，但没有模型列表接口；
  - 429：服务可达，但被限流。
- 只能测试已保存的供应商（按 ID）。添加对话框在保存前既不能测试，也拿不到模型列表。
- 读出的密钥用完没有清零（`app.go:337`）。对比 `conversations.go:635` 有 `clear(key)`。

### 3.3 终端里的模型输入框和刷新按钮 [代码]

**模型输入框**

- 是一个带 `<datalist>` 的 `<input list=…>`（`conversation-view.js:34-36`）。
- 初始值是供应商的默认模型，或恢复记录里的模型（`terminal.js:128-130`；`conversations.go:124-132`）。
- 首条消息发出后、或恢复会话后，输入框被禁用（`conversation-view.js:34`；`conversation-state.js:14`；`terminal.js:131`）。
- datalist 只是输入建议，不像下拉框那样显眼。这可能是用户觉得“只能手填”的原因之一 **[推测]**。

**刷新按钮**

- 调用 `ConversationModels(id)`，使用该会话绑定的**生效中供应商快照**（`conversations.go:87-100`、`620`）。
- 从钥匙串读 key，宿主直连拉取列表，解析 `{"data":[{"id":…}]}`（正文上限 2 MiB）。
- 返回“默认模型 + 其他 ID”，最多 500 个，只对默认模型去重（`:625-662`）。
- 没有缓存，每次点击都重新请求。失败时提示“可手动填写”。

**两处小差异**

- `ConversationModels` 对所有协议都带 `anthropic-version`（`:642`），而测试连接只对 Anthropic 协议带（`app.go:355-357`）。
- Anthropic 官方的 `/v1/models` 默认分页（约 20 条）。当前代码没有带 `limit`，也不翻页 **[外部常识，未实测]**。

### 3.4 模型改动如何生效 [代码]

- 每次发送都带上 `{model, effort}`（`sandbox-detail.js:196`）。后端只校验三点：非空、不超过 200 字节、不含控制字符（`conversations.go:146-161`），不检查模型是否在列表里。
- 首轮把模型作为 CLI 参数传入：Codex 用 `-c model=…`，Claude Code 用 `--model`，dsh 用单次 `--patch` 覆盖文件（`agent-turn.py:17-46`；`runtime/conversation.go:19-23`）。不会改写供应商的默认模型，也不会改沙箱配置。
- 原生会话建立后，禁止更换模型或思考强度（`conversations.go:194-196`），要换只能新建会话。
- Claude Code 的后台小模型仍使用供应商默认模型，即 `ANTHROPIC_SMALL_FAST_MODEL`（`agentconfig.go:69-70`）。
- **对计费的影响**：实际使用的模型与供应商配置的模型不同时，门卫把事件标为 `model-mismatch`，不估算费用（`internal/gatekeeper/proxy.go:482-505`）。允许从下拉框切换模型后，这类“未计价”事件会变多。

### 3.5 推荐的实现位置

1. **抽出公共取数函数（宿主侧 Go）**
   - 纯函数放在例如 `internal/provider/models.go`：按协议生成列表地址；解析 `data[].id`；用 `modelPattern`（`provider.go:51`）校验；去重；限制数量。
   - 唯一的 HTTP 实现放在 `internal/service`，`TestProvider`、`ConversationModels` 和新接口都调用它。
   - 认证头按供应商自己的设置发送（bearer 或 x-api-key），不要固定成 x-api-key。
2. **新增两个绑定方法**
   - `ProviderModels(providerID, refresh)`：用于已保存的供应商，编辑对话框和终端共用。
   - `PreviewProviderModels(request)`：添加对话框保存前使用。key 来自本次输入（只进不出，与 `SaveProvider` 相同），或来自 `allowedExistingKeyRef` 允许的条目（`app.go:274-284`）。可以同时返回连通性结果，省掉“先保存再测试”这一步。
   - 对 `/anthropic/v1/models` 这类不存在列表接口的地址（例如 DeepSeek），可以给预设加一个同域名的 `models_url`。CC Switch 也有同名字段，见 4.6。
3. **缓存与终端复用**
   - 缓存放在 App 内存里。缓存键用指纹（host + base + protocol + auth + keyRef），不要只用供应商 ID。这样“生效中的快照”和“已编辑但未生效的配置”会各自命中自己的条目。
   - `SaveProvider`、`DeleteProvider` 时清除缓存；TTL 设为 10 到 30 分钟。
   - 模型名不是秘密，可以另存一份到设置里，供离线显示。但不要放进 `Provider` 结构体，否则会被复制进每个沙箱的 `AppliedProviders` 快照。
   - `StartConversation` 在 `ConversationInfo`（`conversations.go:26-36`）里带回缓存的 `models`，`terminal.js:130` 直接使用；终端的刷新按钮更新同一份缓存。
4. **前端**
   - `providers.js:180-181` 改成“输入框 + 候选列表 + 获取模型列表按钮”。
   - 只有地址固定的预设（例如七牛）才能在填完 key 后自动拉取。自定义地址必须由用户点击，防止把 key 发给还没填完的域名。

### 3.6 安全约束

- **key 只在宿主内存中短暂持有。** 从 `vault.Read` 拿到 `[]byte`，用完立即 `clear`（`TestProvider` 需要补上）。前端输入的 key 是 Go 字符串，无法清零，所以不缓存、不记录、用完就丢。缓存键里不要包含 key，也不要包含 key 的哈希。
- **请求目标必须先通过 `Provider.Endpoint()`。** 只允许 https，不能带端口、用户信息或查询参数，域名不能是 IP（`provider.go:121-140`；`policy.go:80-96`）。如果加了 `models_url`，它必须与 base 同域名。
- **重定向会泄露 key（读代码发现的问题）。**
  - `a.client` 没有设置 `CheckRedirect`（`app.go:58`）。
  - Go 在跨域重定向时只剥掉 `Authorization`、`Cookie` 等少数请求头，自定义的 `X-Api-Key` 会原样发到新域名；Go 也不阻止 https 降级到 http（Go 标准库行为，**[外部]**）。
  - 因此 `TestProvider` 和 `ConversationModels` 对 x-api-key 类供应商有被重定向泄露的可能。`explain.go` 使用的 `explainClient`（`app.go:62`）同理。
  - 新接口和这两个旧接口都应该在 `CheckRedirect` 里返回 `http.ErrUseLastResponse`，不跟随重定向。
- **预设的钥匙串条目只能发往预设域名。** 现在只要勾选了 `existing_key_ref`，就可以配置任意 base 地址再点测试，比赛 key 会被发过去（`app.go:230-234`、`330-359`）。新的预览接口应校验 host 与预设一致，`SaveProvider` 最好也加同样的约束。
- **不回显上游错误正文。** 有些服务会在错误信息里带上部分 key，保留现在的固定文案。
- **在界面上说明请求路径。** 宿主直连的请求不进审计、不占沙箱额度，界面上应写明“由本机直接读取”。也不要定时轮询（`product-workspace.md:58`）。
- **过滤模型 ID。** 返回给前端和 CLI 的模型 ID 先用 `modelPattern` 过滤。CLI 参数不经过 shell（`agent-turn.py:113`）。
- **不增加钥匙串读取。** `ListProviders` 已经会为每个供应商读一次 key 来判断是否存在（`app.go:142-148`），新功能不要再额外增加读取次数。

---

## 四、供应商预设

### 4.1 数据结构 [代码]

**后端**

- 预设结构是 `Preset{id, agent, name, protocol, base_url, model, auth, existing_key_ref?, note?}`（`internal/provider/presets.go:11-21`）。
- 内容嵌入在 `presets.json` 里，运行时解析（`:23-32`），通过 `ListPresets` 原样交给前端（`app.go:124`；`main.js:67`）。
- **预设里没有价格。** 价格属于保存后的供应商（`provider.go:35-48`；`pricing.go:8-18`），由用户手填，或从 models.dev 查询（`pricing.js`；`pricing_catalog.go:17`）。

**前端**

- 按当前智能体筛选预设并渲染成卡片（`providers.js:164-174`）。
- 选中卡片后填入表单。如果预设带 `existing_key_ref`，会默认勾选“使用钥匙串中已有的条目 <条目名>”，并隐藏 API Key 输入框（`:124-130`、`196-203`）。
- 编辑已有供应商时，按 `initial.preset` 找回对应的预设（`:116`）。

**测试**

- `TestPresetsAreValid` 要求预设 ID 全局唯一，且所有带地址的预设都能通过 `Validate`（`provider_test.go:94-112`）。

### 4.2 现有的“七牛 xapi”预设 [代码]

**只有 Codex 有一条。Claude Code 和 dsh 都没有七牛预设。**

| 字段 | 值（presets.json:2-12） |
|---|---|
| id / agent | `qiniu-xapi` / `codex` |
| name | 七牛 xapi（比赛接口） |
| protocol / auth | `openai-responses` / `bearer` |
| base_url | `https://xapi.qiniu.io/v1` |
| model | `gpt-6.1-sol` |
| existing_key_ref | `sbx-builderday-key`。保存后 `KeyManaged=false`，删除供应商时不会删这个钥匙串条目（`app.go:313-318`） |
| 价格 | 无 |

其他预设：

- Claude Code：Anthropic 官方；DeepSeek、Kimi、智谱的 Anthropic 兼容接口；自定义（`presets.json:32-76`）。
- dsh：DeepSeek 官方（anthropic + x-api-key）；自定义（openai-chat）（`:77-95`）。

旧文档写着“七牛 xapi 只有 OpenAI 格式接口”（`docs/development-plan.md:32`）。

### 4.3 门卫白名单如何从供应商地址推导 [代码]

1. `Endpoint()` 从 base URL 拆出 host 和 base 路径（`provider.go:121-140`）。
2. `Rules()` 按协议生成放行规则，每条规则带 `SecretRef=供应商ID`（`provider.go:142-166`）：

   | 协议 | 放行规则 |
   |---|---|
   | responses | `POST {base}/responses/*`、`GET {base}/models` |
   | chat | `POST {base}/chat/completions`、`GET {base}/models` |
   | anthropic | `POST {base}/v1/messages/*`、`GET {base}/v1/models` |

3. 沙箱启动时，规则连同真 key（来自钥匙串）、占位符和注入头一起下发给门卫（`manager.go:637-651`）。其余请求默认拒绝。对注入了密钥的域名，门卫只接受本项目的占位符（`proxy.go:331` 起）。
4. 多个供应商使用同一域名、同一路径时（例如 Codex 和 Claude Code 都请求 `llmapi.qiniu.io` 的 `/v1/models`），门卫按请求携带的占位符选择规则（`proxy.go:300-324`）。读代码看可行，未实测。
5. 智能体侧的配置使用同一个 base：Codex 写进 `base_url`（`agentconfig.go:107`），Claude Code 写进 `ANTHROPIC_BASE_URL`（`:63`），dsh 写进 `baseURL`（`:171`）。

### 4.4 改名为“七牛”、默认地址改为 `https://llmapi.qiniu.io`

**[已有探测]**（`5-qiniu-api-probe.md`，2026-10-29 在宿主上用 curl 实测）：

- llmapi 是一个 LiteLLM 风格的“XHub API”。它的 OpenAPI 里有：`/v1/models`、`/v1/chat/completions`、`/v1/responses`（及 compact、cancel、input_items）、`/v1/messages`、`/v1/messages/count_tokens`、`/anthropic/{endpoint}`。
- **比赛 key 在 llmapi 上返回 401 `token_not_found_in_db`，即无效。**

由此得出两点：

- **不能把根地址原样交给三个智能体。** Codex 需要 `https://llmapi.qiniu.io/v1`；Claude Code 需要根地址（或 `/anthropic`）；dsh 取决于所选协议。卡片上可以显示统一的厂商地址，但每个智能体的预设要分别存完整的 base。
- **新的“七牛”预设不应再默认勾选 `sbx-builderday-key`。** 如果还需要比赛接口，单独保留一条 xapi 预设。

下面是需要修改的位置。检索命令为 `grep -rn "xapi\.qiniu\.io\|sbx-builderday-key\|七牛 xapi\|qiniu-xapi"`，已排除 `bin/` 和未跟踪的 `research_notes/`。

**A. 必改（影响产品行为）**

| 位置 | 说明 |
|---|---|
| internal/provider/presets.json:2-12 | 改名称、地址、模型、`existing_key_ref` 和 note。保留 ID `qiniu-xapi` 或兼容旧 ID，否则已保存的供应商在编辑时找不到预设（providers.js:116） |
| presets.json（新增条目） | 增加 Claude Code 和 dsh 的七牛预设，ID 全局唯一，例如 `qiniu-claude`、`qiniu-dsh` |
| internal/provider/presets.go:11-21 | 做成厂商快捷选项时可加字段：官网、获取 key 的链接、`models_url`、候选地址、厂商分组和图标 |
| frontend/src/js/providers.js:124-130、164-174、180-181、196-198 | 预设卡片改为厂商快捷选项；模型改为可选列表；不要把内部条目名（如 `sbx-builderday-key`）直接显示给用户 |
| internal/service/app.go:274-284 | 可复用的钥匙串条目来自预设。去掉后，已有供应商（KeyRef 已保存）照常可用，但新建或编辑时不能再选这个条目 |
| internal/service/app_test.go:86-92 | 测试依赖 `sbx-builderday-key` 在预设中，需要同步修改 |
| internal/service/e2e_test.go:24、42-43、78、87、94、98 | 真实 VM 端到端测试固定使用 xapi 和比赛 key。改成 llmapi 后要换 key 的来源，并修改断言里的域名 |
| internal/service/workspace_e2e_test.go:34 | 引用本机真实存储里 ID 为 `xapi` 的供应商 |

**B. 建议改（当前文档和注释）**

| 位置 | 说明 |
|---|---|
| docs/features.md:13 | 供应商条目补充“七牛快捷选项、模型列表” |
| docs/modules.md:87、98-99 | cagectl 示例仍是 xapi 加比赛 key。这是早期接口文档，可以加一句说明，而不是改写 |
| cmd/cagectl/main.go:133 | 注释提到 `sbx-builderday-key`，可以保留 |
| CLAUDE.md:41-45、AGENTS.md:41-45 | 前期实验记录。建议追加 llmapi 的记录，不改写历史。按约定，改动 CLAUDE.md 的 PR 不能由作者自己合并 |

**C. 测试夹具（行为不依赖它们，可以不改）**

- internal/provider/provider_test.go:10-84（`xapi()` 夹具）。建议补一个“根路径 + Anthropic 协议”的 Rules 用例。
- internal/runtime/runtime_test.go:20-27、44、111-119
- internal/policy/policy_test.go:7-9、64-74、86-103
- internal/service/monitor_test.go:22、49、67-77
- internal/service/app_test.go:43-44

**D. 历史记录（不要改）**

- docs/validation/ 下：2026-10-23 的各份 md 和 json、2026-10-24-conversations.md:17-18、2026-10-25-final-delivery.md:42、76、README.md:58
- docs/architecture.md:175、355、432-469、589-601、858
- docs/development-plan.md:11、32、56
- outputs/ 下的各份验收报告
- experiments/validation/：README.md:82、agent_init.py:23、probes.py:105、runtime.py:307、345、lab.py:117

另外两点：

- 已构建的 `bin/*.app` 内嵌的是旧版 presets.json，改完需要重新 `make app`。
- 用户本机已保存的供应商（例如 ID 为 `xapi` 的那条，见 `outputs/workspace-acceptance/report.md:125`）不会自动迁移，需要用户手动编辑或新建。

### 4.5 三个智能体各自需要确认的点

**Codex**

- **base 必须带 `/v1`。** Codex 会在 base_url 后面拼 `/responses`；门卫规则据此变为 `POST /v1/responses/*` 和 `GET /v1/models`（`provider.go:155-157`）。Codex 只支持 Responses API（`provider.go:61-63`；`core.js:40-44`）。
- **用新的七牛 key 实测以下能力**：流式事件、推理强度（`model_reasoning_effort`）、工具调用、`--image` 图片输入、`/v1/responses/compact`（规则已覆盖）。`GET /v1/responses/{id}` 不在规则内，需要确认 Codex 会不会用到。
- **默认模型不要写死。** xapi 的 `GET /v1/models` 只返回 5 个模型，`gpt-5.5` 已经不在列表里（已有探测）。llmapi 有哪些模型，要用新 key 去取。
- **参考 [外部]**：CC Switch 的七牛 Codex 预设用的是 `https://api.qnaigc.com/bypass/openai/v1`。可见七牛不同域名下的 Responses 路径并不一致，llmapi 应以它自己的 OpenAPI 为准。

**Claude Code**

- **base 用根地址。** `ANTHROPIC_BASE_URL` 是根地址，Claude Code 会自己拼 `/v1/messages`，所以填 `https://llmapi.qiniu.io`。对应的门卫规则是 `POST /v1/messages/*`（含 count_tokens）和 `GET /v1/models`。备选是 `https://llmapi.qiniu.io/anthropic`，规则会随之改变（`provider.go:161-163`）。
- **确认认证方式。** bearer 写进 `ANTHROPIC_AUTH_TOKEN`；x-api-key 写进 `ANTHROPIC_API_KEY`，并预先批准（`agentconfig.go:64-68`、`148-150`）。需要确认 llmapi 接受哪一种。
- **确认模型与兼容性。** 哪些模型可以通过 Messages 协议调用；流式、tool_use、thinking/effort 是否兼容。后台小模型也用默认模型（`agentconfig.go:70`），所以默认模型必须真实可用。
- **参考 [外部]**：CC Switch 的七牛 Claude 预设是 `https://api.qnaigc.com` 加 `ANTHROPIC_AUTH_TOKEN`，没有写模型。

**dsh**

- **协议与认证绑定。** 三种协议都能用（`provider.go:66-68`），但 anthropic 协议必须用 x-api-key，OpenAI 协议必须用 bearer（`provider.go:115-117`；前端在 `providers.js:142`、`192` 也强制如此）。
- **推荐 OpenAI 兼容方式。** 七牛官方的 dsh 指南用的是 OpenAI 兼容地址 `https://api.qnaigc.com/v1`（已有资料 `raw/13550…`），对应到 llmapi 就是 `https://llmapi.qiniu.io/v1` 加 openai-chat 协议（`POST /v1/chat/completions`）。如果选 anthropic 协议，需要确认 llmapi 接受 `x-api-key`。
- **核对固定版本。** 按 npm 0.2.1-rc.2 核对 llm-pi-ai 拼接 baseURL 的方式（`agentconfig.go:160-172`）。图片能力需要按模型手动勾选（`providers.js:182-183`；`conversations.go:197-199`）。
- **模型 ID 可能含 `/`**，例如七牛文档示例里的 `z-ai/glm-4.6`。`modelPattern` 允许这种写法（`provider.go:51`）。

**价格**：七牛预设没有价格。xapi 的 `/v1/model/info` 返回 403、`/key/info` 返回 400（已有探测），目前没法自动获取单价和 key 额度。

### 4.6 参考：CC Switch 的预设结构 [外部]

- CC Switch 的 `ProviderPreset` 包含 `name`、`websiteUrl`、`apiKeyUrl`、`settingsConfig`、`category`、`apiKeyField`、`endpointCandidates`（多个候选地址，用于测速）、`icon`/`theme`、`modelsUrl`（覆盖模型列表地址）等字段。
- 它的七牛预设分类是 `aggregator`，候选地址为 `api.qnaigc.com` 和 `api.modelink.ai`。
- AIofCage 如果要做“厂商快捷选项”，至少需要：厂商名和图标、获取 key 的链接、每个智能体的完整 base、可选的 `models_url`，以及“只需填写 API Key”的提示。

---

## 五、终端右侧文件侧栏的接入点

### 5.1 组件结构与 CSS 布局 [代码]

```
TerminalTab（sandbox-detail.js:86-187）
└ div.terminal-panel（:153）
  │  竖向 flex，高度 calc(100dvh - 272px)，最小 460px（main.css:326）；窗口 ≤1120px 时最小 360px（:429）
   ├ 恢复面板 .conversation-recovery（:159；css:332-336）
   ├ div.terminal-tabs（:160-175；css:331，377 起不换行）
   │   ├ .terminal-session-tabs（css:337、378）
   │   ├ .terminal-new > details 菜单（css:344、380-385；菜单以 .terminal-panel 为边界定位，见 terminal-menu.js:2-14）
   │   └ “在系统终端中打开” .system-terminal（css:346）
   ├ TerminalView（terminal.js:200-204）
   │   ├ 智能体：.conversation-container（conversation-view.js:31；css:386）
   │   │        → .conversation-controls（:387）→ .conversation-messages（:394，flex:1，可滚动）
   │   └ Shell：.terminal-view（terminal.js:224；css:347）→ .terminal-host（xterm；css:348）
   ├ .terminal-exited（:178-179；css:349）
   ├ form.terminal-composer（:184、189-228；css:356-373）
   └ .terminal-drop-hint（:185；css:374），绝对定位，覆盖整个面板
```

接入要点：

- **布局**：需要在标签栏下面加一层横向容器。左列放视图、结束提示和输入框，右列放侧栏，两列都设 `min-width:0` 和 `min-height:0`。输入框要不要放在左列里，是产品选择。
- **xterm 尺寸**：Shell 的 xterm 用 ResizeObserver 自动 refit（`terminal.js:214-218`），侧栏开合时会自动重排。不显示时，终端停放在屏幕外一个 900×500 的容器里（`:70-77`）。
- **事件处理范围**：粘贴、Ctrl+V 和拖放都在整个 `.terminal-panel` 上捕获（`sandbox-detail.js:154-158`）。这会带来两个问题：侧栏里的输入框会受 Ctrl+V 图片快捷键影响（`terminal.js:60-67`）；把文件拖到侧栏会被当作图片附件（`:158`）。这些处理需要限定在左列。
- **窄窗口**：`main` 最大宽度 1640px（css:49），断点为 1120、760、540px（css:418-476）。窄窗口下侧栏应能收起。
- **图标**：`icons.js` 里有 `folder`，没有 `file`。

### 5.2 现有的文件类后端接口 [代码]

| 接口 | 作用 |
|---|---|
| `OpenProjectFolder(id)`（sandboxes.go:233-240） | 用 `/usr/bin/open <项目目录>` 在访达中打开 |
| `RevealAuditLog(id)`（monitor.go:416-423） | 用 `open -R` 在访达中显示审计 JSONL |
| `ExportAuditTimeline(id)`（monitor.go:428-450） | 写出脱敏的时间线 JSON |
| `SelectProject()`（app.go:109-114） | 目录选择器 |
| `prepareProject`、`checkCopySize`、`copyTree`（sandboxes.go:260-386） | 导入项目：上限 2 GB 或 20 万个文件；符号链接复制链接本身，不跟随 |
| 终端图片 `WriteTerminalImages`（runtime/terminal_images.go:22-61） | 只写 Agent home，逐级用 `O_NOFOLLOW` 打开（terminal-images.py:12-22） |

**没有任何列举目录或读取项目文件的接口。** 在 `internal/service` 里 grep `ReadDir|os.ReadFile|WalkDir|os.Open(`，只命中审计、用量和项目复制相关代码。

注意：App 的所有导出方法都会绑定到前端（`main.go:78`），新增的接口也会对前端可见。

### 5.3 宿主与容器的路径关系 [代码]

- **同一绝对路径。** 项目以相同的绝对路径绑定挂载：`type=bind,src=<ProjectPath>,dst=<ProjectPath>`，工作目录也是这个路径（`manager.go:543-544`；`ExecArgs` 见 `:1064`）。CLI 报告的路径可以一一对应到宿主路径。
- **项目位置受限。** 项目必须位于 `~/CageWorkspace` 内，且路径本身不能含符号链接（`sandbox.go:160-163`、`189-215`）。这是 VM 唯一的共享目录（见 CLAUDE.md）。
- **home 不在宿主上。** Agent home 是 Docker 卷，挂在 `/home/agent`（`manager.go:542`），宿主不能直接访问。容器根文件系统只读，`/tmp` 是 tmpfs（`:538-541`）。
- **宿主可以直接读项目。** 用 `os` 包直接读取项目目录即可，沙箱停止时也能浏览，不需要 `docker exec`。容器内 uid 1000 写入的文件在宿主上的属主和权限映射未验证 **[推测]**。

### 5.4 需要新增的 service 接口（建议）

1. **`ListProjectEntries(sandboxID, dir)`**：只列一级。返回名称、相对路径、类型（目录、文件、链接、其他）、大小、修改时间、是否默认隐藏，以及结果是否被截断。
2. **`ReadProjectFile(sandboxID, path)`**：只读常规文件，限制读取字节数（例如 256 到 512 KiB）。区分文本、二进制和图片，返回 UTF-8 文本或大小受限的图片数据。
3. **`RevealProjectPath(sandboxID, path)`**：只做 `open -R`（在访达中显示）。**不提供“用默认应用打开”。**
4. **改动标记**
   - 提供 `ConversationChanges(conversationID)`，或者在每轮结束时发出 `conversation:event{type:"files"}`。
   - 实现方式：在 `SendConversationMessage`（`conversations.go:164-312`）每轮开始（`:242` 启动进程）和结束（`:293-309`）时，对项目做一次有上限的元数据快照（路径 → 大小、修改时间、类型），排除忽略目录后求差。
5. **可选：`WatchProject` / `UnwatchProject`**，推送 `project:changed` 事件。不过仓库目前没有 fsnotify 或 FSEvents 依赖；macOS 上的 kqueue 每个文件要占一个句柄，不适合大目录；virtiofs 的写入能否触发宿主的 FSEvents 也未验证 **[推测]**。按轮次比较快照更简单。

**“本会话改动”的两种做法**

- **从 CLI 事件取路径。** Codex 的 `file_change` 项、Claude Code 的 Write/Edit 工具参数、dsh 的 `tool_call` 都能拿到文件路径。但有两个问题：
  - `agent-turn.py` 目前**有意只转发工具类型或名称，不转发参数**（`:62-63`、`75-76`、`87-90`），测试 `test_agent_turn.py:29-38` 也断言参数不外泄；
  - Shell 命令（如 `sed -i`、`npm install`）造成的改动抓不到。

  各 CLI 事件的字段结构需按固定版本核对 **[推测]**。
- **比较宿主快照。** 与智能体无关，Shell 造成的改动也能覆盖。但多个会话或 Shell 并行时，无法把改动归到某一个会话，所以文案应写“本轮期间项目内的改动”。

建议以快照比较为主，CLI 报告的路径只用作归属提示。只显示路径，不写入审计（与“正文不进审计”的原则一致）。

### 5.5 约束

- **防止路径越界**
  - 前端只传相对路径。后端使用沙箱记录里的 `ProjectPath`，不接受宿主绝对路径。
  - 用 Go 的 `os.OpenRoot(ProjectPath)`（go.mod 为 1.26.4，`os.Root` 从 1.24 起可用），通过 `Root.Open`、`Root.Lstat` 和 `File.ReadDir` 访问，防止 `..` 和指向根目录以外的链接。
  - 拒绝含 NUL 的路径。
- **符号链接**
  - 链接由智能体控制，可以指向 `~/.ssh/id_rsa` 这样的宿主路径。这个路径在容器里不存在，但宿主一旦跟随，就会读到真实文件。这是典型的“沙箱植入、宿主跟随”。
  - 列表用 Lstat，把链接显示为链接，不展开（同时避免目录成环）。预览时不跟随链接，或只允许指向根目录内的目标。
  - 不要先 Lstat 再 Open：两步之间文件可能被替换。应该用 Root 打开后，对句柄调用 `Stat()` 判断类型。
- **特殊文件**：打开 FIFO 会一直阻塞，导致绑定调用挂起；套接字和设备文件也一律拒绝。打开时用非阻塞方式，并用句柄的 `Stat` 确认是常规文件。
- **大目录**
  - 按需展开，每层设条目上限（例如 1000 到 2000 条，显示“还有 N 项”）。
  - `node_modules`、`.git`、`.venv`、`dist`、`build`、`target`、`__pycache__`、`.next`、`.cache` 默认折叠或隐藏。
  - 不计算目录总大小。
  - 快照限制文件数和耗时（可参照 `sandboxes.go:260-263` 的 2 GB / 20 万），超限时显示“改动过多，未完整统计”。
- **不在宿主上执行项目内容**
  - 项目文件由智能体控制。不能在宿主上对项目运行 `git status` 这类命令：恶意 `.git/config` 里的 `core.fsmonitor` 可以在宿主上执行命令。需要 git 信息时，用 `docker exec` 在容器内运行。
  - 同样不要用 `/usr/bin/open` 打开项目里的文件，`.command`、`.app`、`.terminal` 会在宿主上执行；只用 `open -R`。
- **前端渲染**
  - 继续只用文本节点渲染（沿用 `conversation-view.js:4-18` 的做法），不用 innerHTML 或 iframe 预览 HTML、SVG。
  - 图片用文件字节生成 blob 或 data URL，不用 `file://`。
  - 单行长度和总长度都设上限。
- **隐私**：文件路径和内容不写进审计、日志或诊断信息。

---

## 附：外部资料（2026-10-29 读取）

- [七牛 AI 编程工具配置大全](https://developer.qiniu.com/aitokenapi/13417/tools-AI-Coding-api)：只列出 `https://api.qnaigc.com/v1`，没有提到 llmapi。
- [CC Switch Claude 预设](https://raw.githubusercontent.com/farion1231/cc-switch/main/src/config/claudeProviderPresets.ts)：七牛条目为 `https://api.qnaigc.com`，分类 `aggregator`，有 `modelsUrl` 字段。
- [CC Switch Codex 预设](https://raw.githubusercontent.com/farion1231/cc-switch/main/src/config/codexProviderPresets.ts)：七牛条目为 `https://api.qnaigc.com/bypass/openai/v1`，模型 `gpt-6-astra`。
- [ngrok：Claude Code 接入网关](https://ngrok.com/docs/ai-gateway/agents/claude-code)：`ANTHROPIC_BASE_URL` 不带 `/v1`。
- 两次网页搜索都没有找到 llmapi.qiniu.io 的公开文档。
