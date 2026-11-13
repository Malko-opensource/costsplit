// Command polybench runs the Aider Polyglot benchmark through three arms and
// reports pass rate and cost per arm and language.
//
//	polybench run -bench ~/bench-data/polyglot-benchmark -langs go,java -n 3 -arms small,large,graybox -out results.jsonl
//	polybench summary results.jsonl
//
// Models are called through `claude -p` with the logged-in account.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/Malko-opensource/costsplit/internal/bench"
	"github.com/Malko-opensource/costsplit/internal/policy"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: polybench run|summary ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "summary":
		summary(os.Args[2:])
	case "swe":
		sweCmd(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: polybench run|summary ...")
		os.Exit(2)
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	benchRoot := fs.String("bench", filepath.Join(home, "bench-data/polyglot-benchmark"), "polyglot-benchmark checkout")
	repo := fs.String("repo", wd, "graybox repo root (adapters, jars)")
	jsDeps := fs.String("js-deps", filepath.Join(home, "bench-data/js-deps"), "dir with node_modules for jest")
	javaHome := fs.String("java-home", "/opt/homebrew/opt/openjdk", "JDK home")
	langs := fs.String("langs", "go,java,python,javascript", "languages")
	exercises := fs.String("exercises", "", "comma-separated exercise names (default: all, or a sample with -n)")
	pairs := fs.String("pairs", "", "file with one lang/exercise per line; overrides -langs, -exercises and -n")
	n := fs.Int("n", 0, "random sample size per language (0 = all)")
	seed := fs.Int64("seed", 1, "sampling seed")
	arms := fs.String("arms", "small,large,graybox", "arms to run")
	small := fs.String("small", "haiku", "small model")
	large := fs.String("large", "opus", "large model")
	dec := fs.String("decider", "haiku", "decider model")
	smallRetries := fs.Int("small-retries", 2, "graybox: small model attempts before escalation (provisional, Q-007)")
	largeRetries := fs.Int("large-retries", 2, "graybox: large model attempts before escalation to human (provisional, Q-007)")
	threshold := fs.Float64("threshold", 0.8, "graybox: confidence threshold (provisional, Q-012)")
	advisoryPlan := fs.Bool("advisory-plan", false, "graybox: plan gate only advises")
	testsDecide := fs.Bool("tests-decide-done", false, "graybox: done gate follows the tests; only test gaming can fail it")
	jobs := fs.Int("j", 4, "parallel exercises")
	out := fs.String("out", "results.jsonl", "results file (appended)")
	_ = fs.Parse(args)

	env := &bench.Env{BenchRoot: *benchRoot, RepoRoot: *repo, JSDeps: *jsDeps, JavaHome: *javaHome}
	os.Setenv("JAVA_HOME", *javaHome)
	os.Setenv("GRAYBOX_JAVA_CP", bench.JavaClasspath(env))
	cfg := bench.Config{Small: *small, Large: *large, Decider: *dec, Tries: 2,
		Limits: policy.Limits{SmallRetries: *smallRetries, LargeRetries: *largeRetries, ConfidenceThreshold: *threshold,
			AdvisoryPlan: *advisoryPlan, TestsDecideDone: *testsDecide}}

	type job struct {
		x   *bench.Exercise
		arm string
	}
	var jobsList []job
	if *pairs != "" {
		b, err := os.ReadFile(*pairs)
		if err != nil {
			die(err)
		}
		for _, ln := range strings.Fields(string(b)) {
			lang, nm, ok := strings.Cut(ln, "/")
			if !ok {
				die(fmt.Errorf("%s: want lang/exercise, got %q", *pairs, ln))
			}
			x, err := bench.Load(env, lang, nm)
			if err != nil {
				die(err)
			}
			for _, a := range strings.Split(*arms, ",") {
				jobsList = append(jobsList, job{x, a})
			}
		}
		*langs = ""
	}
	for _, lang := range strings.Fields(strings.ReplaceAll(*langs, ",", " ")) {
		names, err := bench.List(env, lang)
		if err != nil {
			die(err)
		}
		if *exercises != "" {
			want := map[string]bool{}
			for _, e := range strings.Split(*exercises, ",") {
				want[e] = true
			}
			var keep []string
			for _, nm := range names {
				if want[nm] {
					keep = append(keep, nm)
				}
			}
			names = keep
		} else if *n > 0 && *n < len(names) {
			r := rand.New(rand.NewSource(*seed))
			r.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
			names = names[:*n]
			sort.Strings(names)
		}
		for _, nm := range names {
			x, err := bench.Load(env, lang, nm)
			if err != nil {
				die(err)
			}
			for _, a := range strings.Split(*arms, ",") {
				jobsList = append(jobsList, job{x, a})
			}
		}
	}

	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		die(err)
	}
	defer f.Close()
	var mu sync.Mutex
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	ctx := context.Background()
	for i, jb := range jobsList {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, jb job) {
			defer wg.Done()
			defer func() { <-sem }()
			var r bench.Result
			switch jb.arm {
			case "small":
				r = bench.RunSingle(ctx, env, cfg, jb.x, "small", cfg.Small)
			case "large":
				r = bench.RunSingle(ctx, env, cfg, jb.x, "large", cfg.Large)
			case "graybox":
				r = bench.RunGraybox(ctx, env, cfg, jb.x)
			default:
				opt, ok := bench.ParseCascadeArm(jb.arm)
				if !ok {
					die(fmt.Errorf("unknown arm %q", jb.arm))
				}
				r = bench.RunCascade(ctx, env, cfg, jb.x, jb.arm, opt)
			}
			b, _ := json.Marshal(r)
			mu.Lock()
			f.Write(append(b, '\n'))
			fmt.Fprintf(os.Stderr, "[%d/%d] %-10s %-24s %-8s %-15s $%.4f %.0fs\n", i+1, len(jobsList), r.Lang, r.Exercise, r.Arm, r.Outcome, r.Total.CostUSD, r.Seconds)
			mu.Unlock()
		}(i, jb)
	}
	wg.Wait()
}

