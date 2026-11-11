package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SWEOpt selects what a model gets beyond the issue and the source file.
// It tests one idea: a small model fails for lack of a contract, not ability.
type SWEOpt struct {
	ShowTests bool // the existing test files, as they are before the fix, are in the prompt
	Oracle    bool // the hidden tests are in the prompt and their results are fed back: an upper bound
	TDD       bool // the model first writes a test that fails on the current code, then fixes against it
	Tries     int  // fix attempts; later attempts get the verifier's output
	// Author names who writes the test in TDD mode: "" is the fixing model
	// itself, "large" or "small" a separate author. With IssueOnly the author
	// sees the issue and the head of the existing test file but not the
	// implementation, and the fixer gets the source file and that test only.
	Author    string
	IssueOnly bool
	// Slice: the fixing model reads a slice of the file instead of the file.
	// "oracle" selects the functions the reference patch touches; "small" has
	// the small model read the whole file and name them.
	Slice string
	// Lean drops the file outline from the slice. Arch splits the fix: the
	// arm's model only writes instructions, the small model writes the edit.
	Lean bool
	Arch bool
	// More lets the instructing model say once that the fix belongs
	// elsewhere; the slice is then rebuilt around what it asks for.
	More bool
	// LocateOnly stops after the slice is built: it measures localization
	// without paying for a fix.
	LocateOnly bool
}

// ParseSWEArm reads arm names like small-ctx, small-tdd and large-oracle.
func ParseSWEArm(arm string) (role string, opt SWEOpt, ok bool) {
	role, mode, found := strings.Cut(arm, "-")
	if !found || (role != "small" && role != "large") {
		return "", SWEOpt{}, false
	}
	switch mode {
	case "ctx":
		return role, SWEOpt{ShowTests: true, Tries: 2}, true
	case "tdd":
		return role, SWEOpt{ShowTests: true, TDD: true, Tries: 3}, true
	case "oracle":
		return role, SWEOpt{ShowTests: true, Oracle: true, Tries: 3}, true
	case "ltest":
		return role, SWEOpt{TDD: true, Author: "large", IssueOnly: true, Tries: 3}, true
	case "stest":
		return role, SWEOpt{TDD: true, Author: "small", IssueOnly: true, Tries: 3}, true
	case "oslice":
		return role, SWEOpt{Slice: "oracle"}, true
	case "hslice":
		return role, SWEOpt{Slice: "small"}, true
	case "hlean":
		return role, SWEOpt{Slice: "small", Lean: true}, true
	case "harch":
		return role, SWEOpt{Slice: "small", Arch: true}, true
	case "hleanarch":
		return role, SWEOpt{Slice: "small", Lean: true, Arch: true}, true
	case "locate":
		return role, SWEOpt{Slice: "small", Lean: true, LocateOnly: true}, true
	case "hleanarch2":
		return role, SWEOpt{Slice: "small", Lean: true, Arch: true, More: true}, true
	}
	return "", SWEOpt{}, false
}

const sweReproFile = "test_repro_gb.py"

