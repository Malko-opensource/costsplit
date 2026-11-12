"""End to end: the diet-cola arms against Opus alone and the v2 pipeline.

usage: cola_e2e_report.py swe45-cola.jsonl swe45-opus.jsonl swe20-holdout.jsonl swe45-v2.jsonl
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
        if r.get("arm") in ("large", "large-hleanarch2", "large-cola", "large-colaarch"):
            arms[r["arm"]][r["exercise"]] = r
names = {"large": "Opus alone", "large-hleanarch2": "v2 pipeline", "large-cola": "cola, Opus edits", "large-colaarch": "cola, Haiku edits"}
ids = sorted(set.intersection(*(set(d) for d in arms.values())))
n = len(ids)


def mcnemar(b, c):
    m = b + c
    if m == 0:
        return 1.0
    return min(1.0, 2 * sum(math.comb(m, i) for i in range(min(b, c) + 1)) / 2 ** m)


print(f"== {n} instances")
for a in ("large", "large-hleanarch2", "large-cola", "large-colaarch"):
    if a not in arms:
        continue
    rs = [arms[a][i] for i in ids]
    k = sum(r["pass"] for r in rs)
    cost = [r["total"]["cost_usd"] for r in rs]
    big = [r["usage"].get("large", {}) for r in rs]
    roles = collections.Counter()
    for r in rs:
        for role, u in r["usage"].items():
            roles[role] += u["cost_usd"] / n
    line = f"  {names[a]:18s} pass {k:2d}/{n} = {100 * k / n:4.1f}%  ${statistics.mean(cost):.4f}/task (median ${statistics.median(cost):.4f})  Opus input {statistics.mean(u.get('input_tokens', 0) for u in big):6.0f} tok"
    if a != "large":
        b = sum(arms[a][i]["pass"] and not arms["large"][i]["pass"] for i in ids)
        c = sum(arms["large"][i]["pass"] and not arms[a][i]["pass"] for i in ids)
        line += f"  vs Opus alone: only this {b}, only Opus {c}, p={mcnemar(b, c):.2f}"
    print(line)
    print(f"  {'':18s} by role {dict((k, round(v, 4)) for k, v in roles.items())}")
for a in ("large-cola", "large-colaarch"):
    if a not in arms:
        continue
    rs = [arms[a][i] for i in ids]
    inf = [r.get("info") or {} for r in rs]
    cut = [x for x in inf if x.get("file_tokens", 0) >= 4000]
    print(f"\n  {names[a]}: {len(cut)} files cut; asked to see code {sum(bool(x.get('asked')) for x in inf)}; gate fired {sum(bool(x.get('gated')) for x in inf)}; "
          f"edit failures {sum(1 for r in rs for s in r['steps'] if 'search block' in s or 'does not compile' in s)}; model errors {sum(1 for r in rs for s in r['steps'] if 'model error' in s)}")
    cov = [r for r, x in zip(rs, inf) if x.get("covers_gold")]
    nc = [r for r, x in zip(rs, inf) if not x.get("covers_gold")]
    print(f"    view held the reference change: {len(cov)} (pass {sum(r['pass'] for r in cov)}, Opus alone {sum(arms['large'][r['exercise']]['pass'] for r in cov)}); "
          f"did not: {len(nc)} (pass {sum(r['pass'] for r in nc)}, Opus alone {sum(arms['large'][r['exercise']]['pass'] for r in nc)})")
    vt = [x["view_tokens"][-1] / x["file_tokens"] for x in cut if x.get("view_tokens")]
    if vt:
        print(f"    last view / file tokens on cut files: median {statistics.median(vt):.0%}")
print("\nspend (cola arms):", round(sum(r["total"]["cost_usd"] for a in ("large-cola", "large-colaarch") for r in arms.get(a, {}).values()), 2))
