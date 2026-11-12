package bench

// The diet-cola arms: the large model reads a cut view of the file instead of
// the file. Haiku names the blocks the fix needs, the view keeps them plus
// the best of the rest up to a budget, small files pass whole, and every
// hidden run is a marker naming the functions in it. The large model may ask
// to see hidden functions, and a gate sends back any answer whose quoted line
// or edit search text holds a line of the file it was not shown.
//
//	large-cola      the large model reads the view and writes the edits
//	large-colaarch  the large model reads the view and writes instructions;
//	                the small model writes the edits from the same view

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Malko-opensource/costsplit/internal/cola"
)

const (
	colaBudget    = 0.2  // share of the file's tokens the view keeps beyond the picks
	colaPassWhole = 4000 // files under this many (estimated) tokens are shown whole
	colaShows     = 2    // requests to see hidden code
	colaGates     = 2    // times an answer that quotes unseen code is sent back
)

const colaViewNote = `The file is shown with line numbers ("  12| code"); those numbers are not part of the code.
Parts of the file are hidden to save reading. Each hidden run is one line "… [eN: lines A-B hidden: names]" naming every function in it.
If you need hidden code, list exactly the functions to show you in "show" and leave the rest of the answer empty. Ask only for what you need.`

var lineNo = regexp.MustCompile(`^\s*\d+\| ?`)

func colaDoc(env *SWEEnv, work, path string) (*cola.Doc, error) {
	src, err := os.ReadFile(filepath.Join(work, path))
	if err != nil {
		return nil, err
	}
	var spans []cola.Span
	if strings.HasSuffix(path, ".py") {
		out, err := exec.Command(env.Python, filepath.Join(env.RepoRoot, "adapters/python/slice.py"), filepath.Join(work, path), "--list").Output()
		if err != nil {
			return nil, fmt.Errorf("list symbols: %v", err)
		}
		var l struct {
			Symbols []struct {
				Q          string `json:"q"`
				Cls        bool   `json:"cls"`
				Start, End int
			} `json:"symbols"`
		}
		if err := json.Unmarshal(out, &l); err != nil {
			return nil, err
		}
		for _, s := range l.Symbols {
			spans = append(spans, cola.Span{Label: s.Q, Start: s.Start, End: s.End, Container: s.Cls})
		}
	}
	return cola.FromSpans(string(src), spans, 40), nil
}

// colaView is the cut file and what was done to it.
type colaView struct {
	doc    *cola.Doc
	kept   map[string]bool
	asked  []string
	gated  []string
	rounds int
}

func (v *colaView) text() string { return cola.RenderNames(v.doc, v.kept, 0).Text }

func (v *colaView) hidden() []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range v.doc.Blocks {
		if !v.kept[b.ID] && !seen[b.Label] {
			seen[b.Label] = true
			out = append(out, b.Label)
		}
	}
	return out
}

func (v *colaView) show(labels []string) {
	want := map[string]bool{}
	for _, l := range labels {
		want[l] = true
	}
	for _, b := range v.doc.Blocks {
		if want[b.Label] {
			v.kept[b.ID] = true
		}
	}
}

// unseen returns the hidden blocks holding any of the lines: lines the model
// wrote that are in the file but not in what it was shown.
func (v *colaView) unseen(lines []string) []int {
	seen := map[int]bool{}
	var out []int
	for _, l := range lines {
		q := cola.CheckQuote(v.doc, v.kept, l)
		if q.InText && !q.Shown {
			for _, b := range q.Hidden {
				if !seen[b] {
					seen[b] = true
					out = append(out, b)
				}
			}
		}
	}
	return out
}

func (v *colaView) reveal(blocks []int) {
	for _, b := range blocks {
		v.kept[v.doc.Blocks[b].ID] = true
		v.gated = append(v.gated, v.doc.Blocks[b].Label)
	}
}

func (v *colaView) covers(gold []int) bool {
	for _, l := range gold {
		if b := v.doc.BlockOf(l); b >= 0 && !v.kept[v.doc.Blocks[b].ID] {
			return false
		}
	}
	return true
}

// stripLineNumbers removes "  12| " prefixes a model copied from the view.
func stripLineNumbers(s string) string {
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) != "" && !lineNo.MatchString(l) {
			return s
		}
	}
	for i, l := range lines {
		lines[i] = lineNo.ReplaceAllString(l, "")
	}
	return strings.Join(lines, "\n")
}