// RunSWEContract runs one model on one instance with the contract the arm
// names. Without a verifier (ctx) the model answers once; with one (tdd,
// oracle) it gets the failures back and may edit again.
func RunSWEContract(ctx context.Context, env *SWEEnv, cfg Config, s *SWE, arm, role string, opt SWEOpt) (res Result) {
	if opt.Slice != "" {
		return runSWESliced(ctx, env, cfg, s, arm, role, opt)
	}
	start := time.Now()
	res = Result{Lang: "python", Exercise: s.ID, Arm: arm}
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

	var existing []string
	for _, f := range s.TestFiles {
		if _, err := os.Stat(filepath.Join(work, f)); err == nil {
			existing = append(existing, f)
		}
	}
	contract := ""
	if opt.ShowTests && len(existing) > 0 {
		contract += "# Existing tests for this code (they pass now and must keep passing; they show how the code is used)\n" + fileList(Files(work, existing))
	}
	if opt.Oracle {
		contract += "# Tests your fix must make pass (this diff will be applied to the test files)\n```diff\n" + s.TestPatch + "\n```\n"
	}

	// The model's own contract: a test that fails now. "Red" is checked by
	// running it on the unfixed code, not by asking a model.
	var baseline map[string]string
	if opt.TDD {
		baseline = s.failing(ctx, env, work, existing)
		authorRole, authorModel := role, model
		switch opt.Author {
		case "large":
			authorRole, authorModel = "author", cfg.Large
		case "small":
			authorRole, authorModel = "author", cfg.Small
		}
		feedback := ""
		for a := 1; a <= 2 && !res.Red; a++ {
			var code string
			var err error
			if opt.IssueOnly {
				code, err = sweAcceptance(ctx, meter, authorRole, authorModel, s, work, existing, feedback)
			} else {
				code, err = sweRepro(ctx, meter, authorRole, authorModel, s, work, contract, feedback)
			}
			if err != nil {
				step("repro test %d: model error: %v", a, err)
				continue
			}
			if err := writeFile(work, sweReproFile, code); err != nil {
				res.Outcome, res.Error = "error", err.Error()
				return res
			}
			exit, out := s.pytest(ctx, env, work, "-x", "--tb=short", sweReproFile)
			switch exit {
			case 1:
				res.Red = true
				res.Contract = code
				// Diagnostic only, never shown to a model: does the reference
				// fix satisfy this test? If not, the test asks for something
				// the maintainers did not.
				res.GoldGreen = s.goldAccepts(ctx, env, code)
				contract += "# Reproduction test (fails on the original code; must pass after your fix)\n```python\n" + code + "\n```\n"
				step("repro test %d: fails on the unfixed code, as required (reference fix passes it: %v)", a, res.GoldGreen)
			case 0:
				feedback = "\n# Your previous test file was rejected\nEvery test passes on the current code, so the file does not reproduce the issue.\n"
				step("repro test %d: rejected: passes on the unfixed code", a)
			default:
				feedback = "\n# Your previous test file was rejected\nThe tests do not run:\n" + tail(out, 1500) + "\n"
				step("repro test %d: rejected: does not run", a)
			}
		}
		if !res.Red {
			os.Remove(filepath.Join(work, sweReproFile))
		}
	}

	schema := obj(map[string]any{"edits": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"path": map[string]any{"type": "string", "enum": s.Files}, "search": str(), "replace": str()})}})
	feedback := ""
	for try := 1; try <= opt.Tries; try++ {
		res.Try = try
		prompt := fmt.Sprintf(`Fix this issue in the repository.

# Issue
%s

# Files
%s
%sReturn edits. Each edit replaces one exact, unique block of text ("search", copied verbatim including indentation) in a file with "replace".
%s`, s.Problem, fileList(Files(work, s.Files)), contract, feedback)
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
		var ok bool
		var out string
		switch {
		case opt.Oracle:
			ok, out = s.Score(ctx, env, work)
		case opt.TDD:
			ok, out = s.verifyOwn(ctx, env, work, res.Red, existing, baseline)
		default:
			step("fix %d: edits applied", try)
			try = opt.Tries // no verifier: one applied answer is the answer
			continue
		}
		if ok {
			res.OwnGreen = true
			step("fix %d: verifier passes", try)
			break
		}
		step("fix %d: verifier fails: %s", try, tail(strings.ReplaceAll(out, "\n", " | "), 200))
		feedback = "\n# Your edits are applied in the files above, but the checks still fail\n" + tail(out, 3000) + "\nReturn further edits.\n"
	}
	res.Pass, res.ScoreOutput = s.Score(ctx, env, work)
	res.Outcome = map[bool]string{true: "pass", false: "fail"}[res.Pass]
	return res
}

