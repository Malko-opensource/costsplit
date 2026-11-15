"""Fault injection with a check that needs the code: the answer must copy one
line the fix changes. A line the model was never shown is a guess (or
memory of the repository), whether or not it happens to exist.

usage: quote_report.py recover.jsonl quote.jsonl   (later files win: quote last)
"""
import collections
import json
import statistics
import sys

by = collections.defaultdict(dict)
for p in sys.argv[1:]:
    for l in open(p):
        r = json.loads(l)
        if not r.get("error"):
            by[r["arm"]][r["id"]] = r
ids = sorted(set(by["cola"]) & set(by["inject"]) & set(by["full"]))
n = len(ids)
print(f"== {n} tasks")
for a in ("cola", "inject"):
    rs = [by[a][i] for i in ids]
    asked = [r for r in rs if r.get("asked")]
    unseen = [r for r in rs if r.get("quote") and not r.get("quote_seen")]
    print(f"  {a:6s} asked to see code {len(asked):2d}/{n}"
          + (f" (asked for a hidden answer {sum(r.get('asked_injected', False) for r in rs)})" if a == "inject" else "")
          + f"; quoted line exists {sum(r.get('quote_in_file', False) for r in rs)}, was shown {sum(r.get('quote_seen', False) for r in rs)}, "
          f"in gold {sum(r.get('quote_in_gold', False) for r in rs)}; quoted code it was not shown {len(unseen)} "
          f"(of which the line exists {sum(r.get('quote_in_file', False) for r in unseen)})")
    print(f"  {'':6s} names every gold function {sum(r['hit_all'] for r in rs)}/{n}; same answer as full read {sum(set(r['fix_in']) == set(by['full'][r['id']]['fix_in']) for r in rs)}/{n}; "
          f"cost ${statistics.mean(r['large_usd'] + r['small_usd'] for r in rs):.4f}/task, large-model calls {statistics.mean(r['rounds'] for r in rs):.2f}")
inj = [by["inject"][i] for i in ids]
noask = [r for r in inj if not r.get("asked")]
print(f"\n  inject, did not ask ({len(noask)}): answer still the full read's {sum(set(r['fix_in']) == set(r['injected']) for r in noask)}; "
      f"quoted an unseen line {sum(bool(r.get('quote')) and not r.get('quote_seen') for r in noask)} (exists {sum(r.get('quote_in_file', False) and not r.get('quote_seen') for r in noask)})")
ask = [r for r in inj if r.get("asked")]
print(f"  inject, asked ({len(ask)}): for the hidden answer {sum(r.get('asked_injected', False) for r in ask)}; then quoted a shown line {sum(r.get('quote_seen', False) for r in ask)}")
print("\nspend:", round(sum(r["large_usd"] + r["small_usd"] for a in ("cola", "inject") for r in by[a].values()), 2))
