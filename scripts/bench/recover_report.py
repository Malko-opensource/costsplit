"""Stage 3 of diet-cola: when the cut hides what a fix needs, does the large
model notice and ask for it, and does its answer match a full read?

usage: recover_report.py recover.jsonl
"""
import collections
import json
import math
import statistics
import sys

rows = [json.loads(l) for l in open(sys.argv[1])]
errs = [r for r in rows if r.get("error")]
by = collections.defaultdict(dict)
for r in rows:
    if not r.get("error"):
        by[r["arm"]][r["id"]] = r
arms = [a for a in ("full", "full2", "cola") if a in by]
ids = sorted(set.intersection(*(set(by[a]) for a in arms)))
n = len(ids)


def mcnemar(b, c):
    m = b + c
    if m == 0:
        return 1.0
    return min(1.0, 2 * sum(math.comb(m, i) for i in range(min(b, c) + 1)) / 2 ** m)


def cost(r):
    return r["large_usd"] + r["small_usd"]


print(f"== {n} tasks with every arm ({len(errs)} errors)")
for a in arms:
    rs = [by[a][i] for i in ids]
    print(f"  {a:6s} names every gold function {sum(r['hit_all'] for r in rs):2d}/{n}, at least one {sum(r['hit_any'] for r in rs):2d}/{n}; "
          f"cost mean ${statistics.mean(cost(r) for r in rs):.4f} (large ${statistics.mean(r['large_usd'] for r in rs):.4f}); "
          f"large input {statistics.mean(r['large_in'] for r in rs):.0f} tok; calls {statistics.mean(r['rounds'] for r in rs):.2f}")


def same(a, b):
    return set(a["fix_in"]) == set(b["fix_in"])


def overlap(a, b):
    return bool(set(a["fix_in"]) & set(b["fix_in"]))


print("\n== agreement of the answer (same set of functions | sets overlap)")
for a, b in (("full", "full2"), ("full", "cola"), ("full2", "cola")):
    if a in by and b in by:
        print(f"  {a:5s} vs {b:5s}: same {sum(same(by[a][i], by[b][i]) for i in ids):2d}/{n}, overlap {sum(overlap(by[a][i], by[b][i]) for i in ids):2d}/{n}")
if "full2" in by:
    # cola agrees with at least one of two full reads: the fair comparison
    # when two full reads already disagree.
    either = sum(overlap(by["cola"][i], by["full"][i]) or overlap(by["cola"][i], by["full2"][i]) for i in ids)
    print(f"  cola overlaps one of the two full reads: {either}/{n}")

for ref in ("full", "full2"):
    if ref in by:
        b = sum(by["cola"][i]["hit_all"] and not by[ref][i]["hit_all"] for i in ids)
        c = sum(by[ref][i]["hit_all"] and not by["cola"][i]["hit_all"] for i in ids)
        print(f"  names every gold function: only cola {b}, only {ref} {c}; exact p = {mcnemar(b, c):.2f}")

c = [by["cola"][i] for i in ids]
cut = [r for r in c if not r["passed"]]
print(f"\n== cola: {len(c) - len(cut)} small files shown whole, {len(cut)} cut")
hb = [r for r in cut if not r["visible_before"]]
vb = [r for r in cut if r["visible_before"]]
print(f"  gold hidden at first: {len(hb)}; asked to see something {sum(bool(r['asked']) for r in hb)}; gold visible after asking {sum(r['visible_after'] for r in hb)}; answer names gold {sum(r['hit_all'] for r in hb)} (full read: {sum(by['full'][r['id']]['hit_all'] for r in hb)})")
print(f"  gold visible at first: {len(vb)}; still asked {sum(bool(r['asked']) for r in vb)}; answer names gold {sum(r['hit_all'] for r in vb)} (full read: {sum(by['full'][r['id']]['hit_all'] for r in vb)})")
first = [r["view_tokens"][0] / r["tokens"] for r in cut]
last = [r["view_tokens"][-1] / r["tokens"] for r in cut]
print(f"  view size vs file (estimated tokens): first median {statistics.median(first):.0%}, last median {statistics.median(last):.0%}")
fc = sum(cost(by["full"][i]) for i in ids)
cc = sum(cost(by["cola"][i]) for i in ids)
fc_cut = sum(cost(by["full"][r["id"]]) for r in cut)
cc_cut = sum(cost(r) for r in cut)
print(f"  cost: cola ${cc:.2f} vs full ${fc:.2f} ({1 - cc / fc:.0%} less); on cut files only ${cc_cut:.2f} vs ${fc_cut:.2f} ({1 - cc_cut / fc_cut:.0%} less)")
print(f"  large-model input: cola {sum(r['large_in'] for r in c)} vs full {sum(by['full'][i]['large_in'] for i in ids)} tokens")
print("\n  hidden at first, per task: asked -> visible after, hit (full hit)")
for r in hb:
    print(f"    {r['id']}: asked {r['asked']} -> {r['visible_after']}, {r['hit_all']} ({by['full'][r['id']]['hit_all']}); gold {r['gold']}; said {r['fix_in']}")
print("\nspend:", round(sum(cost(r) for r in rows), 2))
