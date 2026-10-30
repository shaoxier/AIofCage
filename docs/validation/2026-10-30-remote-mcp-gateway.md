# 远程 MCP 经门卫按工具授权验证

[验收记录索引](README.md)

## 改动

对应首轮评审的第一条保留意见：远程 HTTP/OAuth MCP 原来只按服务授权，数据库连接器按工具授权，两者粒度不同。见[第二轮 todo 清单](../round2/todo.md)第八节。

1. **所有 MCP 服务都只经门卫的 MCP 入口访问。** Agent 配置里的地址一律是 `http://gatekeeper:8890/mcp/<服务>`。
   - 代理不再为远程 MCP 加放行规则，也不再持有它的令牌，Agent 经代理直连远程服务会被拒绝。
   - 门卫转发时自己加上令牌（钥匙串里的 Bearer 令牌，或宿主登录后得到的 OAuth 访问令牌），用代理同一套传输连接上游：只连公网地址、校验证书、不跟随跳转（`internal/gatekeeper/mcp.go`）。
2. **按工具授权。** 门卫逐条检查 JSON-RPC 消息：
   - `tools/call` 只放行沙箱授权的工具；
   - `tools/list` 的回复里只保留授权的工具；
   - 远程服务 `initialize` 回复里的 resources、prompts、completions 能力被去掉，对应的列表请求由门卫直接回空列表，读取资源和提示词被拒绝，因为工具授权管不到它们。
3. **默认只开放标注只读的工具，新工具默认不开放。**
   - 宿主按 MCP 协议向服务要工具清单，在保存服务、测试连接、OAuth 登录后和首次启动沙箱时获取，并记下服务自己的 `readOnlyHint` 标注（`internal/mcp/client.go`）。
   - 沙箱启用远程服务时，默认只授权标了只读的工具。
   - 授权一旦确定就固定保存（`internal/runtime/connectors.go` 的 `prepareRemote`），服务之后新增的工具要用户勾选才开放。
   - 服务换了地址，旧工具清单作废，各沙箱对它的授权也一并清掉，拿到新清单后重新按默认只读固定。
4. **堵住工具名检查的绕过。** 原来门卫用 Go 结构体读 `params.name`：Go 匹配键名不分大小写（还做 Unicode 折叠，`paramſ` 等于 `params`），重复的键以最后一个为准；JavaScript、Python 写的服务按原样匹配。于是 `{"name":"未授权工具","Name":"授权工具"}` 能让门卫检查一个工具、服务执行另一个，`"method"` 和 `"Method"` 同理。
   - 现在消息外层和 `tools/call` 的参数里，只要出现重复的键，或者与已知键名只差大小写的键，整条消息都拒绝（`internal/gatekeeper/mcp_message.go`）。
   - 批量消息、对象后面还跟着内容、非 UTF-8 正文也一并拒绝。
   - 这条同样适用于原有的数据库连接器。
5. **凭据不进沙箱，也不经回复泄露。**
   - 远程令牌只在门卫的连接器配置里。
   - 回复和错误正文里出现的令牌会被打码，错误正文先解码再打码。
   - 服务拒绝令牌（401/403）时，门卫给 Agent 返回说明原因的 JSON-RPC 错误，不转出 401，免得 Agent 对门卫发起 OAuth 登录。
6. **公开服务。** 自定义 HTTP MCP 可以勾选“不需要令牌”，用于文档检索一类的公开服务。
7. **界面与命令行。**
   - 工具连接页显示每个服务有几个工具、几个标注只读。
   - 沙箱的 MCP 设置里逐个勾选工具，标签分“标注只读”“标注可能删改”“未标注只读”，并提供“只开放标注只读的”和“全部关闭”。
   - `cagectl` 新增 `mcp add --no-auth`、`mcp tools ID`，以及 `sandbox mcp … --grant 服务=工具,工具`；事件输出带服务名和工具名。

## 自动测试

- `internal/gatekeeper`：
  - 十种歧义消息全部返回 400，连接器一次也没收到。包括 name 与 Name 并存、只有 Name、重复 name、method 与 Method 并存、`paramſ`、重复 method、批量、两段 JSON、name 不是字符串、非 UTF-8。
  - 一条测试先证明 Go 自己确实会读出折叠后的键，说明这些检查是必要的。
  - 远程服务用带校验证书的 TLS 上游测试：initialize 只剩 tools 能力；tools/list 只剩授权工具；工具结果里的令牌被打码；未授权工具、resources/read 被拒绝；resources/list 由门卫回空列表，不访问上游；审计记录服务名、主机和工具名，不含参数和令牌。
  - 上游返回 401 时不出现 WWW-Authenticate，302 不跟随也不暴露跳转目标，400 正文里的令牌被打码。
  - 事件流里一条消息分成多行 `data:` 时，转发后仍是一条完整的 JSON；被截断的事件丢弃。
  - 远程地址只接受 `https://域名[:443]/规范路径`；空令牌可以（公开服务）；远程服务没有授权时拒绝所有工具。
