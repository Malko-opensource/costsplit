"""Report on the test-author experiment.

usage: author_report.py author.jsonl haiku45.jsonl opus25.jsonl
"""
import collections
import json
import math
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
for arm in ("small", "small-tdd", "small-stest", "small-ltest", "small-oracle", "large"):
    if arm not in runs:
        continue
    rs = [runs[arm][i] for i in ids if i in runs[arm]]
    ok = sum(r["pass"] for r in rs)
    cost = sum(r["total"]["cost_usd"] for r in rs) / len(rs)
    roles = collections.Counter()
    for r in rs:
        for k, u in r["usage"].items():
            roles[k] += u["cost_usd"] / len(rs)
    errs = sum(1 for r in rs for s in (r.get("steps") or []) if "model error" in s)
    print(f"  {arm:13s} n={len(rs)} pass {ok:2d} = {100 * ok / len(rs):5.1f}%  ${cost:.4f}/task  {dict((k, round(v, 4)) for k, v in roles.items())}  model errors {errs}")

for arm in ("small-stest", "small-ltest"):
    if arm not in runs:
        continue
    rs = [runs[arm][i] for i in ids if i in runs[arm]]
    red = [r for r in rs if r.get("red")]
    gg = [r for r in red if r.get("gold_green")]
    green = [r for r in rs if r.get("own_green")]
    rg = [r for r in rs if r.get("red") and r.get("own_green")]
    print(f"\n== {arm}: the contract")
    print(f"  test fails on the unfixed code (red): {len(red)} of {len(rs)}")
    print(f"  of the red tests, the reference fix passes: {len(gg)}  (asks for something else: {len(red) - len(gg)})")
    print(f"  red, then verifier green: {len(rg)}; hidden tests pass: {sum(r['pass'] for r in rg)}; false pass: {len(rg) - sum(r['pass'] for r in rg)}")
    for name, sel in (("contract agrees with the reference fix", lambda r: r.get("red") and r.get("gold_green")),
                      ("contract disagrees with the reference fix", lambda r: r.get("red") and not r.get("gold_green")),
                      ("no red test", lambda r: not r.get("red"))):
        g = [r for r in rs if sel(r)]
        print(f"  {name}: {len(g)}; own verifier green {sum(bool(r.get('own_green')) for r in g)}; hidden pass {sum(r['pass'] for r in g)}")
    au = [r["usage"]["author"] for r in rs if "author" in r["usage"]]
    if au:
        print(f"  author: {sum(u['calls'] for u in au) / len(au):.2f} calls, in {sum(u['input_tokens'] for u in au) / len(au):.0f}, out {sum(u['output_tokens'] for u in au) / len(au):.0f} tokens, ${sum(u['cost_usd'] for u in au) / len(au):.4f}/task")
    print("  fix attempts used:", dict(collections.Counter(r.get("try", 0) for r in rs)))

print("\n== paired")
for a, b in (("small-ltest", "large"), ("small-ltest", "small"), ("small-ltest", "small-stest"), ("small-stest", "small"), ("small-ltest", "small-oracle")):
    if a in runs and b in runs:
        both = [i for i in ids if i in runs[a] and i in runs[b]]
        oa = sum(runs[a][i]["pass"] and not runs[b][i]["pass"] for i in both)
        ob = sum(runs[b][i]["pass"] and not runs[a][i]["pass"] for i in both)
        print(f"  {a} vs {b}: {sum(runs[a][i]['pass'] for i in both)} vs {sum(runs[b][i]['pass'] for i in both)}; only {a} {oa}, only {b} {ob}; exact p = {mcnemar(oa, ob):.2f}")

L = runs["large"]


def policy(name, first, escalate):
    ok = cost = esc = 0
    for i in ids:
        f = runs[first][i]
        c, p = f["total"]["cost_usd"], f["pass"]
        if escalate(f):
            esc += 1
            c += L[i]["total"]["cost_usd"]
            p = L[i]["pass"]
        ok += p
        cost += c
    print(f"  {name:62s} pass {ok:2d}/{n} = {100 * ok / n:5.1f}%  ${cost / n:.4f}/task  Opus fixes {esc}")


print("\n== policies")
policy("Opus alone", "large", lambda f: False)
for arm in ("small-stest", "small-ltest"):
    if arm in runs:
        policy(f"{arm} alone", arm, lambda f: False)
        policy(f"{arm}; Opus fixes when no red test or verifier never green", arm, lambda f: not (f.get("red") and f.get("own_green")))
        policy(f"{arm}; Opus fixes when hidden tests fail (perfect verifier)", arm, lambda f: not f["pass"])

print("\n== per instance: pass, r = red, c = contract agrees with reference fix, g = own verifier green")
arms = [a for a in ("small", "small-stest", "small-ltest", "small-oracle", "large") if a in runs]
print("  " + " ".join(f"{a:>12s}" for a in arms))
for i in ids:
    cells = []
    for a in arms:
        r = runs[a].get(i)
        cells.append("-" if r is None else ("1" if r["pass"] else "0") + ("r" if r.get("red") else "") + ("c" if r.get("gold_green") else "") + ("g" if r.get("own_green") else ""))
    print("  " + " ".join(f"{c:>12s}" for c in cells) + "  " + i)
