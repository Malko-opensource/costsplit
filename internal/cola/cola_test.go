package cola

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The invariants every compressed view must keep, whatever the ranker:
//  1. blocks partition the text: in order, no gaps, no overlap;
//  2. every numbered line in the view is the original line, unchanged;
//  3. expanding every marker restores the original text exactly;
//  4. the kept blocks fit the budget;
//  5. the same inputs give the same view.

var (
	numbered = regexp.MustCompile(`^ *(\d+)\| (.*)$`)
	marker   = regexp.MustCompile(`^… \[(e\d+): lines (\d+)-(\d+) hidden: .*\]$`)
)

func checkPartition(t *testing.T, d *Doc) {
	t.Helper()
	next := 1
	for _, b := range d.Blocks {
		if b.Start != next || b.End < b.Start {
			t.Fatalf("block %s covers %d-%d, want it to start at %d", b.ID, b.Start, b.End, next)
		}
		next = b.End + 1
	}
	if next != len(d.Lines)+1 {
		t.Fatalf("blocks end at line %d of %d", next-1, len(d.Lines))
	}
}

// restore rebuilds the text from a view, expanding every marker.
func restore(t *testing.T, d *Doc, v View) string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(v.Text, "\n") {
		if m := numbered.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			if m[2] != d.Lines[n-1] {
				t.Fatalf("line %d changed: %q, want %q", n, m[2], d.Lines[n-1])
			}
			out = append(out, m[2])
			continue
		}
		m := marker.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("view line is neither text nor marker: %q", l)
		}
		h := v.Hidden[m[1]]
		if fmt.Sprint(h.Start) != m[2] || fmt.Sprint(h.End) != m[3] {
			t.Fatalf("marker %s says %s-%s, records %d-%d", m[1], m[2], m[3], h.Start, h.End)
		}
		out = append(out, d.Expand(h))
	}
	return strings.Join(out, "\n")
}

func checkView(t *testing.T, d *Doc, ranking []string, budget int) {
	t.Helper()
	kept := Keep(d, ranking, budget)
	v := Render(d, kept)
	if v.KeptTokens > budget {
		t.Fatalf("kept %d tokens, budget %d", v.KeptTokens, budget)
	}
	if got := restore(t, d, v); got != strings.Join(d.Lines, "\n") {
		t.Fatalf("expanding the view does not restore the text")
	}
	if again := Render(d, Keep(d, ranking, budget)); again.Text != v.Text {
		t.Fatalf("same inputs, different view")
	}
}

const pySample = `import os


class Printer:
    """Prints things."""

    def print_add(self, e):
        def inner(x):
            return x
        return self.join(e, "+")

    @staticmethod
    def print_mul(e):
        return "*".join(e)

    def join(self, e, op):
        return op.join(e)


def helper(x):
    return x
`

func pySpans() []Span {
	return []Span{
		{Label: "Printer", Start: 4, End: 17, Container: true},
		{Label: "Printer.print_add", Start: 7, End: 10},
		{Label: "Printer.print_add.inner", Start: 8, End: 9},
		{Label: "Printer.print_mul", Start: 12, End: 14},
		{Label: "Printer.join", Start: 16, End: 17},
		{Label: "helper", Start: 20, End: 21},
	}
}

