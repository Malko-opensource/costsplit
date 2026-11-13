package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Malko-opensource/costsplit/internal/extadapter"
	"github.com/Malko-opensource/costsplit/internal/manifest"
	"github.com/Malko-opensource/costsplit/internal/policy"
	"github.com/Malko-opensource/costsplit/internal/runner"
)

// SWE is one SWE-bench instance: a bug report against an existing repository.
// Unlike Polyglot there is no stub to start from: the graybox author picks
// which existing functions become stubs.
type SWE struct {
	ID         string
	Problem    string
	TestPatch  string
	F2P, P2P   []string
	Files      []string // files the reference patch touches ("oracle retrieval"; the patch itself stays hidden)
	TestFiles  []string
	LocateOnly bool   // no tests: scored by whether the edits land where the reference patch's do
	gold       string // the reference patch: for diagnostics only, never shown to a model
}

type SWEEnv struct {
	RepoDir  string // checkout at base_commit
	Python   string // interpreter with the repository's dependencies
	RepoRoot string // graybox repo (adapters)
}

const (
	sweStub      = `raise NotImplementedError("graybox:stub")`
	sweStubMark  = "graybox:stub"
	sweTestFile  = "test_graybox.py"
	sweTestClass = "GrayboxTest"
)

var diffFile = regexp.MustCompile(`(?m)^diff --git a/(\S+)`)

