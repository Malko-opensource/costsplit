"""Report on the fixed pipeline (v2) over all 45 instances.

usage: v2_report.py v2.jsonl opus25.jsonl holdout.jsonl arch25.jsonl slice25.jsonl
"""
import collections
import json
import math
import statistics
import sys

v2 = {}
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r["arm"] == "large-hleanarch2":
        v2[r["exercise"]] = r
opus, v1, base_locate = {}, {}, []
for path in sys.argv[2:]:
    for line in open(path):
        r = json.loads(line)
        if r["arm"] == "large":
            opus[r["exercise"]] = r
        if r["arm"] == "large-hleanarch":
            v1[r["exercise"]] = r
        if r["arm"] == "large-hslice" and "locate" in r["usage"]:
            base_locate.append(r["usage"]["locate"]["cost_usd"])
ids = sorted(v2)
n = len(ids)
loc = statistics.mean(base_locate) if base_locate else 0


def mcnemar(b, c):
    m = b + c
    if m == 0:
        return 1.0
    k = min(b, c)
    return min(1.0, 2 * sum(math.comb(m, i) for i in range(k + 1)) / 2 ** m)


def cost(r):
    c = r["total"]["cost_usd"]
    if (r.get("info") or {}).get("named_reused"):
        c += loc
    return c


print(f"== all {n} instances")
for name, d in (("Opus alone", opus), ("pipeline v1", v1), ("pipeline v2", v2)):
    rs = [d[i] for i in ids if i in d]
    cs = [cost(r) for r in rs]
    print(f"  {name:12s} n={len(rs)} pass {sum(r['pass'] for r in rs):2d} = {100 * sum(r['pass'] for r in rs) / len(rs):5.1f}%  mean ${statistics.mean(cs):.4f}  median ${statistics.median(cs):.4f}  total ${sum(cs):.2f}")
for a, da, b, db in (("v2", v2, "Opus alone", opus), ("v2", v2, "v1", v1), ("v1", v1, "Opus alone", opus)):
    both = [i for i in ids if i in da and i in db]
    oa = sum(da[i]["pass"] and not db[i]["pass"] for i in both)
    ob = sum(db[i]["pass"] and not da[i]["pass"] for i in both)
    print(f"  {a} vs {b}: only {a} {oa}, only {b} {ob}; exact p = {mcnemar(oa, ob):.2f}")

rs = [v2[i] for i in ids]
roles = collections.Counter()
for r in rs:
    for k, u in r["usage"].items():
        roles[k] += u["cost_usd"] / n
L = [r["usage"].get("large", {}) for r in rs]
tin = sum(u.get("input_tokens", 0) for u in L) / n
tout = sum(u.get("output_tokens", 0) for u in L) / n
th = sum(u.get("thinking_tokens", 0) for u in L) / n
calls = sum(u.get("calls", 0) for u in L) / n
print(f"\n== v2 cost per task by role: {dict((k, round(v, 4)) for k, v in roles.items())}")
print(f"  Opus: {calls:.2f} calls, in {tin:.0f} tok (${tin * 4e-6:.4f}), thinking {th:.0f} (${th * 20e-6:.4f}), visible {tout - th:.0f} (${(tout - th) * 20e-6:.4f})")
inf = [(r, r.get("info") or {}) for r in rs]
whole = [r for r, i in inf if i.get("whole_file")]
els = [(r, i) for r, i in inf if i.get("elsewhere")]
print(f"  whole-file fallbacks: {len(whole)}; model errors: {sum(1 for r in rs for s in r['steps'] if 'model error' in s)}; edit failures: {sum(1 for r in rs for s in r['steps'] if 'search block' in s or 'does not compile' in s)}")
print(f"  Opus said the fix belongs elsewhere: {len(els)}; of those pass {sum(r['pass'] for r, _ in els)}; Opus alone on the same {sum(opus[r['exercise']]['pass'] for r, _ in els)}; v1 on the same {sum(v1[r['exercise']]['pass'] for r, _ in els if r['exercise'] in v1)}")
for r, i in els:
    print(f"     {r['exercise']}: pass {r['pass']}, asked: {str(i.get('elsewhere'))[:110]!r} -> {i.get('named_more')}, covers now {i.get('covers_gold')}")
cov = [r for r, i in inf if i.get("covers_gold")]
nc = [r for r, i in inf if not i.get("covers_gold")]
print(f"  final slice contains the reference change: {len(cov)} of {n}; pipeline pass {sum(r['pass'] for r in cov)}, Opus alone {sum(opus[r['exercise']]['pass'] for r in cov)}")
print(f"  does not: {len(nc)}; pipeline pass {sum(r['pass'] for r in nc)}, Opus alone {sum(opus[r['exercise']]['pass'] for r in nc)}")
sl = [i for _, i in inf if not i.get("whole_file") and i.get("slice_lines")]
print(f"  slice size: median {statistics.median(i['slice_lines'] for i in sl):.0f} lines of {statistics.median(i['file_lines'] for i in sl):.0f}")
print("  costliest five:")
for r in sorted(rs, key=lambda r: -r["total"]["cost_usd"])[:5]:
    u = r["usage"].get("large", {})
    print(f"     {r['exercise']} ${r['total']['cost_usd']:.3f} (Opus alone ${opus[r['exercise']]['total']['cost_usd']:.3f}): Opus calls {u.get('calls')}, in {u.get('input_tokens')}, thinking {u.get('thinking_tokens', 0)}, slice {(r.get('info') or {}).get('slice_lines')} lines, pass {r['pass']}")

print("\n== per instance: Opus alone | v1 | v2 (c covers, e asked for other code, w whole file)")
for i in ids:
    r = v2[i]
    f = r.get("info") or {}
    flags = ("c" if f.get("covers_gold") else "") + ("e" if f.get("elsewhere") else "") + ("w" if f.get("whole_file") else "")
    print(f"   {'1' if opus[i]['pass'] else '0'}   {('1' if v1[i]['pass'] else '0') if i in v1 else '-'}   {'1' if r['pass'] else '0'}{flags:3s} {i}  ${opus[i]['total']['cost_usd']:.3f} -> ${r['total']['cost_usd']:.3f}")
print("\nspend:", round(sum(r["total"]["cost_usd"] for r in rs), 2))
