#!/usr/bin/env python3
"""依赖许可证检查：扫描 Go 模块与前端依赖，拒绝非白名单许可证。

关键步骤：
1. 通过 `go list -m -json all` 枚举实际参与构建的 Go 模块，读取模块目录内的许可证文件并按文本特征识别 SPDX。
2. 读取 web/package-lock.json 中的依赖声明，支持 SPDX 表达式的 OR/AND 组合。
3. 输出违规依赖并以非零码退出；`scripts/license_allowlist.txt` 可记录经过法务确认的例外。
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from typing import Iterable

ROOT_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ALLOWLIST_PATH = os.path.join(ROOT_DIR, "scripts", "license_allowlist.txt")

# 允许的许可证标识：均为宽松许可或已获合规确认的弱 copyleft（MPL-2.0 仅文件级）。
ALLOWED_LICENSES = {
    "MIT",
    "MIT-0",
    "Apache-2.0",
    "BSD-2-Clause",
    "BSD-3-Clause",
    "ISC",
    "0BSD",
    "BlueOak-1.0.0",
    "CC0-1.0",
    "Unlicense",
    "MPL-2.0",
    "CC-BY-4.0",
    "Python-2.0",
    "Zlib",
}

LICENSE_FILE_PREFIXES = ("LICENSE", "LICENCE", "COPYING", "NOTICE")
MAX_LICENSE_BYTES = 1 << 20


def normalize(text: str) -> str:
    """折叠空白并转小写，便于跨换行与缩进匹配许可证文本。"""
    return re.sub(r"\s+", " ", text.lower())


def detect_licenses(text: str) -> set[str]:
    """从许可证文本识别 SPDX 标识；无法识别时返回空集合。"""
    normalized = normalize(text)
    spdx = re.search(r"spdx-license-identifier: *([^\n*]+)", text, re.IGNORECASE)
    if spdx:
        return {token.strip() for token in re.split(r"\s+OR\s+|\s+AND\s+", spdx.group(1)) if token.strip()}
    found: set[str] = set()
    if "mit no attribution" in normalized:
        found.add("MIT-0")
    if "mit license" in normalized or "permission is hereby granted, free of charge, to any person obtaining a copy" in normalized:
        found.add("MIT")
    if "apache license" in normalized and "version 2.0" in normalized:
        found.add("Apache-2.0")
    if "redistribution and use in source and binary forms" in normalized:
        if "neither the name" in normalized:
            found.add("BSD-3-Clause")
        else:
            found.add("BSD-2-Clause")
    if "permission to use, copy, modify, and/or distribute this software for any purpose" in normalized:
        found.add("ISC")
    if "mozilla public license version 2.0" in normalized:
        found.add("MPL-2.0")
    if "free and unencumbered software released into the public domain" in normalized:
        found.add("Unlicense")
    if "cc0 1.0 universal" in normalized:
        found.add("CC0-1.0")
    if "blue oak model license" in normalized:
        found.add("BlueOak-1.0.0")
    if "creative commons attribution 4.0" in normalized:
        found.add("CC-BY-4.0")
    if "python software foundation license" in normalized:
        found.add("Python-2.0")
    if "zlib license" in normalized or "altered versions must be plainly marked" in normalized:
        found.add("Zlib")
    return found


def read_license_texts(directory: str) -> list[str]:
    """读取目录下所有许可证相关文件的文本。"""
    texts: list[str] = []
    try:
        entries = sorted(os.listdir(directory))
    except OSError:
        return texts
    for name in entries:
        if not name.upper().startswith(LICENSE_FILE_PREFIXES):
            continue
        path = os.path.join(directory, name)
        if not os.path.isfile(path):
            continue
        try:
            if os.path.getsize(path) > MAX_LICENSE_BYTES:
                continue
            with open(path, "r", encoding="utf-8", errors="ignore") as handle:
                texts.append(handle.read())
        except OSError:
            continue
    return texts


def load_allowlist() -> set[str]:
    """读取人工确认的例外模块；每行支持 `module # 理由`。"""
    if not os.path.isfile(ALLOWLIST_PATH):
        return set()
    allowed: set[str] = set()
    with open(ALLOWLIST_PATH, "r", encoding="utf-8") as handle:
        for line in handle:
            entry = line.split("#", 1)[0].strip()
            if entry:
                allowed.add(entry)
    return allowed


def go_modules() -> Iterable[dict]:
    """枚举实际参与构建的 Go 模块（去重），避免检查未下载的测试依赖图。"""
    output = subprocess.check_output(["go", "list", "-deps", "-json", "./..."], cwd=ROOT_DIR, text=True)
    decoder = json.JSONDecoder()
    index = 0
    seen: set[str] = set()
    while index < len(output):
        while index < len(output) and output[index].isspace():
            index += 1
        if index >= len(output):
            break
        package, index = decoder.raw_decode(output, index)
        module = package.get("Module")
        if not module or module.get("Main"):
            continue
        path = module.get("Path", "")
        if path in seen:
            continue
        seen.add(path)
        yield module


def check_go(allowlist: set[str]) -> list[str]:
    """检查 Go 模块许可证，返回违规描述列表。"""
    violations: list[str] = []
    checked = 0
    for module in go_modules():
        if module.get("Main"):
            continue
        path = module.get("Path", "")
        directory = module.get("Dir", "")
        if path in allowlist:
            continue
        if not directory:
            violations.append(f"Go module {path}: missing module directory")
            continue
        licenses: set[str] = set()
        for text in read_license_texts(directory):
            licenses |= detect_licenses(text)
        if not licenses:
            violations.append(f"Go module {path}: no recognizable license")
            continue
        checked += 1
        if not licenses <= ALLOWED_LICENSES:
            violations.append(f"Go module {path}: disallowed license {sorted(licenses - ALLOWED_LICENSES)}")
    print(f"Go modules checked: {checked}")
    return violations


def expression_allowed(expression: str) -> bool:
    """按 SPDX 表达式的 OR（任一）与 AND（全部）语义判断是否允许。"""
    cleaned = expression.strip().strip("()")
    if not cleaned:
        return False
    for alternative in re.split(r"\s+OR\s+", cleaned):
        terms = [term.strip(" ()") for term in re.split(r"\s+AND\s+", alternative)]
        if all(term in ALLOWED_LICENSES for term in terms if term):
            return True
    return False


def check_npm(allowlist: set[str]) -> list[str]:
    """检查前端依赖的许可证声明。"""
    lock_path = os.path.join(ROOT_DIR, "web", "package-lock.json")
    if not os.path.isfile(lock_path):
        print("web/package-lock.json not found; skipping npm license check")
        return []
    with open(lock_path, "r", encoding="utf-8") as handle:
        lock = json.load(handle)
    violations: list[str] = []
    checked = 0
    for path, metadata in lock.get("packages", {}).items():
        if not path.startswith("node_modules/") or metadata.get("extraneous"):
            continue
        name = metadata.get("name") or path.rsplit("node_modules/", 1)[-1]
        if name in allowlist:
            continue
        expression = metadata.get("license")
        if not expression:
            manifest = os.path.join(ROOT_DIR, "web", path, "package.json")
            if os.path.isfile(manifest):
                with open(manifest, "r", encoding="utf-8") as handle:
                    expression = json.load(handle).get("license")
        if not expression:
            violations.append(f"npm package {name}: missing license")
            continue
        checked += 1
        if not expression_allowed(str(expression)):
            violations.append(f"npm package {name}: disallowed license {expression}")
    print(f"npm packages checked: {checked}")
    return violations


def main() -> int:
    allowlist = load_allowlist()
    violations = check_go(allowlist) + check_npm(allowlist)
    if violations:
        print("License check failed:", file=sys.stderr)
        for violation in violations:
            print(f"  - {violation}", file=sys.stderr)
        return 1
    print("License check passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
