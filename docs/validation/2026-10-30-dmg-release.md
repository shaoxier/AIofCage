# DMG 安装包验证

[验收记录索引](README.md)

## 改动

对应首轮评审的第二条保留意见“只能源码安装，没有 Release”，见[第二轮 todo 清单](../round2/todo.md)第八节。

- **`make dmg`**：用 `Info.plist` 里的版本号重新构建应用，并把版本号写进应用里显示的版本；然后调用 `scripts/package-dmg.sh` 打成拖拽安装的 DMG（应用加“应用程序”文件夹的快捷方式），并写出同名的 `.sha256` 校验文件。
- **打包脚本自带检查**：打包前校验应用签名；打包后校验镜像，挂载后再校验一次里面的应用签名。签名损坏的应用在别的 Mac 上会被提示“已损坏”，而不是“无法验证开发者”。
- **README 新增“下载安装”一节**：安装 Colima 和 Docker、下载并核对 SHA256、拖入“应用程序”，以及第一次打开时在“隐私与安全性”里放行的步骤。开发指南增加打包和发布说明。

## 自测（2026-10-30，本机 macOS 26.6.1，Apple Silicon）

在单独的工作目录里从本分支执行 `make dmg`，用时 15 秒（Go 编译有缓存）。生成的 `AiofCage-0.1.0-arm64.dmg` 约 17 MB（17,155,019 字节；镜像里带时间戳，每次打包的大小和校验值都略有不同）。

另外检查了脚本的出错路径：应用不存在时给出中文提示并以 1 退出；打包结束后临时目录都已清理。

| 检查 | 结果 |
| --- | --- |
| `shasum -a 256 -c AiofCage-0.1.0-arm64.dmg.sha256` | OK |
| `hdiutil verify` | 通过（脚本内） |
| 挂载后的内容 | `Ai of Cage.app` 和指向 `/Applications` 的快捷方式 |
| 应用版本 | `Info.plist` 为 0.1.0；桌面程序写入 0.1.0（内嵌的 Linux 门卫和连接器仍是 0.1.0-dev，它们不显示版本） |
| 架构与构建 | Mach-O arm64，`-tags=production` |
| 签名 | 本机临时签名（adhoc），标识 `io.aiofcage.desktop`，挂载后 `codesign --verify --deep --strict` 通过 |
| `spctl --assess --type execute` | rejected（未公证的应用都会这样） |
| `syspolicy_check distribution` | 临时签名（警告），缺少公证票据（致命） |

`spctl` 和 `syspolicy_check` 的结果说明：从网上下载的安装包，第一次打开一定会被 Gatekeeper 拦下，需要用户按 README 的步骤在“隐私与安全性”里放行。

## 未验证

- **另一台 Mac 或新账户试装**：浏览器下载后打开、放行，再完成新手教程，这一步需要用户来做。系统提示的原话以实际看到的为准。
- **发布到 GitHub Releases**：需要团队确认，本次没有发布。当前 Releases 页面还没有安装包，README 已写明这种情况下从源码安装或自己打包。
- **本机试运行**：没有在本机打开安装包里的应用，因为会弹出窗口并读写用户正在用的数据。安装包里的应用就是 `make app` 的产物，只多写了版本号。

## 限制

- 没有 Apple Developer ID 证书和公证（10-30 决定先不做），第一次打开需要手动放行。
- 安装包不带 Colima 和 Docker，仍要用 Homebrew 安装。
- 首次启动沙箱时，还要从 GitHub 下载虚拟机镜像，从 npm 下载智能体。
- 只有 Apple Silicon 版本。
