"""Rewrite each issue in plain words, without the identifiers Grep would find.

usage: vague_issues.py IDS.txt INST_DIR OUT_DIR [MODEL]
  writes OUT_DIR/<id>.md and prints, per issue, how many of the original's
  code identifiers survive in the rewrite.

The rewrite keeps what is observed and what is expected, and drops function,
class, module and file names, code blocks and error text. Names the fix must
introduce are kept, since the hidden tests call them.
"""
import concurrent.futures
import json
import os
import re
import subprocess
import sys

SYSTEM = """You rewrite bug reports for a Python library. The reader will fix the bug, so every fact must survive, but the rewrite must read as if written by a user who has never seen the library's source.

Rules:
- Keep what was done, what happened, and what should have happened, including concrete values, shapes and numbers.
- Describe operations in plain words. Do not use any existing function, method, class, module, attribute or parameter name, any file path, any code block or inline code, or the text of any error message or traceback. Say what kind of error it was in words.
- Mathematical expressions may be written as ordinary maths (x**2 + 1, a 0-by-3 matrix), not as library calls.
- Exception: if the report asks for something new to be added (a new function, argument, option or method), keep that new name exactly.
- Do not add guesses about the cause or the location of the bug that the report does not state. If the report states a cause in terms of code, restate it in words without names.
- Reply with the rewritten report only."""


def one(iid, inst, out, model):
    dst = os.path.join(out, iid + ".md")
    src = json.load(open(os.path.join(inst, iid + ".json")))["problem_statement"]
    if not os.path.exists(dst):
        env = {k: v for k, v in os.environ.items() if k != "CLAUDECODE"}
        r = subprocess.run(["claude", "-p", "--model", model, "--system-prompt", SYSTEM, "--tools", "", "--setting-sources", "",
                            "--no-session-persistence", "--output-format", "json"],
                           input="# Bug report\n" + src, capture_output=True, text=True, env=env, cwd=out)
        res = json.loads(r.stdout)
        open(dst, "w").write(res["result"].strip() + "\n")
        cost = res.get("total_cost_usd", 0)
    else:
        cost = 0
    new = open(dst).read()
    # Identifiers Grep could have used: code-looking words of the original still present.
    code = set(re.findall(r"[A-Za-z_][A-Za-z0-9_]{3,}", " ".join(re.findall(r"```.*?```|`[^`]+`", src, re.S))))
    left = sorted(w for w in code if re.search(r"\b" + re.escape(w) + r"\b", new) and (w.lower() != w or "_" in w))
    return iid, len(src), len(new), left, cost


if __name__ == "__main__":
    ids = [l.strip() for l in open(sys.argv[1]) if l.strip()]
    inst, out = sys.argv[2], sys.argv[3]
    model = sys.argv[4] if len(sys.argv) > 4 else "claude-opus-5-5"
    os.makedirs(out, exist_ok=True)
    total = 0
    with concurrent.futures.ThreadPoolExecutor(4) as ex:
        for iid, a, b, left, cost in ex.map(lambda i: one(i, inst, out, model), ids):
            total += cost
            print(f"{iid}  {a} -> {b} chars  kept code names: {', '.join(left) or '-'}")
    print(f"cost ${total:.2f}")
