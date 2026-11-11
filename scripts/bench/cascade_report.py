"""Report on the cascade experiment.

usage: cascade_report.py cascade.jsonl full.jsonl
"""
import collections
import json
import sys

casc_path, full_path = sys.argv[1], sys.argv[2]
full = collections.defaultdict(dict)
for line in open(full_path):
    r = json.loads(line)
    full[(r["lang"], r["exercise"])][r["arm"]] = r
N = len(full)
runs = collections.defaultdict(dict)  # arm -> task -> result
for line in open(casc_path):
    r = json.loads(line)
    runs[r["arm"]][(r["lang"], r["exercise"])] = r


def small_tries(arm):
    return int(arm[1])


def cum(arm):
    """Passes by overall attempt number."""
    c = collections.Counter()
    for r in runs[arm].values():
        if r["pass"]:
            c[r["try"]] += 1
    out, s = [], 0
    for k in range(1, max(c, default=0) + 1):
        s += c[k]
        out.append(s)
    return out


print("tasks per arm:", {a: len(v) for a, v in runs.items()})
for arm in sorted(runs):
    rs = list(runs[arm].values())
    n = len(rs)
    errs = sum(1 for r in rs for s in r["steps"] if "model error" in s)
    by_stage = collections.Counter(r.get("stage", "-") for r in rs if r["pass"])
    cost = collections.Counter()
    calls = collections.Counter()
    think = out_tok = 0
    for r in rs:
        for role, u in r["usage"].items():
            cost[role] += u["cost_usd"]
            calls[role] += u["calls"]
            if role == "small":
                think += u.get("thinking_tokens", 0)
                out_tok += u["output_tokens"]
    print(f"\n== {arm}: n={n} pass={sum(r['pass'] for r in rs)} by stage={dict(by_stage)} model errors={errs}")
    print("   cumulative passes by attempt:", cum(arm))
    print("   cost/task by role:", {k: round(v / n, 4) for k, v in cost.items()}, "calls:", dict(calls))
    if calls["small"]:
        print(f"   small: ${cost['small'] / calls['small']:.4f} per call, thinking share of output {think / max(out_tok, 1):.0%}")
    if calls.get("hint"):
        reached = [r for r in rs if "hint" in r["usage"]]
        won = [r for r in reached if r["pass"] and r.get("stage") == "hint"]
        hint_cost = sum(r["usage"]["hint"]["cost_usd"] for r in reached) / len(reached)
        hint_out = sum(r["usage"]["hint"]["output_tokens"] for r in reached) / len(reached)
        hint_in = sum(r["usage"]["hint"]["input_tokens"] for r in reached) / len(reached)
        opus = [full[k]["large"] for k, r in runs[arm].items() if "hint" in r["usage"]]
        print(f"   hint stage: reached {len(reached)}, solved {len(won)} ({len(won) / len(reached):.0%})")
        print(f"   hint call: ${hint_cost:.4f}, in {hint_in:.0f}, out {hint_out:.0f} tokens")
        print(f"   Opus direct on the same {len(opus)} tasks (full run): pass {sum(o['pass'] for o in opus)}, ${sum(o['total']['cost_usd'] for o in opus) / len(opus):.4f}/task, out {sum(o['total']['output_tokens'] for o in opus) / len(opus):.0f} tokens")

# plain vs pseudocode over the first five small tries, paired by task
if "c5h2" in runs and "c5p" in runs:
    both = sorted(set(runs["c5h2"]) & set(runs["c5p"]))
    plain = {k: runs["c5h2"][k]["pass"] and runs["c5h2"][k].get("stage") == "small" for k in both}
    plan = {k: runs["c5p"][k]["pass"] for k in both}
    print("\n== five small tries: plain vs pseudocode first (paired)")
    print("   plain", sum(plain.values()), "pseudocode", sum(plan.values()), "of", len(both))
    print("   both", sum(plain[k] and plan[k] for k in both), "only plain", sum(plain[k] and not plan[k] for k in both),
          "only pseudocode", sum(plan[k] and not plain[k] for k in both), "neither", sum(not plain[k] and not plan[k] for k in both))
    for t in range(1, 6):
        a = sum(1 for k in both if plain[k] and runs["c5h2"][k]["try"] <= t)
        b = sum(1 for k in both if plan[k] and runs["c5p"][k]["try"] <= t)
        print(f"   by try {t}: plain {a}, pseudocode {b}")

# whole-benchmark policies: the original two small tries stay as stage 1 for all tasks
base_small = sum(a["small"]["total"]["cost_usd"] for a in full.values())
base_pass = sum(a["small"]["pass"] for a in full.values())
print(f"\n== whole benchmark ({N} tasks), measured basis")
print(f"   Haiku alone (2 tries):        pass {base_pass}/{N} = {100 * base_pass / N:.1f}%  ${base_small / N:.4f}/task")
lg = sum(a["large"]["total"]["cost_usd"] for a in full.values())
print(f"   Opus alone (2 tries):         pass {sum(a['large']['pass'] for a in full.values())}/{N} = {100 * sum(a['large']['pass'] for a in full.values()) / N:.1f}%  ${lg / N:.4f}/task")
fail = [k for k, a in full.items() if not a["small"]["pass"]]
c0 = base_small + sum(full[k]["large"]["total"]["cost_usd"] for k in fail)
p0 = base_pass + sum(full[k]["large"]["pass"] for k in fail)
print(f"   Haiku, then Opus direct:      pass {p0}/{N} = {100 * p0 / N:.1f}%  ${c0 / N:.4f}/task  Opus solves {len(fail)}")
for arm in sorted(runs):
    rs = runs[arm]
    if set(rs) != set(fail):
        print(f"   {arm}: covers {len(rs)} of {len(fail)} failed tasks; skipped")
        continue
    rest = [k for k in fail if not rs[k]["pass"]]
    cost = base_small + sum(r["total"]["cost_usd"] for r in rs.values()) + sum(full[k]["large"]["total"]["cost_usd"] for k in rest)
    ok = base_pass + sum(r["pass"] for r in rs.values()) + sum(full[k]["large"]["pass"] for k in rest)
    print(f"   Haiku, then {arm}, then Opus:  pass {ok}/{N} = {100 * ok / N:.1f}%  ${cost / N:.4f}/task  Opus solves {len(rest)}")
    # the same policy without sending the rest to Opus
    cost2 = base_small + sum(r["total"]["cost_usd"] for r in rs.values())
    ok2 = base_pass + sum(r["pass"] for r in rs.values())
    print(f"   Haiku, then {arm}, no Opus code: pass {ok2}/{N} = {100 * ok2 / N:.1f}%  ${cost2 / N:.4f}/task")

# residue: what nothing but Opus-written code solved
for arm in sorted(runs):
    rest = sorted(f"{k[0]}/{k[1]}" for k, r in runs[arm].items() if not r["pass"])
    print(f"\n{arm} unsolved ({len(rest)}):", " ".join(rest))
