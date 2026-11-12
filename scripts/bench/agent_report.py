"""Section 9: diet-cola inside a multi-turn agent loop, caching on, no file list.

usage: agent_report.py swe45-agent.jsonl [swe45-opus.jsonl swe20-holdout.jsonl swe45-cola.jsonl]
  the optional files add the single-call arms of section 8 for reference.
"""
import collections
import json
import math
import statistics
import sys

arms = collections.defaultdict(dict)
for p in sys.argv[1:]:
    for l in open(p):
        r = json.loads(l)
        if r.get("arm") in ("agent-full", "agent-cola", "large", "large-cola"):
            arms[r["arm"]][r["exercise"]] = r
ids = sorted(set(arms["agent-full"]) & set(arms["agent-cola"]))
n = len(ids)


def mcnemar(b, c):
    m = b + c
    return 1.0 if m == 0 else min(1.0, 2 * sum(math.comb(m, i) for i in range(min(b, c) + 1)) / 2 ** m)


def info(r, k, d=0):
    return (r.get("info") or {}).get(k, d)


print(f"== {n} instances, agent loop (Opus + Grep/Glob + read_file/edit[/show]), caching on, files not given")
for a in ("agent-full", "agent-cola"):
    rs = [arms[a][i] for i in ids]
    k = sum(r["pass"] for r in rs)
    errs = sum(r["outcome"] == "error" for r in rs)
    cost = [r["total"]["cost_usd"] for r in rs]
    u = [info(r, "usage", {}) for r in rs]
    calls = collections.Counter()
    for r in rs:
        calls.update(info(r, "tool_calls", {}))
    roles = collections.Counter()
    for r in rs:
        for role, x in r["usage"].items():
            roles[role] += x["cost_usd"] / n
    print(f"  {a:11s} pass {k:2d}/{n} = {100 * k / n:4.1f}%  errors {errs}  ${statistics.mean(cost):.4f}/task (median ${statistics.median(cost):.4f})  "
          f"turns {statistics.mean(info(r, 'turns') for r in rs):.1f}")
    print(f"  {'':11s} Opus tokens/task: fresh {statistics.mean(x.get('input_tokens', 0) for x in u):.0f}, cache write {statistics.mean(x.get('cache_creation_input_tokens', 0) for x in u):.0f}, "
          f"cache read {statistics.mean(x.get('cache_read_input_tokens', 0) for x in u):.0f}, output {statistics.mean(x.get('output_tokens', 0) for x in u):.0f}")
    print(f"  {'':11s} by role {dict((k, round(v, 4)) for k, v in roles.items())}")
    print(f"  {'':11s} tool calls/task {dict((k, round(v / n, 2)) for k, v in calls.most_common())}")
    print(f"  {'':11s} read_file view tokens/task {statistics.mean(info(r, 'view_tokens') for r in rs):.0f} (files {statistics.mean(info(r, 'file_tokens') for r in rs):.0f}); "
          f"saw every gold file {sum(info(r, 'read_gold_files') == info(r, 'gold_files', 1) for r in rs)}/{n}; "
          f"gate fired {sum(info(r, 'gated') > 0 for r in rs)}; show used {sum(info(r, 'shows') > 0 for r in rs)}; no edit {sum(info(r, 'edits_ok') == 0 for r in rs)}; "
          f"hit budget {sum(info(r, 'subtype') == 'error_max_budget_usd' for r in rs)}")
b = sum(arms["agent-cola"][i]["pass"] and not arms["agent-full"][i]["pass"] for i in ids)
c = sum(arms["agent-full"][i]["pass"] and not arms["agent-cola"][i]["pass"] for i in ids)
print(f"\n  cola vs full: only cola {b}, only full {c}, McNemar p={mcnemar(b, c):.2f}")
fc = [arms["agent-full"][i]["total"]["cost_usd"] for i in ids]
cc = [arms["agent-cola"][i]["total"]["cost_usd"] for i in ids]
print(f"  cost: cola/full = {sum(cc) / sum(fc):.2f}; cola cheaper on {sum(x < y for x, y in zip(cc, fc))}/{n}")
for a in ("large", "large-cola"):
    if a in arms:
        rs = [arms[a][i] for i in ids if i in arms[a]]
        print(f"  (section 8, one call, file given) {a:10s} pass {sum(r['pass'] for r in rs)}/{len(rs)}  ${statistics.mean(r['total']['cost_usd'] for r in rs):.4f}/task")
print("\nspend (agent arms):", round(sum(r["total"]["cost_usd"] for a in ("agent-full", "agent-cola") for r in arms[a].values()), 2))