func LoadSWE(path string) (*SWE, error) {
	var row struct {
		ID        string `json:"instance_id"`
		Problem   string `json:"problem_statement"`
		Patch     string `json:"patch"`
		TestPatch string `json:"test_patch"`
		F2P       string `json:"FAIL_TO_PASS"`
		P2P       string `json:"PASS_TO_PASS"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &row); err != nil {
		return nil, err
	}
	s := &SWE{ID: row.ID, Problem: row.Problem, TestPatch: row.TestPatch, gold: row.Patch}
	_ = json.Unmarshal([]byte(row.F2P), &s.F2P)
	_ = json.Unmarshal([]byte(row.P2P), &s.P2P)
	for _, m := range diffFile.FindAllStringSubmatch(row.Patch, -1) {
		s.Files = append(s.Files, m[1])
	}
	for _, m := range diffFile.FindAllStringSubmatch(row.TestPatch, -1) {
		s.TestFiles = append(s.TestFiles, m[1])
	}
	// An instance without a test patch is scored by where the edits land (swe_locate.go).
	s.LocateOnly = row.TestPatch == ""
	if len(s.Files) == 0 || (len(s.TestFiles) == 0 && !s.LocateOnly) {
		return nil, fmt.Errorf("%s: patch or test_patch names no files", path)
	}
	return s, nil
}

// SetSWEAdapterEnv switches the Python adapter to existing-repository mode.
// The adapter subprocess inherits the process environment.
func SetSWEAdapterEnv(env *SWEEnv, s *SWE) {
	os.Setenv("GRAYBOX_PY_SCOPE", strings.Join(append(append([]string{}, s.Files...), sweTestFile), ","))
	os.Setenv("GRAYBOX_PY_TEST_CMD", env.Python+" -m pytest -q -x -p no:cacheprovider "+sweTestFile)
	os.Setenv("GRAYBOX_PY_TYPECHECK", "off")
	os.Setenv("GRAYBOX_PY_STUB_MARKER", sweStubMark)
}

func (s *SWE) prepare(env *SWEEnv, arm string) (work string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "swe-"+arm+"-")
	if err != nil {
		return "", nil, err
	}
	work = filepath.Join(tmp, "r")
	// -c clones on APFS, so a copy of a large repository is cheap.
	if out, err := exec.Command("cp", "-cR", env.RepoDir, work).CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("copy repo: %v: %s", err, out)
	}
	return work, func() { os.RemoveAll(tmp) }, nil
}

func (s *SWE) django() bool { return strings.HasPrefix(s.ID, "django__") }

var djangoTestLine = regexp.MustCompile(`^(\S+ \([\w.]+\))`)

// runDjangoTests runs the instance's test modules with Django's own runner,
// which names tests "test_x (module.Class)" as SWE-bench records them.
func (s *SWE) runDjangoTests(ctx context.Context, env *SWEEnv, work string) (string, map[string]bool) {
	args := []string{"runtests.py", "--parallel", "1", "-v", "2"}
	for _, f := range s.TestFiles {
		if rest, ok := strings.CutPrefix(f, "tests/"); ok && strings.HasSuffix(rest, ".py") {
			args = append(args, strings.ReplaceAll(strings.TrimSuffix(rest, ".py"), "/", "."))
		}
	}
	cmd := exec.CommandContext(ctx, env.Python, args...)
	cmd.Dir = filepath.Join(work, "tests")
	cmd.Env = append(os.Environ(), "PYTHONPATH="+work, "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	_ = cmd.Run()
	out := buf.String()
	return out, djangoPassed(out)
}

// djangoPassed reads unittest's verbose output. A test with a docstring
// prints its name on one line and "docstring ... ok" on the next.
func djangoPassed(out string) map[string]bool {
	passed := map[string]bool{}
	pending := ""
	for _, ln := range strings.Split(out, "\n") {
		name := ""
		if m := djangoTestLine.FindStringSubmatch(ln); m != nil {
			name = m[1]
		}
		if strings.HasSuffix(strings.TrimSpace(ln), "... ok") {
			if name != "" {
				passed[name] = true
			} else if pending != "" {
				passed[pending] = true
			}
			pending = ""
		} else if name != "" && !strings.Contains(ln, " ... ") {
			pending = name
		} else if strings.Contains(ln, " ... ") {
			pending = ""
		}
	}
	return passed
}

// Score applies the hidden test patch and requires every FAIL_TO_PASS and
// PASS_TO_PASS test to pass.
func (s *SWE) Score(ctx context.Context, env *SWEEnv, work string) (bool, string) {
	// Score a copy: the test patch applies only once, and arms that get
	// feedback score more than once.
	tmp, err := os.MkdirTemp("", "swe-score-")
	if err != nil {
		return false, err.Error()
	}
	defer os.RemoveAll(tmp)
	src := work
	work = filepath.Join(tmp, "r")
	if out, err := exec.Command("cp", "-cR", src, work).CombinedOutput(); err != nil {
		return false, fmt.Sprintf("copy for scoring: %v: %s", err, out)
	}
	os.Remove(filepath.Join(work, sweTestFile))
	os.Remove(filepath.Join(work, sweReproFile))
	ap := exec.CommandContext(ctx, "git", "apply", "-")
	ap.Dir = work
	ap.Stdin = strings.NewReader(s.TestPatch)
	if out, err := ap.CombinedOutput(); err != nil {
		return false, fmt.Sprintf("test patch does not apply: %v: %s", err, out)
	}
	var out string
	var passed map[string]bool
	if s.django() {
		out, passed = s.runDjangoTests(ctx, env, work)
	} else {
		args := append([]string{"-m", "pytest", "-q", "-rA", "-p", "no:cacheprovider"}, s.TestFiles...)
		cmd := exec.CommandContext(ctx, env.Python, args...)
		cmd.Dir = work
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		_ = cmd.Run()
		out = buf.String()
		passed = map[string]bool{}
		for _, ln := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(ln, "PASSED "); ok {
				passed[rest[strings.LastIndex(rest, "::")+2:]] = true
			}
		}
	}
	var missing []string
	for _, t := range append(append([]string{}, s.F2P...), s.P2P...) {
		if !passed[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return false, "not passing: " + strings.Join(missing, ", ") + "\n" + tail(out, 1500)
	}
	return true, tail(out, 300)
}

// ---------- single-model arm: search/replace edits, no hidden-test feedback ----------

func RunSWESingle(ctx context.Context, env *SWEEnv, cfg Config, s *SWE, arm, model string) (res Result) {
	start := time.Now()
	res = Result{Lang: "python", Exercise: s.ID, Arm: arm}
	meter := NewMeter(cfgLLM(ctx))
	defer func() {
		res.Usage, res.Total, res.Seconds = meter.Snapshot(), meter.Total(), time.Since(start).Seconds()
	}()
	work, cleanup, err := s.prepare(env, arm)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer cleanup()
	schema := obj(map[string]any{"edits": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"path": map[string]any{"type": "string", "enum": s.Files}, "search": str(), "replace": str()})}})
	feedback := ""
	applied := false
	for try := 1; try <= cfg.Tries && !applied; try++ {
		prompt := fmt.Sprintf(`Fix this issue in the repository.

# Issue
%s

# Files
%s
Return edits. Each edit replaces one exact, unique block of text ("search", copied verbatim including indentation) in a file with "replace".
%s`, s.Problem, fileList(Files(work, s.Files)), feedback)
		raw, err := meter.Complete(ctx, arm, model, coderSystem, prompt, schema)
		if err != nil {
			res.Steps = append(res.Steps, fmt.Sprintf("try %d: model error: %v", try, err))
			continue
		}
		if err := applyEdits(env, work, raw); err != nil {
			res.Steps = append(res.Steps, fmt.Sprintf("try %d: %v", try, err))
			feedback = "\n# Your previous edits could not be used\n" + err.Error() + "\n"
			continue
		}
		applied = true
		res.Steps = append(res.Steps, fmt.Sprintf("try %d: edits applied", try))
	}
	res.Pass, res.ScoreOutput = s.Score(ctx, env, work)
	res.Outcome = map[bool]string{true: "pass", false: "fail"}[res.Pass]
	return res
}

func applyEdits(env *SWEEnv, work string, raw json.RawMessage) error {
	var out struct {
		Edits []struct{ Path, Search, Replace string } `json:"edits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	orig := map[string]string{}
	cur := map[string]string{}
	for _, e := range out.Edits {
		if _, ok := cur[e.Path]; !ok {
			b, err := os.ReadFile(filepath.Join(work, e.Path))
			if err != nil {
				return err
			}
			orig[e.Path], cur[e.Path] = string(b), string(b)
		}
		if n := strings.Count(cur[e.Path], e.Search); n != 1 {
			return fmt.Errorf("%s: search block found %d times, need exactly 1:\n%s", e.Path, n, tail(e.Search, 300))
		}
		cur[e.Path] = strings.Replace(cur[e.Path], e.Search, e.Replace, 1)
	}
	for p, c := range cur {
		if err := os.WriteFile(filepath.Join(work, p), []byte(c), 0o644); err != nil {
			return err
		}
		if !strings.HasSuffix(p, ".py") {
			continue
		}
		if msg, err := exec.Command(env.Python, "-m", "py_compile", filepath.Join(work, p)).CombinedOutput(); err != nil {
			for q, o := range orig {
				_ = os.WriteFile(filepath.Join(work, q), []byte(o), 0o644)
			}
			return fmt.Errorf("%s does not compile: %s", p, tail(string(msg), 400))
		}
	}
	return nil
}

// ---------- graybox arm ----------

type sweReq struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Test     string `json:"test"`
	TestBody string `json:"test_body"`
}

