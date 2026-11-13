"""Section 13: any set of agent arms against one baseline arm.

usage: arms_report.py BASELINE_ARM results.jsonl [more.jsonl ...]
  later files override earlier ones for the same arm and task (reruns).
"""
import collections
import json
import math
import statistics
import sys

base = sys.argv[1]
arms = collections.defaultdict(dict)
for p in sys.argv[2:]:
    for l in open(p):
        r = json.loads(l)
        if r.get("arm", "").startswith("agent-"):
            arms[r["arm"]][r["exercise"]] = r
full = arms[base]


def mcnemar(b, c):
    m = b + c
    return 1.0 if m == 0 else min(1.0, 2 * sum(math.comb(m, i) for i in range(min(b, c) + 1)) / 2 ** m)


def info(r, k, d=0):
    return (r.get("info") or {}).get(k, d)


def short(k):
    return k.replace("mcp__graft__", "").replace("mcp__graphify__", "").replace("mcp__cola__", "")


for a in [base] + sorted(x for x in arms if x != base):
    ids = sorted(set(arms[a]) & set(full))
    if not ids:
        continue
    rs = [arms[a][i] for i in ids]
    n = len(rs)
    cost = [r["total"]["cost_usd"] for r in rs]
    u = [info(r, "usage", {}) for r in rs]
    calls = collections.Counter()
    for r in rs:
        calls.update(info(r, "tool_calls", {}))
    b = sum(r["pass"] and not full[r["exercise"]]["pass"] for r in rs)
    c = sum(full[r["exercise"]]["pass"] and not r["pass"] for r in rs)
    print(f"{a:28s} pass {sum(r['pass'] for r in rs):2d}/{n}  errors {sum(r['outcome'] == 'error' for r in rs)}  "
          f"${statistics.mean(cost):.3f}/task (median ${statistics.median(cost):.3f})  turns {statistics.mean(info(r, 'turns') for r in rs):.1f}  "
          f"vs {base}: only this {b}, only base {c}, p={mcnemar(b, c):.2f}")
    print(f"{'':28s} tokens/task: output {statistics.mean(x.get('output_tokens', 0) for x in u):.0f}, cache read {statistics.mean(x.get('cache_read_input_tokens', 0) for x in u):.0f}, "
          f"cache write {statistics.mean(x.get('cache_creation_input_tokens', 0) for x in u):.0f}; read_file view {statistics.mean(info(r, 'view_tokens') for r in rs):.0f}; "
          f"saw every gold file {sum(info(r, 'read_gold_files') == info(r, 'gold_files', 1) for r in rs)}/{n}; no edit {sum(info(r, 'edits_ok') == 0 for r in rs)}")
    print(f"{'':28s} tool calls/task {dict((short(k), round(v / n, 1)) for k, v in calls.most_common(6))}")
print("\nspend:", round(sum(r["total"]["cost_usd"] for rs in arms.values() for r in rs.values()), 2))