// sweCmd runs SWE-bench instances through the arms.
//
//	polybench swe -instance ~/bench-data/swe/sympy__sympy-24152.json -repo-dir ~/bench-data/swe/repos/sympy__sympy-24152 -python ~/bench-data/swe/venv/bin/python
//	polybench swe -ids ids.txt -inst-dir ~/bench-data/swe/inst -repos-dir ~/bench-data/swe/repos -python ... -arms small,small-ctx,small-tdd,small-oracle,large
func sweCmd(args []string) {
	fs := flag.NewFlagSet("swe", flag.ExitOnError)
	wd, _ := os.Getwd()
	inst := fs.String("instance", "", "one instance JSON (one SWE-bench row); use with -repo-dir")
	repoDir := fs.String("repo-dir", "", "repository checkout at base_commit for -instance")
	ids := fs.String("ids", "", "file with instance ids, one per line; use with -inst-dir and -repos-dir")
	instDir := fs.String("inst-dir", "", "directory with <id>.json")
	reposDir := fs.String("repos-dir", "", "directory with <id>/ checkouts")
	python := fs.String("python", "python3", "interpreter with the repository's dependencies")
	repo := fs.String("repo", wd, "graybox repo root")
	arms := fs.String("arms", "small,large,graybox", "small, large, graybox, check, or <small|large>-<ctx|tdd|oracle>")
	small := fs.String("small", "haiku", "small model")
	large := fs.String("large", "opus", "large model")
	dec := fs.String("decider", "haiku", "decider model")
	jobs := fs.Int("j", 4, "parallel runs")
	out := fs.String("out", "swe.jsonl", "results file (appended)")
	_ = fs.Parse(args)

	type target struct {
		s   *bench.SWE
		env *bench.SWEEnv
	}
	var targets []target
	add := func(path, dir string) {
		s, err := bench.LoadSWE(path)
		if err != nil {
			die(err)
		}
		targets = append(targets, target{s, &bench.SWEEnv{RepoDir: dir, Python: *python, RepoRoot: *repo}})
	}
	if *ids != "" {
		b, err := os.ReadFile(*ids)
		if err != nil {
			die(err)
		}
		for _, id := range strings.Fields(string(b)) {
			add(filepath.Join(*instDir, id+".json"), filepath.Join(*reposDir, id))
		}
	} else {
		add(*inst, *repoDir)
	}
	armList := strings.Split(*arms, ",")
	for _, a := range armList {
		// The graybox arm configures the adapter through the process
		// environment, so it cannot serve several instances at once.
		if a == "graybox" {
			if len(targets) != 1 {
				die(fmt.Errorf("the graybox arm runs one instance at a time"))
			}
			bench.SetSWEAdapterEnv(targets[0].env, targets[0].s)
		}
	}
	cfg := bench.Config{Small: *small, Large: *large, Decider: *dec, Tries: 2,
		Limits: policy.Limits{SmallRetries: 2, LargeRetries: 2, ConfidenceThreshold: 0.5, AdvisoryPlan: true, TestsDecideDone: true}}
	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		die(err)
	}
	defer f.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, *jobs)
	n, total := 0, len(targets)*len(armList)
	for _, t := range targets {
		for _, a := range armList {
			wg.Add(1)
			sem <- struct{}{}
			go func(t target, a string) {
				defer wg.Done()
				defer func() { <-sem }()
				var r bench.Result
				switch a {
				case "small":
					r = bench.RunSWESingle(ctx, t.env, cfg, t.s, "small", cfg.Small)
				case "large":
					r = bench.RunSWESingle(ctx, t.env, cfg, t.s, "large", cfg.Large)
				case "graybox":
					r = bench.RunSWEGraybox(ctx, t.env, cfg, t.s)
				case "agent-full", "agent-cola", "agent-haiku", "agent-scout":
					r = bench.RunSWEAgent(ctx, t.env, cfg, t.s, a)
				case "check":
					r = bench.SWECheck(ctx, t.env, t.s)
				default:
					role, opt, ok := bench.ParseSWEArm(a)
					if !ok {
						die(fmt.Errorf("unknown arm %q", a))
					}
					r = bench.RunSWEContract(ctx, t.env, cfg, t.s, a, role, opt)
				}
				b, _ := json.Marshal(r)
				mu.Lock()
				n++
				f.Write(append(b, '\n'))
				fmt.Fprintf(os.Stderr, "[%d/%d] %-22s %-13s %-5s $%.4f %.0fs\n", n, total, r.Exercise, r.Arm, r.Outcome, r.Total.CostUSD, r.Seconds)
				if len(targets) == 1 {
					for _, st := range r.Steps {
						fmt.Fprintln(os.Stderr, "   ", st)
					}
				}
				mu.Unlock()
			}(t, a)
		}
	}
	wg.Wait()
}

