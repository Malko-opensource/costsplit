// Command colabench measures stage 1 of the diet-cola plan: when a tool
// output is cut to a budget, does what the task needs survive?
//
//	colabench swe -inst-dir ~/bench-data/swe/inst -repos-dir ~/bench-data/swe/repos -out swe.jsonl
//	colabench hotpot -data ~/bench-data/hotpot -n 200 -out hotpot.jsonl
//	colabench recover -inst-dir ... -repos-dir ... -out recover.jsonl   (see recover.go)
//
// SWE: the observation is each file the reference patch touches, as it was
// before the fix; the task is the issue; what must survive is every block
// holding a line the patch changes. HotpotQA (distractor): the observation is
// the ten paragraphs, one sentence per block; the task is the question; what
// must survive is every supporting-fact sentence.
//
// Every ranker orders the blocks once; each budget then keeps blocks in that
// order. Rankers: head (truncation), random, lexical (BM25), haiku (the small
// model names blocks, the rest follow in lexical order), haiku-wide (asked
// for recall, 1.5x as many blocks allowed), haiku-x3 (haiku-wide three times,
// blocks ordered by votes), trace (functions in the traceback of the
// report's own example, then functions it ran, then lexical), haiku+trace
// (traceback, then haiku's picks, then functions that ran), opus-outline
// (the large model names blocks from an outline: each block's first two
// lines).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Malko-opensource/costsplit/internal/bench"
	"github.com/Malko-opensource/costsplit/internal/cola"
)

type record struct {
	Track      string             `json:"track"`
	ID         string             `json:"id"`
	File       string             `json:"file,omitempty"`
	Ranker     string             `json:"ranker"`
	Lines      int                `json:"lines"`
	Blocks     int                `json:"blocks"`
	Tokens     int                `json:"tokens"`
	GoldBlocks int                `json:"gold_blocks"`
	NeedFrac   float64            `json:"need_frac"` // share of tokens read, in rank order, until every gold block is in
	Covered    map[string]bool    `json:"covered"`   // budget share -> every gold block kept
	Recall     map[string]float64 `json:"recall"`    // budget share -> share of gold blocks kept
	Picks      int                `json:"picks,omitempty"`
	PicksFrac  float64            `json:"picks_frac,omitempty"`  // tokens of the model's own picks
	PicksCover bool               `json:"picks_cover,omitempty"` // the picks alone hold every gold block
	Outline    bool               `json:"outline,omitempty"`
	CostUSD    float64            `json:"cost_usd,omitempty"`
	Err        string             `json:"error,omitempty"`
	Trace      *traceStats        `json:"trace,omitempty"`
}

// traceStats says how good the execution signal was for one task.
type traceStats struct {
	traceInfo
	Raised       int     `json:"raised"`
	Called       int     `json:"called"`
	CalledFrac   float64 `json:"called_frac"`    // tokens of the called functions
	GoldInRaised bool    `json:"gold_in_raised"` // every gold block raised
	GoldInCalled bool    `json:"gold_in_called"` // every gold block ran
	AnyRaised    bool    `json:"any_gold_raised"`
}