- `internal/mcp`：工具清单按协议获取（initialize、notifications/initialized、分页 tools/list、结束会话），只按原样的 `name` 键读工具名，跳转不跟随，401、JSON-RPC 错误、非 MCP 回复分别报错；默认授权只包含标注只读的工具。
- `internal/runtime`：
  - 门卫配置里远程服务只出现在连接器路由中，没有代理规则和代理密钥；真实门卫接受这份配置。
  - 工具已知时固定默认授权；服务新增一个标注只读的工具后，已固定的授权不变。
  - 不再提供的工具从原授权里去掉；请求授权清单里没有的工具会被拒绝；工具未知的服务不会被固定成空授权。
- `internal/service`：公开服务保存后自动获取工具清单，且不发送令牌；改回令牌必须填令牌；改回公开服务会删除旧令牌；换地址后清空旧清单。服务层测试改为默认不联网，原来保存 GitHub 服务时会真的连 api.githubcopilot.com。
- 界面：在浏览器里用模拟后端检查了工具连接页、沙箱 MCP 设置和创建向导。已删除的工具从授权里去掉；两个快捷按钮和保存传给后端的授权与界面一致；没有工具清单的服务给出获取方法。
- `make check`、`make test`（含竞态检查）、前端 `node --test` 通过。

## 真机验证（2026-10-30）

环境：专用 Colima profile `aiofcage`（colima 0.10.3），本分支构建的 `cagectl` 和门卫镜像。新建沙箱 `t2-remote-mcp`，装了三款 Agent：Codex 0.164.0（七牛 xapi，`gpt-6.1-sol`）、Claude Code 2.1.309 和 DeepSeek Harness 0.2.1-rc.2（DeepSeek，`deepseek-flash`）。远程服务用了两个不需要令牌的公开服务：

- Cloudflare 文档 `https://docs.mcp.cloudflare.com/mcp`，2 个工具，都标了只读；
- DeepWiki `https://mcp.deepwiki.com/mcp`，3 个工具，都没标注。

创建沙箱后，Cloudflare 默认开放 2 个工具，DeepWiki 默认 0 个。

**沙箱内直接请求门卫：**

| 检查 | 结果 |
| --- | --- |
| initialize 的能力 | Cloudflare 只剩 `tools`；DeepWiki 剩 `experimental`、`tools` |
| tools/list | Cloudflare 2 个；DeepWiki 0 个 |
| resources/list | 门卫回空列表 |
| 调用已授权工具 | Cloudflare 文档检索返回真实结果 |
| 调用未授权工具 | JSON-RPC 错误 -32001，说明该服务目前没有开放任何工具 |
| name/Name、method/Method、`paramſ`、批量消息 | 都是 400 `mcp_invalid_message` |
| 用 Cloudflare 服务的凭据访问 DeepWiki 的入口 | 403 `credential_mismatch` |
| 经代理直连 `docs.mcp.cloudflare.com` | CONNECT 被拒（403，`default_deny`） |
| Agent 配置里的地址 | 只有 `gatekeeper:8890/mcp/cfdocs` 和 `gatekeeper:8890/mcp/deepwiki`，没有远程地址 |

**运行中改授权：** 用 `cagectl sandbox mcp t2-remote-mcp cfdocs deepwiki --grant deepwiki=read_wiki_structure` 放开一个工具后，不重启沙箱，DeepWiki 的 tools/list 立即只多出这一个工具，调用返回 `modelcontextprotocol/python-sdk` 的文档目录；另一个工具仍被拒，错误里列出了已开放的工具名。

**三款 Agent 的真实任务（按应用调用 CLI 的参数执行）：**

| Agent | 任务 | 结果 |
| --- | --- | --- |
| Codex | 只用 Cloudflare 文档的工具回答 Workers KV 是什么 | 调用 `search_cloudflare_documentation`，回答正确，46 秒；Codex 访问 chatgpt.com、github.com 的请求被默认拒绝 |
| Claude Code | 只用 DeepWiki 的工具列出 python-sdk 文档的前 3 个主题 | 两个服务都是 connected，可见的 MCP 工具只有授权的 3 个；调用 `read_wiki_structure`，回答正确，7 秒 |
| DeepSeek Harness | 只用 Cloudflare 文档的工具回答 D1 是什么 | 调用 `search_cloudflare_documentation`，回答正确，18 秒 |

门卫的审计事件里，每次 `tools/call` 都带服务名、主机和工具名，例如 `mcp:cfdocs/search_cloudflare_documentation`。拒绝的调用也有记录，例如 `mcp:deepwiki/ask_wiki_question` 记为 `mcp_tool_denied`。事件里没有提问内容和工具参数。

