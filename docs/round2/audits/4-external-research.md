# AIofCage 第二轮外部调研：文件侧栏、供应商预设、七牛接口

调研日期：2026-10-29。资料来源分四类，文中逐条标注：

- **官方文档**：写页面自带的“最近更新时间”或发布日期；页面没有日期的写“访问于 2026-10-29”。
- **源码**：用 `gh api` 读取 GitHub 仓库，写分支和提交日期。
- **无凭据探测**：本机 curl 只发 GET 或空 JSON POST，不带任何 key，用 401（路由存在、要鉴权）和 404（路由不存在）判断接口有没有开。全程没有读取或使用比赛 key 或其他密钥。
- **同目录资料**：`5-qiniu-api-probe.md` 是同一轮调研的七牛接口实测记录；llmapi 的接口清单取自它公开的 `/openapi.json`，原文件没有入库。引用时注明。

本轮没有改动仓库。

## 结论速览

1. **文件侧栏**：传统 Shell 软件的“文件面板”其实是远程 SFTP 浏览器；“跟随终端目录”都依赖 shell 主动上报目录，厂商自己也标注为实验性、不保证所有系统可用。几款 agent 工作台（Codex app、Cursor 3 Agents Window、Conductor、Zed、Warp）的共同做法是：右侧面板优先放“变更/审阅”，其次放项目文件树；根目录固定为项目或工作树，不跟随 cwd；路径可以插入输入框、拖进输入框或用 @ 引用；面板都能收起，多数能拖宽。AIofCage 建议采用“右侧可收起面板，分‘变更’和‘全部文件’两个标签，根目录固定为沙箱项目目录”。第一版只做只读：浏览、Git 状态、插入路径、复制路径、在访达中显示。
2. **安全红线**：宿主不能直接在项目目录里运行 git，否则 agent 改写 `.git/config` 就能在宿主执行任意命令；宿主读文件不能跟随符号链接；预览只能按纯文本渲染，因为 Wails 网页里的脚本可以调用全部后端方法。
3. **CC Switch**（v4.0.21，2026-10-26；main 分支截至 2026-10-29）：Claude Code 和 Codex 各有 136 个预设，`main` 分支 10-27 新增 DeepSeek Harness 预设，尚未发版。用户选好预设后通常只需填 API Key。有“七牛云”预设：Claude Code 地址是 `https://api.qnaigc.com`，Codex 地址是 `https://api.qnaigc.com/bypass/openai/v1`，没有 llmapi 或 xapi 预设。可以拉取模型列表，按错误类型给出提示，模型框始终允许手填。
4. **七牛公开文档**：只列出 `https://api.qnaigc.com`（国内）和 `https://api.modelink.ai`（海外）。`/v1/chat/completions`、`/v1/messages`、`/v1/models` 走标准路径；Responses **不在** `/v1/responses`，只能走 `/bypass/openai/v1/responses`。Claude Code 的 `ANTHROPIC_BASE_URL` 填 `https://api.qnaigc.com`，不带 `/v1`。`llmapi.qiniu.io` 和 `xapi.qiniu.io` 都不在公开文档里：两者用的是七牛 `*.qiniu.io` 证书，都是 XHub（LiteLLM 内核）网关。

---

## 一、终端与 agent 应用的文件侧栏

### 1.1 逐个核实

下表每行依次回答六个问题：侧栏放在哪里；展示目录树还是列表；是否跟随终端目录；有没有 Git 标记；有没有预览或 diff；主要交互；默认状态、能否收起和调宽。文档没写的标“未说明”。

#### iTerm2 3.7.6（2026-10-15 构建）

