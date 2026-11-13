"""Export and validate SWE-bench sympy and django instances in the local environment.

For each candidate: export the tree at base_commit, then run the instance's
test files twice in throwaway copies: on the unfixed code and with the
reference patch. An instance is usable when the reference patch makes every
FAIL_TO_PASS test pass and at least one of them fails without it.
PASS_TO_PASS is narrowed to the tests that pass with the reference patch here,
so differences between this environment and SWE-bench's do not count against
any arm.

usage: swe_validate.py <swe dir> <python> [workers] [candidates file]
  candidates default to <swe dir>/candidates.txt (lines of "instance_id base_commit").
  The bare clone is <swe dir>/sympy.git or <swe dir>/django.git, by instance id.
  Django instances run with Django's own test runner; give a Python that has
  Django's test dependencies but not Django itself.
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
cand = sys.argv[4] if len(sys.argv) > 4 else f"{root}/candidates.txt"
DJ_TEST = re.compile(r"^(\S+ \([\w.]+\))")
DIFF = re.compile(r"^diff --git a/(\S+)", re.M)


def run_django(tree, files, timeout=900):
    t0 = time.time()
    labels = [f[len("tests/"):-3].replace("/", ".") for f in files if f.startswith("tests/") and f.endswith(".py")]
    env = dict(os.environ, PYTHONPATH=tree, LANG="en_US.UTF-8", LC_ALL="en_US.UTF-8")
    try:
        p = subprocess.run([py, "runtests.py", "--parallel", "1", "-v", "2", *labels], cwd=tree + "/tests",
                           capture_output=True, text=True, timeout=timeout, env=env)
        out = p.stdout + p.stderr
    except subprocess.TimeoutExpired:
        return set(), "timeout", time.time() - t0
    passed, pending = set(), ""
    for line in out.splitlines():
        m = DJ_TEST.match(line)
        name = m.group(1) if m else ""
        if line.strip().endswith("... ok"):
            if name or pending:
                passed.add(name or pending)
            pending = ""
        elif name and " ... " not in line:
            pending = name
        elif " ... " in line:
            pending = ""
    return passed, out, time.time() - t0


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
        ar = subprocess.Popen(["git", "-C", f"{root}/{'django' if iid.startswith('django__') else 'sympy'}.git", "archive", sha], stdout=subprocess.PIPE)
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
            passed, log, dur = (run_django if iid.startswith("django__") else run_tests)(work, tests)
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


lines = [l for l in open(cand).read().splitlines() if l.strip()]
with cf.ThreadPoolExecutor(workers) as ex:
    results = list(ex.map(validate, lines))
results.sort(key=lambda r: (not r["valid"], r["id"]))
for r in results:
    print(f"{r['id']:22s} v{r['version']:5s} {'ok ' if r['valid'] else 'NO '} f2p {r['f2p']:2d} p2p {r['p2p']:3d}->{r.get('p2p_kept', '-')!s:>3s} "
          f"gold {r.get('sec_gold', '-')!s:>4s}s src {r['src_lines']:5d} lines  {r['why']}")
valid = [r for r in results if r["valid"]]
print(f"\nvalid {len(valid)} of {len(results)}")
name = "validation.json" if cand.endswith("/candidates.txt") else "validation-" + os.path.basename(cand).rsplit(".", 1)[0] + ".json"
json.dump(results, open(f"{root}/{name}", "w"), indent=1)
