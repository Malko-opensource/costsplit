package cola

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"sort"
	"strings"
)

// Head ranks blocks in document order: plain truncation.
func Head(d *Doc) []string {
	out := make([]string, len(d.Blocks))
	for i, b := range d.Blocks {
		out[i] = b.ID
	}
	return out
}

// Random ranks blocks in a seeded random order: the floor any ranker must beat.
func Random(d *Doc, seed int64) []string {
	out := Head(d)
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

var wordRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]*`)

// terms splits text into lowercase words, also splitting snake_case and
// camelCase, and drops words shorter than three letters.
func terms(s string) []string {
	var out []string
	for _, w := range wordRE.FindAllString(s, -1) {
		parts := []string{w}
		if sub := splitCamel(w); len(sub) > 1 {
			parts = append(parts, sub...)
		}
		for _, p := range parts {
			if p = strings.ToLower(p); len(p) >= 3 {
				out = append(out, p)
			}
		}
	}
	return out
}

func splitCamel(w string) []string {
	var out []string
	start := 0
	for i := 1; i < len(w); i++ {
		if w[i] >= 'A' && w[i] <= 'Z' && w[i-1] >= 'a' && w[i-1] <= 'z' {
			out = append(out, w[start:i])
			start = i
		}
	}
	return append(out, w[start:])
}

// Lexical ranks blocks by BM25 against the task text, ties in document
// order. It is free and deterministic: the baseline a model ranker must beat.
func Lexical(d *Doc, task string) []string {
	q := map[string]bool{}
	for _, t := range terms(task) {
		q[t] = true
	}
	n := len(d.Blocks)
	tf := make([]map[string]int, n)
	lens := make([]int, n)
	df := map[string]int{}
	total := 0
	for i, b := range d.Blocks {
		tf[i] = map[string]int{}
		text := strings.Join(d.Lines[b.Start-1:b.End], "\n") + " " + b.Label
		for _, t := range terms(text) {
			tf[i][t]++
			lens[i]++
		}
		for t := range tf[i] {
			df[t]++
		}
		total += lens[i]
	}
	avg := float64(total) / math.Max(1, float64(n))
	const k1, bb = 1.2, 0.75
	score := make([]float64, n)
	for i := range d.Blocks {
		for t := range q {
			f := float64(tf[i][t])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (float64(n)-float64(df[t])+0.5)/(float64(df[t])+0.5))
			score[i] += idf * f * (k1 + 1) / (f + k1*(1-bb+bb*float64(lens[i])/math.Max(1, avg)))
		}
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
	out := make([]string, n)
	for i, j := range idx {
		out[i] = d.Blocks[j].ID
	}
	return out
}

// Completer asks a model for JSON matching schema and reports what it cost.
type Completer func(ctx context.Context, system, prompt string, schema map[string]any) (json.RawMessage, float64, error)

// ModelRank is a model's ranking: the blocks it picked, then the rest in
// lexical order so a budget larger than its picks is still filled.
type ModelRank struct {
	Picks   []string `json:"picks"`
	Ranking []string `json:"-"`
	CostUSD float64  `json:"cost_usd"`
	// Outline is true when the text was too long to show whole and the model
	// saw only the first lines of each block.
	Outline bool `json:"outline,omitempty"`
}

const rankSystem = `You decide which parts of a tool output an agent must read to do its task.
The output is split into blocks with ids. Name the blocks the agent needs, most important first.
Answer with block ids only.`

const wideNote = `Missing a block the agent needs costs far more than including one it does not:
include every block that may have to change or that the change depends on. When unsure, include it.`

// ModelOpts sets how a model ranks.
type ModelOpts struct {
	Max       int  // most blocks one answer may name
	MaxPrompt int  // above this many tokens the text is shown as an outline
	Wide      bool // ask for recall: include a block when unsure
	Samples   int  // answers to ask for; blocks are ordered by how many named them
}

// Model asks a model which blocks the task needs. History is what the agent
// did so far (may be empty). Text longer than MaxPrompt tokens is shown as an
// outline: each block's first two lines. With several samples a block named
// by more answers ranks higher; ties go to the better average position.
func Model(ctx context.Context, c Completer, d *Doc, task, history string, o ModelOpts) (ModelRank, error) {
	r := ModelRank{}
	body := renderBlocks(d, 0)
	if EstTokens(body)+EstTokens(task)+EstTokens(history) > o.MaxPrompt {
		body, r.Outline = renderBlocks(d, 2), true
	}
	var p strings.Builder
	fmt.Fprintf(&p, "Task:\n%s\n\n", strings.TrimSpace(task))
	if history != "" {
		fmt.Fprintf(&p, "What the agent did so far:\n%s\n\n", strings.TrimSpace(history))
	}
	fmt.Fprintf(&p, "Tool output, in blocks:\n%s\n\nName at most %d blocks the agent must read, most important first.", body, o.Max)
	system := rankSystem
	if o.Wide {
		system += "\n" + wideNote
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"blocks": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": Head(d)}, "maxItems": o.Max},
		},
		"required":             []string{"blocks"},
		"additionalProperties": false,
	}
	votes, posSum := map[string]int{}, map[string]int{}
	n := o.Samples
	if n < 1 {
		n = 1
	}
	for k := 0; k < n; k++ {
		raw, cost, err := c(ctx, system, p.String(), schema)
		r.CostUSD += cost
		if err != nil {
			return r, err
		}
		var out struct {
			Blocks []string `json:"blocks"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return r, err
		}
		seen := map[string]bool{}
		for _, id := range out.Blocks {
			if d.Index(id) >= 0 && !seen[id] {
				seen[id] = true
				posSum[id] += len(seen)
				votes[id]++
				if votes[id] == 1 {
					r.Picks = append(r.Picks, id)
				}
			}
		}
	}
	sort.SliceStable(r.Picks, func(a, b int) bool {
		x, y := r.Picks[a], r.Picks[b]
		if votes[x] != votes[y] {
			return votes[x] > votes[y]
		}
		return posSum[x]*votes[y] < posSum[y]*votes[x]
	})
	r.Ranking = append([]string{}, r.Picks...)
	for _, id := range Lexical(d, task+"\n"+history) {
		if votes[id] == 0 {
			r.Ranking = append(r.Ranking, id)
		}
	}
	return r, nil
}

// renderBlocks shows every block under a header; with head > 0 only its
// first head lines.
func renderBlocks(d *Doc, head int) string {
	var b strings.Builder
	for _, bl := range d.Blocks {
		fmt.Fprintf(&b, "<<%s lines %d-%d %s>>\n", bl.ID, bl.Start, bl.End, bl.Label)
		end := bl.End
		if head > 0 && bl.Start+head-1 < end {
			end = bl.Start + head - 1
		}
		for ln := bl.Start; ln <= end; ln++ {
			b.WriteString(d.Lines[ln-1])
			b.WriteByte('\n')
		}
		if end < bl.End {
			fmt.Fprintf(&b, "... (%d more lines)\n", bl.End-end)
		}
	}
	return b.String()
}
