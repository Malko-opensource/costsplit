package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A fileSlice is what a model reads instead of the whole file.
type fileSlice struct {
	FileLines int          `json:"file_lines"`
	Head      string       `json:"head"`
	Outline   string       `json:"outline"`
	Blocks    []sliceBlock `json:"blocks"`
	Related   []sliceBlock `json:"related"`
	Added     []string     `json:"added"`
	Missing   []string     `json:"missing"`
}

type sliceBlock struct {
	Label string `json:"label"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

func (f *fileSlice) lines() int {
	n := strings.Count(f.Head, "\n") + strings.Count(f.Outline, "\n") + 2
	for _, b := range append(append([]sliceBlock{}, f.Blocks...), f.Related...) {
		n += b.End - b.Start + 1
	}
	return n
}

func (f *fileSlice) covers(lines []int) bool {
	for _, l := range lines {
		in := false
		for _, b := range f.Blocks {
			// Two lines of slack: a statement added right after a function
			// is anchored on the line that follows it.
			if b.Start-2 <= l && l <= b.End+2 {
				in = true
			}
		}
		if !in {
			return false
		}
	}
	return true
}

func (f *fileSlice) labels() []string {
	var out []string
	for _, b := range f.Blocks {
		out = append(out, b.Label)
	}
	return out
}

func buildSlice(env *SWEEnv, work, path string, lines []int, names []string, lean bool, strict ...bool) (*fileSlice, error) {
	args := []string{filepath.Join(env.RepoRoot, "adapters/python/slice.py"), filepath.Join(work, path)}
	if lean {
		args = append(args, "--outline", "lean")
	}
	if len(strict) > 0 && strict[0] {
		args = append(args, "--strict")
	}
	if len(lines) > 0 {
		var s []string
		for _, l := range lines {
			s = append(s, strconv.Itoa(l))
		}
		args = append(args, "--lines", strings.Join(s, ","))
	}
	if len(names) > 0 {
		args = append(args, "--names", strings.Join(names, ","))
	}
	out, err := exec.Command(env.Python, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("slice %s: %v", path, err)
	}
	var f fileSlice
	return &f, json.Unmarshal(out, &f)
}

func (f *fileSlice) prompt(path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# File %s (%d lines). Shown: its imports, an outline, and the code relevant to the issue. The rest of the file exists but is not shown.\n", path, f.FileLines)
	fmt.Fprintf(&b, "## Imports\n```python\n%s\n```\n", f.Head)
	if f.Outline != "" {
		fmt.Fprintf(&b, "## Outline (signatures only; L = line numbers)\n```\n%s\n```\n", f.Outline)
	}
	b.WriteString("## Relevant code (complete)\n")
	for _, bl := range f.Blocks {
		fmt.Fprintf(&b, "### %s (lines %d-%d)\n```python\n%s\n```\n", bl.Label, bl.Start, bl.End, bl.Text)
	}
	if len(f.Related) > 0 {
		b.WriteString("## Functions it calls\n")
		for _, bl := range f.Related {
			fmt.Fprintf(&b, "### %s (lines %d-%d)\n```python\n%s\n```\n", bl.Label, bl.Start, bl.End, bl.Text)
		}
	}
	return b.String()
}

var hunkHead = regexp.MustCompile(`^@@ -(\d+)`)

// goldLines returns the lines of the original file the reference patch
// touches; an insertion counts as the line before it. Used only to build the
// "exactly the right function" slice, an upper bound on localization.
func goldLines(patch string) []int {
	seen := map[int]bool{}
	old, in := 0, false
	for _, ln := range strings.Split(patch, "\n") {
		if m := hunkHead.FindStringSubmatch(ln); m != nil {
			old, _ = strconv.Atoi(m[1])
			in = true
			continue
		}
		if !in {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "diff ") || strings.HasPrefix(ln, "--- ") || strings.HasPrefix(ln, "+++ "):
			in = false
		case strings.HasPrefix(ln, "-"):
			seen[old] = true
			old++
		case strings.HasPrefix(ln, "+"):
			if old > 1 {
				seen[old-1] = true
			} else {
				seen[1] = true
			}
		default:
			old++
		}
	}
	var out []int
	for l := range seen {
		out = append(out, l)
	}
	sort.Ints(out)
	return out
}

// runSWESliced: the fixing model reads a slice of the file, not the file.
// opt.Slice is "oracle" (the functions the reference patch touches) or "small"
// (the small model reads the whole file and names the functions). With
// opt.Arch the arm's model only says what the fix is and the small model
// writes the edit: the large model's output shrinks to a few sentences.
func runSWESliced(ctx context.Context, env *SWEEnv, cfg Config, s *SWE, arm, role string, opt SWEOpt) (res Result) {
	who := opt.Slice
	start := time.Now()
	res = Result{Lang: "python", Exercise: s.ID, Arm: arm, Info: map[string]any{}}
	meter := NewMeter(cfgLLM(ctx))
	defer func() {
		res.Usage, res.Total, res.Seconds = meter.Snapshot(), meter.Total(), time.Since(start).Seconds()
	}()
	model := cfg.Small
	if role == "large" {
		model = cfg.Large
	}
	step := func(f string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(f, a...)) }
	work, cleanup, err := s.prepare(env, arm)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer cleanup()
	path := s.Files[0]
	gold := goldLines(s.gold)

	var sl *fileSlice
	var names []string
	if who == "oracle" {
		sl, err = buildSlice(env, work, path, gold, nil, opt.Lean)
	} else {
		// Reusing the names an earlier run chose keeps the slice identical
		// across arms, so arms differ only in what is being tested.
		var reused bool
		names, reused = reuseLocate(s.ID)
		if !reused {
			names, err = sweLocalize(ctx, env, meter, cfg.Small, s, work, "")
		}
		if err == nil {
			res.Info["named"], res.Info["named_reused"] = names, reused
			sl, err = buildSlice(env, work, path, nil, names, opt.Lean)
		}
	}
	whole := err != nil || len(sl.Blocks) == 0
	files := ""
	if whole {
		// Nothing usable was named: fall back to the whole file.
		step("slice unavailable (%v): whole file shown", err)
		files = "# Files\n" + fileList(Files(work, s.Files))
	} else {
		files = sl.prompt(path)
		res.Info["file_lines"], res.Info["slice_lines"] = sl.FileLines, sl.lines()
		res.Info["blocks"], res.Info["covers_gold"] = sl.labels(), sl.covers(gold)
		step("slice: %d of %d lines; blocks %s; covers the reference change: %v", sl.lines(), sl.FileLines, strings.Join(sl.labels(), " | "), sl.covers(gold))
	}
	res.Info["whole_file"] = whole
	if !whole {
		res.Info["covers_funcs"] = coversFuncs(env, work, path, gold, sl)
	}
	if opt.LocateOnly {
		res.Outcome = "located"
		return res
	}

	schema := obj(map[string]any{"edits": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"path": map[string]any{"type": "string", "enum": s.Files}, "search": str(), "replace": str()})}})
	task := "Fix this issue in the repository."
	decided := ""
	if opt.Arch {
		instr, elsewhere, err := sweInstruct(ctx, meter, role, model, s, files, opt.More && !whole)
		if err == nil && elsewhere != "" {
			// The model judged the slice to be the wrong place. Its judgment
			// is what it is paid for; finding the code it names is cheap.
			step("instructions: fix belongs elsewhere: %s", tail(strings.ReplaceAll(elsewhere, "\n", " "), 200))
			res.Info["elsewhere"] = elsewhere
			more := sweResolve(ctx, env, meter, cfg.Small, s, work, path, names, elsewhere)
			res.Info["named_more"] = more
			if sl2, err2 := buildSlice(env, work, path, nil, append(append([]string{}, names...), more...), opt.Lean); err2 == nil && len(more) > 0 && len(sl2.Blocks) > 0 {
				sl, files = sl2, sl2.prompt(path)
				res.Info["slice_lines"], res.Info["blocks"], res.Info["covers_gold"] = sl.lines(), sl.labels(), sl.covers(gold)
				step("slice rebuilt: %d of %d lines; blocks %s; covers the reference change: %v", sl.lines(), sl.FileLines, strings.Join(sl.labels(), " | "), sl.covers(gold))
			}
			var again string
			again, _, err = sweInstruct(ctx, meter, role, model, s, files, false)
			if err == nil {
				instr = again
			}
		}
		if err != nil {
			step("instructions: model error: %v", err)
		} else {
			res.Hint = instr
			res.Info["instruction_words"] = len(strings.Fields(instr))
			step("instructions: %d words", len(strings.Fields(instr)))
			task = "Apply the fix a senior engineer decided on. Implement exactly that fix."
			decided = "# The fix, decided by the senior engineer\n" + instr + "\n\n"
		}
		role, model = "edit", cfg.Small
	}
	feedback := ""
	for try := 1; try <= cfg.Tries; try++ {
		res.Try = try
		prompt := fmt.Sprintf(`%s

# Issue
%s

%s
%sReturn edits. Each edit replaces one exact, unique block of text ("search", copied verbatim from the code shown, including indentation) in the file with "replace".
%s`, task, s.Problem, files, decided, feedback)
		raw, err := meter.Complete(ctx, role, model, coderSystem, prompt, schema)
		if err != nil {
			step("fix %d: model error: %v", try, err)
			continue
		}
		if err := applyEdits(env, work, raw); err != nil {
			step("fix %d: %v", try, tail(err.Error(), 200))
			feedback = "\n# Your previous edits could not be used\n" + err.Error() + "\n"
			continue
		}
		step("fix %d: edits applied", try)
		break
	}
	res.Pass, res.ScoreOutput = s.Score(ctx, env, work)
	res.Outcome = map[bool]string{true: "pass", false: "fail"}[res.Pass]
	return res
}

// sweLocalize has the small model read the whole file and say what to change.
// Reading is cheap for it; the large model then reads only what it names.
// The answer is a forced choice: `targets` accepts only names of functions
// that exist in the file, so it cannot carry remarks or point at other files.
// Functions the fix would add go in `new`, the one free-text field.
func sweLocalize(ctx context.Context, env *SWEEnv, m *Meter, model string, s *SWE, work, extra string) ([]string, error) {
	choices := functionNames(env, work, s.Files[0])
	target := str()
	rule := "exactly as they appear (Class.method, or function for a module-level function)"
	if len(choices) > 0 {
		target = map[string]any{"type": "string", "enum": choices}
		rule = "chosen from the names the answer format allows, which are the functions of this file"
	}
	prompt := fmt.Sprintf(`# Issue
%s

# Files
%s
%sThe fix goes in this file. Put the functions or methods whose code must change in `+"`targets`"+`, %s: at most three, most likely first.
If the fix adds a function or method that does not exist yet, put its name in `+"`new`"+` as Class.method or function (at most two names, nothing else); otherwise leave `+"`new`"+` empty.
Do not fix anything.`, s.Problem, fileList(Files(work, s.Files)), extra, rule)
	schema := obj(map[string]any{"targets": map[string]any{"type": "array", "items": target}, "new": map[string]any{"type": "array", "items": str()}})
	raw, err := m.Complete(ctx, "locate", model, coderSystem, prompt, schema)
	if err != nil {
		return nil, err
	}
	var out struct {
		Targets []string `json:"targets"`
		New     []string `json:"new"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	names := append(cleanNames(out.Targets, 3), cleanNames(out.New, 2)...)
	if len(names) == 0 {
		return nil, fmt.Errorf("the localizer named nothing")
	}
	return names, nil
}

