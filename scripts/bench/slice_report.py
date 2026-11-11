"""Report on the slicing experiment.

usage: slice_report.py slice.jsonl haiku45.jsonl opus25.jsonl
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


def mcnemar(b, c):
    m = b + c
    if m == 0:
        return 1.0
    k = min(b, c)
    return min(1.0, 2 * sum(math.comb(m, i) for i in range(k + 1)) / 2 ** m)


print(f"== the {n} instances")
for arm in ("small", "small-oslice", "small-hslice", "large", "large-oslice", "large-hslice"):
    if arm not in runs:
        continue
    rs = [runs[arm][i] for i in ids if i in runs[arm]]
    ok = sum(r["pass"] for r in rs)
    cost = sum(r["total"]["cost_usd"] for r in rs) / len(rs)
    roles = collections.Counter()
    tin = collections.Counter()
    tout = collections.Counter()
    for r in rs:
        for k, u in r["usage"].items():
            roles[k] += u["cost_usd"] / len(rs)
            tin[k] += u["input_tokens"] / len(rs)
            tout[k] += u["output_tokens"] / len(rs)
    errs = sum(1 for r in rs for s in (r.get("steps") or []) if "model error" in s)
    print(f"  {arm:13s} n={len(rs)} pass {ok:2d} = {100 * ok / len(rs):5.1f}%  ${cost:.4f}/task  cost {dict((k, round(v, 4)) for k, v in roles.items())}  in {dict((k, round(v)) for k, v in tin.items())}  out {dict((k, round(v)) for k, v in tout.items())}  errors {errs}")

for arm in ("large-oslice", "large-hslice", "small-oslice", "small-hslice"):
    if arm not in runs:
        continue
    rs = [runs[arm][i] for i in ids if i in runs[arm]]
    inf = [r.get("info") or {} for r in rs]
    sl = [i for i in inf if not i.get("whole_file")]
    ratio = [i["slice_lines"] / i["file_lines"] for i in sl if i.get("file_lines")]
    cov = [r for r in rs if (r.get("info") or {}).get("covers_gold")]
    ncov = [r for r in rs if not (r.get("info") or {}).get("covers_gold")]
    print(f"\n== {arm}")
    print(f"  slice shown instead of the file: {len(sl)} of {len(rs)} (whole-file fallback {len(rs) - len(sl)})")
    if ratio:
        print(f"  slice size: median {statistics.median(i['slice_lines'] for i in sl):.0f} lines of median {statistics.median(i['file_lines'] for i in sl):.0f}; median share {100 * statistics.median(ratio):.0f}%")
    print(f"  slice contains the lines the reference patch changes: {len(cov)} of {len(rs)}; pass {sum(r['pass'] for r in cov)} of {len(cov)}")
    print(f"  slice misses them: {len(ncov)}; pass {sum(r['pass'] for r in ncov)} of {len(ncov)}")
    print("  attempts used:", dict(collections.Counter(r.get("try", 0) for r in rs)))

print("\n== paired")
for a, b in (("large-oslice", "large"), ("large-hslice", "large"), ("large-hslice", "large-oslice"), ("small-oslice", "small"), ("small-hslice", "small"), ("small-oslice", "large")):
    if a in runs and b in runs:
        both = [i for i in ids if i in runs[a] and i in runs[b]]
        oa = sum(runs[a][i]["pass"] and not runs[b][i]["pass"] for i in both)
        ob = sum(runs[b][i]["pass"] and not runs[a][i]["pass"] for i in both)
        print(f"  {a} vs {b}: {sum(runs[a][i]['pass'] for i in both)} vs {sum(runs[b][i]['pass'] for i in both)}; only {a} {oa}, only {b} {ob}; exact p = {mcnemar(oa, ob):.2f}")

print("\n== per instance: pass (c = slice covers the reference change, w = whole-file fallback)")
arms = [a for a in ("small", "small-oslice", "small-hslice", "large", "large-oslice", "large-hslice") if a in runs]
print("  " + " ".join(f"{a:>12s}" for a in arms))
for i in ids:
    cells = []
    for a in arms:
        r = runs[a].get(i)
        if r is None:
            cells.append("-")
            continue
        inf = r.get("info") or {}
        cells.append(("1" if r["pass"] else "0") + ("c" if inf.get("covers_gold") else "") + ("w" if inf.get("whole_file") else ""))
    print("  " + " ".join(f"{c:>12s}" for c in cells) + "  " + i)
print("\nspend:", round(sum(r["total"]["cost_usd"] for a in ("large-oslice", "large-hslice", "small-oslice", "small-hslice") if a in runs for r in runs[a].values()), 2))
