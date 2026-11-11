"""Report on the instruction-only / lean-slice experiment.

usage: arch_report.py arch.jsonl slice.jsonl opus25.jsonl
"""
import collections
import json
import math
import statistics
import sys

runs = collections.defaultdict(dict)
for path in sys.argv[1:]:
    for line in open(path):
        r = json.loads(line)
        runs[r["arm"]][r["exercise"]] = r
ids = sorted(runs["large"])
n = len(ids)
IN, OUT = 4e-6, 20e-6  # Opus list price per token


def mcnemar(b, c):
    m = b + c
    if m == 0:
        return 1.0
    k = min(b, c)
    return min(1.0, 2 * sum(math.comb(m, i) for i in range(k + 1)) / 2 ** m)


base_locate = statistics.mean(r["usage"].get("locate", {}).get("cost_usd", 0) for r in runs["large-hslice"].values())
print(f"locate cost in the baseline run: ${base_locate:.4f}/task (added to arms that reused its names)\n")
print("arm              pass   $/task   Opus: in tok   $in     think tok  $think   visible tok  $visible   Haiku edit $   words")
for arm in ("large", "large-oslice", "large-hslice", "large-hlean", "large-harch", "large-hleanarch"):
    if arm not in runs:
        continue
    rs = [runs[arm][i] for i in ids if i in runs[arm]]
    k = len(rs)
    ok = sum(r["pass"] for r in rs)
    L = [r["usage"].get("large", {}) for r in rs]
    tin = sum(u.get("input_tokens", 0) for u in L) / k
    tout = sum(u.get("output_tokens", 0) for u in L) / k
    th = sum(u.get("thinking_tokens", 0) for u in L) / k
    edit = sum(r["usage"].get("edit", {}).get("cost_usd", 0) for r in rs) / k
    loc = sum(r["usage"].get("locate", {}).get("cost_usd", 0) for r in rs) / k
    total = sum(r["total"]["cost_usd"] for r in rs) / k
    reused = sum(1 for r in rs if (r.get("info") or {}).get("named_reused"))
    if reused:
        total += base_locate * reused / k
    words = [(r.get("info") or {}).get("instruction_words") for r in rs]
    words = [w for w in words if w]
    print(f"{arm:16s} {ok:2d}/{k}  {total:.4f}   {tin:8.0f}    {tin * IN:.4f}   {th:6.0f}    {th * OUT:.4f}    {tout - th:6.0f}      {(tout - th) * OUT:.4f}     {edit:.4f}        {statistics.mean(words) if words else 0:.0f}")

print("\n== paired against the same slices (large-hslice)")
for a in ("large-hlean", "large-harch", "large-hleanarch"):
    b = "large-hslice"
    if a in runs:
        both = [i for i in ids if i in runs[a] and i in runs[b]]
        oa = sum(runs[a][i]["pass"] and not runs[b][i]["pass"] for i in both)
        ob = sum(runs[b][i]["pass"] and not runs[a][i]["pass"] for i in both)
        same = sum(1 for i in both if (runs[a][i].get("info") or {}).get("named") == (runs[b][i].get("info") or {}).get("named"))
        print(f"  {a} vs {b}: {sum(runs[a][i]['pass'] for i in both)} vs {sum(runs[b][i]['pass'] for i in both)}; only {a} {oa}, only {b} {ob}; exact p = {mcnemar(oa, ob):.2f}; same named functions in {same} of {len(both)}")
for a, b in (("large-harch", "large-hleanarch"), ("large-hlean", "large-hleanarch"), ("large-hleanarch", "large")):
    if a in runs and b in runs:
        both = [i for i in ids if i in runs[a] and i in runs[b]]
        oa = sum(runs[a][i]["pass"] and not runs[b][i]["pass"] for i in both)
        ob = sum(runs[b][i]["pass"] and not runs[a][i]["pass"] for i in both)
        print(f"  {a} vs {b}: {sum(runs[a][i]['pass'] for i in both)} vs {sum(runs[b][i]['pass'] for i in both)}; only {a} {oa}, only {b} {ob}; exact p = {mcnemar(oa, ob):.2f}")

for arm in ("large-harch", "large-hleanarch"):
    if arm in runs:
        rs = list(runs[arm].values())
        print(f"\n{arm}: attempts by the editor {dict(collections.Counter(r.get('try', 0) for r in rs))}; model errors {sum(1 for r in rs for s in r['steps'] if 'model error' in s)}; edit failures {sum(1 for r in rs for s in r['steps'] if 'search block' in s or 'does not compile' in s)}")

print("\n== per instance")
arms = [a for a in ("large", "large-hslice", "large-hlean", "large-harch", "large-hleanarch") if a in runs]
print("  " + " ".join(f"{a:>15s}" for a in arms))
for i in ids:
    print("  " + " ".join(f"{('1' if runs[a][i]['pass'] else '0') if i in runs[a] else '-':>15s}" for a in arms) + "  " + i)
print("\nspend:", round(sum(r["total"]["cost_usd"] for a in ("large-hlean", "large-harch", "large-hleanarch") if a in runs for r in runs[a].values()), 2))
