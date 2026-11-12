#!/usr/bin/env python3
"""Run the code in a bug report and record which functions of a file it calls.

usage: trace.py <target.py> [--preamble CODE] < issue.txt

Code is taken from fenced blocks (```...```) and from interactive lines
(">>> ", "... ", "In [n]: "). Each block runs in one shared namespace after
the preamble; a block that does not parse runs statement by statement, and
errors are recorded, not fatal (the bug often is the error). Prints JSON:
  first_lines  first line of each function of <target.py> that was called
               (import statements run untraced: import-time calls do not count)
  raised_in    first lines of the target's functions in the traceback of an
               error the code raised: the strongest signal there is
  blocks       code blocks found;  ran  statements that ran without error
  errors       first errors, shortened
A localization signal, not a test: what runs is what the report's own
example touches. Run it sandboxed; the code comes from the report.
"""
from __future__ import annotations

import ast
import io
import json
import os
import re
import sys
import threading


def snippets(text: str) -> list[str]:
    text = text.replace("\r", "")
    out = [b for b in re.findall(r"```[^\n]*\n(.*?)```", text, re.S)]
    rest = re.sub(r"```.*?```", "", text, flags=re.S)
    inter, cur = [], []
    for line in (rest + "\n" + "\n".join(out)).splitlines():
        s = line.lstrip()
        m = re.match(r"(>>> |\.\.\. |In \[\d+\]: )(.*)", s)
        if m:
            cur.append(m.group(2))
        elif cur:
            inter.append("\n".join(cur))
            cur = []
    if cur:
        inter.append("\n".join(cur))
    # Blocks written in interactive style are replaced by their code lines.
    plain = [b for b in out if not re.search(r"^\s*(>>> |In \[\d+\]: )", b, re.M)]
    return plain + inter


def statements(code: str) -> list[str]:
    try:
        tree = ast.parse(code)
    except SyntaxError:
        # Keep the lines that parse on their own (drops prose and output).
        good = []
        for line in code.splitlines():
            try:
                ast.parse(line.strip())
                if line.strip():
                    good.append(line.strip())
            except SyntaxError:
                pass
        return good
    return [ast.get_source_segment(code, n) or "" for n in tree.body]


def _first(st: str):
    try:
        body = ast.parse(st).body
        return body[0] if body else None
    except SyntaxError:
        return None


def main() -> None:
    target = os.path.realpath(sys.argv[1])
    preamble = ""
    if "--preamble" in sys.argv:
        preamble = sys.argv[sys.argv.index("--preamble") + 1]
    blocks = snippets(sys.stdin.read())
    called: set[int] = set()
    raised: set[int] = set()
    errors: list[str] = []
    ran = 0

    def tracer(frame, event, arg):
        if event == "call" and frame.f_code.co_filename == target and frame.f_code.co_name != "<module>":
            called.add(frame.f_code.co_firstlineno)
        return None

    real_out = sys.stdout
    sys.stdout = io.StringIO()  # the report's prints must not mix with the JSON
    ns: dict = {"__name__": "__report__"}
    if preamble:
        try:
            exec(preamble, ns)
        except Exception as e:  # noqa: BLE001
            errors.append(f"preamble: {type(e).__name__}: {e}"[:200])
    sys.setrecursionlimit(5000)
    threading.settrace(tracer)
    for b in blocks:
        for st in statements(b):
            if not st:
                continue
            is_import = isinstance(_first(st), (ast.Import, ast.ImportFrom))
            if not is_import:
                sys.settrace(tracer)
            try:
                exec(compile(st, "<report>", "exec"), ns)
                ran += 1
            except BaseException as e:  # noqa: BLE001  the bug may raise anything
                if isinstance(e, KeyboardInterrupt):
                    raise
                tb = e.__traceback__
                while tb is not None:
                    if tb.tb_frame.f_code.co_filename == target and tb.tb_frame.f_code.co_name != "<module>":
                        raised.add(tb.tb_frame.f_code.co_firstlineno)
                    tb = tb.tb_next
                if len(errors) < 8:
                    errors.append(f"{type(e).__name__}: {e}"[:200])
            finally:
                sys.settrace(None)
    real_out.write(json.dumps({"first_lines": sorted(called), "raised_in": sorted(raised), "blocks": len(blocks), "ran": ran, "errors": errors}))


if __name__ == "__main__":
    main()