func summary(args []string) {
	if len(args) != 1 {
		die(fmt.Errorf("usage: polybench summary results.jsonl"))
	}
	f, err := os.Open(args[0])
	if err != nil {
		die(err)
	}
	defer f.Close()
	type agg struct {
		n, pass, escalated, needsHuman, invalid, paused, deciderDone, deciderFalsePass int
		cost                                                                           float64
	}
	by := map[string]*agg{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var r bench.Result
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		for _, k := range []string{r.Lang + "\t" + r.Arm, "ALL\t" + r.Arm} {
			a := by[k]
			if a == nil {
				a = &agg{}
				by[k] = a
			}
			a.n++
			a.cost += r.Total.CostUSD
			if r.Pass {
				a.pass++
			}
			if r.Escalated {
				a.escalated++
			}
			switch r.Outcome {
			case "needs_human":
				a.needsHuman++
			case "graybox_invalid":
				a.invalid++
			case "paused":
				a.paused++
			}
			if r.DeciderDone {
				a.deciderDone++
				if !r.Pass {
					a.deciderFalsePass++
				}
			}
		}
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "lang\tarm\tn\tpass\tpass%\t$total\t$/task\t$/solved\tescalated\tneeds_human\tgraybox_invalid\tpaused\tdecider_false_pass")
	for _, k := range keys {
		a := by[k]
		perSolved := "-"
		if a.pass > 0 {
			perSolved = fmt.Sprintf("%.4f", a.cost/float64(a.pass))
		}
		fp := "-"
		if a.deciderDone > 0 {
			fp = fmt.Sprintf("%d/%d", a.deciderFalsePass, a.deciderDone)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%.1f\t%.4f\t%.4f\t%s\t%d\t%d\t%d\t%d\t%s\n", k, a.n, a.pass, 100*float64(a.pass)/float64(a.n),
			a.cost, a.cost/float64(a.n), perSolved, a.escalated, a.needsHuman, a.invalid, a.paused, fp)
	}
	w.Flush()
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "polybench:", err)
	os.Exit(1)
}