type job struct {
	track, id, file, task string
	doc                   *cola.Doc
	gold                  []int // block positions that must survive
	trace                 *traceInfo
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: colabench swe|hotpot|recover [flags]")
		os.Exit(2)
	}
	if os.Args[1] == "recover" {
		recoverMain(os.Args[2:])
		return
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	out := fs.String("out", "", "results file (jsonl, appended)")
	rankers := fs.String("rankers", "head,random,lexical,haiku", "rankers to run")
	ratios := fs.String("ratios", "0.05,0.1,0.2,0.3,0.5", "budgets, as shares of the observation's tokens")
	model := fs.String("model", "haiku", "model for the haiku rankers")
	large := fs.String("large", "opus", "model for the opus-outline ranker")
	jobs := fs.Int("j", 4, "parallel model calls")
	instDir := fs.String("inst-dir", "", "swe: instance json files")
	reposDir := fs.String("repos-dir", "", "swe: checkouts at base_commit, one per instance id")
	ids := fs.String("ids", "", "swe: only these instance ids (comma separated)")
	data := fs.String("data", "", "hotpot: directory of rows files from the datasets server")
	n := fs.Int("n", 200, "hotpot: questions to use")
	python := fs.String("python", "", "swe: interpreter that runs the report's code for the trace rankers")
	_ = fs.Parse(os.Args[2:])
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	var js []job
	var err error
	maxPicks := 8
	switch os.Args[1] {
	case "swe":
		js, err = sweJobs(*instDir, *reposDir, *ids)
	case "hotpot":
		js, err = hotpotJobs(*data, *n)
		maxPicks = 6
	default:
		err = fmt.Errorf("unknown track %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if strings.Contains(*rankers, "trace") {
		if *python == "" {
			fmt.Fprintln(os.Stderr, "trace rankers need -python")
			os.Exit(2)
		}
		tracer, _ := filepath.Abs("adapters/python/trace.py")
		var wg sync.WaitGroup
		sem := make(chan struct{}, *jobs)
		for i := range js {
			wg.Add(1)
			sem <- struct{}{}
			go func(j *job) {
				defer func() { <-sem; wg.Done() }()
				ti := runTrace(context.Background(), *python, tracer, filepath.Join(*reposDir, j.id), j)
				j.trace = &ti
				fmt.Fprintf(os.Stderr, "trace    %-28s ran %d, raised %d, called %d %s\n", j.id, ti.Ran, len(ti.Raised), len(ti.Called), ti.Err)
			}(&js[i])
		}
		wg.Wait()
	}
	var shares []float64
	for _, s := range strings.Split(*ratios, ",") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad ratio", s)
			os.Exit(2)
		}
		shares = append(shares, f)
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	cli := &bench.ClaudeCLI{}
	completer := func(model string) cola.Completer {
		return func(ctx context.Context, system, prompt string, schema map[string]any) (json.RawMessage, float64, error) {
			raw, u, err := cli.Complete(ctx, model, system, prompt, schema)
			return raw, u.CostUSD, err
		}
	}
	small, big := completer(*model), completer(*large)
	var mu sync.Mutex
	var spent float64
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	for _, j := range js {
		for _, rk := range strings.Split(*rankers, ",") {
			wg.Add(1)
			sem <- struct{}{}
			go func(j job, rk string) {
				defer func() { <-sem; wg.Done() }()
				r := measure(context.Background(), j, rk, shares, small, big, maxPicks)
				b, _ := json.Marshal(r)
				mu.Lock()
				defer mu.Unlock()
				spent += r.CostUSD
				f.Write(append(b, '\n'))
				status := fmt.Sprintf("need %.2f", r.NeedFrac)
				if r.Err != "" {
					status = "error: " + r.Err
				}
				fmt.Fprintf(os.Stderr, "%-8s %-28s %-40s %s  ($%.2f so far)\n", rk, j.id, j.file, status, spent)
			}(j, rk)
		}
	}
	wg.Wait()
}

func measure(ctx context.Context, j job, rk string, shares []float64, small, big cola.Completer, maxPicks int) record {
	d := j.doc
	r := record{Track: j.track, ID: j.id, File: j.file, Ranker: rk, Lines: len(d.Lines), Blocks: len(d.Blocks),
		Tokens: d.TotalTokens(), GoldBlocks: len(j.gold), Covered: map[string]bool{}, Recall: map[string]float64{}}
	var ranking []string
	switch rk {
	case "trace", "haiku+trace":
		if j.trace == nil || j.trace.Err != "" {
			r.Err = "no trace"
			if j.trace != nil {
				r.Err += ": " + j.trace.Err
			}
			return r
		}
		raised, called := blockIDs(&j, j.trace.Raised), blockIDs(&j, j.trace.Called)
		if rk == "trace" {
			ranking = tiered(cola.Lexical(d, j.task), raised, called)
			break
		}
		m, err := cola.Model(ctx, small, d, j.task, "", cola.ModelOpts{Max: maxPicks, MaxPrompt: 70000, Samples: 1})
		r.CostUSD = m.CostUSD
		if err != nil {
			r.Err = err.Error()
			return r
		}
		ranking = tiered(m.Ranking, raised, m.Picks, called)
	case "head":
		ranking = cola.Head(d)
	case "random":
		h := fnv.New64a()
		h.Write([]byte(j.id + j.file))
		ranking = cola.Random(d, int64(h.Sum64()))
	case "lexical":
		ranking = cola.Lexical(d, j.task)
	case "haiku", "haiku-wide", "haiku-x3", "opus-outline":
		o := cola.ModelOpts{Max: maxPicks, MaxPrompt: 70000, Samples: 1}
		c := small
		switch rk {
		case "haiku-wide", "haiku-x3":
			o.Max, o.Wide = maxPicks*3/2, true
		case "opus-outline":
			o.MaxPrompt, c = 0, big // the large model sees only each block's first lines
		}
		if rk == "haiku-x3" {
			o.Samples = 3
		}
		m, err := cola.Model(ctx, c, d, j.task, "", o)
		r.CostUSD, r.Outline = m.CostUSD, m.Outline
		if err != nil {
			r.Err = err.Error()
			return r
		}
		ranking = m.Ranking
		r.Picks = len(m.Picks)
		kept := map[string]bool{}
		t := 0
		for _, id := range m.Picks {
			kept[id] = true
			t += d.BlockTokens(d.Index(id))
		}
		r.PicksFrac = float64(t) / float64(r.Tokens)
		r.PicksCover = allKept(d, j.gold, kept)
	default:
		r.Err = "unknown ranker"
		return r
	}
	if j.trace != nil && j.trace.Err == "" {
		st := &traceStats{traceInfo: *j.trace, Raised: len(j.trace.Raised), Called: len(j.trace.Called)}
		in := func(set []int) (all, any bool) {
			has := map[int]bool{}
			for _, b := range set {
				has[b] = true
			}
			all = true
			for _, g := range j.gold {
				all = all && has[g]
				any = any || has[g]
			}
			return
		}
		st.GoldInRaised, st.AnyRaised = in(j.trace.Raised)
		st.GoldInCalled, _ = in(j.trace.Called)
		t := 0
		for _, b := range j.trace.Called {
			t += d.BlockTokens(b)
		}
		st.CalledFrac = float64(t) / float64(r.Tokens)
		r.Trace = st
	}
	// Tokens read in rank order until the last gold block is in.
	pos := map[string]int{}
	for i, id := range ranking {
		pos[id] = i
	}
	last := -1
	for _, g := range j.gold {
		if p := pos[d.Blocks[g].ID]; p > last {
			last = p
		}
	}
	t := 0
	for _, id := range ranking[:last+1] {
		t += d.BlockTokens(d.Index(id))
	}
	r.NeedFrac = float64(t) / float64(r.Tokens)
	for _, s := range shares {
		kept := cola.Keep(d, ranking, int(s*float64(r.Tokens)))
		k := strconv.FormatFloat(s, 'f', -1, 64)
		r.Covered[k] = allKept(d, j.gold, kept)
		n := 0
		for _, g := range j.gold {
			if kept[d.Blocks[g].ID] {
				n++
			}
		}
		r.Recall[k] = float64(n) / float64(len(j.gold))
	}
	return r
}

