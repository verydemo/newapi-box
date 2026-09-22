#!/usr/bin/env bash
# 构建 newapi 桌面版。
#
# 产物是单个 exe：转换服务 + WebView2 窗口在同一个进程里，
# 没有 sidecar，退出也不留后台进程。
#
#   ./build.sh              生成 dist/newapi-box.exe
#   MINIFY=1 ./build.sh        额外生成 dist/newapi-min.exe（约 2.7MB，个别杀软会误报）
#
set -euo pipefail
cd "$(dirname "$0")"
# goversioninfo 是原生 Windows 程序，路径必须是 Windows 风格
ROOT="$(cd .. && { pwd -W 2>/dev/null || pwd; })"
GOVI="${GOVERSIONINFO:-$HOME/.workbuddy/binaries/gobin/goversioninfo.exe}"
ICON="$ROOT/desktop/icons/icon.ico"

echo "==> 1/3 图标"
python icon_gen.py icons/icon.ico

echo "==> 2/3 Windows 资源（应用图标 + 版本信息）"
if [ -x "$GOVI" ]; then
  "$GOVI" -icon "$ICON" -o "$ROOT/desktop/shell/resource.syso" "$ROOT/desktop/versioninfo.json"
else
  echo "    未找到 goversioninfo，跳过：exe 会用默认图标"
  echo "    安装：GOBIN=C:/绝对/路径 go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest"
fi

echo "==> 3/3 编译"
mkdir -p dist
cd "$ROOT"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -H=windowsgui" -o desktop/dist/newapi-box.exe ./desktop/shell

if [ "${MINIFY:-0}" = "1" ]; then
  cp desktop/dist/newapi-box.exe desktop/dist/newapi-min.exe
  upx --best --lzma desktop/dist/newapi-min.exe >/dev/null
fi

cd "$ROOT/desktop"

echo ""
printf '%-24s %s\n' "产物" "大小"
for f in dist/*.exe; do
  printf '%-24s %s\n' "$(basename "$f")" "$(du -h "$f" | cut -f1)"
done
echo ""
echo "newapi.exe 放到任意目录即可运行，config.json 会生成在它旁边。"