type sweGraybox struct {
	Targets      []string `json:"targets"`
	TestHeader   string   `json:"test_header"`
	Requirements []sweReq `json:"requirements"`
}

// RunSWEGraybox: the large model turns the issue into a graybox over existing
// code (target functions become stubs; requirement tests are written in full
// and must fail on the unfixed code). The small model then only supplies the
// bodies of the target functions; the tests are not its to change.
func RunSWEGraybox(ctx context.Context, env *SWEEnv, cfg Config, s *SWE) (res Result) {
	start := time.Now()
	res = Result{Lang: "python", Exercise: s.ID, Arm: "graybox"}
	meter := NewMeter(cfgLLM(ctx))
	defer func() {
		res.Usage, res.Total, res.Seconds = meter.Snapshot(), meter.Total(), time.Since(start).Seconds()
	}()
	step := func(f string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(f, a...)) }
	work, cleanup, err := s.prepare(env, "graybox")
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer cleanup()
	finish := func(outcome string) Result {
		res.Pass, res.ScoreOutput = s.Score(ctx, env, work)
		if outcome == "" {
			outcome = map[bool]string{true: "pass", false: "fail"}[res.Pass]
		}
		res.Outcome = outcome
		return res
	}
	if err := os.MkdirAll(filepath.Join(work, runner.Dot), 0o755); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	a := &extadapter.Adapter{Language: "python", Cmd: []string{env.Python, filepath.Join(env.RepoRoot, "adapters/python/adapter.py")}, Dir: work}
	r := &runner.Runner{Dir: work, A: a, D: &LLMDecider{Meter: meter, Model: cfg.Decider}, Limits: cfg.Limits}
	original := fileList(Files(work, s.Files)) // what the models read: the code before stubbing

	// 1. Large model writes the graybox; lock stands in for human approval.
	var gb *sweGraybox
	feedback := ""
	for try := 1; try <= 2 && gb == nil; try++ {
		g, err := sweAuthor(ctx, meter, cfg.Large, s, original, feedback)
		if err != nil {
			step("graybox try %d: model error: %v", try, err)
			continue
		}
		if msg := sweInstall(ctx, env, r, work, g); msg != "" {
			step("graybox try %d: rejected: %s", try, tail(msg, 300))
			feedback = "\n# Your previous graybox was rejected\n" + msg + "\n"
			_ = exec.Command("git", "-C", work, "checkout", "--", ".").Run()
			continue
		}
		gb = g
		step("graybox try %d: locked (targets %s; %d requirements; tests fail on the unfixed code)", try, strings.Join(g.Targets, ", "), len(g.Requirements))
	}
	if gb == nil {
		return finish("graybox_invalid")
	}

	// 2. Plan: every target serves every requirement, every test its own.
	// The decider's view is advisory when Limits.AdvisoryPlan is set.
	testBodies := map[string]string{}
	plan := &manifest.Plan{}
	var ids []string
	for _, q := range gb.Requirements {
		ids = append(ids, q.ID)
		sym := "test_graybox:" + sweTestClass + "." + q.Test
		testBodies[sym] = q.TestBody
		plan.Targets = append(plan.Targets, manifest.Target{Symbol: sym, Reqs: []string{q.ID}})
	}
	for _, t := range gb.Targets {
		plan.Targets = append(plan.Targets, manifest.Target{Symbol: t, Reqs: ids})
	}
	pr, err := r.Plan(ctx, "T", policy.Small, plan)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	step("plan: %s %s %s", pr.Next, answersBrief(pr), tail(strings.Join(pr.Feedback, " | "), 300))
	if pr.Next != policy.Proceed {
		return finish("needs_human")
	}

	// 3. Small model fills the target bodies; the tool recommends what is next.
	model := policy.Small
	feedback = ""
	for i := 0; i < cfg.Limits.SmallRetries+cfg.Limits.LargeRetries+2; i++ {
		name := cfg.Small
		if model == policy.Large {
			name = cfg.Large
		}
		bodies, err := sweBodies(ctx, meter, string(model), name, s, original, gb, feedback)
		if err != nil {
			step("implement (%s): model error: %v", model, err)
			continue
		}
		for k, v := range testBodies {
			bodies[k] = v
		}
		ar, err := r.ApplyAll(ctx, "T", model, bodies)
		if err != nil {
			res.Outcome, res.Error = "error", err.Error()
			return res
		}
		step("implement (%s) attempt %d: %s %s %s", model, ar.Attempt, ar.Next, answersBrief(ar), tail(strings.Join(ar.Feedback, " | "), 600))
		switch ar.Next {
		case policy.Done:
			res.DeciderDone = true
			return finish("")
		case policy.EscalateLarge:
			model, res.Escalated = policy.Large, true
			feedback = "\n# A previous attempt by another model failed\n" + strings.Join(ar.Feedback, "\n") + "\n"
		case policy.EscalateHuman:
			return finish("needs_human")
		case policy.Paused:
			res.Error = ar.Error
			return finish("paused")
		default:
			feedback = "\n# Your previous attempt failed\n" + strings.Join(ar.Feedback, "\n") + "\nFix the bodies.\n"
		}
	}
	return finish("needs_human")
}

