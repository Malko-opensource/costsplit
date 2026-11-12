"""Stage 1 of diet-cola: does what the task needs survive the cut?

usage: cola_report.py results.jsonl [more.jsonl ...]

For each track and ranker: the share of tasks whose every gold block is kept
at each budget (95% Wilson interval), mean recall of gold blocks, and the
median share of tokens read in rank order before the last gold block is in.
A task with several files counts as covered only when every file is.
"""
import collections
import json
import math
import statistics
import sys


def wilson(k, n, z=1.96):
    if n == 0:
        return 0, 0
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return c - h, c + h


rows = [json.loads(l) for p in sys.argv[1:] for l in open(p)]
errors = [r for r in rows if r.get("error")]
rows = [r for r in rows if not r.get("error")]
order = ["head", "random", "lexical", "haiku", "haiku-wide", "haiku-x3", "opus-outline"]
for track in sorted({r["track"] for r in rows}):
    tr = [r for r in rows if r["track"] == track]
    shares = sorted(tr[0]["covered"], key=float)
    tasks = sorted({r["id"] for r in tr})
    print(f"== {track}: {len(tasks)} tasks, median {statistics.median({(r['id'], r.get('file')): r['tokens'] for r in tr}.values()):.0f} tokens per observation")
    print("   share of tasks with every gold block kept, by budget (share of the observation's tokens kept)")
    print(f"   {"ranker":10s} " + " ".join(f"{float(s):>14.0%}" for s in shares) + "   need(median)  recall@" + shares[len(shares) // 2])
    for rk in [o for o in order if any(r["ranker"] == o for r in tr)]:
        by = collections.defaultdict(list)
        for r in tr:
            if r["ranker"] == rk:
                by[r["id"]].append(r)
        n = len(by)
        cells = []
        for s in shares:
            k = sum(all(r["covered"][s] for r in rs) for rs in by.values())
            lo, hi = wilson(k, n)
            cells.append(f"{k / n:5.0%} [{lo:3.0%}-{hi:3.0%}]")
        need = statistics.median(max(r["need_frac"] for r in rs) for rs in by.values())
        rec = statistics.mean(r["recall"][shares[len(shares) // 2]] for rs in by.values() for r in rs)
        print(f"   {rk:10s} " + " ".join(f"{c:>14s}" for c in cells) + f"   {need:11.2f}  {rec:.2f}" + (f"   (n={n})" if n != len(tasks) else ""))
    for name in [o for o in order if o.startswith(("haiku", "opus"))]:
        hk = [r for r in tr if r["ranker"] == name]
        if not hk:
            continue
        cov = sum(r.get("picks_cover", False) for r in hk)
        print(f"   {name} own picks: {statistics.mean(r.get("picks", 0) for r in hk):.1f} blocks, median {statistics.median(r.get("picks_frac", 0) for r in hk):.0%} of the tokens; "
              f"hold every gold block in {cov}/{len(hk)} = {cov / len(hk):.0%}; outline only {sum(r.get('outline', False) for r in hk)}; "
              f"cost ${sum(r['cost_usd'] for r in hk):.3f} total, ${statistics.mean(r['cost_usd'] for r in hk):.4f} each")
    print()
if errors:
    print(f"errors: {len(errors)}", collections.Counter((r["track"], r["ranker"]) for r in errors))
    for r in errors[:5]:
        print("  ", r["id"], r["error"][:150])