// functionNames lists the functions and methods of a file, the choices the
// localizer may pick from. Empty when the file cannot be parsed.
func functionNames(env *SWEEnv, work, path string) []string {
	out, err := exec.Command(env.Python, filepath.Join(env.RepoRoot, "adapters/python/slice.py"), filepath.Join(work, path), "--list").Output()
	if err != nil {
		return nil
	}
	var r struct {
		Symbols []struct {
			Q   string `json:"q"`
			Cls bool   `json:"cls"`
		} `json:"symbols"`
	}
	if json.Unmarshal(out, &r) != nil {
		return nil
	}
	var names []string
	for _, sym := range r.Symbols {
		if !sym.Cls {
			names = append(names, sym.Q)
		}
	}
	return uniq(names)
}

var leadingName = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)

// cleanNames keeps the first identifier of each answer. The model sometimes
// appends remarks ("foo (in another file)"), and a remark is not a name: it
// matched nothing and sent four runs back to the whole file.
func cleanNames(raw []string, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range raw {
		n := leadingName.FindString(r)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
		if len(out) == max {
			break
		}
	}
	return out
}

// sweInstruct has the model decide the fix without writing it out. Output
// tokens are the costly ones; a few sentences replace the edit. With more it
// may instead answer that the fix belongs in code it was not shown.
func sweInstruct(ctx context.Context, m *Meter, role, model string, s *SWE, files string, more bool) (instr, elsewhere string, err error) {
	props := map[string]any{"instructions": str()}
	escape := ""
	if more {
		props["elsewhere"] = str()
		escape = "\nIf the right fix cannot be made in the code shown because it belongs in another function of this file, leave `instructions` empty and put the name of that function, or a description of the code you need to see, in `elsewhere`. Only this file can be shown or changed, so do not ask for other files. Otherwise leave `elsewhere` empty."
	}
	prompt := fmt.Sprintf(`You decide what the fix is. A junior engineer will write the edit from your instructions and sees the same code.

# Issue
%s

%s
Write the fix as instructions: which function and lines to change, and exactly what the new logic is. Use short precise statements or pseudocode, with the exact expressions, names and values where they matter. Give only the change: no reasons, no tests, no summary of the issue. Do not write out the edited code. At most 80 words.%s`, s.Problem, files, escape)
	raw, err := m.Complete(ctx, role, model, coderSystem, prompt, obj(props))
	if err != nil {
		return "", "", err
	}
	var out struct {
		Instructions string `json:"instructions"`
		Elsewhere    string `json:"elsewhere"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", err
	}
	out.Elsewhere = strings.TrimSpace(out.Elsewhere)
	if strings.TrimSpace(out.Instructions) == "" && out.Elsewhere == "" {
		return "", "", fmt.Errorf("empty instructions")
	}
	return out.Instructions, out.Elsewhere, nil
}

var identifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)

// sweResolve turns what the instructing model asked for into function names
// in the file: names it wrote that exist are taken as they are; otherwise the
// small model reads the file again with the request in hand.
func sweResolve(ctx context.Context, env *SWEEnv, m *Meter, small string, s *SWE, work, path string, have []string, request string) []string {
	seen := map[string]bool{}
	for _, h := range have {
		seen[h] = true
	}
	var cands []string
	for _, id := range identifier.FindAllString(request, -1) {
		if len(id) >= 4 && !seen[id] && (strings.ContainsAny(id, "_.") || id != strings.ToLower(id)) {
			seen[id] = true
			cands = append(cands, id)
		}
	}
	if len(cands) > 12 {
		cands = cands[:12]
	}
	if len(cands) > 0 {
		if sl, err := buildSlice(env, work, path, nil, cands, true, true); err == nil {
			gone := map[string]bool{}
			for _, x := range sl.Missing {
				gone[x] = true
			}
			var found []string
			for _, c := range cands {
				if !gone[c] {
					found = append(found, c)
				}
			}
			if len(found) > 0 {
				if len(found) > 3 {
					found = found[:3]
				}
				return found
			}
		}
	}
	extra := fmt.Sprintf("A senior engineer looked at %s and says the fix belongs elsewhere:\n%s\n\nFind what the senior engineer asks for. Do not repeat the functions already looked at.\n", strings.Join(have, ", "), request)
	names, err := sweLocalize(ctx, env, m, small, s, work, extra)
	if err != nil {
		return nil
	}
	return names
}

var (
	locateOnce  sync.Once
	locateNames map[string][]string
)

// reuseLocate returns the function names a previous run chose for an
// instance, read from the results file named by POLYBENCH_LOCATE_FROM (the
// rows of the arm named by POLYBENCH_LOCATE_ARM, "large-hslice" by default).
func reuseLocate(id string) ([]string, bool) {
	locateOnce.Do(func() {
		locateNames = map[string][]string{}
		path := os.Getenv("POLYBENCH_LOCATE_FROM")
		if path == "" {
			return
		}
		arm := os.Getenv("POLYBENCH_LOCATE_ARM")
		if arm == "" {
			arm = "large-hslice"
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, ln := range strings.Split(string(b), "\n") {
			var r struct {
				Exercise string `json:"exercise"`
				Arm      string `json:"arm"`
				Info     struct {
					Named []string `json:"named"`
				} `json:"info"`
			}
			if json.Unmarshal([]byte(ln), &r) == nil && r.Arm == arm && len(r.Info.Named) > 0 {
				locateNames[r.Exercise] = r.Info.Named
			}
		}
	})
	names, ok := locateNames[id]
	return names, ok
}

// coversFuncs is the fair test of a slice: every function the reference
// patch changes is shown. Reference lines outside any function (an added
// import, say) do not count, unless the patch touches nothing else.
func coversFuncs(env *SWEEnv, work, path string, gold []int, sl *fileSlice) bool {
	oracle, err := buildSlice(env, work, path, gold, nil, true)
	if err != nil {
		return sl.covers(gold)
	}
	var inFunc []int
	for _, l := range gold {
		for _, b := range oracle.Blocks {
			if !strings.HasPrefix(b.Label, "lines ") && b.Start <= l && l <= b.End {
				inFunc = append(inFunc, l)
				break
			}
		}
	}
	if len(inFunc) == 0 {
		return sl.covers(gold)
	}
	return sl.covers(inFunc)
}