func sweAuthor(ctx context.Context, m *Meter, model string, s *SWE, files, feedback string) (*sweGraybox, error) {
	prompt := fmt.Sprintf(`# Issue
%s

# Files
%s
# Your job: write a graybox for this fix, not the fix
A smaller model will fix the issue. It can ONLY replace the bodies of the functions you name; it cannot change signatures, add functions, or touch other code (helpers must be nested inside the body).

1. targets: the existing functions or methods whose bodies must change, as module.path:Qual.name (for example pkg.mod:Class.method). Name as few as possible.
2. requirements: 3 to 6 requirements with ids REQ-001, REQ-002, ... Each has a one-sentence text and one test. Cover the reported bug, its close variants, and behavior that must not regress.
3. Tests: test_header is the import lines for a unittest file at the repository root. Each requirement's test is a method name (test_REQ001_short_name) and test_body, the statements inside "def <name>(self) -> None:". Tests must be complete assertions. At least one must fail on the current code, and all must pass once the issue is fixed. Do not test anything the issue does not require.
%s`, s.Problem, files, feedback)
	schema := obj(map[string]any{
		"targets":      map[string]any{"type": "array", "minItems": 1, "items": str()},
		"test_header":  str(),
		"requirements": map[string]any{"type": "array", "minItems": 1, "items": obj(map[string]any{"id": str(), "text": str(), "test": str(), "test_body": str()})},
	})
	raw, err := m.Complete(ctx, "author", model, coderSystem, prompt, schema)
	if err != nil {
		return nil, err
	}
	var g sweGraybox
	return &g, json.Unmarshal(raw, &g)
}

