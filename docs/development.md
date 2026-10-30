# 开发指南

[文档目录](README.md) · [从源码安装](../README.md#从源码安装)

## 构建与运行

环境要求见源码安装说明，以下命令在仓库根目录执行。Wails 固定为 v2.17.0，前端使用随包固定版本的 Preact、htm 和 xterm.js，没有 npm 构建步骤。

```bash
make app                 # 生成 bin/Ai of Cage.app
open "bin/Ai of Cage.app"
make cagectl             # 共用相同运行时的命令行工具
make dev                 # Wails 热开发
```

`make app` 和 `make cagectl` 会先生成并嵌入 Linux/arm64 门卫与连接器。首次运行 `make dev` 或单独执行检查前，先生成这些资源：

```bash
make gatekeeper-linux connector-linux
```

## 打包安装包

```bash
make dmg                 # 生成 bin/AiofCage-<版本>-arm64.dmg 和同名 .sha256
```

- 版本号取自 `packaging/macos/Info.plist` 的 `CFBundleShortVersionString`，同时写进应用里显示的版本；发新版本前先改它。
- 脚本 `scripts/package-dmg.sh` 会校验应用签名，打包后再挂载检查一遍：镜像完整，里面的应用签名仍然有效。
- 安装包只有本机临时签名（ad-hoc），没有 Developer ID 证书和公证。用户第一次打开要在「隐私与安全性」里放行，步骤见 [README](../README.md#下载安装)。
- 发布到 GitHub Releases 前需团队确认，上传 `.dmg` 和 `.sha256` 两个文件。

## 检查与测试

```bash
make check               # Go 静态检查与生产构建
make test                # 后端测试与竞态检查
```

前端回归测试额外需要 Node.js；使用支持 ES 模块自动识别的版本（既有验收使用 Node 26）：

```bash
node --test frontend/tests/*.test.js frontend/tests/*.test.mjs
```

PostgreSQL 连接器策略测试首次运行前，在 `internal/runtime/assets/connector/postgres` 执行 `npm ci --ignore-scripts`，然后回到仓库根目录执行：

```bash
node --test internal/runtime/assets/connector/postgres/policy.test.mjs
```

真实环境测试默认跳过。启用方式、测试供应商和清理范围见[验收记录索引](validation/README.md)中的整体验收报告；会调用已配置供应商的测试产生实际用量。历史命令中的测试沙箱 ID 需替换为自己的测试资源，跳过项不能算作通过。

## 源码入口

| 路径 | 用途 |
| --- | --- |
| `main.go`、`frontend/src/` | Wails 桌面入口与界面 |
| `internal/service/` | 前端调用的产品接口、会话、用量与审计采集 |
| `internal/runtime/` | 虚拟机、镜像、容器与沙箱生命周期 |
| `internal/gatekeeper/`、`internal/connector/` | 网络策略、凭据注入与可信 MCP 服务 |
| `cmd/` | cagectl、门卫及连接器入口 |

模块职责和配置、权限、日志之间的关系以[当前架构](architecture-current.md)为准，旧版接口过程保留在[早期模块记录](modules.md)。所有 Docker 操作显式指定 `colima-aiofcage` context，使用专用 `aiofcage` Colima profile。

## 图标

矢量源文件为 `frontend/src/assets/logo.svg`。修改后运行 `make icons`，通过系统 WebKit 和 `iconutil` 生成 `packaging/macos/AppIcon.png` 与 `AppIcon.icns`，再运行 `make app` 打包。日常构建使用仓库中已有的图标。

## 提交约定

- 一个任务一个分支，完成相关检查后提交 commit，通过 PR 合入；不直接推送 `main`。
- 按仓库约定使用 merge commit 合并，保留开发过程；依赖尚未合并的分支时，在 PR 中写明依赖。
- 文档与界面文案使用中文。功能范围更新到[功能清单](features.md)，使用变化更新到[使用指南](user-guide.md)，验证证据放入[验收目录](validation/README.md)。
- 不提交密钥、用户数据或原始敏感日志；提交前检查凭据格式和测试产物，验收报告只保留脱敏证据。

完整团队约定见 [CLAUDE.md](../CLAUDE.md)。