func allKept(d *cola.Doc, gold []int, kept map[string]bool) bool {
	for _, g := range gold {
		if !kept[d.Blocks[g].ID] {
			return false
		}
	}
	return true
}

func sweJobs(instDir, reposDir, only string) ([]job, error) {
	paths, _ := filepath.Glob(filepath.Join(instDir, "*.json"))
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		if id != "" {
			want[id] = true
		}
	}
	slicer, _ := filepath.Abs("adapters/python/slice.py")
	var js []job
	for _, p := range paths {
		if strings.HasSuffix(p, ".raw.json") {
			continue
		}
		s, err := bench.LoadSWE(p)
		if err != nil {
			return nil, err
		}
		if len(want) > 0 && !want[s.ID] {
			continue
		}
		for file, lines := range s.GoldLinesByFile() {
			path := filepath.Join(reposDir, s.ID, file)
			src, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", s.ID, err)
			}
			var spans []cola.Span
			if strings.HasSuffix(file, ".py") {
				spans, err = pySpans(slicer, path)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %v", s.ID, file, err)
				}
			}
			d := cola.FromSpans(string(src), spans, 40)
			seen := map[int]bool{}
			var gold []int
			for _, l := range lines {
				b := d.BlockOf(l)
				if b < 0 {
					b = len(d.Blocks) - 1
				}
				if !seen[b] {
					seen[b] = true
					gold = append(gold, b)
				}
			}
			sort.Ints(gold)
			js = append(js, job{track: "swe", id: s.ID, file: file, task: s.Problem, doc: d, gold: gold})
		}
	}
	return js, nil
}

func pySpans(slicer, path string) ([]cola.Span, error) {
	out, err := exec.Command("python3", slicer, path, "--list").Output()
	if err != nil {
		return nil, err
	}
	var l struct {
		Symbols []struct {
			Q     string `json:"q"`
			Cls   bool   `json:"cls"`
			Start int    `json:"start"`
			End   int    `json:"end"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, err
	}
	var spans []cola.Span
	for _, s := range l.Symbols {
		spans = append(spans, cola.Span{Label: s.Q, Start: s.Start, End: s.End, Container: s.Cls})
	}
	return spans, nil
}

// hotpotJobs reads rows files saved from the Hugging Face datasets server
// (hotpotqa/hotpot_qa, config distractor, split validation).
func hotpotJobs(dir string, n int) ([]job, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(paths)
	var js []job
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var page struct {
			Rows []struct {
				Row struct {
					ID       string `json:"id"`
					Question string `json:"question"`
					Support  struct {
						Title []string `json:"title"`
						Sent  []int    `json:"sent_id"`
					} `json:"supporting_facts"`
					Context struct {
						Title     []string   `json:"title"`
						Sentences [][]string `json:"sentences"`
					} `json:"context"`
				} `json:"row"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		for _, row := range page.Rows {
			q := row.Row
			var units, labels []string
			at := map[string]int{}
			for pi, title := range q.Context.Title {
				for si, s := range q.Context.Sentences[pi] {
					at[title+"\x00"+strconv.Itoa(si)] = len(units)
					units = append(units, "["+title+"] "+strings.TrimSpace(s))
					labels = append(labels, fmt.Sprintf("%s #%d", title, si))
				}
			}
			var gold []int
			for i, title := range q.Support.Title {
				if b, ok := at[title+"\x00"+strconv.Itoa(q.Support.Sent[i])]; ok {
					gold = append(gold, b)
				}
			}
			if len(gold) == 0 {
				continue // a supporting fact that points past its paragraph
			}
			js = append(js, job{track: "hotpot", id: q.ID, task: q.Question, doc: cola.FromUnits(units, labels), gold: gold})
			if len(js) == n {
				return js, nil
			}
		}
	}
	return js, nil
}
