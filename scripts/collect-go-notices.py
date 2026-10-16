#!/usr/bin/env python3
import argparse
import json
import pathlib
import subprocess


def load_modules(text):
    decoder = json.JSONDecoder()
    modules = {}
    while text.strip():
        item, consumed = decoder.raw_decode(text.lstrip())
        modules[item["Path"]] = item
        text = text.lstrip()[consumed:]
    return modules


def main():
    parser = argparse.ArgumentParser(description="汇总实际编入 Go 二进制的模块许可证，不输出开发目录")
    parser.add_argument("binary")
    parser.add_argument("module_metadata")
    parser.add_argument("go_license")
    parser.add_argument("output")
    parser.add_argument("--go", default="go")
    args = parser.parse_args()
    output = pathlib.Path(args.output)
    if output.exists():
        raise SystemExit("许可汇总已存在，未覆盖")
    modules = load_modules(pathlib.Path(args.module_metadata).read_text())
    information = subprocess.check_output([args.go, "version", "-m", args.binary], text=True)
    dependencies = []
    for line in information.splitlines():
        fields = line.split()
        if fields and fields[0] == "dep" and len(fields) >= 3:
            dependencies.append((fields[1], fields[2]))
    if not dependencies:
        raise SystemExit("未读取到实际编入的模块，停止生成许可汇总")
    sections = ["IM Agent 第三方依赖许可文件汇总\n\n以下保留原始许可文本，不替代各依赖自身许可条款。\n",
                "Go runtime\n" + pathlib.Path(args.go_license).read_text()]
    missing = []
    for name, version in dependencies:
        entry = modules.get(name, {})
        resolved = entry.get("Replace", entry)
        directory = resolved.get("Dir")
        if not directory:
            missing.append(name)
            continue
        root = pathlib.Path(directory)
        notices = [file for file in root.iterdir() if file.is_file() and not file.is_symlink()
                   and file.name.upper().startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE", "COPYRIGHT"))]
        if not notices:
            missing.append(name)
            continue
        section = [f"{name} {version}\n"]
        for file in sorted(notices):
            section.append(file.name + "\n" + file.read_text(errors="replace"))
        sections.append("\n".join(section))
    if missing:
        raise SystemExit("以下模块未找到许可文件，暂不继续打包：\n" + "\n".join(missing))
    output.write_text(("\n\n" + "=" * 72 + "\n\n").join(sections) + "\n")
    print(f"已汇总 Go 运行时及 {len(dependencies)} 个实际编入模块的许可文件")


if __name__ == "__main__":
    main()
