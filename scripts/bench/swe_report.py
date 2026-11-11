"""Report on the SWE-bench contract experiment.

usage: swe_report.py haiku.jsonl [opus.jsonl]
"""
import collections
import json
import math
import sys

runs = collections.defaultdict(dict)  # arm -> id -> result
for path in sys.argv[1:]:
    for line in open(path):
        r = json.loads(line)
        runs[r["arm"]][r["exercise"]] = r


def mcnemar(b, c):
    n = b + c
    if n == 0:
        return 1.0
    k = min(b, c)
    p = sum(math.comb(n, i) for i in range(k + 1)) / 2 ** n
    return min(1.0, 2 * p)


def line(arm, ids=None):
    rs = [r for i, r in runs[arm].items() if ids is None or i in ids]
    n = len(rs)
    if not n:
        return
    ok = sum(r["pass"] for r in rs)
    cost = sum(r["total"]["cost_usd"] for r in rs) / n
    tin = sum(r["total"]["input_tokens"] for r in rs) / n
    tout = sum(r["total"]["output_tokens"] for r in rs) / n
    calls = sum(r["total"]["calls"] for r in rs) / n
    sec = sum(r["seconds"] for r in rs) / n
    errs = sum(1 for r in rs for s in (r.get("steps") or []) if "model error" in s)
    print(f"  {arm:13s} n={n:2d} pass {ok:2d} = {100 * ok / n:5.1f}%  ${cost:.4f}/task  calls {calls:.1f}  in {tin / 1000:5.1f}k out {tout / 1000:4.1f}k  {sec:4.0f}s  model errors {errs}")


def paired(a, b, ids=None):
    both = [i for i in runs[a] if i in runs[b] and (ids is None or i in ids)]
    pa = {i: runs[a][i]["pass"] for i in both}
    pb = {i: runs[b][i]["pass"] for i in both}
    oa = sum(pa[i] and not pb[i] for i in both)
    ob = sum(pb[i] and not pa[i] for i in both)
    print(f"  {a} vs {b} (n={len(both)}): {sum(pa.values())} vs {sum(pb.values())}; only {a} {oa}, only {b} {ob}; exact p = {mcnemar(oa, ob):.2f}")


print("== all instances")
for arm in ("small", "small-ctx", "small-tdd", "small-oracle", "large"):
    if arm in runs:
        line(arm)
print("\n== paired, Haiku arms")
for b in ("small-ctx", "small-tdd", "small-oracle"):
    if b in runs and "small" in runs:
        paired("small", b)
if "small-tdd" in runs and "small-oracle" in runs:
    paired("small-tdd", "small-oracle")

if "small-tdd" in runs:
    rs = list(runs["small-tdd"].values())
    red = [r for r in rs if r.get("red")]
    green = [r for r in rs if r.get("own_green")]
    print("\n== small-tdd: the model's own contract")
    print(f"  wrote a test that fails on the unfixed code: {len(red)} of {len(rs)}")
    print(f"  own verifier passed: {len(green)} of {len(rs)}; of those, hidden tests pass: {sum(r['pass'] for r in green)}  (false pass {len(green) - sum(r['pass'] for r in green)})")
    ng = [r for r in rs if not r.get("own_green")]
    print(f"  own verifier never passed: {len(ng)}; of those, hidden tests pass anyway: {sum(r['pass'] for r in ng)}")
    rg = [r for r in rs if r.get("red") and r.get("own_green")]
    print(f"  red and then green: {len(rg)}; hidden pass {sum(r['pass'] for r in rg)}")
    nr = [r for r in rs if not r.get("red")]
    print(f"  no red test: {len(nr)}; hidden pass {sum(r['pass'] for r in nr)}")
    print("  fix attempts used:", dict(collections.Counter(r.get("try", 0) for r in rs)))
if "small-oracle" in runs:
    rs = list(runs["small-oracle"].values())
    print("\n== small-oracle: fix attempts used when passing:", dict(collections.Counter(r.get("try", 0) for r in rs if r["pass"])), "failed:", sum(not r["pass"] for r in rs))

if "large" in runs:
    ids = set(runs["large"])
    print(f"\n== the {len(ids)} instances Opus also ran")
    for arm in ("small", "small-ctx", "small-tdd", "small-oracle", "large"):
        if arm in runs:
            line(arm, ids)
    for a in ("small", "small-ctx", "small-tdd", "small-oracle"):
        if a in runs:
            paired(a, "large", ids)
    L = runs["large"]

    def policy(name, first, escalate):
        ok = cost = n_esc = 0
        for i in ids:
            f = runs[first][i]
            c = f["total"]["cost_usd"]
            p = f["pass"]
            if escalate(f):
                n_esc += 1
                c += L[i]["total"]["cost_usd"]
                p = L[i]["pass"]
            ok += p
            cost += c
        print(f"  {name:58s} pass {ok:2d}/{len(ids)} = {100 * ok / len(ids):5.1f}%  ${cost / len(ids):.4f}/task  Opus runs {n_esc}")

    print("\n== policies on those instances")
    policy("Opus alone", "large", lambda f: False)
    for a in ("small", "small-ctx", "small-tdd"):
        if a in runs:
            policy(f"{a} alone", a, lambda f: False)
    if "small-tdd" in runs:
        policy("small-tdd; Opus when its own verifier never passed", "small-tdd", lambda f: not f.get("own_green"))
        policy("small-tdd; Opus when no red test or verifier never passed", "small-tdd", lambda f: not (f.get("own_green") and f.get("red")))
    for a in ("small", "small-tdd"):
        if a in runs:
            policy(f"{a}; Opus when the hidden tests fail (perfect verifier)", a, lambda f: not f["pass"])

print("\n== per instance (1 = pass)")
arms = [a for a in ("small", "small-ctx", "small-tdd", "small-oracle", "large") if a in runs]
print("  " + " ".join(f"{a:>12s}" for a in arms))
for i in sorted(set().union(*[set(runs[a]) for a in arms])):
    cells = []
    for a in arms:
        r = runs[a].get(i)
        cells.append("-" if r is None else ("1" if r["pass"] else "0") + ("g" if r.get("own_green") else "") + ("r" if r.get("red") else ""))
    print("  " + " ".join(f"{c:>12s}" for c in cells) + "  " + i)
