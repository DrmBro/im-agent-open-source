#!/bin/bash
set -euo pipefail

project_dir="$(cd "$(dirname "$0")/.." && pwd)"
source_app="${1:?请提供干净构建的 im-agent.app 路径}"
notices="${2:?请提供第三方许可证汇总文件}"
output_dir="${3:-$project_dir/dist}"
label="${4:-preview.$(date +%Y%m%d-%H%M%S)}"

if [[ "$(uname -s)" != Darwin || ! -f "$source_app/Contents/MacOS/im-agent" || ! -s "$notices" ]]; then
  printf '需要 macOS、有效 App 和非空第三方许可文件。\n' >&2
  exit 1
fi
if [[ ! "$label" =~ ^[A-Za-z0-9._-]+$ ]]; then
  printf '发行标签仅允许英文字母、数字、点、下划线和连字符。\n' >&2
  exit 1
fi
architecture="$(/usr/bin/lipo -archs "$source_app/Contents/MacOS/im-agent")"
if [[ "$architecture" != arm64 ]]; then
  printf '本脚本首版只打包 arm64，不将其它架构标成 M 系列版本。\n' >&2
  exit 1
fi
/usr/bin/codesign --verify --deep --strict "$source_app"
version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$source_app/Contents/Info.plist")"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf '应用版本必须为三段数字。\n' >&2
  exit 1
fi
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
base="IM-Agent-$version-$label-arm64"
image="$output_dir/$base.dmg"
if [[ -e "$image" || -e "$image.sha256" || -e "$output_dir/$base.manifest.json" ]]; then
  printf '发行文件已存在，请使用新的发行标签；未覆盖。\n' >&2
  exit 1
fi
staging="$(mktemp -d "$output_dir/.im-agent-package.XXXXXX")"
trap 'rm -rf "$staging"' EXIT
mkdir -p "$staging/image"
/usr/bin/ditto --norsrc --noextattr "$source_app" "$staging/image/IM Agent.app"
cp "$notices" "$staging/image/THIRD_PARTY_NOTICES.txt"
cp "$project_dir/docs/distribution/朋友试用说明.txt" "$staging/image/先读我-安装与配置.txt"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $version" "$staging/image/IM Agent.app/Contents/Info.plist"
/usr/bin/codesign --force --sign - "$staging/image/IM Agent.app"
/usr/bin/codesign --verify --deep --strict "$staging/image/IM Agent.app"
ln -s /Applications "$staging/image/Applications"
/usr/bin/python3 - "$staging/image/IM Agent.app" <<'PY'
import sys
from pathlib import Path
application = Path(sys.argv[1])
allowed = {'Contents/Info.plist', 'Contents/MacOS/im-agent', 'Contents/Resources/iconfile.icns', 'Contents/_CodeSignature/CodeResources'}
files = set()
for path in application.rglob('*'):
    if path.is_symlink():
        raise SystemExit('应用内出现非预期符号链接，停止打包')
    if path.is_file():
        files.add(path.relative_to(application).as_posix())
if files != allowed:
    raise SystemExit('应用文件不符合发行白名单，停止打包')
private_home = str(Path.home()).encode()
if any(private_home in (application / name).read_bytes() for name in files):
    raise SystemExit('发现开发者本机路径，请使用 GOFLAGS=-trimpath 的独立构建')
print('发行白名单和开发者路径检查通过')
PY
/usr/bin/hdiutil create -volname 'IM Agent Preview' -srcfolder "$staging/image" -fs HFS+ -format UDZO "$image"
/usr/bin/hdiutil verify "$image"
/usr/bin/python3 - "$image" "$staging/image/IM Agent.app" "$version" "$label" <<'PY'
import sys, json, hashlib, datetime
from pathlib import Path
image, application = Path(sys.argv[1]), Path(sys.argv[2])
digest = hashlib.sha256(image.read_bytes()).hexdigest()
image.with_suffix('.dmg.sha256').write_text(digest + '  ' + image.name + '\n')
manifest = {
    'product': 'IM Agent', 'version': sys.argv[3], 'release': sys.argv[4], 'architecture': 'arm64',
    'created_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'sha256': digest, 'bytes': image.stat().st_size, 'signing': 'ad-hoc', 'notarized': False,
    'binary_sha256': hashlib.sha256((application / 'Contents/MacOS/im-agent').read_bytes()).hexdigest(),
    'contains_user_data': False, 'fresh_machine_verified': False,
    'requires': ['用户自己的可用 Codex', '兼容的本机桌面连接入口（桌面模式）', '用户自己的飞书机器人配置'],
}
image.with_suffix('.manifest.json').write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + '\n')
print('已生成未公证私测包：' + str(image))
print('SHA-256：' + digest)
PY
