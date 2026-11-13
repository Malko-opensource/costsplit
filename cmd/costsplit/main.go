// Command costsplit measures which agent setting to use in a repository.
//
//	costsplit calibrate -repo owner/name [-n 45] [-max-usd 30]
//	    collects past bug fixes of a GitHub repository, runs each agent
//	    setting on them, and prints which is the cheapest that still works
//	costsplit report results.jsonl [more.jsonl ...]
//	    prints the same report from result files
//
// Needs the gh and claude CLIs, logged in, and bin/colamcp next to this binary.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Malko-opensource/costsplit/internal/bench"
	"github.com/Malko-opensource/costsplit/internal/calibrate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "calibrate":
		calibrateCmd(os.Args[2:])
	case "report":
		reportCmd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: costsplit calibrate -repo owner/name | costsplit report results.jsonl ...")
	os.Exit(2)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "costsplit:", err)
	os.Exit(1)
}

func say(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }

func load(paths ...string) []bench.Result {
	var rs []bench.Result
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			die(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<28)
		for sc.Scan() {
			var r bench.Result
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Arm != "" {
				rs = append(rs, r)
			}
		}
		f.Close()
	}
	return rs
}

func reportCmd(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	baseline := fs.String("baseline", "agent-full", "setting the others are compared with")
	arms := fs.String("arms", "", "settings to include (default: all in the files)")
	tol := fs.Int("tolerance", -1, "net tasks a setting may lose (default: 5% of the tasks)")
	name := fs.String("name", "", "repository name for the heading")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		usage()
	}
	rs := load(fs.Args()...)
	if *arms != "" {
		keep := map[string]bool{}
		for _, a := range strings.Split(*arms, ",") {
			keep[a] = true
		}
		var f []bench.Result
		for _, r := range rs {
			if keep[r.Arm] {
				f = append(f, r)
			}
		}
		rs = f
	}
	fmt.Print(calibrate.Analyze(rs, *baseline, *tol).Markdown(*name))
}