// sweInstall writes the graybox into work and locks it. It returns a message
// for the author when the graybox cannot be used.
func sweInstall(ctx context.Context, env *SWEEnv, r *runner.Runner, work string, g *sweGraybox) string {
	for _, t := range g.Targets {
		if strings.HasPrefix(t, "test_graybox:") {
			return "targets must be repository functions, not tests"
		}
		if _, err := r.A.Resolve(t); err != nil {
			return fmt.Sprintf("target %s: %v", t, err)
		}
	}
	var b strings.Builder
	b.WriteString("import unittest\n" + g.TestHeader + "\n\n\nclass " + sweTestClass + "(unittest.TestCase):\n")
	man := manifest.Manifest{Schema: "graybox.manifest/v1"}
	for _, q := range g.Requirements {
		fmt.Fprintf(&b, "    def %s(self) -> None:\n%s\n\n", q.Test, indent(q.TestBody, "        "))
		man.Requirements = append(man.Requirements, manifest.Requirement{ID: q.ID, Text: q.Text, Tests: []string{q.Test}, Coverage: "full"})
	}
	if err := writeFile(work, sweTestFile, b.String()); err != nil {
		return err.Error()
	}
	// Red before green: the tests must catch the bug on the unfixed code.
	cmd := exec.CommandContext(ctx, env.Python, "-m", "pytest", "-q", "-p", "no:cacheprovider", sweTestFile)
	cmd.Dir = work
	out, err := cmd.CombinedOutput()
	if err == nil {
		return "every test already passes on the current code, so the tests do not capture the bug"
	}
	if !strings.Contains(string(out), " failed") {
		return "the tests do not run:\n" + tail(string(out), 1200)
	}
	// Now turn tests and targets into stubs and lock.
	for _, q := range g.Requirements {
		if err := r.A.Apply("test_graybox:"+sweTestClass+"."+q.Test, `self.skipTest("graybox")`); err != nil {
			return "test " + q.Test + ": " + err.Error()
		}
	}
	for _, t := range g.Targets {
		if err := r.A.Apply(t, sweStub); err != nil {
			return "target " + t + ": " + err.Error()
		}
	}
	if err := writeFile(work, filepath.Join(runner.Dot, "manifest.json"), string(mustJSON(man))); err != nil {
		return err.Error()
	}
	_ = writeFile(work, filepath.Join(runner.Dot, "questions.json"), `{"schema":"graybox.questions/v1","questions":[]}`)
	if lr, err := r.Lock(); err != nil {
		return lr.Error + "\n" + strings.Join(failDetails(lr.Checks), "\n")
	}
	return ""
}

func sweBodies(ctx context.Context, m *Meter, role, model string, s *SWE, files string, g *sweGraybox, feedback string) (map[string]string, error) {
	targets := append([]string{}, g.Targets...)
	sort.Strings(targets)
	var reqs strings.Builder
	for _, q := range g.Requirements {
		fmt.Fprintf(&reqs, "%s: %s\n    def %s(self):\n%s\n", q.ID, q.Text, q.Test, indent(q.TestBody, "        "))
	}
	prompt := fmt.Sprintf(`# Issue
%s

# Files (current code)
%s
# Requirements and their tests (fixed; imports: %s)
%s
# Functions to rewrite
%s
Return the complete new body of every function listed: only the statements inside it, without the def line and without the docstring (the docstring is kept). Signatures are fixed; put any helper inside the body.
%s`, s.Problem, files, strings.ReplaceAll(g.TestHeader, "\n", "; "), reqs.String(), strings.Join(targets, "\n"), feedback)
	schema := obj(map[string]any{"bodies": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"symbol": map[string]any{"type": "string", "enum": targets}, "body": str()})}})
	raw, err := m.Complete(ctx, role, model, coderSystem, prompt, schema)
	if err != nil {
		return nil, err
	}
	var out struct {
		Bodies []struct{ Symbol, Body string } `json:"bodies"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	bodies := map[string]string{}
	for _, b := range out.Bodies {
		bodies[b.Symbol] = b.Body
	}
	for _, t := range targets {
		if _, ok := bodies[t]; !ok {
			return nil, fmt.Errorf("no body returned for %s", t)
		}
	}
	return bodies, nil
}

func indent(s, pad string) string {
	lines := strings.Split(strings.Trim(s, "\n"), "\n")
	// Drop the common leading whitespace, then pad.
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if min < 0 || n < min {
			min = n
		}
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
		} else {
			lines[i] = pad + l[min:]
		}
	}
	return strings.Join(lines, "\n")
}

// SWECheck scores the unfixed code: it must fail, or the scorer is wrong.
func SWECheck(ctx context.Context, env *SWEEnv, s *SWE) Result {
	res := Result{Lang: "python", Exercise: s.ID, Arm: "check"}
	work, cleanup, err := s.prepare(env, "check")
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer cleanup()
	res.Pass, res.ScoreOutput = s.Score(ctx, env, work)
	res.Outcome = map[bool]string{true: "pass", false: "fail"}[res.Pass]
	res.Steps = []string{tail(res.ScoreOutput, 400)}
	return res
}