type colaEdit struct {
	Path    string `json:"path"`
	Search  string `json:"search"`
	Replace string `json:"replace"`
}

func searchLines(edits []colaEdit) []string {
	var out []string
	for _, e := range edits {
		for _, l := range strings.Split(e.Search, "\n") {
			if strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

func runSWECola(ctx context.Context, env *SWEEnv, cfg Config, s *SWE, arm string, opt SWEOpt) (res Result) {
	start := time.Now()
	res = Result{Lang: "python", Exercise: s.ID, Arm: arm, Info: map[string]any{}}
	meter := NewMeter(cfgLLM(ctx))
	defer func() {
		res.Usage, res.Total, res.Seconds = meter.Snapshot(), meter.Total(), time.Since(start).Seconds()
	}()
	step := func(f string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(f, a...)) }
	work, cleanup, err := s.prepare(env, arm)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer cleanup()
	path := s.Files[0]
	gold := s.GoldLinesByFile()[path]
	d, err := colaDoc(env, work, path)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	v := &colaView{doc: d, kept: map[string]bool{}}
	total := d.TotalTokens()
	if total < colaPassWhole {
		for _, b := range d.Blocks {
			v.kept[b.ID] = true
		}
		step("file of %d tokens shown whole", total)
	} else {
		small := func(ctx context.Context, system, prompt string, schema map[string]any) (json.RawMessage, float64, error) {
			raw, err := meter.Complete(ctx, "locate", cfg.Small, system, prompt, schema)
			return raw, 0, err
		}
		m, err := cola.Model(ctx, small, d, s.Problem, "", cola.ModelOpts{Max: 8, MaxPrompt: 70000, Samples: 1})
		if err != nil {
			step("ranking failed (%v): file shown whole", err)
			for _, b := range d.Blocks {
				v.kept[b.ID] = true
			}
		} else {
			v.kept = cola.Keep(d, m.Ranking, int(colaBudget*float64(total)))
			for _, id := range m.Picks {
				v.kept[id] = true
			}
		}
	}
	res.Info["file_tokens"] = total
	res.Info["covers_gold_first"] = v.covers(gold)
	viewTokens := []int{}
	files := func() string {
		t := v.text()
		viewTokens = append(viewTokens, cola.EstTokens(t))
		return fmt.Sprintf("# File %s\n%s\n", path, t)
	}

	editSchema := func(withShow bool) map[string]any {
		edit := obj(map[string]any{"path": map[string]any{"type": "string", "enum": s.Files}, "search": str(), "replace": str()})
		props := map[string]any{"edits": map[string]any{"type": "array", "items": edit}}
		if withShow {
			props["show"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": v.hidden()}, "maxItems": 12}
		}
		return obj(props)
	}
	editRules := `Return edits. Each edit replaces one exact, unique block of text ("search", copied verbatim from the code shown, including indentation, without the line numbers) in the file with "replace".`

	decided, task := "", "Fix this issue in the repository."
	editRole, editModel := "large", cfg.Large
	if opt.ColaArch {
		instr, err := colaInstruct(ctx, meter, cfg, s, v, files, step)
		if err != nil {
			step("instructions: model error: %v", err)
		} else {
			res.Hint = instr
			res.Info["instruction_words"] = len(strings.Fields(instr))
			decided = "# The fix, decided by the senior engineer\n" + instr + "\n\n"
			task = "Apply the fix a senior engineer decided on. Implement exactly that fix."
		}
		editRole, editModel = "edit", cfg.Small
	}

	feedback := ""
	shows, gates := 0, 0
	for try := 1; try <= cfg.Tries; {
		res.Try = try
		// Only the reading model may ask to see more; the editor gets the
		// view the instructions were written from, plus what the gate shows.
		withShow := !opt.ColaArch && shows < colaShows && len(v.hidden()) > 0
		system := coderSystem + "\n" + colaViewNote
		prompt := fmt.Sprintf("%s\n\n# Issue\n%s\n\n%s\n%s%s\n%s", task, s.Problem, files(), decided, editRules, feedback)
		raw, err := meter.Complete(ctx, editRole, editModel, system, prompt, editSchema(withShow))
		v.rounds++
		if err != nil {
			step("fix %d: model error: %v", try, err)
			try++
			continue
		}
		var out struct {
			Edits []colaEdit `json:"edits"`
			Show  []string   `json:"show"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			step("fix %d: %v", try, err)
			try++
			continue
		}
		if withShow && len(out.Show) > 0 {
			shows++
			v.asked = append(v.asked, out.Show...)
			v.show(out.Show)
			step("asked to see %s", strings.Join(out.Show, ", "))
			continue
		}
		for i := range out.Edits {
			out.Edits[i].Search = stripLineNumbers(out.Edits[i].Search)
			out.Edits[i].Replace = stripLineNumbers(out.Edits[i].Replace)
		}
		if hid := v.unseen(searchLines(out.Edits)); len(hid) > 0 && gates < colaGates {
			gates++
			v.reveal(hid)
			step("gate: edits copy code that was not shown; showing %d blocks", len(hid))
			feedback = "\n# Your previous edits copied code you were not shown\nThat code is now shown above. Copy search text only from the code shown.\n"
			continue
		}
		if len(out.Edits) == 0 {
			step("fix %d: no edits", try)
			feedback = "\n# Your previous answer had no edits\n"
			try++
			continue
		}
		fixed, _ := json.Marshal(map[string]any{"edits": out.Edits})
		if err := applyEdits(env, work, fixed); err != nil {
			step("fix %d: %v", try, tail(err.Error(), 200))
			feedback = "\n# Your previous edits could not be used\n" + err.Error() + "\n"
			try++
			continue
		}
		step("fix %d: edits applied", try)
		break
	}
	res.Info["asked"], res.Info["gated"], res.Info["view_tokens"] = v.asked, v.gated, viewTokens
	res.Info["covers_gold"] = v.covers(gold)
	res.Pass, res.ScoreOutput = s.Score(ctx, env, work)
	res.Outcome = map[bool]string{true: "pass", false: "fail"}[res.Pass]
	return res
}

// colaInstruct has the large model read the view and say what the fix is,
// quoting one line it changes; it may ask to see hidden code, and an answer
// quoting a line it was not shown is sent back.
func colaInstruct(ctx context.Context, m *Meter, cfg Config, s *SWE, v *colaView, files func() string, step func(string, ...any)) (string, error) {
	note := ""
	shows, gates := 0, 0
	for {
		withShow := shows < colaShows && len(v.hidden()) > 0
		props := map[string]any{"instructions": str(), "line": str()}
		if withShow {
			props["show"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": v.hidden()}, "maxItems": 12}
		}
		prompt := fmt.Sprintf(`You decide what the fix is. A junior engineer will write the edit from your instructions and sees the same code.

# Issue
%s

%s
Write the fix as instructions: which function and lines to change, and exactly what the new logic is. Use short precise statements or pseudocode, with the exact expressions, names and values where they matter. Give only the change: no reasons, no tests, no summary of the issue. Do not write out the edited code. At most 80 words.
In "line", copy exactly one line of the code shown (without its line number) that the fix changes, or after which new code goes.%s`, s.Problem, files(), note)
		raw, err := m.Complete(ctx, "large", cfg.Large, coderSystem+"\n"+colaViewNote, prompt, obj(props))
		v.rounds++
		if err != nil {
			return "", err
		}
		var out struct {
			Instructions string   `json:"instructions"`
			Line         string   `json:"line"`
			Show         []string `json:"show"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", err
		}
		if withShow && len(out.Show) > 0 {
			shows++
			v.asked = append(v.asked, out.Show...)
			v.show(out.Show)
			step("asked to see %s", strings.Join(out.Show, ", "))
			continue
		}
		if hid := v.unseen([]string{stripLineNumbers(out.Line)}); len(hid) > 0 && gates < colaGates {
			gates++
			v.reveal(hid)
			step("gate: quoted a line that was not shown; showing %d blocks", len(hid))
			note = "\n\nYour previous answer quoted a line you were not shown. That code is now shown above. Quote only from the code shown."
			continue
		}
		if strings.TrimSpace(out.Instructions) == "" {
			return "", fmt.Errorf("empty instructions")
		}
		step("instructions: %d words", len(strings.Fields(out.Instructions)))
		return out.Instructions, nil
	}
}