func TestFromSpansKeepsFunctionsWhole(t *testing.T) {
	d := FromSpans(pySample, pySpans(), 40)
	checkPartition(t, d)
	var got []string
	for _, b := range d.Blocks {
		got = append(got, fmt.Sprintf("%d-%d %s", b.Start, b.End, b.Label))
	}
	want := []string{
		"1-3 module",
		"4-6 Printer (body)",
		"7-11 Printer.print_add",  // the nested helper stays inside; the blank line joins
		"12-15 Printer.print_mul", // the decorator is part of the function
		"16-19 Printer.join",
		"20-22 helper",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocks:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if i := d.BlockOf(9); d.Blocks[i].Label != "Printer.print_add" {
		t.Errorf("line 9 is in %s", d.Blocks[i].Label)
	}
}

func TestLongModuleRunsAreCut(t *testing.T) {
	text := strings.Repeat("x = 1\n", 95) + "y = 2"
	d := FromSpans(text, nil, 40)
	checkPartition(t, d)
	if len(d.Blocks) != 3 || d.Blocks[2].End-d.Blocks[2].Start != 15 {
		t.Errorf("got %d blocks: %+v", len(d.Blocks), d.Blocks)
	}
}

func TestRenderMarksHiddenRuns(t *testing.T) {
	d := FromSpans(pySample, pySpans(), 40)
	keep := map[string]bool{d.Blocks[2].ID: true}
	v := Render(d, keep)
	lines := strings.Split(v.Text, "\n")
	if lines[0] != "… [e1: lines 1-6 hidden: module, Printer (body)]" {
		t.Errorf("first marker: %q", lines[0])
	}
	if lines[1] != " 7|     def print_add(self, e):" {
		t.Errorf("first kept line: %q", lines[1])
	}
	if last := lines[len(lines)-1]; last != "… [e2: lines 12-22 hidden: Printer.print_mul, Printer.join, helper]" {
		t.Errorf("last marker: %q", last)
	}
}

func TestKeepSkipsWhatDoesNotFit(t *testing.T) {
	d := FromUnits([]string{strings.Repeat("a", 400), "short", "also short"}, nil)
	kept := Keep(d, []string{"b1", "b2", "nope", "b2", "b3"}, 10)
	if !reflect.DeepEqual(kept, map[string]bool{"b2": true, "b3": true}) {
		t.Errorf("kept %v", kept)
	}
}

func TestLexicalFindsTheNamedFunction(t *testing.T) {
	d := FromSpans(pySample, pySpans(), 40)
	r := Lexical(d, "print_mul returns the wrong separator")
	if d.Blocks[d.Index(r[0])].Label != "Printer.print_mul" {
		t.Errorf("ranked first: %s", d.Blocks[d.Index(r[0])].Label)
	}
}

func TestModelRankFillsWithLexical(t *testing.T) {
	d := FromSpans(pySample, pySpans(), 40)
	var gotSchema map[string]any
	fake := func(_ context.Context, _, prompt string, schema map[string]any) (json.RawMessage, float64, error) {
		gotSchema = schema
		if !strings.Contains(prompt, "<<b3 lines 7-11 Printer.print_add>>") {
			t.Errorf("prompt lacks block headers:\n%s", prompt)
		}
		return json.RawMessage(`{"blocks":["b5","b5","zz"]}`), 0.001, nil
	}
	r, err := Model(context.Background(), fake, d, "join is wrong", "", ModelOpts{Max: 3, MaxPrompt: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Picks, []string{"b5"}) || len(r.Ranking) != len(d.Blocks) || r.Ranking[0] != "b5" {
		t.Errorf("picks %v ranking %v", r.Picks, r.Ranking)
	}
	enum := gotSchema["properties"].(map[string]any)["blocks"].(map[string]any)["items"].(map[string]any)["enum"]
	if len(enum.([]string)) != len(d.Blocks) {
		t.Errorf("enum %v", enum)
	}
	if r2, _ := Model(context.Background(), fake, d, "join", "", ModelOpts{Max: 3, MaxPrompt: 10}); !r2.Outline {
		t.Error("a prompt over the limit should show the outline")
	}
}

// Property test: random texts, spans, rankers and budgets.
func TestInvariantsRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		n := 1 + r.Intn(120)
		lines := make([]string, n)
		for i := range lines {
			switch r.Intn(5) {
			case 0:
				lines[i] = ""
			case 1:
				lines[i] = "  1| looks numbered … [e1: lines 1-2 hidden: x]"
			default:
				lines[i] = strings.Repeat("w", r.Intn(90))
			}
		}
		text := strings.Join(lines, "\n")
		var spans []Span
		for k := r.Intn(8); k > 0; k-- {
			s := 1 + r.Intn(n)
			e := s + r.Intn(n-s+1)
			spans = append(spans, Span{Label: fmt.Sprint("f", k), Start: s, End: e, Container: r.Intn(3) == 0})
		}
		docs := []*Doc{FromSpans(text, spans, 1+r.Intn(40)), Paragraphs(text, 1+r.Intn(40)), FromUnits(lines, nil)}
		for _, d := range docs {
			checkPartition(t, d)
			total := d.TotalTokens()
			for _, ranking := range [][]string{Head(d), Random(d, int64(iter)), Lexical(d, "www looks")} {
				for _, frac := range []float64{0, 0.1, 0.5, 1} {
					checkView(t, d, ranking, int(frac*float64(total)))
				}
			}
		}
	}
}

func TestModelSamplesVote(t *testing.T) {
	d := FromSpans(pySample, pySpans(), 40)
	answers := []string{`{"blocks":["b2","b5"]}`, `{"blocks":["b5","b6"]}`, `{"blocks":["b6","b5"]}`}
	k := 0
	fake := func(_ context.Context, system, _ string, _ map[string]any) (json.RawMessage, float64, error) {
		if !strings.Contains(system, "When unsure, include it") {
			t.Error("wide ranking should ask for recall")
		}
		k++
		return json.RawMessage(answers[k-1]), 0.001, nil
	}
	r, err := Model(context.Background(), fake, d, "join", "", ModelOpts{Max: 3, MaxPrompt: 10000, Wide: true, Samples: 3})
	if err != nil {
		t.Fatal(err)
	}
	// b5 named three times, b6 twice, b2 once.
	if !reflect.DeepEqual(r.Picks, []string{"b5", "b6", "b2"}) || math.Abs(r.CostUSD-0.003) > 1e-9 {
		t.Errorf("picks %v cost %v", r.Picks, r.CostUSD)
	}
}

func TestCheckQuote(t *testing.T) {
	d := FromSpans(pySample, pySpans(), 40)
	kept := map[string]bool{"b3": true} // Printer.print_add
	if q := CheckQuote(d, kept, "  return self.join(e, \"+\")  "); !q.InText || !q.Shown || len(q.Hidden) != 0 {
		t.Errorf("shown line: %+v", q)
	}
	if q := CheckQuote(d, kept, "return op.join(e)"); !q.InText || q.Shown || !reflect.DeepEqual(q.Hidden, []int{4}) {
		t.Errorf("hidden line: %+v", q)
	}
	// "return x" occurs in a shown block (the nested helper) and a hidden one.
	if q := CheckQuote(d, kept, "return x"); !q.Shown || !reflect.DeepEqual(q.Hidden, []int{5}) {
		t.Errorf("line in both: %+v", q)
	}
	if q := CheckQuote(d, kept, "return made_up()"); q.InText || q.Shown {
		t.Errorf("invented line: %+v", q)
	}
	if q := CheckQuote(d, kept, "   "); q.InText {
		t.Errorf("empty quote: %+v", q)
	}
}
