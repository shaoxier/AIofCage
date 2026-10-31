#!/bin/sh
# 把 Ai of Cage.app 打成拖拽安装的 DMG，并写出 SHA256 校验文件。由 make dmg 调用：它先用发布
# 版本号重新构建应用，再执行 scripts/package-dmg.sh "bin/Ai of Cage.app" 版本号 bin/AiofCage-版本号-arm64.dmg
set -eu

app=$1
version=$2
dmg=$3
if [ ! -d "$app" ]; then
	echo "找不到 ${app}，请运行 make dmg" >&2
	exit 1
fi
# 文件名里的版本必须就是应用自己的版本，开发版不能装进发布名字的安装包。
plist_version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist")
if [ -z "$version" ] || [ "$plist_version" != "$version" ]; then
	echo "应用的版本（${plist_version}）和安装包的版本（${version}）不一致，请运行 make dmg" >&2
	exit 1
fi
# 只支持 Apple Silicon：文件名写着 arm64，应用在别的架构上也会拒绝运行。
archs=$(lipo -archs "$app/Contents/MacOS/AIofCage")
if [ "$archs" != "arm64" ]; then
	echo "应用的架构是 ${archs}，安装包只支持 arm64" >&2
	exit 1
fi
# 安装包里的应用必须带有效签名；签名坏了，系统会提示“已损坏”而不是“无法验证开发者”。
codesign --verify --deep --strict "$app"

work=$(mktemp -d "${TMPDIR:-/tmp}/aiofcage-dmg.XXXXXX")
mounted=""
detach() {
	hdiutil detach -quiet "$1" 2>/dev/null || { sleep 2; hdiutil detach -quiet -force "$1"; }
}
cleanup() {
	if [ -n "$mounted" ]; then
		detach "$mounted" || true
		rmdir "$mounted" 2>/dev/null || true
	fi
	rm -rf "$work"
}
# 被中断时清理后退出，不要接着往下执行。
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# 先在临时目录里打包和检查，全部通过后才放到发布路径，检查没过的镜像不会留在那里。
mkdir "$work/content"
ditto "$app" "$work/content/$(basename "$app")"
ln -s /Applications "$work/content/Applications"
image="$work/$(basename "$dmg")"
hdiutil create -quiet -volname "Ai of Cage" -srcfolder "$work/content" -fs HFS+ -format UDZO -imagekey zlib-level=9 "$image"

# 挂载检查：镜像完整，里面的应用签名仍然有效。
hdiutil verify -quiet "$image"
mounted=$(mktemp -d "${TMPDIR:-/tmp}/aiofcage-mount.XXXXXX")
hdiutil attach -quiet -nobrowse -readonly -mountpoint "$mounted" "$image"
codesign --verify --deep --strict "$mounted/$(basename "$app")"
detach "$mounted"
rmdir "$mounted"
mounted=""

(cd "$work" && shasum -a 256 "$(basename "$dmg")" >"$(basename "$dmg").sha256")
mkdir -p "$(dirname "$dmg")"
rm -f "$dmg.sha256"
mv -f "$image" "$dmg"
mv -f "$image.sha256" "$dmg.sha256"

commit=$(git rev-parse --short HEAD 2>/dev/null || echo "未知")
dirty=""
if ! git diff --quiet HEAD 2>/dev/null; then
	dirty="（警告：工作区有未提交的改动，这个安装包不对应任何提交）"
fi
echo "已生成 ${dmg}，来自提交 ${commit}${dirty}"
cat "$dmg.sha256"
