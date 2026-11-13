// Package calibrate measures, on one repository's own past bug fixes, which
// agent setting is the cheapest that still does the work.
package calibrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// A Task is one merged bug fix, in the shape of a SWE-bench row without a
// test patch: the harness scores it by where the edits land.
type Task struct {
	Repo      string `json:"repo"`
	ID        string `json:"instance_id"`
	Base      string `json:"base_commit"`
	Patch     string `json:"patch"`
	TestPatch string `json:"test_patch"`
	Problem   string `json:"problem_statement"`
	Created   string `json:"created_at"`
	Issue     int    `json:"issue"`
	PRTitle   string `json:"pr_title"`
	F2P       string `json:"FAIL_TO_PASS"`
	P2P       string `json:"PASS_TO_PASS"`
}

// MineOptions bound what counts as a task.
type MineOptions struct {
	MaxFiles int      // source files the fix may touch
	MaxLines int      // changed lines in those files
	MinIssue int      // characters in the issue body
	Pool     int      // stop after this many tasks
	Pages    int      // pull-request pages of 50 to read at most
	Exts     []string // source file extensions
	Progress func(seen, kept int)
	Skipped  map[string]int // if set, filled with why pull requests were left out
}

func DefaultMineOptions() MineOptions {
	return MineOptions{MaxFiles: 3, MaxLines: 60, MinIssue: 200, Pool: 200, Pages: 60,
		Exts: strings.Split(".c,.cc,.cpp,.cxx,.h,.hh,.hpp,.m,.mm,.go,.py,.js,.jsx,.ts,.tsx,.java,.kt,.rs,.rb,.php,.cs,.swift,.scala", ",")}
}

const mineQuery = `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){
 pullRequests(states:MERGED,first:50,after:$after,orderBy:{field:CREATED_AT,direction:DESC}){
  pageInfo{endCursor hasNextPage}
  nodes{number title createdAt changedFiles
   mergeCommit{oid} commits{totalCount}
   files(first:30){nodes{path}}
   closingIssuesReferences(first:2){totalCount nodes{number title body createdAt}}}}}}`

type minePage struct {
	Data struct {
		Repository struct {
			PullRequests struct {
				PageInfo struct {
					EndCursor   string
					HasNextPage bool
				}
				Nodes []struct {
					Number       int
					Title        string
					CreatedAt    string
					ChangedFiles int
					MergeCommit  *struct{ Oid string }
					Commits      struct{ TotalCount int }
					Files        struct{ Nodes []struct{ Path string } }
					Closing      struct {
						TotalCount int
						Nodes      []struct {
							Number    int
							Title     string
							Body      string
							CreatedAt string
						}
					} `json:"closingIssuesReferences"`
				}
			}
		}
	}
}

var testPath = regexp.MustCompile(`(?i)(^|/)(tests?|spec|specs|__tests__|testdata|fixtures)/|(^|/)test_[^/]*$|_test\.[a-z]+$|\.(test|spec)\.[a-z]+$|(^|/)conftest\.py$`)

// IsTest reports whether a path looks like test code.
func IsTest(path string) bool { return testPath.MatchString(path) }

func isSource(path string, exts []string) bool {
	for _, e := range exts {
		if strings.HasSuffix(path, e) {
			return true
		}
	}
	return false
}

func git(gitDir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"--git-dir", gitDir}, args...)...).Output()
	return string(out), err
}

