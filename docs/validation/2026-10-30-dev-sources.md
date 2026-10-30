# 开发环境与门卫下载修复验证

[验收记录索引](README.md)

## 改动

对应第二轮会议第 7 条（创建沙箱时的开发环境）和 todo 清单第 7 项的最小版本，见[第二轮 todo 清单](../round2/todo.md)。

1. **开发环境预设**（`internal/devsource`）。创建沙箱时或在沙箱设置里选择：
   - npm：不开放、官方源（registry.npmjs.org）、国内镜像（registry.npmmirror.com、cdn.npmmirror.com），三选一；
   - GitHub 只读：克隆公开仓库，下载源码包、Release 文件和原始文件。

   每个预设的规则都是：只放行 GET/HEAD 下载（GitHub 克隆另放行 `git-upload-pack` 的 POST）；发布（PUT/POST 等写方法）和推送（`git-receive-pack`）显式拒绝；请求自带凭据（Authorization、API Key、Cookie）时拒绝。GitHub 的其他页面和 api.github.com 不开放。同时写入客户端设置：npm 的源地址、关闭 audit 和更新提示，`GIT_TERMINAL_PROMPT=0` 让 git 遇到私有仓库时直接报错，不卡在输入密码。运行中修改即时生效。
2. **路径规则支持单段通配**，例如 `/*/*/info/refs`，才写得出 GitHub 克隆的规则。规则可以标成“只允许匿名”。
3. **门卫放行包管理器的正常路径**：允许字面的 `! ( ) * '`（Go 模块路径用 `!` 表示大写），以及两种编码，都解码后再匹配规则：任意位置的 `%40`（npm 镜像把 scope 写成 `%40types`），和 scope 包名里的 `%2F`（`/@types%2Fnode`）。其他编码会改变路径的切分方式，仍然拒绝。发往上游的路径保持原样。
4. **跳转**：请求没有注入密钥、跳转目标是 https 443、目标路径也允许 GET 时，原样把跳转交给客户端，由客户端经门卫重新请求、重新检查。304 照常放行。其余跳转拒绝，日志记下跳转目标的域名；带注入密钥的请求被拒时，不向 Agent 透露跳转目标。
5. **看得懂的 403**：状态行改为 `403 Blocked by AIofCage sandbox policy`，正文用中英文说明原因，提醒不要重试或换源，附上请求日志编号；客户端要 JSON 时返回 JSON；响应头带 `X-Aiofcage-Event`。只显示状态行的客户端（git、pip、建立隧道失败时的 curl）也能看出是沙箱策略拦的。

## 自动测试

- `internal/policy`：单段通配的匹配和校验（克隆、源码包放行，推送、多一段或少一段拒绝）；匿名标记传到门卫。
- `internal/devsource`：
  - 选择校验：未知预设、重复、官方源和镜像同时选都会被拒；
  - 规则只开放下载：发布命中显式拒绝，GitHub 的其他页面、api.github.com 和未选中的镜像都不开放；
  - 客户端设置。
- `internal/gatekeeper`：
  - scope 包、`%40`、`!` 和 `( ) * '` 路径都能通过，且上游收到的路径与原样一致；`%2e%2e`、其他位置的 `%2f` 和两处编码都被拒绝；
  - 自带凭据被拒（中文正文、日志编号、状态行）；
  - 发布被拒时按 JSON 回答；
  - 跳转：同站相对跳转和允许的目标透传，外站、http、其他端口、需要密钥的目标都被拒，日志记下目标域名；304 透传；
  - 建立隧道被拒时状态行写明沙箱策略。
- `internal/runtime`：开发环境的规则进入门卫配置，Agent 环境写入 npm 源和 git 设置；没选开发环境时一条规则都没有；冲突选择被拒绝。
- 界面：用模拟后端检查了创建向导里的“开发环境”一节、沙箱详情的“开发环境”一行和设置对话框，传给后端的选择正确，官方源和镜像互斥。
- `make check`、`make test`（含竞态检查）、前端 `node --test` 通过。