- **位置**：Toolbelt 在窗口右侧。工具有 Actions、Captured Output、Codecierge（AI 助手）、Command History、Jobs、Named Marks、Notes、Paste History、Profiles、Recent Directories、Snippets，3.7 新增 Session Status。**没有文件树**。
- **目录**：Recent Directories 按使用频率和最近使用时间列出目录，需要安装 Shell Integration；在提示符里点击路径可以打开 Navigator 窗口浏览文件系统。
- **Git / diff**：Toolbelt 没有 Git 标记。3.7 的“Claude Code Workgroup”会在 Claude 会话旁生成 Diff 会话（并排显示工作区改动）和 Code Review 会话，用工具栏一键切换。
- **交互**：⌘ 点击文件名，按 Semantic History 打开，可指定编辑器和行号。把文件拖进本地终端会插入路径（可加引号）；拖进远程会话可选择“上传并粘贴路径”。
- **默认与调整**：⌘⇧B 显示或隐藏。Profile 里有“Open Toolbelt”选项，勾选后新窗口自动打开工具栏；“Set Default Width”保存当前宽度，说明可以拖宽。
- 来源：[3.7 文档](https://iterm2.com/3.7/documentation-one-page.html)（访问于 2026-10-29，含 View > Toolbelt、Session Status、Claude Code Integration、Workgroups 各节）；[下载页与版本说明](https://iterm2.com/downloads.html)（3.7.6 构建于 2026-10-15）。

#### Warp

- **位置**：左侧 Tools 面板的第一个标签是 File Tree，⌘\ 打开左面板，也可以自定义快捷键。Code Review 面板的入口有四个：右上角 Code review 按钮、输入栏的 Git diff 标签、对话底部的 Review changes、对话右下角的标签栏。
- **目录**：目录树，文件增删时自动更新；SSH 场景需要在远端装 Warp 扩展。文档没说根目录是否随 `cd` 变化，只说“复制相对路径”是相对当前工作目录。
- **Git**：文件树没有 Git 颜色。相关需求 [#8969](https://github.com/warpdotdev/warp/issues/8969) 于 2026-03-29 提出，截至今日仍未关闭。Code Review 面板列出改动文件和增删行数。
- **预览 / diff**：自带编辑器，支持 LSP。Code Review 可以看未提交改动、对比 main 或任意分支；逐段（hunk）撤销、整体丢弃、直接编辑 diff；把行内评论**批量发给正在运行的 Claude Code、Codex 等命令行 agent**。
- **交互**：文件右键菜单有 Attach as context（附加到 agent 提示）、Copy path、Copy relative path、在新窗格或新标签中打开。输入框输入 @ 可以选文件、目录或符号，搜索始终相对 Git 根目录。
- **默认与调整**：文档没说默认是否可见、能否调宽。
- 来源：[File Tree](https://docs.warp.dev/code/code-editor/file-tree/)；[Code 文档合集](https://docs.warp.dev/_llms-txt/code.txt)（含 Code Review 全文）；[@ 上下文](https://docs.warp.dev/agents/local-agents/agent-context/using-to-add-context/)。以上均访问于 2026-10-29。

#### Wave Terminal v0.14.5（2026-04-16）

- **位置**：没有常驻侧栏。右侧“widget bar”的 files 按钮默认在 `~`（家目录）打开一个目录预览块。终端右键菜单的“File Browser”会在**该终端当前目录**新开一个预览块，目录靠 OSC 7 上报，打开时取一次，之后不跟随 `cd`。也可以用 `wsh view .` 打开当前目录。
- **目录**：类似访达的表格（`ls -alh` 风格），不是树。
- **Git**：无。
- **预览**：双击文件预览：文本进 Codeedit 编辑器，Markdown 渲染显示，按空格调用 Quick Look。两个目录块之间可以拖拽复制，能跨连接。
- **交互**：0.14.5 起可以从访达把文件拖进终端，粘贴带引号的路径；也可以把预览里的文件拖给 Wave AI。
- **默认与调整**：块可以拖动重排，拖动分隔线调整大小；widget bar 可以从 View 菜单显示或隐藏，按工作区记忆。
- 来源：[Widgets 文档源文件](https://github.com/wavetermdev/waveterm/blob/main/docs/docs/widgets.mdx)；[Release Notes](https://docs.waveterm.dev/releasenotes)；源码 `frontend/app/view/term/term-model.ts` 和 `pkg/wconfig/defaultconfig/widgets.json`（main 分支，最近提交 2026-10-05）。

#### Tabby v1.0.238（2026-10-07）

- **位置**：只有 SSH 标签页有 SFTP 按钮。面板从底部滑出，盖住终端 80% 高度、全宽；点一下终端区域就收起。本地终端没有文件面板。
- **目录**：面包屑加列表，双击面包屑可以直接输入路径；不是树。
- **跟随**：打开时定位到远端 shell 的当前目录，需要开启工作目录检测，否则显示帮助链接；打开后不再跟随。
- **Git**：无。
- **交互**：点击文件即下载。右键菜单有 Edit locally（下载到临时目录打开，监视改动，保存后自动上传）、Download、Delete、Create directory、Copy full path。支持拖入上传。没有“把路径插入终端”。
- **默认与调整**：高度在样式里写死为 80%，不能调。
- 来源：[源码](https://github.com/Eugeny/tabby)，master 分支（2026-10-29）中的 `tabby-ssh/src/components/sshTab.component.{pug,ts}`、`sftpPanel.component.{pug,ts,scss}`、`tabby-electron/src/sftpContextMenu.ts`。

#### Termius（桌面版更新日志最新一条 2026-10-28）

- **位置**：SFTP 是**独立标签页**，左右两栏分别是本地和远程，不是终端旁的侧栏。同一主机可以在终端和 SFTP 之间切换。
- **目录与跟随**：列表，可以输入完整路径跳转。文档没提跟随终端目录。2025-10 起“工作区”可以保存和恢复工作目录，但不是实时同步。
- **交互**：双击或选择“Open with”会把文件下载到临时目录编辑，检测到修改后提示上传。支持拖拽传输；隐藏文件默认不显示。
- 来源：[SFTP 文档](https://docs.termius.com/organize-and-connect-to-hosts/managing-files-with-sftp)（访问于 2026-10-29）；[更新日志](https://docs.termius.com/changelog)。

#### MobaXterm v26.6

- **位置**：SSH 登录后，左侧边栏自动弹出“SSH-browser”（走 SFTP 或 SCP）。
- **跟随**：会话设置里有“Follow SSH path (experimental)”，作用是自动把 SSH-browser 放到和终端相同的目录；官方注明老 Unix 系统不可用。
- **Git**：无。
- **交互**：双击远程文件用自己选的程序编辑，保存后自动传回服务器；支持拖放传输。
- 来源：[官方文档](https://mobaxterm.mobatek.net/documentation.html)；[下载页](https://mobaxterm.mobatek.net/download-home-edition.html)（当前版本 v26.6）。均访问于 2026-10-29。

#### FinalShell 4.6.3（官网标注更新日期 2025-05-21）

- 官方特性列表写的是“终端，sftp 同屏显示，同步切换目录”，另有“命令输入框快速显示并选择主机上的文件”和内置文本编辑器。官网没有写面板位置，也没写能否收起。
- 来源：[官网介绍页](https://www.hostbuf.com/t/988.html)（访问于 2026-10-29）。

#### WindTerm 2.6.0 起（最新 2.7.0，2025-03-11）

- **位置**：Filer（文件管理）窗格，带“Sync Terminal Folder”开关。
- **跟随**：终端 `cd` 后 Filer 自动跟上；也支持手动同步，或把 Filer 当前路径复制到终端（菜单“Copy Directory Path To Terminal”和“Copy Paths To Terminal”）。官方注明“不是所有本地或远程系统都支持”。2.7.0 修过一个 bug：没勾选同步开关，切换会话时仍会同步。
- 来源：[GitHub Releases](https://github.com/kingToolbox/WindTerm/releases)（2.6.0 和 2.7.0 的说明）。

#### Xshell 8 + Xftp

- **位置**：Xshell 自带 File Manager，从 View > Tunneling Pane 打开，列出远端**当前路径**下的文件，支持拖放上传和右键下载。Xftp 是另一个程序，点 Xshell 工具栏的“New File Transfer”，用同一个账号打开。
- **跟随**：Xshell 从终端标题读当前目录，需要配置 shell 的 PROMPT_COMMAND，把路径放在标题末尾。这套机制同时用于调用 Xftp、复制会话和 File Manager。没配置时 Xftp 打开家目录。
- 来源：
  - [Xshell 如何确定当前目录](https://netsarang.atlassian.net/wiki/spaces/ENSUP/pages/1076133906)（更新于 2024-04-15）
  - [Xshell 8 File Manager](https://netsarang.atlassian.net/wiki/spaces/EN8/pages/2237306932/File+Manager)（更新于 2025-11-17，通过 Confluence 接口读取）
  - [从 Xshell 打开 Xftp](https://netsarang.atlassian.net/wiki/spaces/ENSUP/pages/31523239)（更新于 2021-08-10）

#### VS Code（集成终端 + 资源管理器）

- **位置**：资源管理器在主侧栏，默认在左边，可以移到右边，⌘B 显示或隐藏，可以拖宽。
- **跟随**：资源管理器不跟随终端目录。终端默认在资源管理器打开的文件夹启动；在资源管理器右键选“Open in Integrated Terminal”可以在指定目录开终端。
- **Git**：资源管理器、编辑器标签和源代码管理视图都用 U/M/D 字母加颜色标出改动；行号旁有改动标记；diff 编辑器可以按行暂存或撤销。
- **交互**：
  - 把文件拖进终端，插入按当前 shell 规则转义的路径。
  - 终端里 ⌘/Ctrl 点击文件链接（支持 `文件:行:列`）打开编辑器。
  - 右键“Reveal in Finder”在访达中显示。
  - 把文件或文件夹拖进 Chat 作为上下文；在 Chat 输入 `#` 可以引用文件、文件夹、符号、终端输出、源代码管理改动。
- 来源：[终端基础](https://code.visualstudio.com/docs/terminal/basics)；[暂存与提交](https://code.visualstudio.com/docs/sourcecontrol/staging-commits)；[界面说明](https://code.visualstudio.com/docs/editing/getting-started/userinterface)；[Chat 上下文](https://code.visualstudio.com/docs/chat/copilot-chat-context)。均访问于 2026-10-29，页脚日期为 2026-10-28。

#### Zed

- **位置**：项目面板现在默认停靠在**右侧**，默认宽度 240，默认打开。“Agentic 布局”把 Agent 面板和 Threads 侧栏放左边，项目面板和 Git 面板放右边；也可以切回“Classic”布局。
- **跟随**：`auto_reveal_entries` 让面板跟随**当前编辑的文件**，不跟随终端目录。
- **Git**：默认按 Git 状态给文件名着色；可以另开 M/A/D/U/! 字母标记；诊断问题会标到上层文件夹。
- **diff**：Agent 面板显示改了哪些文件、多少行；Review Changes 打开多文件 diff，可以逐段或整体接受、拒绝；有 Restore Checkpoint 回到改动前。
- **交互**：Agent 输入框输入 @ 可以引用文件、目录、符号、历史对话、诊断、分支 diff 和网址；面板内可以拖拽移动文件，也能从访达拖入；有“在文件管理器中显示”。
- 来源：[项目面板](https://zed.dev/docs/project-panel)；[全部设置](https://zed.dev/docs/reference/all-settings)（`project_panel.dock` 默认 `"right"`）；[Agent 面板](https://zed.dev/docs/ai/agent-panel)；[并行 agent](https://zed.dev/docs/ai/parallel-agents)。均访问于 2026-10-29。

#### Claude Code 桌面版

- **位置**：**没有常驻文件树**。Code 标签页是可自由排布的窗格布局，包括对话、diff、浏览器、终端、文件编辑器、计划、任务等。拖动窗格标题可以换位置，拖动边缘调大小，⌘\ 关闭当前窗格；会话详情栏可以隐藏。
- **文件**：点击对话里的文件路径，在文件窗格打开；本地会话可以直接编辑，默认自动保存。
- **diff**：⌘⇧D 打开 diff 窗格，逐文件查看改动；在行号旁点 + 写评论，⌘↩ 一次全部提交给 Claude 修改。
- **终端**：终端窗格在会话的工作目录打开。
- **交互**：右键任意文件路径（对话、diff、文件窗格里都可以）有 Attach as context、Open in（VS Code、Cursor、Zed 等）、Show in Finder、复制绝对路径/相对路径/文件名。输入框用 @ 引用文件，或把文件拖进输入框。
- 来源：[桌面版文档](https://code.claude.com/docs/en/desktop)（访问于 2026-10-29；窗格布局要求桌面版 v1.2581.0 及以上）。

#### OpenAI Codex app

文档已迁到 learn.chatgpt.com，版本号按日期编排，例如 26.410 是 2026-04-10。

- **位置**：右侧是线程侧面板，里面有 Review（需要 Git 仓库）和工作区文件标签。View 菜单有“Toggle File Tree”。
- **目录**：
  - Review 里有改动文件树，26.318 起可以按百分比拖宽。
  - 26.409 起侧面板可以开工作区文件标签。
  - 26.410 起侧栏文件查看器能预览图片、PDF、Markdown；⌘P 搜索工作区文件。
- **diff**：Review 有五种范围：Unstaged（默认）、Staged、Commit、Branch、Last turn。暂存、取消暂存、撤销都可以按全部、单个文件或单个 hunk 操作。在行旁点 + 写行内评论，作为审阅意见交给 Codex。
- **交互**：
  - 点文件名用你选的编辑器打开；⌘ 点击某一行跳到编辑器里的那一行。
  - 线程菜单有 Open in Finder（26.313）；文件引用可以在文件管理器中显示（26.206）。
  - 可以用 @ 引用文件。
- **终端**：每个线程一个终端，Ctrl+` 打开，范围是该项目或工作树，Codex 能读取终端输出。
- **问题**：
  - 有用户报告 Toggle File Tree 点了没反应（[#20552](https://github.com/openai/codex/issues/20552)，2026-05-01，已关闭）。
  - 有用户报告 26.623 版整个文件树和 Review 入口都不见了（[#30484](https://github.com/openai/codex/issues/30484)，2026-06-28，仍未关闭）。
  - 26.224 版曾去掉 Review 里的文件夹层级，引起抱怨（[#12760](https://github.com/openai/codex/issues/12760)）。
- 来源：[Review 文档](https://learn.chatgpt.com/docs/code-review?surface=app)；[集成终端](https://learn.chatgpt.com/docs/integrated-terminal)；[更新日志](https://learn.chatgpt.com/docs/changelog)（26.206、26.313、26.318、26.409、26.410、26.608 各条）。均访问于 2026-10-29。

#### Cursor 3.x Agents Window（3.0 于 2026-04-02 发布）

- **位置**：左边是 agent 列表，右边面板有 Files、Changes、Terminal、Browser、PR 等标签。3.4（2026-05-13）起右面板可以全屏放大（⌘⇧M），对话收成浮动输入条。3.1（2026-04-13）起支持多个 agent 平铺并记住布局，同时修过文件树闪烁、不刷新的问题。
- **目录**：Files 标签是目录树加一个“轻量查看器”，只能打开工作区内的文件。2026-10-18 工作人员回复说 Files 里已经内置 Git blame 开关，并且可以开启语言服务。
- **diff**：点 Review 按钮进入 diff 标签，按文件暂存、取消暂存或撤销。逐处 Approve/Undo 只在 Editor 窗口有。从 diff 可以跳到文件对应行（3.1）。
- **交互**：⌘P 搜文件，⌘⇧F 全局搜索。“在 IDE 中打开”目前还是功能请求（2026-10-23）。
- 来源：[3.0](https://cursor.com/changelog/3-0)、[3.1](https://cursor.com/changelog/3-1)、[3.4](https://cursor.com/changelog/3-4) 更新日志；[Agents Window 文档](https://cursor.com/docs/agent/agents-window)；论坛帖[一](https://forum.cursor.com/t/when-using-agents-window-should-approve-undo-diffs-be-visible-and-actionable-in-the-files-tab-and-other-questions/160526)（2026-05-14 回复）和[二](https://forum.cursor.com/t/agents-window-files-ide-parity-for-extensions-and-open-files-outside-workspace/169863)（2026-10-18 工作人员回复）。

#### Conductor（最新 0.92.0，2026-10-23）

- **位置**：左边是工作区列表，0.17.4（2025-10-28）起显示 Git 状态；0.20.0（2025-11-07）起左侧栏可以收起。Git 面板有 Changes 标签。
- **目录**：0.16.0（2025-10-21）起能浏览工作区全部文件，0.20.0 起文件树会把只含一个子目录的目录链合并显示。每个任务一个工作树，根目录固定。
- **Git**：0.87.0（2026-09-18）起 Changes 可以按路径过滤；⌘⌥U 切换显示未提交改动。
- **diff**：0.16.0 起 diff 并入主对话面板。0.84.0（2026-09-02）起改动文件默认直接在 diff 里编辑，并可以加评论发给 agent。还支持统一 diff 视图和按提交过滤。
- **交互**：⌘P 打开文件（0.20.0），⌘⇧F 跨文件搜索（0.87.0）。有 Big Terminal Mode，把终端放到中间面板。
- **未说明**：文档没写面板默认宽度，也没写能否拖宽。
- 来源：[Diff viewer 文档](https://www.conductor.build/docs/reference/diff-viewer)；[更新日志](https://www.conductor.build/changelog)，含 0.16.0、0.17.4、0.20.0、0.84.0、0.87.0、0.92.0 各条。

### 1.2 共性

- **Shell 软件**：MobaXterm、FinalShell、WindTerm、Xshell、Tabby 的文件面板都是远程 SFTP 浏览器。“跟随终端目录”都要 shell 通过终端标题、OSC 7 或 OSC 1337 上报当前目录，厂商自己标注为实验性或“并非所有系统支持”。Tabby 和 Wave 只在打开面板的那一刻取一次目录。
- **Agent 工作台**：Codex app、Cursor、Conductor、Zed、Warp 都把“变更/审阅”放在第一位，文件树放第二位；根目录固定为项目或工作树；diff 支持行内评论回传给 agent；都有 ⌘P 文件搜索。
- **通用交互**：点击后预览或进编辑器；右键菜单有附加为上下文、复制路径、在访达中显示、用外部编辑器打开；拖进输入框或终端插入路径；用 @ 引用文件。
- **布局**：Zed 的 Agentic 布局和 Codex、Cursor 都把文件和变更放在右侧；各家都能收起，多数能拖宽，宽度会被记住。

### 1.3 推荐形态

终端标签页的布局建议如下：

```
┌ 终端 ─────────────────────────────────────────┬ 项目文件 ───────── [刷新][收起] ┐
│ [Codex ✕][Claude ✕][+ 新建 ▾]   在系统终端中打开 │ [变更 3] [全部文件]   [显示隐藏] │
│                                                │  M  src/main.go        +12 −3   │
│   结构化对话视图（现有）                          │  A  docs/plan.md       +40      │
│                                                │  ── 只读预览 / 只读 diff ──      │
│ ┌ 输入框：可把文件从右侧拖进来，插入路径 ──────┐ │                                 │
└────────────────────────────────────────────────┴─────────────────────────────────┘
```

- **位置和尺寸**：放在对话右侧；窗口够宽时默认展开，窄时默认收起；可以用按钮或快捷键收起；可以拖宽，建议 240–480 像素，并按沙箱记住。
- **两个标签**：
  - “变更”：有改动时默认选中，按 Git 状态列出 M/A/D/U/R 和增删行数。
  - “全部文件”：目录树按需逐层加载，目录排前；隐藏 `.git`，被 `.gitignore` 忽略的文件变灰；可以切换是否显示隐藏文件。
- **根目录**：固定为沙箱项目目录，宿主和容器里是同一个路径。不跟随 cwd，理由有两个：对话视图没有单一的 cwd，多个会话共用一个项目；各家“跟随目录”的实现都不稳定。以后可以给 Shell 会话加一个“定位到当前目录”按钮，用 OSC 7 实现。
- **交互**：
  - 单击文件打开只读预览；单击变更项打开只读 diff。
  - “插入路径”按钮，以及把文件拖进输入框，都只插入项目内相对路径的文本，建议用反引号包住。不要依赖 @ 展开，Codex exec 和 `claude --print` 不保证支持。
  - 右键菜单：复制相对路径、复制绝对路径、在访达中显示。

### 1.4 第一版最小范围

1. 终端标签页右侧加可收起、可拖宽的“项目文件”面板，记住每个沙箱的展开状态和宽度。
2. “全部文件”目录树：按需加载，显示隐藏文件开关，手动刷新，加文件变动监听并防抖。
3. “变更”列表：显示 Git 状态字母和增删行数，点开看只读 diff。不是 Git 仓库时隐藏这个标签。
4. 插入路径：按钮和拖拽两种方式，拖拽使用自定义数据类型（见 1.5 第 6 条）。另有复制路径和在访达中显示。
5. 只读文本预览：设大小上限，例如 512 KiB；能识别二进制文件；符号链接显示为链接，不预览目标内容。
6. **第一版不做**：编辑；新建、重命名、删除；上传下载；行内评论回传 agent；暂存或撤销；全局搜索；跟随 cwd。第二版建议优先加 ⌘P 文件搜索和“diff 评论发给 agent”。

验收建议：

- agent 在沙箱里新建或修改文件后，几秒内在两个标签里都能看到。
- 拖拽插入的路径，在三种 agent 里都能被正确读取。
- 在项目里放一个指向 `~/.ssh` 的符号链接、一个改写了 `core.fsmonitor` 的 `.git/config`、一个含 `<script>` 的 `.md` 或 `.svg`，宿主都不读取、不执行、不渲染。

### 1.5 安全约束（与第一版一起交付）

1. **宿主读文件限定在项目根目录内**：逐级用 Lstat 检查，不跟随符号链接，拒绝 `..`。现有图片附件“逐级打开路径时拒绝符号链接”（`docs/terminal-images.md`）就是这种做法。原因：agent 可以在项目里创建 `leak -> ~/.ssh/id_rsa` 这样的链接，宿主一旦跟随，就会把宿主文件读出来。
2. **只插入路径，不由宿主读出内容再塞进 agent 输入**：让 agent 在沙箱里自己读。这样即使项目里有指向宿主的链接，沙箱里也解析不到宿主文件。
3. **预览一律按纯文本渲染**：如果要渲染 Markdown，必须禁止原始 HTML；不渲染 HTML 和 SVG，不用 iframe。原因：前端通过 `window.go.service.App` 调用全部后端方法（`frontend/src/js/core.js:16`），预览里一旦注入脚本，就等于拿到整个应用的接口。
4. **不在宿主对项目目录运行 git**：
   - `.git/config` 对 agent 可写。`core.fsmonitor` 可以是一条命令，`git status` 会去执行它（[git-config 文档](https://git-scm.com/docs/git-config)）。
   - 已有同类漏洞：[CVE-2026-67623 Mistral Vibe](https://www.vulncheck.com/advisories/mistral-vibe-arbitrary-command-execution-via-git-fsmonitor-hook)（2026-08-05，触发点就是 `git status --porcelain`）、[TALOS-2025-2243](https://talosintelligence.com/vulnerability_reports/TALOS-2025-2243)、[CVE-2026-45033 Copilot CLI](https://advisories.gitlab.com/npm/@github/copilot/CVE-2026-45033/)。
   - git 自带的 `safe.directory` 只拦截“属主不同”的仓库，这里属主相同，拦不住。
   - 建议做法：在该沙箱容器里用 `docker exec` 运行 `git -c core.fsmonitor=false status --porcelain=v2 -z`，diff 加 `--no-ext-diff --no-textconv`；或者在宿主用纯 Go 读取 index，不执行任何配置里的命令。用容器方案前，要确认镜像里装了 git。
5. **“打开”只提供两种**：在访达中显示（`open -R`），以及用明确指定的编辑器打开。不要用系统默认程序打开 agent 写出的文件，`.app`、`.command`、`.terminal` 这类文件会被直接执行。
6. **拖拽使用自定义数据类型**，例如 `application/x-aiofcage-path`。现有终端面板的拖放会检查 `Files` 类型并当图片附件处理（`sandbox-detail.js:156-158`），两者要分开。
7. **刷新机制**：宿主用 FSEvents 或 fsnotify 监听并防抖，忽略 `.git` 和 `node_modules`。virtiofs 下容器内的写入能否触发宿主事件需要实测，不能触发就退回定时轮询。

### 1.6 待验证

- Codex app 的 Toggle File Tree 和侧面板布局没有官方文档，只能从更新日志和 issue 拼出来；Cursor 的 Files 标签同理，主要来自论坛工作人员回复。
- Warp 文件树是否跟随 `cd`、默认是否可见，文档都没写。
- Conductor 面板能否拖宽，文档没写。

---

## 二、CC Switch 的厂商预设与模型列表（及 Cherry Studio、LobeHub、Chatbox）

### 2.1 版本

- 读取的是 [farion1231/cc-switch](https://github.com/farion1231/cc-switch) main 分支（提交 `ddcb111`，2026-10-29）；最新发版 v4.0.21（2026-10-26）。
- README 写“90+ 供应商预设”、支持 12 个工具。
- DeepSeek Harness 集成在 2026-10-27 合入 main（[#8795](https://github.com/farion1231/cc-switch/pull/8795)），还没发版。

### 2.2 预设列表

下面按界面上的分组整理，分组规则见 `presetGroups.ts`。数量是统计 `src/config/*ProviderPresets.ts` 得出的。

**Claude Code，共 136 个**

- 登录类（4）：Claude Official、GitHub Copilot、Codex（ChatGPT 账号反代）、xAI (Grok)
- 厂商（37）：Kimi、Kimi Global、Kimi For Coding（含 Global）、火山 Agent Plan、火山 Coding Plan、BytePlus、Volcengine Doubao、DeepSeek、Tencent Token Plan（6 个版本）、Zhipu GLM（含 en）、Baidu Qianfan Coding Plan、Baidu Qianfan Token Plan、千问AI平台（含 Coding Plan、Token Plan）、QwenCloud（含 For Coding、Token Plan）、StepFun（含 en）、StreamLake（含 en）、Longcat、MiniMax（含 en）、BaiLing、Meta Model API、Xiaomi MiMo（含 Token Plan）、Mistral AI（隐藏）
- 第三方和聚合（83）：PackyCode、APINebula、AICodeMirror、PatewayAI、hao.ai、FennoAI、RunAPI、OpenModel、GrooRoute Pro、DIT.ai、BuyToken、V-API、TokenFactory、FluxA Token Plan、AIGoCode、**Qiniu（七牛云）**、AICoding、SubRouter、Shengsuanyun、88API、APIKEY.FUN、9527CODE、ClaudeAPI、Code0、TeamoRouter、PPIO、ClaudeCN、SiliconFlow（含 en）、A6API、Compshare（含 Coding Plan）、CCSub、SSSAiCode、SoleAPI、Micu、RightCode、Cubence、CrazyRouter、DMXAPI、SudoCode（.chat 和 .us）、XycAi、Modelflare、AllTokens（含 SI）、Tu-zi、Amux、AtlasCloud、Soshow、Gemini Native、OpenCode Go、OpenCode Zen、ModelScope、AiHubMix、CherryIN、RelaxyCode、E-FlowCode、OpenRouter、TheRouter、Baseten、Novita AI、DeepInfra、Vercel AI Gateway、Nebius Token Factory、ClinePass、Groq、Hugging Face、Nvidia、PIPELLM、JieKou AI、AICodeWith、Command Code、模力方舟、SoruxGPT（含 Coding Plan）、IKunCode、RunToken、BotCF、YYLX、TokenFlux、NekoCode、AIComing
- 云厂商（8）：Ollama Cloud、SambaNova、DigitalOcean、Microsoft Foundry、Fireworks AI、Cloudflare AI Gateway、AWS Bedrock（AKSK 和 API Key 两种）
- 本地（4）：Ollama、LM Studio、llama.cpp、vLLM

**Codex，共 136 个**

- 登录类（3）：OpenAI Official、GitHub Copilot、xAI (Grok) OAuth
- 厂商（41）：比 Claude 那份多了 Baidu Qianfan、Tencent Hunyuan、StepFun API（含 en）、Astron Coding Plan，少了千问AI平台 Coding Plan；隐藏的 Mistral AI 同样在列
- 第三方和聚合（83）：比 Claude 那份多了 Azure OpenAI、xAI (Grok)，少了 ClaudeAPI、Gemini Native，其余相同
- 云厂商（5）：Ollama Cloud、SambaNova、DigitalOcean、Fireworks AI、Cloudflare AI Gateway；没有 AWS Bedrock 和 Microsoft Foundry
- 本地（4）：Ollama、LM Studio、llama.cpp、vLLM

**DeepSeek Harness**：`dshProviderPresets.ts` 由 Pi 预设派生，只保留 openai-completions、openai-responses、anthropic-messages 三种协议，约 114 个。

### 2.3 选中预设后自动填什么，用户填什么

**Claude Code 预设**（`claudeProviderPresets.ts`、`ClaudeFormFields.tsx`）会自动带入：

- `env.ANTHROPIC_BASE_URL`。
- 认证变量：默认 `ANTHROPIC_AUTH_TOKEN`，少数预设用 `ANTHROPIC_API_KEY`。界面说明写道：经本地路由转发时，AUTH_TOKEN 以 Bearer 头发出，API_KEY 以 x-api-key 头发出。
- 部分预设带 `ANTHROPIC_MODEL` 和 Haiku、Sonnet、Opus 三档默认模型。
- 上游格式 `apiFormat`：`anthropic` 是直连；`openai_chat`、`openai_responses`、`gemini_native` 需要开启本地路由做格式转换。
- `endpointCandidates`：多个请求地址候选，用于“请求地址管理”的测速和自动选择。
- `modelsUrl`：覆盖模型列表地址。
- `websiteUrl` 和 `apiKeyUrl`：表单上的“获取 API Key”链接。
- 云厂商预设另有 `templateValues`，需要用户填，例如 AWS Region 和 AK/SK、Azure 资源名、Cloudflare 的 Account ID 和 Gateway ID。

**Codex 预设**（`codexProviderPresets.ts`）会写两个文件：

- `auth.json`：`OPENAI_API_KEY` 留空，由用户填。
- `config.toml`：由 `generateThirdPartyConfig()` 生成，内容是 `model_provider = "custom"`、`model`、`model_reasoning_effort = "high"`、`disable_response_storage = true`，以及 `[model_providers.custom]` 段的 `name`、`base_url`、`wire_api = "responses"`、`requires_openai_auth = true`。
- 只支持 Chat Completions 的厂商标为 `apiFormat: "openai_chat"`，要经本地路由转换；部分预设还带模型目录 `modelCatalog`。

**用户只需填 API Key**（云厂商另填模板变量）；模型可以改，也可以不改。

**添加流程分两步**（`ProviderPresetSelector.tsx`、`presetStep.tsx`）：

- 第一步选预设：顶部常驻搜索框，可以按中文名、域名或别名搜；左侧分组；两列列表；“自定义配置”固定在第一行；同一家的不同套餐或地区合并成一行。
- 第二步填表：表单顶部有一条“预设条”，点“更换”回到第一步。切换版本时，如果用户已经手动改过表单，会先确认。

### 2.4 七牛预设

CC Switch 里有“Qiniu”预设，中文显示名是“七牛云”，分类为 aggregator，界面上归入“第三方”分组，并标为合作伙伴。“获取 API Key”链接是 `https://s.qiniu.com/nMvAvy`。推广文案原文：“七牛云是多协议 AI 中转平台，一个 Key 即可调用原生 Claude、GPT、Gemini 模型，兼容 Anthropic、OpenAI、Gemini 多种接口。”

| 工具 | 地址 | 协议和认证 | 预置模型 | 地址候选 |
| --- | --- | --- | --- | --- |
| Claude Code | `https://api.qnaigc.com` | Anthropic；`ANTHROPIC_AUTH_TOKEN` | 无，使用 Claude 默认模型 ID | `api.qnaigc.com`、`api.modelink.ai` |
| Codex | `https://api.qnaigc.com/bypass/openai/v1` | `wire_api = "responses"`，`requires_openai_auth = true` | `gpt-6-astra` | 另有 `https://api.modelink.ai/bypass/openai/v1` |
| DeepSeek Harness（由 Pi 预设派生） | `https://api.qnaigc.com/v1` | `openai-completions` | `gpt-6-astra`、`moonshotai/kimi-k3`、`z-ai/glm-5.3`、`z-ai/glm-5.3-flash` | — |
| 其他工具 | Gemini CLI 用 `/bypass/vertex`；Grok Build 用 `/bypass/openai/v1`；OpenCode、OpenClaw、Hermes 用 `/v1`；Claude Desktop 用根地址 | — | — | — |

**没有** `llmapi.qiniu.io` 或 `xapi.qiniu.io` 的预设。

注意模型名前缀不一致：CC Switch 的 Codex 预设写 `gpt-6-astra`，七牛官方工具 coding-helper 写 `openai/gpt-6-astra`，见 3.4。需要用真实 key 确认哪种能用。

### 2.5 模型列表怎么拉、失败怎么办

依据：`src-tauri/src/services/model_fetch.rs`、`src/lib/api/model-fetch.ts`、`ModelInputWithFetch.tsx`、`FetchedModelPicker.tsx`、`src/i18n/locales/zh.json`。

- **前置检查**：没填地址或 key 时直接提示，例如“请先填写 API 端点和 API Key”，不发请求。
- **请求方式**：由 Rust 后端发 GET，超时 15 秒。候选地址按顺序尝试，最多 4 个，只有遇到 404 或 405 才换下一个：
  1. 预设里的 `modelsUrl`，有就只用它；
  2. 地址加 `/v1/models`；如果地址已以 `/vN` 结尾，改为加 `/models`；
  3. 版本号不是 v1 时，再试一次 `/v1/models`；
  4. 去掉 `/anthropic`、`/api/anthropic`、`/apps/anthropic` 这类兼容子路径后重试；千问的 `/apps/anthropic` 还会试 `/compatible-mode/v1/models`。
- **认证头**：anthropic-messages 协议用 `x-api-key`，其余用 `Bearer`；可以带自定义请求头和 UA。
- **解析**：兼容三种格式：OpenAI 的 `data[].id/owned_by`、Anthropic 的 `data[].id`、智谱的 `models[].slug`；结果排序并去重。错误响应正文截断到 512 字符，并抹掉 key。
- **错误提示**：

| 情况 | 提示文案 |
| --- | --- |
| 401 或 403 | API Key 无效或无权限 |
| 所有候选都 404/405 | 未找到可用的模型列表端点，请检查 Base URL 或确认供应商是否开放该接口 |
| 超时 | 请求超时，请检查网络连接 |
| 解析失败 | 该供应商不支持获取模型列表 |
| 返回空列表 | 未找到可用模型 |
| 其他错误 | 获取模型列表失败 |
| 成功 | 获取到 N 个模型 |

- **失败后的回退**：模型字段始终是可以手输的输入框，拉到列表后只是旁边多一个可搜索的下拉。Codex 表单用选择弹窗：勾选即加入、取消勾选即移除，点“应用”才生效；有“重新获取”、搜索、全选、全不选、“只看已选”；配置里有但供应商没返回的模型单独列在最前面（“不在供应商列表中”）；用 models.dev 的数据补上下文窗口、推理能力、图片能力。
- **七牛实测**：Claude 预设会请求 `https://api.qnaigc.com/v1/models`，Codex 预设会请求 `.../bypass/openai/v1/models`。两个地址不带凭据都返回 200，见 3.6。

### 2.6 Cherry Studio、LobeHub、Chatbox 的模型列表

**Cherry Studio v2.1.8（2026-10-26）**

- 源码 [CherryHQ/cherry-studio](https://github.com/CherryHQ/cherry-studio)：`ProviderSettings/ModelList/*`、`src/main/ai/provider/listModels.ts`、`zh-cn.json`。
- 点“同步模型”打开抽屉，标题显示“{服务商} 模型 + 数量”。可以搜索模型，整组或全部添加、移除。供应商不再返回的模型标“失效”，可以一键“清理失效模型”。
- 拉取失败时，标题栏显示一个红点和重试按钮，悬停可以看到失败原因，提示“拉取模型失败”。
- 新的引导流程：填 key →“选择模型”→ 没有返回模型时显示“服务商未返回任何可用模型”，并提供“手动添加模型”入口 →“验证模型可用性”→ 启用服务商。发起模型检测前会先提示：会发送真实请求，可能产生费用。
- 拉到的列表会补上自带模型库里的能力、上下文长度和价格。
- 内置七牛服务商：`baseUrl` 和 Anthropic 地址都是 `https://api.qnaigc.com`。

**LobeHub v2.2.21（2026-10-29）**

- 源码 [lobehub/lobehub](https://github.com/lobehub/lobehub) canary 分支：`features/Settings/provider/features/ModelList/*`、`locales/zh-CN/modelProvider.json`。
- 模型列表区有“获取模型列表”按钮，获取中显示“正在获取模型列表…”，失败时提示“获取模型列表失败：{原因}”；显示“上次更新时间”；可以“清除获取的模型”；重复的模型会被移除并提示。
- 支持搜索，分“已启用”和“未启用”两区，有全部启用、全部禁用和多种排序。
- 可以“创建自定义 AI 模型”。没有模型时显示空状态：“请创建自定义模型或拉取模型后开始使用吧”。另有“连通性检查”。
- 七牛服务商的默认地址还是旧域名 `https://openai.qiniu.com/v1`。拉取时会把七牛返回的 `context_length` 和 `max_tokens` 映射为上下文窗口和最大输出。

**Chatbox v1.23.5（2026-09-24）**

- 源码 [chatboxai/chatbox](https://github.com/chatboxai/chatbox)：`routes/settings/provider/$providerId.tsx`、`model-setting-utils/base-config.ts`。
- 模型区有三个按钮：“新建”（手动添加）、“重置”（恢复默认列表）、“获取”（拉取）。拉取后弹出模型窗口，可以搜索，逐个添加或移除。
- 列表合并四个来源：程序内置、后端配置、服务商接口、models.dev。服务商接口返回空时，回退到 models.dev 的精选列表。
- 接口出错会被吞掉；合并后仍为空才提示“获取模型失败”。
- 另有“检查”功能，可以选测试模型；“API 主机”下方显示最终请求地址的预览。

### 2.7 对 AIofCage 的建议

- **流程**：沿用 CC Switch 的两步式：先选厂商（按 agent 过滤、可搜索、按厂商分组），再填写。厂商预设负责带入地址、协议、认证头样式、默认模型和“获取 key”链接；用户只填 key，key 存进钥匙串。
- **七牛云预设**：每个 agent 一条，地址不同（Codex 用 bypass 路径），另加海外 `api.modelink.ai` 作为地址候选。比赛用的 xapi 接口单独保留一条，见第三节。
- **拉取模型列表**：
  - 由宿主 Go 后端直连发请求，key 不进入前端网页；超时 15 秒左右。
  - 错误分类和提示参考 2.5 的表；模型框始终允许手填；记录“上次获取时间”。
  - 不要跟随跨域重定向。现有 HTTP 客户端没有设置 `CheckRedirect`，见 todo 清单第四节。
- **七牛专用的增强**：
  - 用 `GET /v1/market/models` 的 `support_api_protocols` 字段按协议过滤模型：Claude Code 只显示含 anthropic 的模型，DeepSeek Harness 显示含 openai 的模型。
  - 用 `GET /v3/stat/usage` 做“测试连接”，不消耗 token。
  - 这两个接口都是七牛私有接口，见 3.6。

---

## 三、七牛 AI 大模型推理接口

### 3.1 接入地址和各域名的关系

**公开文档列出的接入点**

| 区域 | Base URL | 说明 |
| --- | --- | --- |
| 国内 | `https://api.qnaigc.com` | 面向中国大陆用户 |
| 海外 | `https://api.modelink.ai` | 接口和模型与国内完全一致，只是网络路由不同 |

来源：[服务接入点](https://apidocs.qnaigc.com/8937183m0.md)（apidocs.qnaigc.com，访问于 2026-10-29，页面没有日期）。developer.qiniu.com 各页也只写 `https://api.qnaigc.com/v1`：[实时推理 API 接入说明](https://developer.qiniu.com/aitokenapi/13379/real-time-ai-interface-api)（2026-09-17）、[获取 API Key](https://developer.qiniu.com/aitokenapi/12884/how-to-get-api-key)（2026-04-29）。

**`openai.qiniu.com`**：旧域名。七牛官方工具 coding-helper 把旧线路 id `qiniu`（对应 openai.qiniu.com）映射到 api.qnaigc.com（`src/lib/endpoints.ts`）；LobeHub 仍在用这个旧地址。

**`llmapi.qiniu.io`**：在 developer.qiniu.com、apidocs.qnaigc.com 和 [qiniu.com/llms.txt](https://www.qiniu.com/llms.txt) 里都**找不到**。目前掌握的情况：

- 证书是七牛的 `*.qiniu.io`（TrustAsia 签发），前面是 AWS Global Accelerator。
- 不带 key 访问时返回 LiteLLM 风格的报错（“No api key passed in”）。
- llmapi 公开的 `/openapi.json` 显示它是“XHub API 1.95.32”，有 533 个路由，包括 `/v1/chat/completions`、`/v1/responses`、`/v1/responses/compact`、`/v1/messages`、`/v1/messages/count_tokens`、`/v1/models`。
- 同目录 `5-qiniu-api-probe.md` 记录：用比赛 key 访问返回 `token_not_found_in_db`，说明需要另配 key。
- 社区代码里有人在用：coding-helper 维护者的个人脚本 [setup-fenno-codex.sh](https://github.com/krisxia0506/script-hub/blob/main/public/scripts/setup-fenno-codex.sh)（2026-09-10）写的是 `base_url = "https://llmapi.qiniu.io/v1"` 加 `wire_api = "responses"`，模型名不带 `openai/` 前缀（如 gpt-5.6-sol、grok-4.6）；另一个项目 [voxcode](https://github.com/liyan-04/voxcode)（2026-10-25）用 `gpt-6.1-sol` 请求 `https://llmapi.qiniu.io/v1/responses`。
- 本次 06:2x 再访问时连接超时，可用性不稳定。

**`xapi.qiniu.io`**：同样不在公开文档里。证书同样是 `*.qiniu.io`；首页标题是“XHub 控制台”；响应头带 `x-litellm-version: xhub-dev`，并把请求转给内部 `gateway:4000`。同目录 `5-qiniu-api-probe.md` 记录：用比赛 key 拉 `/v1/models` 返回 5 个模型：gpt-6-astra、gpt-6.1-sol、gpt-6-sol、gpt-5.6-terra、gpt-5.6-sol。`gpt-5.5` 已经不在列表里。

**结论**：公开产品是 api.qnaigc.com 和 api.modelink.ai；llmapi 和 xapi 是七牛自己运营的 XHub 网关，看起来用于比赛或内部，和公开接口的 key、模型名都不通用。

### 3.2 各接口是否支持

| 接口 | 官方文档 | 无凭据探测（2026-10-29） |
| --- | --- | --- |
| `POST /v1/chat/completions` | 支持，是核心接口，覆盖几乎全部模型 | 401 |
| `POST /v1/responses` | **未列出**。FAQ（2026-10-23）只写兼容 OpenAI `/v1/chat/completions` 和 Anthropic `/v1/messages` | **404**（not found or method not allowed） |
| `POST /bypass/openai/v1/responses` | 原厂直通，Responses 原生协议，只支持 OpenAI 官方 GPT 系列，模型名形如 `openai/gpt-5.5` | 401；海外节点同样 401 |
| `POST /bypass/openai/v1/responses/compact` | 未列出 | **404** |
| `POST /v1/messages` | Anthropic 兼容接口。OpenAPI 示例用 `X-Api-Key` 头，示例模型名为 `claude-4.5-sonnet` | 401，报错原文为“Provide Authorization: Bearer <key> or x-api-key header”，两种头都接受 |
| `POST /v1/messages/count_tokens` | 未列出 | **404** |
| `POST /bypass/anthropic/v1/messages` | 原厂直通，支持 Extended Thinking、Prompt Caching、联网搜索；模型为 `claude-opus-4-6`、`claude-sonnet-4-6`、`claude-haiku-4-5-20251001` | 401 |
| `GET /v1/models` | OpenAI 兼容格式；FAQ 说只有这个接口用 GET | **200，不需要 key** |

来源：[服务接入点](https://apidocs.qnaigc.com/8937183m0.md)；[Anthropic 协议](https://apidocs.qnaigc.com/413432574e0.md)；[bypass Anthropic](https://apidocs.qnaigc.com/432088078e0.md)；[bypass Responses](https://apidocs.qnaigc.com/432091837e0.md)；[调用 FAQ](https://developer.qiniu.com/aitokenapi/13462/aitoken-use-faq)（2026-10-23）。

### 3.3 Claude Code 怎么配

- `ANTHROPIC_BASE_URL=https://api.qnaigc.com`，**不带 `/v1`**；key 放在 `ANTHROPIC_AUTH_TOKEN`，或者 `ANTHROPIC_API_KEY`。来源：[获取 API Key](https://developer.qiniu.com/aitokenapi/12884/how-to-get-api-key)（2026-04-29）原文：“ANTHROPIC_API_KEY 或 ANTHROPIC_AUTH_TOKEN(对应ANTHROPIC_BASE_URL=https://api.qnaigc.com)”。
- **七牛官方配置工具** [qiniu/coding-helper](https://github.com/qiniu/coding-helper)（v0.4.0 发布于 2026-09-11，main 分支 2026-10-29 仍有提交，文件 `src/lib/tools/claude-code-tool.ts`）在 `~/.claude/settings.json` 的 `env` 里写入：
  - `ANTHROPIC_BASE_URL=https://api.qnaigc.com`（海外线路是 `https://api.modelink.ai`）
  - `ANTHROPIC_AUTH_TOKEN=<key>`
  - `API_TIMEOUT_MS=3000000`（50 分钟）
  - `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`
  - `CLAUDE_CODE_ATTRIBUTION_HEADER=0`（注释说明：提高网关的提示缓存命中率）
  - 可选：`ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL` 和 `CLAUDE_CODE_SUBAGENT_MODEL`
  - 另外在 `~/.claude.json` 写 `hasCompletedOnboarding: true`，跳过首次引导。
  - 界面提示原文：“七牛 API 支持 Claude 默认模型 ID（Claude 系列），自定义模型配置是可选的”。
- 七牛开发者中心原本有专门的 Claude Code 页面（文章 13416，页面元数据写更新于 2026-09-17），**今天访问返回 404**。

### 3.4 Codex 怎么配

coding-helper 的 `src/lib/tools/codex-tool.ts` 写入：

- `~/.codex/config.toml`：
  - 顶层：`model = "openai/gpt-6-astra"`、`model_provider = "qnaigc"`、`model_catalog_json = "~/.codex/model-catalogs/qnaigc.json"`
  - `[model_providers.qnaigc]`：`name = "Qiniu"`、`base_url = "https://api.qnaigc.com/bypass/openai/v1"`、`requires_openai_auth = true`、`wire_api = "responses"`
  - 另有 `[profiles.qn-gpt]` 段
- `~/.codex/auth.json`：`{"auth_mode": "apikey", "OPENAI_API_KEY": "<key>"}`
- 模型目录文件 `qnaigc.json`：列出 openai/gpt-5.x 到 gpt-6-astra 共 18 个模型的上下文长度和推理档位。

`model_catalog_json` 是 Codex 官方配置项，说明是“启动时加载的 JSON 模型目录”（[配置参考](https://learn.chatgpt.com/docs/config-file/config-reference.md)，访问于 2026-10-29）。CLAUDE.md 记录过“Codex 不识别所选模型”的问题，可以试这个办法解决。

CC Switch 的七牛 Codex 预设地址相同，但模型名写 `gpt-6-astra`，不带前缀，与官方工具不一致，需要实测确认。

### 3.5 DeepSeek Harness 怎么配

七牛官方页面写的是：在设置 → 模型 → 自定义设置里，API 地址填 `https://api.qnaigc.com/v1`，模型 ID 去控制台模型广场查。来源：[DeepSeek Harness 配置接入 AI](https://developer.qiniu.com/aitokenapi/13550/deepseek-harness-configuration-access-ai)（2026-08-24）。

### 3.6 `/v1/models` 的返回格式，以及七牛私有接口

- **OpenAPI 定义**：`{"object":"list","data":[{"id","object","created","owned_by"}]}`，四个字段都必填。来源：[模型列表](https://apidocs.qnaigc.com/397274067e0.md)。
- **实际返回**（2026-10-29，不带 key）：每个模型多了 `context_length` 和 `max_tokens` 两个字段，共 78 个模型，`owned_by` 都是 `"system"`。**列表里没有 openai、claude、gemini 系列**，带 key 后列表是否不同还不知道。`/bypass/openai/v1/models` 和海外节点的 `/v1/models` 不带 key 也返回 200。
- **`GET /v1/market/models`**：不在 apidocs 目录里，coding-helper 在用。不带 key 也返回 200，格式是 `{"status":true,"data":[...]}`，共 104 条，每条有 name、description、features（如“工具调用”“深度思考”）、`model_constraints`、`architecture`、`support_api_protocols`、价格规则等。`support_api_protocols` 的分布：60 个模型是 `["openai","anthropic"]`，18 个只有 `["openai"]`，3 个是 `["fal_ai"]`，23 个为空。
- **用量接口 `GET /v3/stat/usage`**：支持 API Key 鉴权，返回当前 key 的用量；限速为同一 IP 每秒 5 次。来源：[用量查询](https://developer.qiniu.com/aitokenapi/13382/new-usage-api)（2026-07-29）。coding-helper 用 `/v2/stat/usage` 校验 key，不消耗 token。
- **key 的模型范围**：官方 FAQ 写明 API Key 可以设置“模型范围”，越界调用会报 `not available in your assigned model groups`；FAQ 还说七牛返回结构“不能保证与 OpenAI 和 Anthropic 官网逐字段完全一致”。所以列表里有的模型，不代表当前 key 能调用。

### 3.7 对 AIofCage 的影响

- **门卫放行路径要按预设逐条写**：
  - Codex 走七牛公开接口：`POST /bypass/openai/v1/responses`、`GET /bypass/openai/v1/models`。这条线路**没有** `/responses/compact`，需要确认 Codex 遇到 404 时能否退回本地压缩。
  - Claude Code：`POST /v1/messages`。`count_tokens` 返回 404，要确认不影响使用。
  - DeepSeek Harness：`POST /v1/chat/completions`。
  - 如果改用 llmapi：OpenAPI 里 Responses、compact、`count_tokens` 都有，但需要另配 key。
- **认证注入**：`/v1/messages` 同时接受 Bearer 和 x-api-key 两种头；门卫只需按预设统一注入一种，再拒绝另一种外来的值。
- **沙箱内 Claude Code 的设置**：建议参考 coding-helper 设置 `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`，减少被门卫拒绝的遥测请求。
- **待用真实 key 复核**：
  1. 带 key 时 `/v1/models` 是否包含 openai 和 claude 模型；
  2. Codex 的模型名是 `openai/gpt-6-astra` 还是 `gpt-6-astra`；
  3. Claude Code 用默认模型 ID（例如 claude-sonnet-5-5）请求 `/v1/messages` 能否成功，还是必须走 `/bypass/anthropic`；
  4. 流式输出、工具调用、图片输入在三种 agent 里是否都正常。

---

## 附：仍不确定的地方

1. 七牛公开文档没有提到 llmapi 和 xapi。“用 llmapi 作为第二轮默认地址”只能以比赛方或用户的说明为准，公开资料无法证实。
2. 公开 `/v1/models` 不含 GPT 和 Claude 系列。带 key 后的真实列表、模型名前缀、key 的模型范围，都要用新 key 复测。
3. Codex app、Cursor Agents Window 文件面板的细节主要来自更新日志、issue 和论坛，不是正式文档。
4. virtiofs 下宿主能否收到容器内写入触发的文件事件，需要在 AIofCage 专用实例上实测。
