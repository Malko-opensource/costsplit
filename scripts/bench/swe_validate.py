"""Export and validate SWE-bench sympy instances in the local environment.

For each candidate: export the tree at base_commit, then run the instance's
test files twice in throwaway copies: on the unfixed code and with the
reference patch. An instance is usable when the reference patch makes every
FAIL_TO_PASS test pass and at least one of them fails without it.
PASS_TO_PASS is narrowed to the tests that pass with the reference patch here,
so differences between this environment and SWE-bench's do not count against
any arm.

usage: swe_validate.py <swe dir> <python> [workers]
"""
import concurrent.futures as cf
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

root, py = sys.argv[1], sys.argv[2]
workers = int(sys.argv[3]) if len(sys.argv) > 3 else 8
DIFF = re.compile(r"^diff --git a/(\S+)", re.M)


def run_tests(tree, files, timeout=900):
    t0 = time.time()
    try:
        p = subprocess.run([py, "-m", "pytest", "-q", "-rA", "-p", "no:cacheprovider", *files], cwd=tree,
                           capture_output=True, text=True, timeout=timeout)
        out = p.stdout + p.stderr
    except subprocess.TimeoutExpired:
        return set(), "timeout", time.time() - t0
    passed = set()
    for line in out.splitlines():
        if line.startswith("PASSED "):
            passed.add(line[7:].split("::")[-1].strip())
    return passed, out, time.time() - t0


def apply(tree, patch):
    p = subprocess.run(["git", "apply", "-"], cwd=tree, input=patch, capture_output=True, text=True)
    return p.returncode == 0, p.stderr


def validate(line):
    iid, sha = line.split()
    row = json.load(open(f"{root}/inst/{iid}.raw.json"))
    tree = f"{root}/repos/{iid}"
    if not os.path.isdir(tree):
        os.makedirs(tree)
        ar = subprocess.Popen(["git", "-C", f"{root}/sympy.git", "archive", sha], stdout=subprocess.PIPE)
        subprocess.run(["tar", "-x", "-C", tree], stdin=ar.stdout, check=True)
        ar.wait()
    tests = DIFF.findall(row["test_patch"])
    src = DIFF.findall(row["patch"])
    f2p, p2p = json.loads(row["FAIL_TO_PASS"]), json.loads(row["PASS_TO_PASS"])
    res = {"id": iid, "version": row["version"], "f2p": len(f2p), "p2p": len(p2p),
           "src_lines": sum(len(open(f"{tree}/{f}", errors="ignore").read().splitlines()) for f in src if os.path.isfile(f"{tree}/{f}"))}
    out = {}
    for mode in ("unfixed", "gold"):
        tmp = tempfile.mkdtemp(prefix="swe-val-")
        work = os.path.join(tmp, "r")
        subprocess.run(["cp", "-cR", tree, work], check=True)
        try:
            ok, err = apply(work, row["test_patch"])
            if not ok:
                return {**res, "valid": False, "why": "test patch does not apply: " + err[:80]}
            if mode == "gold":
                ok, err = apply(work, row["patch"])
                if not ok:
                    return {**res, "valid": False, "why": "reference patch does not apply"}
            passed, log, dur = run_tests(work, tests)
            out[mode] = passed
            res["sec_" + mode] = round(dur)
            if log == "timeout":
                return {**res, "valid": False, "why": mode + " timed out"}
            if not passed and mode == "gold":
                return {**res, "valid": False, "why": "no test passes with the reference patch: " + log[-200:].replace("\n", " ")}
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
    missing = [t for t in f2p if t not in out["gold"]]
    if missing:
        return {**res, "valid": False, "why": f"{len(missing)} of {len(f2p)} FAIL_TO_PASS do not pass with the reference patch"}
    if all(t in out["unfixed"] for t in f2p):
        return {**res, "valid": False, "why": "FAIL_TO_PASS already pass on the unfixed code"}
    keep = [t for t in p2p if t in out["gold"]]
    row["PASS_TO_PASS"] = json.dumps(keep)
    row["local"] = {"p2p_dropped": len(p2p) - len(keep), "sec_gold": res["sec_gold"], "src_lines": res["src_lines"]}
    json.dump(row, open(f"{root}/inst/{iid}.json", "w"))
    return {**res, "valid": True, "why": "", "p2p_kept": len(keep)}


lines = [l for l in open(f"{root}/candidates.txt").read().splitlines() if l.strip()]
with cf.ThreadPoolExecutor(workers) as ex:
    results = list(ex.map(validate, lines))
results.sort(key=lambda r: (not r["valid"], r["id"]))
for r in results:
    print(f"{r['id']:22s} v{r['version']:5s} {'ok ' if r['valid'] else 'NO '} f2p {r['f2p']:2d} p2p {r['p2p']:3d}->{r.get('p2p_kept', '-')!s:>3s} "
          f"gold {r.get('sec_gold', '-')!s:>4s}s src {r['src_lines']:5d} lines  {r['why']}")
valid = [r for r in results if r["valid"]]
print(f"\nvalid {len(valid)} of {len(results)}")
json.dump(results, open(f"{root}/validation.json", "w"), indent=1)