## 真机验证（2026-10-30）

本分支构建的 `cagectl` 和门卫，新建沙箱 `t4-dev`，开启 npm 国内镜像和 GitHub 只读，在沙箱里执行：

| 检查 | 结果 |
| --- | --- |
| `npm config get registry` | `https://registry.npmmirror.com/` |
| `npm view @types/node version`（scope 包元数据） | 26.6.6 |
| `npm install @types/node lodash` | 成功（含 CDN 上带 `%40types` 的跳转下载） |
| `curl -X PUT` 发布到镜像 | 403，命中禁止规则，中文说明 |
| 带 `Authorization` 下载 | 403，开发环境只允许匿名下载 |
| 访问未开放的 registry.npmjs.org | 建立隧道被拒，状态行 `403 Blocked by AIofCage sandbox policy` |
| `git clone --depth 1` 公开仓库 | 成功 |
| `git push` | 失败：没有凭据时 git 直接报错，不弹出密码提示 |
| 直接 POST `git-receive-pack` | 403，命中禁止规则 |
| 源码包下载（github.com → codeload） | 200，跟随 1 次跳转 |
| Release 文件下载 | 200，跟随 1 次跳转 |
| raw.githubusercontent.com | 200 |
| GitHub 其他页面 | 403，“github.com 不在允许列表里” |
| api.github.com | 建立隧道被拒 |

- **运行中切换**：执行 `cagectl sandbox dev t4-dev npm github`，不重启沙箱，npm 源立即变为官方源，`npm install @types/node` 成功。
- **全部关闭后让 Agent 装包**：Claude Code（DeepSeek）执行 `npm install lodash` 被拦，重试一次后停手，回答说明是沙箱策略拦下的、需要放开访问；没有换源，也没有改配置。
- **网络情况**：同一天早些时候，这台机器到 GitHub 的 HTTPS 连不上（宿主直连和经代理都失败，虚拟机里 raw.githubusercontent.com 被解析到 0.0.0.0），门卫当时记录的是“已放行、连接上游失败”；上表是网络恢复后的结果。

## 复查

独立复查的子代理因为接口额度限流中途退出，没有给出结论；这一轮是我自己按威胁模型逐项复查的，结论如下：

- **路径**：规则按解码后的路径判断，上游收到的是原样写法；双重编码、`..` 段和其他位置的 `%2F` 都被拒绝。
- **跳转**：目标的域名、端口、协议都要检查，并按 GET 重新判断是否允许；注入了密钥的请求不透露跳转目标。
- **拦截提示**：出现的域名都经过校验；HEAD 请求不带正文。
- **旧门卫**：拒收带新字段的配置，不会放宽。

额度恢复后补一次独立复查。

## 范围与限制

- **只放行下载，不防数据外传**：URL 里仍可能夹带数据，包的作者也能看到下载记录。
- **GitHub 只覆盖公开仓库的匿名访问**：私有仓库要凭据注入，留到以后；git LFS 和 api.github.com 不开放。
- **“只允许匿名”只检查请求头**：检查的是 Authorization、API Key 和 Cookie；写在 URL 里的令牌（例如 raw.githubusercontent.com 的 `?token=`）查不出来。这类令牌只能用来读，下载源本身也不放行任何写操作。
- **没有做的部分**：PyPI、Go 模块、crates、Maven 的预设，工具链档位，限时放行，以及给 Agent 的全局说明，都没有做。
- **需要重启沙箱**：已在运行的沙箱要重启一次才换成新门卫。旧门卫不认识新规则字段，应用给它下发开发环境时会提示更新失败。
- **新旧版本混用会丢设置**：存储层写回时只保留自己认识的字段。升级后如果还用旧版本的应用或 `cagectl` 改同一份数据，会丢掉开发环境和远程 MCP 授权的地址等新字段。
