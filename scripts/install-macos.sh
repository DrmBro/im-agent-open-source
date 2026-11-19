#!/bin/bash
set -euo pipefail

project_dir="$(cd "$(dirname "$0")/.." && pwd)"
source_app="${1:-$project_dir/build/bin/im-agent.app}"
destination="${2:-/Applications}"
client_app="$destination/IM Agent.app"
background_app="$destination/IM Agent 后台.app"

if [[ "$(uname -s)" != Darwin || ! -f "$source_app/Contents/MacOS/im-agent" ]]; then
  echo "请在 macOS 上提供已构建的 im-agent.app。" >&2
  exit 1
fi
if /bin/ps -axo command= | /usr/bin/grep -F -x -- "$client_app/Contents/MacOS/im-agent" >/dev/null; then
  echo "请先正常关闭应用程序中的 IM Agent 客户端；后台不需要停止。" >&2
  exit 1
fi
/usr/bin/codesign --verify --deep --strict "$source_app"
mkdir -p "$destination"
staging="$(mktemp -d "$destination/.im-agent-install.XXXXXX")"
trap 'rm -rf "$staging"' EXIT
/usr/bin/ditto "$source_app" "$staging/IM Agent.app"
mkdir -p "$staging/IM Agent 后台.app/Contents/MacOS" "$staging/IM Agent 后台.app/Contents/Resources"
cat > "$staging/IM Agent 后台.app/Contents/MacOS/start-background" <<'LAUNCHER'
#!/bin/bash
set -eu
applications_dir="$(cd "$(dirname "$0")/../../.." && pwd)"
client_app="$applications_dir/IM Agent.app"
if "$client_app/Contents/MacOS/im-agent" --background-start; then
  exit 0
fi
exec /usr/bin/open "$client_app"
LAUNCHER
chmod 755 "$staging/IM Agent 后台.app/Contents/MacOS/start-background"
cat > "$staging/IM Agent 后台.app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleName</key><string>IM Agent 后台</string>
<key>CFBundleDisplayName</key><string>IM Agent 后台</string>
<key>CFBundleIdentifier</key><string>io.im-agent.background-launcher</string>
<key>CFBundleExecutable</key><string>start-background</string>
<key>CFBundleVersion</key><string>1</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
<key>CFBundleIconFile</key><string>iconfile</string>
<key>LSUIElement</key><true/>
</dict></plist>
PLIST
cp "$source_app/Contents/Resources/iconfile.icns" "$staging/IM Agent 后台.app/Contents/Resources/iconfile.icns"
/usr/bin/plutil -lint "$staging/IM Agent 后台.app/Contents/Info.plist"
/usr/bin/codesign --force --sign - "$staging/IM Agent 后台.app"
/usr/bin/codesign --verify --deep --strict "$staging/IM Agent 后台.app"
# 交换后旧应用暂存在本次 staging，退出时清理。
installed=()
install_app() {
  local target="$1" incoming="$staging/${1##*/}"
  if [[ -e "$target" || -L "$target" ]]; then
    python3 "$project_dir/scripts/swap_macos.py" "$incoming" "$target"
  else
    mv "$incoming" "$target"
  fi
  installed+=("$target")
}
finish_install() {
  local status=$? i target incoming
  trap - EXIT
  if (( status != 0 )); then
    for ((i=${#installed[@]}-1; i>=0; i--)); do
      target="${installed[i]}"
      incoming="$staging/${target##*/}"
      if [[ -e "$incoming" || -L "$incoming" ]]; then
        python3 "$project_dir/scripts/swap_macos.py" "$incoming" "$target"
      else
        mv "$target" "$incoming"
      fi
    done
  fi
  rm -rf "$staging"
  exit "$status"
}
trap finish_install EXIT
install_app "$client_app"
install_app "$background_app"
registration="/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
"$registration" -f "$client_app"
"$registration" -f "$background_app"
printf '已安装：\n%s\n%s\n' "$client_app" "$background_app"
printf '仅安装启动入口；不修改登录自启，不停止或重启正在运行的后台。\n'
