"""Collect bug-fix tasks from a GitHub repository that has no usable test suite.

usage: obs_mine.py OWNER/NAME BARE_REPO.git OUT_DIR [PAGES]
  writes OUT_DIR/inst/<id>.json in the SWE-bench instance shape (no test_patch)
  and OUT_DIR/candidates.txt. Needs the gh CLI, logged in.

A task is a merged pull request that
  - has one commit, so its parent is the state the issue was reported against,
  - closes exactly one issue, opened before the pull request, with a body of
    at least 200 characters,
  - changes at most 3 files, all C, C++ or Objective-C sources, and at most
    60 lines.
The issue title and body are the problem statement. Issue comments are left out.
"""
import json
import os
import subprocess
import sys

QUERY = """query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){
 pullRequests(states:MERGED,first:50,after:$after,orderBy:{field:CREATED_AT,direction:DESC}){
  pageInfo{endCursor hasNextPage}
  nodes{number title createdAt additions deletions changedFiles
   mergeCommit{oid} commits{totalCount}
   files(first:10){nodes{path}}
   closingIssuesReferences(first:3){totalCount nodes{number title body createdAt}}}}}}"""
EXT = (".c", ".cpp", ".cc", ".h", ".hpp", ".m", ".mm")


def git(repo, *a):
    return subprocess.run(["git", "--git-dir", repo, *a], capture_output=True, text=True)


if __name__ == "__main__":
    owner, name = sys.argv[1].split("/")
    repo, out = sys.argv[2], sys.argv[3]
    pages = int(sys.argv[4]) if len(sys.argv) > 4 else 60
    os.makedirs(os.path.join(out, "inst"), exist_ok=True)
    after, seen, kept = None, 0, []
    for _ in range(pages):
        args = ["gh", "api", "graphql", "-f", "query=" + QUERY, "-f", "owner=" + owner, "-f", "name=" + name]
        if after:
            args += ["-f", "after=" + after]
        r = subprocess.run(args, capture_output=True, text=True)
        if r.returncode != 0:
            sys.exit(r.stderr)
        prs = json.loads(r.stdout)["data"]["repository"]["pullRequests"]
        for p in prs["nodes"]:
            seen += 1
            iss = p["closingIssuesReferences"]
            paths = [f["path"] for f in p["files"]["nodes"]]
            if (p["commits"]["totalCount"] != 1 or iss["totalCount"] != 1 or not p["mergeCommit"]
                    or p["changedFiles"] > 3 or p["additions"] + p["deletions"] > 60
                    or not all(x.endswith(EXT) for x in paths)):
                continue
            i = iss["nodes"][0]
            if len(i["body"] or "") < 200 or i["createdAt"] >= p["createdAt"]:
                continue
            oid = p["mergeCommit"]["oid"]
            base = git(repo, "rev-parse", oid + "^").stdout.strip()
            patch = git(repo, "diff", base, oid).stdout
            if not base or not patch or len(git(repo, "rev-list", "--parents", "-n", "1", oid).stdout.split()) != 2:
                continue
            iid = f"{owner}__{name}-{p['number']}"
            json.dump({"repo": f"{owner}/{name}", "instance_id": iid, "base_commit": base, "patch": patch, "test_patch": "",
                       "problem_statement": i["title"] + "\n\n" + i["body"], "created_at": p["createdAt"], "issue": i["number"],
                       "pr_title": p["title"], "FAIL_TO_PASS": "[]", "PASS_TO_PASS": "[]"},
                      open(os.path.join(out, "inst", iid + ".json"), "w"), indent=1)
            kept.append(iid)
        after = prs["pageInfo"]["endCursor"]
        if not prs["pageInfo"]["hasNextPage"]:
            break
    open(os.path.join(out, "candidates.txt"), "w").write("\n".join(kept) + "\n")
    print(f"{seen} merged pull requests, {len(kept)} tasks")