func calibrateCmd(args []string) {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	home, _ := os.UserHomeDir()
	repo := fs.String("repo", "", "GitHub repository, owner/name")
	dir := fs.String("dir", "", "working directory (default ~/.costsplit/calibrate/<owner>__<name>)")
	n := fs.Int("n", 45, "tasks to run")
	seed := fs.Int64("seed", 1, "sampling seed")
	arms := fs.String("arms", "agent-full,agent-low,agent-haiku", "settings to compare; the first is the baseline")
	jobs := fs.Int("j", 4, "parallel runs")
	maxUSD := fs.Float64("max-usd", 30, "stop starting runs once this much is spent")
	tol := fs.Int("tolerance", -1, "net tasks a setting may lose (default: 5% of the tasks)")
	dry := fs.Bool("dry-run", false, "collect the tasks and stop before any model call")
	small := fs.String("small", "haiku", "small model")
	large := fs.String("large", "opus", "large model")
	python := fs.String("python", "python3", "interpreter used to syntax-check edited .py files")
	_ = fs.Parse(args)
	if !strings.Contains(*repo, "/") {
		die(fmt.Errorf("-repo owner/name is required"))
	}
	if *dir == "" {
		*dir = filepath.Join(home, ".costsplit", "calibrate", strings.ReplaceAll(*repo, "/", "__"))
	}
	gitDir, instDir, reposDir := filepath.Join(*dir, "repo.git"), filepath.Join(*dir, "inst"), filepath.Join(*dir, "repos")
	for _, d := range []string{instDir, reposDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			die(err)
		}
	}
	for _, tool := range []string{"gh", "git", "claude"} {
		if _, err := exec.LookPath(tool); err != nil {
			die(fmt.Errorf("%s is not on PATH", tool))
		}
	}

	// 1. A bare clone: the tasks' "before" states are cut from it.
	if _, err := os.Stat(gitDir); err != nil {
		say("cloning %s", *repo)
		if out, err := exec.Command("gh", "repo", "clone", *repo, gitDir, "--", "--bare", "--quiet").CombinedOutput(); err != nil {
			die(fmt.Errorf("clone: %v: %s", err, out))
		}
	}

	// 2. Tasks: mined once, then reused, so a rerun measures the same set.
	idsPath := filepath.Join(*dir, "ids.txt")
	var ids []string
	if b, err := os.ReadFile(idsPath); err == nil {
		ids = strings.Fields(string(b))
		say("reusing %d tasks from %s", len(ids), idsPath)
	} else {
		o := calibrate.DefaultMineOptions()
		o.Progress = func(seen, kept int) {
			if seen%500 == 0 {
				say("  read %d merged pull requests, %d usable", seen, kept)
			}
		}
		o.Skipped = map[string]int{}
		tasks, seen, err := calibrate.Mine(*repo, gitDir, o)
		if err != nil {
			die(err)
		}
		say("%d of %d merged pull requests are usable tasks", len(tasks), seen)
		var whys []string
		for why := range o.Skipped {
			whys = append(whys, why)
		}
		sort.Slice(whys, func(i, j int) bool { return o.Skipped[whys[i]] > o.Skipped[whys[j]] })
		for _, why := range whys {
			say("  left out %5d: %s", o.Skipped[why], why)
		}
		if len(tasks) == 0 {
			die(fmt.Errorf("no usable tasks: the repository needs merged one-commit pull requests that close an issue"))
		}
		rng := rand.New(rand.NewSource(*seed))
		rng.Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] })
		if len(tasks) > *n {
			tasks = tasks[:*n]
		}
		for _, t := range tasks {
			if err := t.Save(instDir); err != nil {
				die(err)
			}
			ids = append(ids, t.ID)
		}
		if err := os.WriteFile(idsPath, []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
			die(err)
		}
	}
	if len(ids) < 20 {
		say("warning: only %d tasks; differences of a few tasks will not mean much", len(ids))
	}

	// 3. One checkout per task, at the commit before its fix.
	type target struct {
		s   *bench.SWE
		env *bench.SWEEnv
	}
	var targets []target
	for _, id := range ids {
		s, err := bench.LoadSWE(filepath.Join(instDir, id+".json"))
		if err != nil {
			die(err)
		}
		work := filepath.Join(reposDir, id)
		if _, err := os.Stat(work); err != nil {
			var t calibrate.Task
			b, _ := os.ReadFile(filepath.Join(instDir, id+".json"))
			_ = json.Unmarshal(b, &t)
			if err := calibrate.Checkout(gitDir, t.Base, work); err != nil {
				os.RemoveAll(work)
				die(err)
			}
		}
		targets = append(targets, target{s, &bench.SWEEnv{RepoDir: work, Python: *python}})
	}
	armList := strings.Split(*arms, ",")
	say("%d tasks x %d settings in %s", len(targets), len(armList), *dir)
	if *dry {
		return
	}

	// 4. Run. Finished runs are kept, so an interrupted calibration resumes.
	if os.Getenv("COLAMCP_BIN") == "" {
		exe, _ := os.Executable()
		os.Setenv("COLAMCP_BIN", filepath.Join(filepath.Dir(exe), "colamcp"))
	}
	if _, err := os.Stat(os.Getenv("COLAMCP_BIN")); err != nil {
		die(fmt.Errorf("tool server not found at %s: build it with `go build -o bin/colamcp ./cmd/colamcp`", os.Getenv("COLAMCP_BIN")))
	}
	os.Unsetenv("DISABLE_PROMPT_CACHING")
	resPath := filepath.Join(*dir, "results.jsonl")
	done := map[string]bool{}
	spent := 0.0
	for _, r := range load(resPath) {
		spent += r.Total.CostUSD
		if r.Outcome != "error" {
			done[r.Exercise+" "+r.Arm] = true
		}
	}
	f, err := os.OpenFile(resPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		die(err)
	}
	cfg := bench.Config{Small: *small, Large: *large}
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, *jobs)
	total, k, stopped := 0, 0, false
	for _, t := range targets {
		for _, a := range armList {
			if !done[t.s.ID+" "+a] {
				total++
			}
		}
	}
	// Task by task, so that a budget stop leaves whole tasks to compare.
	for _, t := range targets {
		mu.Lock()
		over := spent >= *maxUSD
		mu.Unlock()
		if over {
			stopped = true
			break
		}
		for _, a := range armList {
			if done[t.s.ID+" "+a] {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(t target, a string) {
				defer wg.Done()
				defer func() { <-sem }()
				r := bench.RunSWEAgent(ctx, t.env, cfg, t.s, a)
				b, _ := json.Marshal(r)
				mu.Lock()
				k++
				spent += r.Total.CostUSD
				f.Write(append(b, '\n'))
				say("[%d/%d] %-34s %-12s %-5s $%.3f  (total $%.2f)", k, total, r.Exercise, r.Arm, r.Outcome, r.Total.CostUSD, spent)
				mu.Unlock()
			}(t, a)
		}
	}
	wg.Wait()
	f.Close()
	if stopped {
		say("stopped at the -max-usd limit ($%.2f spent); run again with a higher limit to continue", spent)
	}

	// 5. Report.
	var rs []bench.Result
	keep := map[string]bool{}
	for _, a := range armList {
		keep[a] = true
	}
	for _, r := range load(resPath) {
		if keep[r.Arm] {
			rs = append(rs, r)
		}
	}
	md := calibrate.Analyze(rs, armList[0], *tol).Markdown(*repo)
	_ = os.WriteFile(filepath.Join(*dir, "report.md"), []byte(md), 0o644)
	fmt.Print(md)
	say("report: %s", filepath.Join(*dir, "report.md"))
}
