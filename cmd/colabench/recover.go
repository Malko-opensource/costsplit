package main

// recover: stage 3 of the diet-cola plan. Stage 1 showed that a one-shot cut
// hides the code a fix needs about a third of the time. Here the large model
// reads the cut file, whose markers name every hidden function, and may ask
// to see hidden functions before it says where the fix goes. Arms:
//
//	full   the large model reads the whole file (twice: full, full2, so the
//	       agreement between two full reads is the noise floor)
//	cola   Haiku picks blocks, the cut keeps them plus the best of the rest
//	       up to the budget, files under -small tokens pass whole, and the
//	       large model may ask for hidden functions up to -rounds times
//	inject fault injection: the cola view (or the whole small file) with the
//	       functions a full read named (-ref, its "full" arm) hidden too.
//	       Does the large model notice and ask for them?
//
// What it measures: whether the code the reference patch changes was visible
// before and after asking, whether the answer names it, whether the answer
// agrees with a full read, and what the large model read.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/Malko-opensource/costsplit/internal/bench"
	"github.com/Malko-opensource/costsplit/internal/cola"
)

type recoverRecord struct {
	ID            string   `json:"id"`
	File          string   `json:"file"`
	Arm           string   `json:"arm"`
	Tokens        int      `json:"tokens"`         // the whole file
	ViewTokens    []int    `json:"view_tokens"`    // what the large model read, per round
	Passed        bool     `json:"passed"`         // small file, shown whole
	Rounds        int      `json:"rounds"`         // large-model calls
	Asked         []string `json:"asked"`          // hidden functions it asked for
	FixIn         []string `json:"fix_in"`         // where it says the fix goes
	Gold          []string `json:"gold"`           // labels of the blocks the patch changes
	VisibleBefore bool     `json:"visible_before"` // every gold block in the first view
	VisibleAfter  bool     `json:"visible_after"`  // ... in the last view
	HitAll        bool     `json:"hit_all"`        // the answer names every gold label
	HitAny        bool     `json:"hit_any"`
	LargeUSD      float64  `json:"large_usd"`
	SmallUSD      float64  `json:"small_usd"`
	LargeIn       int      `json:"large_in"`
	LargeThink    int      `json:"large_think"`
	Injected      []string `json:"injected,omitempty"` // inject: functions hidden on purpose
	AskedInjected bool     `json:"asked_injected,omitempty"`
	// With -quote the answer must also copy one line the fix changes.
	Quote       string `json:"quote,omitempty"`
	QuoteInFile bool   `json:"quote_in_file,omitempty"` // the line exists in the file
	QuoteSeen   bool   `json:"quote_seen,omitempty"`    // ... in a part the model was shown
	QuoteInGold bool   `json:"quote_in_gold,omitempty"` // ... in a block the patch changes
	// With -gate an answer that quotes a line it was not shown is sent back:
	// the hidden blocks holding that line, and the hidden functions it named,
	// are shown, and the model answers again.
	GateFired int      `json:"gate_fired,omitempty"`
	GateShown []string `json:"gate_shown,omitempty"`
	Err       string   `json:"error,omitempty"`
}

const locateSystem = `You find where a bug fix goes. You get a bug report and one source file.
Answer with the functions the fix must change, or the function or class body next to which new code must be added.`

const hiddenNote = `Parts of the file are hidden to save reading. Each hidden run is one line:
"… [eN: lines A-B hidden: names]" naming every function in it.
If you cannot decide without reading hidden code, list in "show" exactly the functions to show you, and leave "fix_in" empty.
Reading costs money: ask only for what you need. When what you see is enough, answer "fix_in" and leave "show" empty.`

const quoteNote = `Also copy into "line" one line of the file, exactly as written (without its line number), that the fix changes, or after which new code goes.`