func sweRepro(ctx context.Context, m *Meter, role, model string, s *SWE, work, contract, feedback string) (string, error) {
	prompt := fmt.Sprintf(`# Issue
%s

# Files
%s
%sWrite a pytest file that reproduces this issue. It is saved as %s in the repository root.
- One to four test functions with plain asserts; import from the package the way the existing tests do.
- Each test must fail on the current code and pass once the issue is fixed as the issue describes.
- Test only what the issue states. Do not guess at behavior it does not mention.
%s`, s.Problem, fileList(Files(work, s.Files)), contract, sweReproFile, feedback)
	raw, err := m.Complete(ctx, role, model, coderSystem, prompt, obj(map[string]any{"test_file": str()}))
	if err != nil {
		return "", err
	}
	var out struct {
		TestFile string `json:"test_file"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.TestFile) == "" {
		return "", errors.New("empty test file")
	}
	return out.TestFile, nil
}

// pytest runs in work and returns the exit code (0 pass, 1 test failures,
// anything else: the run itself broke) and the output.
func (s *SWE) pytest(ctx context.Context, env *SWEEnv, work string, args ...string) (int, string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, env.Python, append([]string{"-m", "pytest", "-q", "-p", "no:cacheprovider"}, args...)...)
	cmd.Dir = work
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, buf.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), buf.String()
	}
	return -1, buf.String() + err.Error()
}

// failing returns the tests in files that fail or error, with pytest's
// one-line reason.
func (s *SWE) failing(ctx context.Context, env *SWEEnv, work string, files []string) map[string]string {
	out := map[string]string{}
	if len(files) == 0 {
		return out
	}
	exit, log := s.pytest(ctx, env, work, append([]string{"-rfE", "--tb=no"}, files...)...)
	for _, ln := range strings.Split(log, "\n") {
		for _, p := range []string{"FAILED ", "ERROR "} {
			if rest, ok := strings.CutPrefix(ln, p); ok {
				id, msg, _ := strings.Cut(rest, " - ")
				out[id] = msg
			}
		}
	}
	if exit != 0 && exit != 1 && len(out) == 0 {
		out["(the test files did not run)"] = tail(log, 800)
	}
	return out
}

// verifyOwn is the verifier the tdd arm can really have: its own reproduction
// test passes, and no existing test that passed before fails now.
func (s *SWE) verifyOwn(ctx context.Context, env *SWEEnv, work string, repro bool, existing []string, baseline map[string]string) (bool, string) {
	if repro {
		if exit, out := s.pytest(ctx, env, work, "-x", "--tb=short", sweReproFile); exit != 0 {
			return false, "The reproduction test still fails:\n" + tail(out, 2500)
		}
	}
	var broke []string
	for id, msg := range s.failing(ctx, env, work, existing) {
		if _, was := baseline[id]; !was {
			broke = append(broke, id+" - "+msg)
		}
	}
	if len(broke) > 0 {
		sort.Strings(broke)
		if len(broke) > 10 {
			broke = append(broke[:10], fmt.Sprintf("... and %d more", len(broke)-10))
		}
		return false, "Your edits break existing tests that passed before:\n" + strings.Join(broke, "\n")
	}
	return true, ""
}

// sweAcceptance has the author write the acceptance test from the issue
// alone. It never sees the implementation: a test needs the issue and the
// import conventions, and reading the source is most of what a large model
// costs on a real repository.
func sweAcceptance(ctx context.Context, m *Meter, role, model string, s *SWE, work string, existing []string, feedback string) (string, error) {
	style := ""
	for _, f := range existing {
		b, err := os.ReadFile(filepath.Join(work, f))
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		if len(lines) > 40 {
			lines = lines[:40]
		}
		style += fmt.Sprintf("# Existing test file %s (first lines only, for imports and style)\n```python\n%s\n```\n", f, strings.Join(lines, "\n"))
	}
	prompt := fmt.Sprintf(`# Issue
%s

# Where the fix will go
%s

%sWrite the acceptance tests for this issue as a pytest file, saved as %s in the repository root. You cannot see the implementation; write the tests from the issue.
- One to four test functions with plain asserts; import the way the existing test file does.
- Each test must fail on the current code and pass once the issue is fixed.
- Assert the exact results a maintainer would expect after the fix, not merely that no exception is raised. Where the issue leaves the exact result open, assert only what it does determine.
%s`, s.Problem, strings.Join(s.Files, "\n"), style, sweReproFile, feedback)
	raw, err := m.Complete(ctx, role, model, coderSystem, prompt, obj(map[string]any{"test_file": str()}))
	if err != nil {
		return "", err
	}
	var out struct {
		TestFile string `json:"test_file"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.TestFile) == "" {
		return "", errors.New("empty test file")
	}
	return out.TestFile, nil
}

// goldAccepts reports whether the reference fix passes the given test file.
func (s *SWE) goldAccepts(ctx context.Context, env *SWEEnv, test string) bool {
	tmp, err := os.MkdirTemp("", "swe-gold-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "r")
	if err := exec.Command("cp", "-cR", env.RepoDir, dir).Run(); err != nil {
		return false
	}
	ap := exec.CommandContext(ctx, "git", "apply", "-")
	ap.Dir = dir
	ap.Stdin = strings.NewReader(s.gold)
	if err := ap.Run(); err != nil {
		return false
	}
	if err := writeFile(dir, sweReproFile, test); err != nil {
		return false
	}
	exit, _ := s.pytest(ctx, env, dir, "-x", "--tb=short", sweReproFile)
	return exit == 0
}