// Mine reads merged pull requests of a GitHub repository, newest first, and
// keeps those that are one commit, close exactly one earlier issue with a
// real description, and change a few lines in a few non-test source files.
// The issue's title and body become the task; its comments are left out.
// gitDir is a bare clone, used to cut the reference patch.
func Mine(repo, gitDir string, o MineOptions) ([]Task, int, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, 0, fmt.Errorf("repository must be owner/name, got %q", repo)
	}
	var tasks []Task
	seen, after := 0, ""
	skip := o.Skipped
	if skip == nil {
		skip = map[string]int{}
	}
	for page := 0; page < o.Pages && len(tasks) < o.Pool; page++ {
		args := []string{"api", "graphql", "-f", "query=" + mineQuery, "-f", "owner=" + owner, "-f", "name=" + name}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		cmd := exec.Command("gh", args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return tasks, seen, fmt.Errorf("gh api: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		var p minePage
		if err := json.Unmarshal(out, &p); err != nil {
			return tasks, seen, err
		}
		prs := p.Data.Repository.PullRequests
		for _, pr := range prs.Nodes {
			seen++
			if pr.Closing.TotalCount != 1 {
				skip["closes no issue, or several"]++
				continue
			}
			if pr.MergeCommit == nil || pr.ChangedFiles > len(pr.Files.Nodes) {
				skip["too many files"]++
				continue
			}
			is := pr.Closing.Nodes[0]
			if len(is.Body) < o.MinIssue || is.CreatedAt >= pr.CreatedAt {
				skip["issue too short, or opened after the fix"]++
				continue
			}
			var src []string
			for _, f := range pr.Files.Nodes {
				if isSource(f.Path, o.Exts) && !IsTest(f.Path) {
					src = append(src, f.Path)
				}
			}
			if len(src) == 0 || len(src) > o.MaxFiles {
				skip["no source file changed, or too many"]++
				continue
			}
			oid := pr.MergeCommit.Oid
			var all []string
			for _, f := range pr.Files.Nodes {
				all = append(all, f.Path)
			}
			base := baseOf(gitDir, oid, pr.Commits.TotalCount, all)
			if base == "" {
				skip["state before the fix not found in the clone"]++
				continue
			}
			patch, err := git(gitDir, append([]string{"diff", base, oid, "--"}, src...)...)
			if err != nil || patch == "" || changedLines(patch) > o.MaxLines || strings.Contains(patch, "\n--- /dev/null") {
				skip["change too large, or adds a new file"]++
				continue
			}
			tasks = append(tasks, Task{Repo: repo, ID: fmt.Sprintf("%s__%s-%d", owner, name, pr.Number), Base: base, Patch: patch,
				Problem: is.Title + "\n\n" + is.Body, Created: pr.CreatedAt, Issue: is.Number, PRTitle: pr.Title, F2P: "[]", P2P: "[]"})
			if len(tasks) >= o.Pool {
				break
			}
		}
		if o.Progress != nil {
			o.Progress(seen, len(tasks))
		}
		if !prs.PageInfo.HasNextPage {
			break
		}
		after = prs.PageInfo.EndCursor
	}
	return tasks, seen, nil
}

// baseOf finds the commit the pull request was applied to. A merge commit's
// first parent is it. A squash merge is one commit on top of it. A rebase
// merge is the request's n commits on top of it. The candidate whose diff
// touches exactly the request's files wins.
func baseOf(gitDir, oid string, n int, files []string) string {
	want := map[string]bool{}
	for _, f := range files {
		want[f] = true
	}
	cands := []string{oid + "^"}
	if n > 1 {
		cands = append(cands, fmt.Sprintf("%s~%d", oid, n))
	}
	for _, c := range cands {
		base, err := git(gitDir, "rev-parse", "--verify", "--quiet", c)
		if err != nil {
			continue
		}
		base = strings.TrimSpace(base)
		names, err := git(gitDir, "diff", "--name-only", "--no-renames", base, oid)
		if err != nil {
			continue
		}
		got := strings.Split(strings.TrimSpace(names), "\n")
		same := len(got) == len(want)
		for _, g := range got {
			if !want[g] {
				same = false
			}
		}
		if same {
			return base
		}
	}
	return ""
}

func changedLines(patch string) int {
	n := 0
	for _, l := range strings.Split(patch, "\n") {
		if (strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++")) || (strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---")) {
			n++
		}
	}
	return n
}

// Checkout unpacks the tree at commit into dst (no .git directory).
func Checkout(gitDir, commit, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	ar := exec.Command("git", "--git-dir", gitDir, "archive", commit)
	tar := exec.Command("tar", "-x", "-C", dst)
	pipe, err := ar.StdoutPipe()
	if err != nil {
		return err
	}
	tar.Stdin = pipe
	if err := tar.Start(); err != nil {
		return err
	}
	if err := ar.Run(); err != nil {
		return fmt.Errorf("git archive %s: %v", commit, err)
	}
	return tar.Wait()
}

// Save writes a task where the harness loads instances from.
func (t Task) Save(dir string) error {
	b, _ := json.MarshalIndent(t, "", " ")
	return os.WriteFile(filepath.Join(dir, t.ID+".json"), b, 0o644)
}
