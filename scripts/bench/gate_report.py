"""The quote gate: does sending back an answer that quotes unseen code close
the gap fault injection found?

usage: gate_report.py recover.jsonl quote.jsonl gate.jsonl
  recover.jsonl  holds the full read; quote.jsonl the inject arm without the
  gate; gate.jsonl the inject arm with it.
"""
import json
import statistics
import sys


def load(p, arm):
    return {r["id"]: r for r in map(json.loads, open(p)) if r["arm"] == arm and not r.get("error")}


full = load(sys.argv[1], "full")
arms = {"inject": load(sys.argv[2], "inject"), "inject+gate": load(sys.argv[3], "inject")}
ids = sorted(set(full).intersection(*arms.values()))
n = len(ids)
print(f"== {n} tasks; the functions a full read named are hidden")
for name, d in arms.items():
    rs = [d[i] for i in ids]
    unseen = sum(bool(r.get("quote")) and not r.get("quote_seen") for r in rs)
    print(f"  {name:12s} final answer quotes unseen code {unseen:2d}/{n}; quote in a gold block {sum(r.get('quote_in_gold', False) for r in rs):2d}; "
          f"names every gold function {sum(r['hit_all'] for r in rs):2d}; same answer as full read {sum(set(r['fix_in']) == set(full[r['id']]['fix_in']) for r in rs):2d}; "
          f"asked by itself {sum(bool(r.get('asked')) for r in rs):2d}; gate fired {sum(r.get('gate_fired', 0) > 0 for r in rs):2d}; "
          f"cost ${statistics.mean(r['large_usd'] + r['small_usd'] for r in rs):.4f}/task, calls {statistics.mean(r['rounds'] for r in rs):.2f}")
fr = [full[i] for i in ids]
print(f"  {'full read':12s} names every gold function {sum(r['hit_all'] for r in fr):2d}; cost ${statistics.mean(r['large_usd'] for r in fr):.4f}/task")
g = arms["inject+gate"]
for i in ids:
    r = g[i]
    if r.get("gate_fired"):
        print(f"    gate {r['id']}: showed {r.get('gate_shown')} -> quote seen {r.get('quote_seen', False)}, in gold {r.get('quote_in_gold', False)}")
print("\nspend (gate run):", round(sum(r["large_usd"] + r["small_usd"] for r in g.values()), 2))