func recoverMain(args []string) {
	fs := flag.NewFlagSet("recover", flag.ExitOnError)
	out := fs.String("out", "", "results file (jsonl, appended)")
	instDir := fs.String("inst-dir", "", "instance json files")
	reposDir := fs.String("repos-dir", "", "checkouts at base_commit")
	ids := fs.String("ids", "", "only these instance ids")
	arms := fs.String("arms", "full,full2,cola", "arms")
	small := fs.String("model", "haiku", "small model (picks blocks)")
	large := fs.String("large", "opus", "large model (locates the fix)")
	budget := fs.Float64("budget", 0.2, "cola: share of the file's tokens kept")
	passTokens := fs.Int("small", 4000, "cola: files under this many tokens are shown whole")
	rounds := fs.Int("rounds", 2, "cola: requests to see hidden code")
	jobs := fs.Int("j", 4, "parallel tasks")
	refPath := fs.String("ref", "", "inject: results file holding the full arm")
	quote := fs.Bool("quote", false, "also ask for one line the fix changes, copied exactly")
	gate := fs.Int("gate", 0, "with -quote: times an answer quoting unseen code is sent back")
	_ = fs.Parse(args)
	ref := map[string][]string{}
	if *refPath != "" {
		b, err := os.ReadFile(*refPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var r recoverRecord
			if json.Unmarshal([]byte(l), &r) == nil && r.Arm == "full" && r.Err == "" {
				ref[r.ID] = r.FixIn
			}
		}
	}
	js, err := sweJobs(*instDir, *reposDir, *ids)
	if err != nil || *out == "" {
		fmt.Fprintln(os.Stderr, "recover:", err, "(-out is required)")
		os.Exit(2)
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	cli := &bench.ClaudeCLI{}
	var mu sync.Mutex
	var spent float64
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup
	for _, j := range js {
		for _, arm := range strings.Split(*arms, ",") {
			wg.Add(1)
			sem <- struct{}{}
			go func(j job, arm string) {
				defer func() { <-sem; wg.Done() }()
				r := locate(context.Background(), cli, *small, *large, j, arm, *budget, *passTokens, *rounds, ref[j.id], *quote, *gate)
				b, _ := json.Marshal(r)
				mu.Lock()
				defer mu.Unlock()
				spent += r.LargeUSD + r.SmallUSD
				f.Write(append(b, '\n'))
				status := fmt.Sprintf("rounds %d asked %d visible %v->%v hit %v", r.Rounds, len(r.Asked), r.VisibleBefore, r.VisibleAfter, r.HitAll)
				if r.Err != "" {
					status = "error: " + r.Err
				}
				fmt.Fprintf(os.Stderr, "%-6s %-28s %s  ($%.2f so far)\n", arm, j.id, status, spent)
			}(j, arm)
		}
	}
	wg.Wait()
}

func locate(ctx context.Context, cli *bench.ClaudeCLI, small, large string, j job, arm string, budget float64, passTokens, rounds int, refFix []string, quote bool, gate int) recoverRecord {
	d := j.doc
	r := recoverRecord{ID: j.id, File: j.file, Arm: arm, Tokens: d.TotalTokens()}
	goldSet := map[string]bool{}
	for _, g := range j.gold {
		goldSet[d.Blocks[g].Label] = true
	}
	for l := range goldSet {
		r.Gold = append(r.Gold, l)
	}
	sort.Strings(r.Gold)
	kept := map[string]bool{}
	showAll := func() {
		for _, b := range d.Blocks {
			kept[b.ID] = true
		}
	}
	cut := arm == "cola" || arm == "inject"
	if arm == "inject" && len(refFix) == 0 {
		r.Err = "inject: no full-read answer to hide"
		return r
	}
	switch {
	case !cut:
		showAll()
	case r.Tokens < passTokens:
		showAll()
		r.Passed, cut = true, arm == "inject"
	default:
		smallCall := func(ctx context.Context, system, prompt string, schema map[string]any) (json.RawMessage, float64, error) {
			raw, u, err := cli.Complete(ctx, small, system, prompt, schema)
			return raw, u.CostUSD, err
		}
		m, err := cola.Model(ctx, smallCall, d, j.task, "", cola.ModelOpts{Max: 8, MaxPrompt: 70000, Samples: 1})
		r.SmallUSD = m.CostUSD
		if err != nil {
			r.Err = "small: " + err.Error()
			return r
		}
		kept = cola.Keep(d, m.Ranking, int(budget*float64(r.Tokens)))
		for _, id := range m.Picks { // the picks stay even past the budget
			kept[id] = true
		}
	}
	if arm == "inject" {
		hide := map[string]bool{}
		for _, l := range refFix {
			hide[l] = true
		}
		r.Injected = refFix
		for _, b := range d.Blocks {
			if hide[b.Label] {
				delete(kept, b.ID)
			}
		}
	}
	visible := func() bool {
		for _, g := range j.gold {
			if !kept[d.Blocks[g].ID] {
				return false
			}
		}
		return true
	}
	r.VisibleBefore = visible()
	labels := uniqLabels(d, nil)
	note := ""
	for round := 0; ; round++ {
		v := cola.RenderNames(d, kept, 0)
		r.ViewTokens = append(r.ViewTokens, cola.EstTokens(v.Text))
		hidden := uniqLabels(d, kept)
		ask := cut && round < rounds && len(hidden) > 0
		system := locateSystem
		props := map[string]any{"fix_in": enumArray(labels, 8)}
		req := []string{"fix_in"}
		if quote {
			system += "\n" + quoteNote
			props["line"] = map[string]any{"type": "string"}
			req = append(req, "line")
		}
		if ask {
			system += "\n" + hiddenNote
			props["show"] = enumArray(hidden, 12)
			req = append(req, "show")
		}
		schema := map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
		prompt := fmt.Sprintf("Bug report:\n%s\n\nFile %s:\n%s\n%s", strings.TrimSpace(j.task), j.file, v.Text, note)
		raw, u, err := cli.Complete(ctx, large, system, prompt, schema)
		r.Rounds++
		r.LargeUSD += u.CostUSD
		r.LargeIn += u.InputTokens
		r.LargeThink += u.ThinkingTokens
		if err != nil {
			r.Err = "large: " + err.Error()
			return r
		}
		var a struct {
			FixIn []string `json:"fix_in"`
			Show  []string `json:"show"`
			Line  string   `json:"line"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			r.Err = err.Error()
			return r
		}
		if ask && len(a.Show) > 0 {
			r.Asked = append(r.Asked, a.Show...)
			for _, l := range a.Show {
				for _, x := range r.Injected {
					r.AskedInjected = r.AskedInjected || l == x
				}
			}
			want := map[string]bool{}
			for _, l := range a.Show {
				want[l] = true
			}
			for _, b := range d.Blocks {
				if want[b.Label] {
					kept[b.ID] = true
				}
			}
			continue
		}
		r.FixIn, r.Quote = a.FixIn, a.Line
		if q := cola.CheckQuote(d, kept, a.Line); quote && r.GateFired < gate && !q.Shown {
			r.GateFired++
			show := map[int]bool{}
			for _, b := range q.Hidden {
				show[b] = true
			}
			named := map[string]bool{}
			for _, l := range a.FixIn {
				named[l] = true
			}
			for i, b := range d.Blocks {
				if named[b.Label] && !kept[b.ID] {
					show[i] = true
				}
			}
			listed := map[string]bool{}
			for i := range show {
				kept[d.Blocks[i].ID] = true
				if l := d.Blocks[i].Label; !listed[l] {
					listed[l] = true
					r.GateShown = append(r.GateShown, l)
				}
			}
			note = "\nYour previous answer quoted a line that is not in the code you were shown. " +
				"Answer again, and copy \"line\" only from code shown above."
			if len(show) > 0 {
				note = "\nYour previous answer quoted a line that is not in the code you were shown. " +
					"The code it may come from is now shown above. Answer again, and copy \"line\" only from code shown above."
			}
			continue
		}
		break
	}
	if q := strings.TrimSpace(r.Quote); q != "" {
		goldBlock := map[int]bool{}
		for _, g := range j.gold {
			goldBlock[g] = true
		}
		for i, l := range d.Lines {
			if strings.TrimSpace(l) != q {
				continue
			}
			b := d.BlockOf(i + 1)
			r.QuoteInFile = true
			r.QuoteSeen = r.QuoteSeen || kept[d.Blocks[b].ID]
			r.QuoteInGold = r.QuoteInGold || goldBlock[b]
		}
	}
	r.VisibleAfter = visible()
	r.HitAll, r.HitAny = len(r.Gold) > 0, false
	named := map[string]bool{}
	for _, l := range r.FixIn {
		named[l] = true
	}
	for _, g := range r.Gold {
		r.HitAll = r.HitAll && named[g]
		r.HitAny = r.HitAny || named[g]
	}
	return r
}

// uniqLabels lists block labels in order, only of blocks not kept (nil: all).
func uniqLabels(d *cola.Doc, kept map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range d.Blocks {
		if kept != nil && kept[b.ID] {
			continue
		}
		if !seen[b.Label] {
			seen[b.Label] = true
			out = append(out, b.Label)
		}
	}
	return out
}

func enumArray(vals []string, max int) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": vals}, "maxItems": max}
}