**数据库连接器回归：** 用新门卫启动已有的 `s9-db` 沙箱（演示商城数据库，专用只读账号）。

- tools/list 只有 `query`，只读查询返回结果；
- `{"name":"drop_everything","Name":"query"}` 返回 400；
- 检查后沙箱已停回原状态。

**带令牌的链路：** 用本次生成的合成令牌（不对应任何真实账号）添加了一个指向 Cloudflare 文档的“访问令牌”类服务，经门卫的调用正常返回。随后把沙箱里的环境变量、进程参数、四份 Agent 配置，以及主目录、`/tmp`、`/etc` 下的文件导出到宿主比对，都没有这个令牌。验证结束后已停用这个服务，并删除服务和钥匙串条目。

## 独立复查与修复

改完后另请一个子代理按威胁模型只读复查。它没有找到让未授权的 `tools/call` 绕过门卫的方法，报了 7 个问题，已全部修复：

| 问题 | 修复 |
| --- | --- |
| 沙箱启动和修改服务地址同时发生时，旧服务器的默认授权会固定到新服务器上；保存工具清单时写回整条服务记录，可能覆盖用户刚改的地址 | 远程服务的授权记下它对应的地址（`mcp_grant_urls`），地址不同时不生效，改用新服务器的默认；工具清单用原子更新写入，而且只在地址和凭据类型都没变时写 |
| `cagectl mcp add` 用同一个 ID 会原地换掉地址，授权不清；`mcp rm` 会删除还在用的服务 | `cagectl` 不再原地替换，也不删除在用的服务；地址不同的旧授权本来也已不生效 |
| 换了域名后，旧令牌或 OAuth 登录会直接发给新地址 | 令牌类服务换域名时必须重新填令牌；OAuth 服务删掉旧登录、重新授权；界面里令牌输入框变为必填并说明原因 |
| 打码只比对原样字节，用 JSON 转义写的令牌或 deflate 压缩的成功回复能带出令牌 | 成功回复先解压，无法解压的格式不转交；远程消息按 JSON 解码后再找一遍令牌 |
| 事件流里非 data 的行没有长度上限 | 每一行都受同一个上限约束 |
| 事件流只按 LF 断行，不认单独的 CR 和开头的 BOM，客户端看到的事件可能和门卫检查的不同 | 按客户端的方式在 CRLF、LF、CR 断行，去掉 BOM |
| 两个“转义键名”测试其实没有转义，测试里的事件流解析也只按 LF 断行 | 改成真正带 `\u0061` 的用例，测试解析按客户端方式断行 |

另外补了一项检查：一个沙箱最多启用 16 个 MCP 服务，这是门卫的路由上限，超出时提前报错，不会等到启动时才失败。

修复后用新门卫重启 `t2-remote-mcp` 复测：

- 沙箱内对门卫的直接检查结果与上文相同；之前没带地址的那条授权按新规则失效，恢复为默认；
- 运行中放开工具仍立即生效；
- Claude Code 调用 DeepWiki 正常；
- Codex 复测时 `xapi.qiniu.io` 从宿主和虚拟机都连不上（宿主 curl 15 秒超时），改用 DeepSeek 供应商跑同一个任务，13 秒，结果正确。Codex 启动时会先列资源，门卫直接回空列表，没有产生拒绝记录。

## 范围与限制

- **没有用真实 GitHub、Notion、Linear、Sentry 账号验证。**
  - Bearer 注入在真机上只用合成令牌和不校验令牌的公开服务验证过；OAuth 服务的转发路径与它相同，但没有真实登录。
  - GitHub 官方远程 MCP 需要用户提供只读 PAT 后再验；它的工具是否都带只读标注，也要那时确认。
- **只支持 streamable HTTP。**
  - 只有旧式 HTTP+SSE 接口的服务不能接入。
  - 门卫对 GET 回 405，不提供服务端主动推送的事件流，所以收不到工具列表变化通知；服务新增工具，要在应用里重新测试连接才会出现在可勾选列表里。
- **远程服务的资源和提示词不开放。** 工具授权管不到它们，所以门卫直接不提供。
- **只读标注来自服务自己。** 默认授权依赖这个标注，界面称“标注只读”，不保证工具真的不写。没有标注的工具默认不开放，需要用户自己判断。
- **门卫只按工具名放行，不检查参数。** 已授权的工具仍可能被滥用，例如用检索工具外传数据，这一点和数据库连接器一样。
- **令牌打码不覆盖连接器的成功回复。** 远程服务的回复都会打码；数据库连接器 2xx 回复里的数据库凭据不检测。
- **已在运行的沙箱要重启一次才会改用新门卫。**
  - 重启前仍按服务授权，远程 MCP 也仍走代理。
  - 这期间应用如果要给它下发含远程 MCP 的新配置，旧门卫会拒收，界面提示更新失败，原配置继续生效。重启沙箱即可。
