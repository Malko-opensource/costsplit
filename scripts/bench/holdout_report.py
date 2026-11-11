"""Report on the hold-out confirmation.

usage: holdout_report.py holdout.jsonl haiku45.jsonl [tuned files...]
"""
import collections
import json
import math
import statistics
import sys

hold = collections.defaultdict(dict)
for line in open(sys.argv[1]):
    r = json.loads(line)
    hold[r["arm"]][r["exercise"]] = r
other = collections.defaultdict(dict)
for path in sys.argv[2:]:
    for line in open(path):
        r = json.loads(line)
        other[r["arm"]][r["exercise"]] = r
ids = sorted(hold["large"])
n = len(ids)


def mcnemar(b, c):
    m = b + c
    if m == 0:
        return 1.0
    k = min(b, c)
    return min(1.0, 2 * sum(math.comb(m, i) for i in range(k + 1)) / 2 ** m)


print(f"== hold-out: {n} instances never used while choosing the pipeline")
for arm in ("large", "large-hleanarch"):
    rs = [hold[arm][i] for i in ids]
    ok = sum(r["pass"] for r in rs)
    cost = sum(r["total"]["cost_usd"] for r in rs) / n
    roles = collections.Counter()
    tok = {}
    for r in rs:
        for k, u in r["usage"].items():
            roles[k] += u["cost_usd"] / n
    L = [r["usage"].get("large", {}) for r in rs]
    tin = sum(u.get("input_tokens", 0) for u in L) / n
    tout = sum(u.get("output_tokens", 0) for u in L) / n
    th = sum(u.get("thinking_tokens", 0) for u in L) / n
    errs = sum(1 for r in rs for s in r["steps"] if "model error" in s)
    print(f"  {arm:16s} pass {ok:2d}/{n} = {100 * ok / n:5.1f}%  ${cost:.4f}/task  by role {dict((k, round(v, 4)) for k, v in roles.items())}")
    print(f"  {'':16s} Opus: in {tin:.0f} tok (${tin * 4e-6:.4f}), thinking {th:.0f} (${th * 20e-6:.4f}), visible out {tout - th:.0f} (${(tout - th) * 20e-6:.4f}); model errors {errs}")
for arm in ("small", "small-oracle"):
    if arm in other:
        rs = [other[arm][i] for i in ids if i in other[arm]]
        print(f"  {arm:16s} pass {sum(r['pass'] for r in rs):2d}/{len(rs)}  ${sum(r['total']['cost_usd'] for r in rs) / len(rs):.4f}/task  (earlier run)")

a, b = "large-hleanarch", "large"
oa = sum(hold[a][i]["pass"] and not hold[b][i]["pass"] for i in ids)
ob = sum(hold[b][i]["pass"] and not hold[a][i]["pass"] for i in ids)
print(f"\n  pipeline vs Opus alone: only pipeline {oa}, only Opus alone {ob}; exact p = {mcnemar(oa, ob):.2f}")
rs = [hold[a][i] for i in ids]
inf = [r.get("info") or {} for r in rs]
sl = [i for i in inf if not i.get("whole_file")]
print(f"  slices: {len(sl)} of {n} (whole-file fallback {n - len(sl)}); median {statistics.median(i['slice_lines'] for i in sl):.0f} lines of {statistics.median(i['file_lines'] for i in sl):.0f}")
cov = [r for r in rs if (r.get("info") or {}).get("covers_gold")]
print(f"  slice contains the lines the reference patch changes: {len(cov)} of {n}; pass {sum(r['pass'] for r in cov)}; misses: {n - len(cov)}, pass {sum(r['pass'] for r in rs) - sum(r['pass'] for r in cov)}")
words = [i.get("instruction_words") for i in inf if i.get("instruction_words")]
print(f"  instruction length: mean {statistics.mean(words):.0f} words; editor attempts {dict(collections.Counter(r.get('try', 0) for r in rs))}")

# all 45 together: 25 used for tuning + 20 hold-out
if "large" in other and "large-hleanarch" in other:
    lt = [other["large"][i] for i in other["large"]]
    pt = [other["large-hleanarch"][i] for i in other["large-hleanarch"]]
    base_locate = statistics.mean(r["usage"].get("locate", {}).get("cost_usd", 0) for r in other["large-hslice"].values()) if "large-hslice" in other else 0
    la = lt + [hold["large"][i] for i in ids]
    pa = pt + [hold["large-hleanarch"][i] for i in ids]
    pc = (sum(r["total"]["cost_usd"] for r in pt) + base_locate * len(pt) + sum(hold["large-hleanarch"][i]["total"]["cost_usd"] for i in ids)) / len(pa)
    print(f"\n== all {len(la)} instances (25 tuned + {n} hold-out)")
    print(f"  Opus alone       pass {sum(r['pass'] for r in la)}/{len(la)} = {100 * sum(r['pass'] for r in la) / len(la):.1f}%  ${sum(r['total']['cost_usd'] for r in la) / len(la):.4f}/task")
    print(f"  final pipeline   pass {sum(r['pass'] for r in pa)}/{len(pa)} = {100 * sum(r['pass'] for r in pa) / len(pa):.1f}%  ${pc:.4f}/task")

print("\n== per instance: Haiku alone | Opus alone | pipeline (c = slice covers the reference change, w = whole file)")
for i in ids:
    r = hold["large-hleanarch"][i]
    inf = r.get("info") or {}
    s = other["small"].get(i)
    print(f"   {'1' if s and s['pass'] else '0'}   {'1' if hold['large'][i]['pass'] else '0'}   {'1' if r['pass'] else '0'}{'c' if inf.get('covers_gold') else ''}{'w' if inf.get('whole_file') else ''}   {i}  ${hold['large'][i]['total']['cost_usd']:.3f} -> ${r['total']['cost_usd']:.3f}")
print("\nspend:", round(sum(r["total"]["cost_usd"] for a in hold for r in hold[a].values()), 2))
