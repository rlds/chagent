#!/usr/bin/env python3
"""chagent 的模型可见中文文本审计工具。

这是一个 *审计* 辅助工具，而非硬性门禁：它从一组根目录中提取 Go 字符串字面量，
保留看起来像英文句子的字面量，并打印其源码位置与启发式上下文标签。由人工（或
docs/reference/model-facing-text.zh.md 清单）判断某个命中项是模型可见文本、
内部日志/错误字符串，还是合理的例外（工具名、JSON 字段、标记标签、文件路径、
外部 MCP 文本……）。

用法：
    scripts/check-model-visible-chinese.py                 # 默认根目录
    scripts/check-model-visible-chinese.py pkg/agent pkg/tools
    scripts/check-model-visible-chinese.py --json ...
    scripts/check-model-visible-chinese.py --include-tests ...

除非指定 --fail-on-any-english，否则退出码为 0；指定后只要发现任何纯英文候选
就以 1 退出（适合人工全量审计，在 CI 中噪音较大；默认按设计仅作报告）。
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import dataclass, asdict
from pathlib import Path

DEFAULT_ROOTS = ["pkg/agent", "pkg/tools"]

HAN = re.compile(r"[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]")
# 至少 2 个 ASCII 单词，容忍提示词中常见的标点。
ENGLISH_PHRASE = re.compile(
    r"[A-Za-z][A-Za-z'’\-]*"
    r"(?:\s+(?:[A-Za-z][A-Za-z'’\-]*|\d+|and|or|with|for|the|to|of|in|on|by|is|are|not|no|must|should|can|may|will|be|it|this|that))"
    r"(?:\s+[A-Za-z][A-Za-z'’\-]*){1,}"
)

# 方括号形式的模型可见标记，例如 "[SubTurn Result] ..." 或
# "[Loaded image from tool result above]"。像 "<context_pressure>" 这样的
# 结构化标记故意不匹配（英文标签名是 docs/reference/model-facing-text.zh.md
# 中记录的允许例外）。
BRACKET_ENGLISH = re.compile(r"^\s*\[[A-Za-z][A-Za-z0-9 _\-/:.]*\]")

# 语句明显属于内部的代码行：logger 调用、错误构造、CLI 标志的帮助字符串、
# JSON/YAML 标签等。
INTERNAL_CONTEXT = re.compile(
    r"logger\.|log\.|fmt\.Errorf|errors\.New|fmt\.Sprintf\([^)]*err|t\.Fatal|t\.Error|panic\(|"
    r"MustCompile|json:|yaml:"
)

# 属于结构化标记 / 协议标记而非散文的字面量。
MARKUP = re.compile(r"^\s*[<>{}\[\]/\\]*\s*$")
PROTOCOL_TOKENS = {
    "CONTEXT_SUMMARY:",
    "CONTEXT_SUMMARY",
}


@dataclass
class Hit:
    path: str
    line: int
    kind: str  # "english" | "mixed"
    context: str
    literal: str


def extract_literals(source: str) -> list[tuple[int, str]]:
    """返回每个双引号字符串或原始字符串的 (line_number, literal)。"""
    out: list[tuple[int, str]] = []
    i = 0
    line = 1
    n = len(source)
    while i < n:
        ch = source[i]
        if ch == "\n":
            line += 1
            i += 1
            continue
        if ch == "/" and i + 1 < n and source[i + 1] == "/":
            # 行注释
            while i < n and source[i] != "\n":
                i += 1
            continue
        if ch == "/" and i + 1 < n and source[i + 1] == "*":
            i += 2
            while i + 1 < n and not (source[i] == "*" and source[i + 1] == "/"):
                if source[i] == "\n":
                    line += 1
                i += 1
            i += 2
            continue
        if ch == "`":
            start_line = line
            i += 1
            buf = []
            while i < n and source[i] != "`":
                if source[i] == "\n":
                    line += 1
                    buf.append("\n")
                else:
                    buf.append(source[i])
                i += 1
            i += 1
            out.append((start_line, "".join(buf)))
            continue
        if ch == '"':
            start_line = line
            i += 1
            buf = []
            while i < n and source[i] != '"':
                if source[i] == "\\" and i + 1 < n:
                    buf.append(source[i : i + 2])
                    i += 2
                    continue
                if source[i] == "\n":  # 无效的 Go 代码，予以容错
                    line += 1
                buf.append(source[i])
                i += 1
            i += 1
            out.append((start_line, "".join(buf)))
            continue
        if ch == "'":
            # rune 字面量：跳到收尾引号
            i += 1
            while i < n and source[i] != "'":
                if source[i] == "\\":
                    i += 1
                i += 1
            i += 1
            continue
        i += 1
    return out


def classify(literal: str, statement_context: str) -> str | None:
    text = literal.strip()
    if not text or len(text) < 6:
        return None
    if MARKUP.match(text):
        return None
    if text in PROTOCOL_TOKENS:
        return None
    phrase_match = ENGLISH_PHRASE.search(text)
    bracket_match = BRACKET_ENGLISH.match(text)
    if phrase_match is None and bracket_match is None:
        return None
    if phrase_match is not None:
        phrase = phrase_match.group(0)
    else:
        phrase = bracket_match.group(0)  # type: ignore[union-attr]
    words = [w for w in re.findall(r"[A-Za-z]{2,}", phrase)]
    if phrase_match is not None and len(words) < 3:
        return None
    if phrase_match is None and len(words) < 2:
        return None
    if INTERNAL_CONTEXT.search(statement_context) and not HAN.search(text):
        return "internal"
    return "mixed" if HAN.search(text) else "english"


def scan_file(path: Path) -> list[Hit]:
    source = path.read_text(encoding="utf-8", errors="replace")
    lines = source.splitlines()
    hits: list[Hit] = []
    for line_no, literal in extract_literals(source):
        # 语句上下文：本行加上前两行。
        start = max(0, line_no - 3)
        context = " ".join(lines[start:line_no])
        kind = classify(literal, context)
        if kind is None:
            continue
        hits.append(
            Hit(
                path=str(path),
                line=line_no,
                kind=kind,
                context=context.strip()[:160],
                literal=literal.strip().replace("\n", "\\n")[:200],
            )
        )
    return hits


def iter_go_files(roots: list[str], include_tests: bool) -> list[Path]:
    files: list[Path] = []
    for root in roots:
        base = Path(root)
        if not base.exists():
            print(f"warning: root does not exist: {root}", file=sys.stderr)
            continue
        if base.is_file():
            files.append(base)
            continue
        for path in sorted(base.rglob("*.go")):
            if not include_tests and path.name.endswith("_test.go"):
                continue
            files.append(path)
    return files


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="审计 Go 源码中的英文模型可见文本")
    parser.add_argument("roots", nargs="*", default=DEFAULT_ROOTS, help="扫描根目录（默认 pkg/agent pkg/tools）")
    parser.add_argument("--json", action="store_true", help="输出 JSON")
    parser.add_argument("--include-tests", action="store_true", help="包含 _test.go")
    parser.add_argument(
        "--fail-on-any-english",
        action="store_true",
        help="发现纯英文候选时返回非 0（默认仅报告）",
    )
    parser.add_argument(
        "--exclude-kind",
        default="internal",
        help="省略输出的候选类型：internal|none（默认 internal，保留 english/mixed）",
    )
    args = parser.parse_args(argv)

    roots = args.roots or DEFAULT_ROOTS
    hits: list[Hit] = []
    for path in iter_go_files(roots, args.include_tests):
        hits.extend(scan_file(path))

    if args.exclude_kind != "none":
        hits = [h for h in hits if h.kind != args.exclude_kind]

    if args.json:
        print(json.dumps([asdict(h) for h in hits], ensure_ascii=False, indent=2))
    else:
        print(f"# 英文候选扫描（roots={', '.join(roots)}，共 {len(hits)} 条）")
        print("# kind=english（纯英文，模型可见则需翻译）/ mixed（中文串里混英文句子）")
        for hit in hits:
            print(f"{hit.path}:{hit.line}: [{hit.kind}] {hit.literal}")
            if hit.context:
                print(f"    ctx: {hit.context}")
    if args.fail_on_any_english and any(h.kind == "english" for h in hits):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
