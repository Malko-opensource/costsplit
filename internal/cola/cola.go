// Package cola shrinks what an agent's expensive model reads.
//
// A tool output (a file, a log, search results) is split into blocks that
// partition its lines. A ranker orders the blocks by how much the task needs
// them; Keep takes blocks in that order until a token budget is spent; Render
// prints the kept blocks verbatim with their line numbers and replaces each
// run of dropped blocks with a one-line marker naming what it hides. Expand
// returns the hidden lines exactly, so nothing is lost, only deferred.
//
// The compression is extractive on purpose: kept text is the original text,
// so an editor can still match it, and a wrong choice is recoverable.
package cola

import (
	"fmt"
	"sort"
	"strings"
)

// Block is the run of lines Start..End (1-based, inclusive).
type Block struct {
	ID    string `json:"id"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Label string `json:"label"`
}

// Doc is a text and a partition of its lines into blocks, in order.
type Doc struct {
	Lines  []string
	Blocks []Block
}

// EstTokens estimates tokens as one per four bytes, at least one.
func EstTokens(s string) int { return estBytes(len(s)) }

func estBytes(n int) int {
	if n < 4 {
		return 1
	}
	return (n + 3) / 4
}

// BlockTokens is the estimated size of block i, newlines included.
func (d *Doc) BlockTokens(i int) int {
	b := d.Blocks[i]
	n := 0
	for _, l := range d.Lines[b.Start-1 : b.End] {
		n += len(l) + 1
	}
	return estBytes(n)
}

// TotalTokens is the estimated size of the whole text.
func (d *Doc) TotalTokens() int {
	t := 0
	for i := range d.Blocks {
		t += d.BlockTokens(i)
	}
	return t
}

// Index returns the position of the block with the given id, or -1.
func (d *Doc) Index(id string) int {
	for i, b := range d.Blocks {
		if b.ID == id {
			return i
		}
	}
	return -1
}

// BlockOf returns the position of the block holding line (1-based), or -1.
func (d *Doc) BlockOf(line int) int {
	i := sort.Search(len(d.Blocks), func(i int) bool { return d.Blocks[i].End >= line })
	if i < len(d.Blocks) && d.Blocks[i].Start <= line {
		return i
	}
	return -1
}

// Span is a named region of a text, such as a function or a class.
// A container (a class) holds other spans; its own lines are those no
// function inside it claims.
type Span struct {
	Label     string
	Start     int
	End       int
	Container bool
}

// FromSpans partitions text along spans. A line belongs to the outermost
// function that contains it (nested helpers stay with their function), else
// to the innermost container, else to the module. A run that belongs to no
// function is cut every maxRun lines. Runs of blank lines join the run before.
func FromSpans(text string, spans []Span, maxRun int) *Doc {
	lines := strings.Split(text, "\n")
	owner := make([]int, len(lines)) // span index, or -1 for the module
	for ln := 1; ln <= len(lines); ln++ {
		fn, box := -1, -1
		for i, s := range spans {
			if s.Start > ln || s.End < ln {
				continue
			}
			size := s.End - s.Start
			if !s.Container && (fn < 0 || size > spans[fn].End-spans[fn].Start) {
				fn = i
			}
			if s.Container && (box < 0 || size < spans[box].End-spans[box].Start) {
				box = i
			}
		}
		owner[ln-1] = box
		if fn >= 0 {
			owner[ln-1] = fn
		}
	}
	label := func(o int) string {
		switch {
		case o < 0:
			return "module"
		case spans[o].Container:
			return spans[o].Label + " (body)"
		}
		return spans[o].Label
	}
	isFn := func(o int) bool { return o >= 0 && !spans[o].Container }
	return partition(lines, func(i int) (string, bool) { return label(owner[i]), isFn(owner[i]) },
		func(i int) int { return owner[i] }, maxRun)
}

// Paragraphs partitions plain text at blank lines, cutting long paragraphs
// every maxRun lines.
func Paragraphs(text string, maxRun int) *Doc {
	lines := strings.Split(text, "\n")
	para := make([]int, len(lines))
	p := 0
	for i, l := range lines {
		if strings.TrimSpace(l) == "" && i > 0 && strings.TrimSpace(lines[i-1]) != "" {
			p++
		}
		para[i] = p
	}
	return partition(lines, func(i int) (string, bool) { return firstWords(lines, para, i), false },
		func(i int) int { return para[i] }, maxRun)
}

// FromUnits makes one single-line block per unit (a sentence, a search hit).
// Newlines inside a unit become spaces.
func FromUnits(units, labels []string) *Doc {
	d := &Doc{}
	for i, u := range units {
		d.Lines = append(d.Lines, strings.ReplaceAll(u, "\n", " "))
		l := ""
		if i < len(labels) {
			l = labels[i]
		}
		d.Blocks = append(d.Blocks, Block{ID: fmt.Sprintf("b%d", i+1), Start: i + 1, End: i + 1, Label: l})
	}
	return d
}

func firstWords(lines []string, para []int, i int) string {
	for j := i; j < len(lines) && para[j] == para[i]; j++ {
		if s := strings.TrimSpace(lines[j]); s != "" {
			if len(s) > 40 {
				s = s[:40]
			}
			return s
		}
	}
	return ""
}

// partition groups consecutive lines with the same key into blocks.
func partition(lines []string, info func(int) (string, bool), key func(int) int, maxRun int) *Doc {
	type run struct{ start, end, key int }
	var runs []run
	for i := range lines {
		if n := len(runs); n > 0 && runs[n-1].key == key(i) {
			runs[n-1].end = i
			continue
		}
		runs = append(runs, run{i, i, key(i)})
	}
	// Blank-only runs (the gap between two methods) join the run before.
	var merged []run
	for _, r := range runs {
		blank := true
		for i := r.start; i <= r.end; i++ {
			if strings.TrimSpace(lines[i]) != "" {
				blank = false
				break
			}
		}
		if blank && len(merged) > 0 {
			merged[len(merged)-1].end = r.end
			continue
		}
		merged = append(merged, r)
	}
	d := &Doc{Lines: lines}
	for _, r := range merged {
		lab, fn := info(r.start)
		step := r.end - r.start + 1
		if !fn && maxRun > 0 {
			step = maxRun
		}
		for s := r.start; s <= r.end; s += step {
			e := s + step - 1
			if e > r.end {
				e = r.end
			}
			d.Blocks = append(d.Blocks, Block{ID: fmt.Sprintf("b%d", len(d.Blocks)+1), Start: s + 1, End: e + 1, Label: lab})
		}
	}
	return d
}

// Keep walks the ranking and keeps each block that still fits the budget.
// A block too large for what is left is skipped, and smaller ones after it
// may still fit. Unknown and repeated ids are ignored.
func Keep(d *Doc, ranking []string, budget int) map[string]bool {
	kept := map[string]bool{}
	used := 0
	for _, id := range ranking {
		i := d.Index(id)
		if i < 0 || kept[id] {
			continue
		}
		if t := d.BlockTokens(i); used+t <= budget {
			kept[id] = true
			used += t
		}
	}
	return kept
}

// Hidden is a run of dropped blocks, restorable with Expand.
type Hidden struct {
	Start  int      `json:"start"`
	End    int      `json:"end"`
	Labels []string `json:"labels"`
}

// View is what the model reads instead of the text.
type View struct {
	Text       string
	Hidden     map[string]Hidden
	KeptTokens int
}

// Render prints kept blocks as numbered lines ("  12| text") and each run of
// dropped blocks as one marker line ("… [e1: lines 30-80 hidden: f, g]")
// naming up to six of the hidden blocks.
func Render(d *Doc, kept map[string]bool) View { return RenderNames(d, kept, 6) }

// RenderNames is Render naming up to maxNames hidden blocks per marker; 0
// names them all, so a reader can ask for any hidden function by name.
func RenderNames(d *Doc, kept map[string]bool, maxNames int) View {
	v := View{Hidden: map[string]Hidden{}}
	var out []string
	width := len(fmt.Sprint(len(d.Lines)))
	var cur *Hidden
	flush := func() {
		if cur == nil {
			return
		}
		id := fmt.Sprintf("e%d", len(v.Hidden)+1)
		v.Hidden[id] = *cur
		names := cur.Labels
		more := ""
		if maxNames > 0 && len(names) > maxNames {
			names, more = names[:maxNames], fmt.Sprintf(" and %d more", len(cur.Labels)-maxNames)
		}
		out = append(out, fmt.Sprintf("… [%s: lines %d-%d hidden: %s%s]", id, cur.Start, cur.End, strings.Join(names, ", "), more))
		cur = nil
	}
	for i, b := range d.Blocks {
		if kept[b.ID] {
			flush()
			v.KeptTokens += d.BlockTokens(i)
			for ln := b.Start; ln <= b.End; ln++ {
				out = append(out, fmt.Sprintf("%*d| %s", width, ln, d.Lines[ln-1]))
			}
			continue
		}
		if cur == nil {
			cur = &Hidden{Start: b.Start}
		}
		cur.End = b.End
		if n := len(cur.Labels); b.Label != "" && (n == 0 || cur.Labels[n-1] != b.Label) {
			cur.Labels = append(cur.Labels, b.Label)
		}
	}
	flush()
	v.Text = strings.Join(out, "\n")
	return v
}

// Expand returns the original lines a marker hides, exactly.
func (d *Doc) Expand(h Hidden) string {
	return strings.Join(d.Lines[h.Start-1:h.End], "\n")
}

// Quote is where a line a model quoted occurs in a text.
type Quote struct {
	InText bool  // the line occurs in the text
	Shown  bool  // ... in a kept block, so the model could have read it
	Hidden []int // positions of hidden blocks where it occurs
}

// CheckQuote looks up a quoted line (compared without surrounding space) in
// the text. A quote that is not Shown was not read: either the model guessed
// or it remembered code it was not given. The gate built on it expands the
// Hidden blocks, or the blocks the model named, and asks again.
func CheckQuote(d *Doc, kept map[string]bool, line string) Quote {
	var q Quote
	line = strings.TrimSpace(line)
	if line == "" {
		return q
	}
	seen := map[int]bool{}
	for i, l := range d.Lines {
		if strings.TrimSpace(l) != line {
			continue
		}
		q.InText = true
		b := d.BlockOf(i + 1)
		if kept[d.Blocks[b].ID] {
			q.Shown = true
		} else if !seen[b] {
			seen[b] = true
			q.Hidden = append(q.Hidden, b)
		}
	}
	return q
}
