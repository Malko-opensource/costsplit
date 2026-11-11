package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Malko-opensource/costsplit/internal/adapter"
	"github.com/Malko-opensource/costsplit/internal/extadapter"
	"github.com/Malko-opensource/costsplit/internal/goadapter"
	"github.com/Malko-opensource/costsplit/internal/manifest"
	"github.com/Malko-opensource/costsplit/internal/policy"
	"github.com/Malko-opensource/costsplit/internal/runner"
)

type Config struct {
	Small   string
	Large   string
	Decider string
	Limits  policy.Limits
	Tries   int // attempts for the single-model arms (Aider uses 2)
}

type Result struct {
	Lang      string           `json:"lang"`
	Exercise  string           `json:"exercise"`
	Arm       string           `json:"arm"`
	Pass      bool             `json:"pass"`
	Outcome   string           `json:"outcome"` // pass | fail | graybox_invalid | needs_human | paused | error
	Usage     map[string]Usage `json:"usage"`
	Total     Usage            `json:"total"`
	Escalated bool             `json:"escalated,omitempty"`
	// DeciderDone is true when the graybox pipeline ended in "done"; with
	// Pass it measures the decider's false passes (F4).
	DeciderDone bool `json:"decider_done,omitempty"`
	// Cascade arms: the stage ("small" or "hint") and attempt number that
	// passed, and the hint the large model wrote.
	Stage string `json:"stage,omitempty"`
	Try   int    `json:"try,omitempty"`
	Hint  string `json:"hint,omitempty"`
	// SWE contract arms: Red is true when the model's own test failed on the
	// unfixed code as required; OwnGreen when the arm's verifier passed.
	Red      bool `json:"red,omitempty"`
	OwnGreen bool `json:"own_green,omitempty"`
	// Contract is the test the author wrote; GoldGreen says whether the
	// reference fix passes it (diagnostic, set only when Red).
	Contract  string `json:"contract,omitempty"`
	GoldGreen bool   `json:"gold_green,omitempty"`
	// Info carries arm-specific diagnostics (slice sizes, what was named).
	Info map[string]any `json:"info,omitempty"`
	// Final holds the solution files as they ended, kept only when
	// POLYBENCH_KEEP_FINAL is set (to inspect what a passing answer looks like).
	Final       map[string]string `json:"final,omitempty"`
	Steps       []string          `json:"steps,omitempty"`
	Error       string            `json:"error,omitempty"`
	ScoreOutput string            `json:"score_output,omitempty"`
	Seconds     float64           `json:"seconds"`
}

const coderSystem = "You are an expert programmer solving an exercise. Follow the output format exactly."

// ---------- single-model arm (Aider style: 2 tries, test output as feedback) ----------

func RunSingle(ctx context.Context, env *Env, cfg Config, x *Exercise, arm, model string) Result {
	start := time.Now()
	res := Result{Lang: x.Lang, Exercise: x.Name, Arm: arm}
	meter := NewMeter(cfgLLM(ctx))
	work, err := os.MkdirTemp("", "polybench-"+arm+"-")
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer os.RemoveAll(work)
	if err := x.Prepare(work); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	feedback := ""
	for try := 1; try <= cfg.Tries; try++ {
		prompt := fmt.Sprintf("%s\n\n# Files you must edit\n%s\n# Read-only files\n%s\n%s\nReturn the complete new contents of every file you must edit.",
			x.Instructions, fileList(Files(work, x.Solution)), fileList(Files(work, x.Editor)), feedback)
		raw, err := meter.Complete(ctx, arm, model, coderSystem, prompt, filesSchema(x.Solution))
		if err != nil {
			res.Steps = append(res.Steps, fmt.Sprintf("try %d: model error: %v", try, err))
			continue
		}
		if err := writeFiles(work, raw, x.Solution); err != nil {
			res.Steps = append(res.Steps, fmt.Sprintf("try %d: %v", try, err))
			continue
		}
		ok, out := x.Score(ctx, env, work)
		res.ScoreOutput = tail(out, 1500)
		if ok {
			res.Pass, res.Outcome = true, "pass"
			res.Steps = append(res.Steps, fmt.Sprintf("try %d: pass", try))
			break
		}
		res.Steps = append(res.Steps, fmt.Sprintf("try %d: fail", try))
		feedback = "\n# Your previous answer failed these tests\n" + tail(out, 3000) + "\nFix the code.\n"
	}
	if res.Outcome == "" {
		res.Outcome = "fail"
	}
	res.Usage, res.Total, res.Seconds = meter.Snapshot(), meter.Total(), time.Since(start).Seconds()
	return res
}

// ---------- graybox arm ----------

func RunGraybox(ctx context.Context, env *Env, cfg Config, x *Exercise) (out Result) {
	start := time.Now()
	meter := NewMeter(cfgLLM(ctx))
	// Named result: usage is filled after whichever return path ran.
	defer func() {
		out.Usage, out.Total, out.Seconds = meter.Snapshot(), meter.Total(), time.Since(start).Seconds()
	}()
	return runGraybox(ctx, env, cfg, x, meter)
}

func runGraybox(ctx context.Context, env *Env, cfg Config, x *Exercise, meter *Meter) Result {
	res := Result{Lang: x.Lang, Exercise: x.Name, Arm: "graybox"}
	work, err := os.MkdirTemp("", "polybench-graybox-")
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer os.RemoveAll(work)
	if err := x.Prepare(work); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	if err := os.MkdirAll(filepath.Join(work, runner.Dot), 0o755); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	writeLangConfig(env, x.Lang, work, x.Solution)
	r := &runner.Runner{Dir: work, A: newAdapter(env, x.Lang, work), D: &LLMDecider{Meter: meter, Model: cfg.Decider}, Limits: cfg.Limits}
	step := func(f string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(f, a...)) }

	// 1. Large model writes the graybox; it is locked as if a human approved it.
	var reqs []reqOut
	feedback := ""
	locked := false
	for try := 1; try <= 2 && !locked; try++ {
		gb, err := writeGraybox(ctx, meter, cfg.Large, x, work, feedback)
		if err != nil {
			step("graybox try %d: %v", try, err)
			feedback = "\nYour previous answer could not be used: " + err.Error() + "\n"
			continue
		}
		reqs = gb
		lr, err := r.Lock()
		if err == nil {
			locked = true
			step("graybox try %d: locked (%d requirements)", try, len(reqs))
			break
		}
		step("graybox try %d: not approvable: %s", try, lr.Error)
		feedback = "\n# Your previous graybox failed these checks\n" + strings.Join(failDetails(lr.Checks), "\n") + "\nFix the graybox.\n"
	}
	if !locked {
		res.Outcome = "graybox_invalid"
		res.Pass, res.ScoreOutput = x.Score(ctx, env, work)
		return res
	}

	// 2. Plan gate.
	stubs, err := r.A.Stubs()
	if err != nil || len(stubs) == 0 {
		res.Outcome, res.Error = "error", fmt.Sprintf("no stubs after lock: %v", err)
		return res
	}
	model := policy.Small
	modelName := func() string {
		if model == policy.Large {
			return cfg.Large
		}
		return cfg.Small
	}
	planned := false
	feedback = ""
	for i := 0; i < cfg.Limits.SmallRetries+cfg.Limits.LargeRetries+1 && !planned; i++ {
		p, err := askPlan(ctx, meter, string(model), modelName(), x, work, reqs, stubs, feedback)
		if err != nil {
			step("plan (%s): model error: %v", model, err)
			continue
		}
		pr, err := r.Plan(ctx, "T", model, p)
		if err != nil {
			res.Outcome, res.Error = "error", err.Error()
			return res
		}
		step("plan (%s) attempt %d: %s %s %s", model, pr.Attempt, pr.Next, answersBrief(pr), tail(strings.Join(pr.Feedback, " | "), 400))
		switch pr.Next {
		case policy.Proceed:
			planned = true
		case policy.EscalateLarge:
			model, res.Escalated, feedback = policy.Large, true, ""
		case policy.EscalateHuman:
			return finish(ctx, env, x, work, &res, "needs_human")
		case policy.Paused:
			res.Error = pr.Error
			return finish(ctx, env, x, work, &res, "paused")
		default:
			feedback = "\n# Your previous plan was rejected\n" + strings.Join(pr.Feedback, "\n") + "\n"
		}
	}
	if !planned {
		return finish(ctx, env, x, work, &res, "needs_human")
	}
	plan, _ := manifest.LoadPlan(filepath.Join(work, runner.Dot, "plan.json"))

	// 3. Implementation through ApplyAll, following the tool's recommendation.
	feedback = ""
	for i := 0; i < cfg.Limits.SmallRetries+cfg.Limits.LargeRetries+2; i++ {
		bodies, err := askBodies(ctx, meter, string(model), modelName(), x, work, reqs, plan, feedback)
		if err != nil {
			step("implement (%s): model error: %v", model, err)
			continue
		}
		ar, err := r.ApplyAll(ctx, "T", model, bodies)
		if err != nil {
			res.Outcome, res.Error = "error", err.Error()
			return res
		}
		step("implement (%s) attempt %d: %s %s %s", model, ar.Attempt, ar.Next, answersBrief(ar), tail(strings.Join(ar.Feedback, " | "), 400))
		switch ar.Next {
		case policy.Done:
			res.DeciderDone = true
			return finish(ctx, env, x, work, &res, "")
		case policy.EscalateLarge:
			model, res.Escalated = policy.Large, true
			feedback = "\n# A previous attempt by another model failed\n" + strings.Join(ar.Feedback, "\n") + "\n"
		case policy.EscalateHuman:
			return finish(ctx, env, x, work, &res, "needs_human")
		case policy.Paused:
			res.Error = ar.Error
			return finish(ctx, env, x, work, &res, "paused")
		default:
			feedback = "\n# Your previous attempt failed\n" + strings.Join(ar.Feedback, "\n") + "\nFix the bodies.\n"
		}
	}
	return finish(ctx, env, x, work, &res, "needs_human")
}

func answersBrief(r runner.Result) string {
	keys := make([]string, 0, len(r.Answers))
	for k := range r.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		a := r.Answers[k]
		parts = append(parts, fmt.Sprintf("%s=%s@%.2f", k, a.Choice, a.Confidence))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func finish(ctx context.Context, env *Env, x *Exercise, work string, res *Result, outcome string) Result {
	ok, out := x.Score(ctx, env, work)
	res.Pass, res.ScoreOutput = ok, tail(out, 1500)
	switch {
	case outcome != "":
		res.Outcome = outcome
	case ok:
		res.Outcome = "pass"
	default:
		res.Outcome = "fail"
	}
	return *res
}

func failDetails(cs []adapter.Check) []string {
	var out []string
	for _, c := range cs {
		if c.Status == adapter.CheckFail {
			out = append(out, c.Name+": "+tail(c.Detail, 1500))
		}
	}
	return out
}

// ---------- graybox writing ----------

type reqOut struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Test string `json:"test"`
}

func writeGraybox(ctx context.Context, m *Meter, model string, x *Exercise, work, feedback string) ([]reqOut, error) {
	testFile := GrayboxTestFile[x.Lang]
	prompt := fmt.Sprintf(`%s

# Your job: write a graybox, not the solution
A graybox fixes the structure so a smaller model can fill in function bodies later.
The smaller model can ONLY replace bodies of stub functions; it cannot add or change signatures, types, fields, files or imports.

Rules for %s:
%s

Also write requirement tests in %s: one skipped test per requirement, named as shown above.
List 3 to 8 requirements that together cover the exercise; each has an id REQ-001, REQ-002, ..., a one-sentence text, and the exact name of its test.

# Current files you may rewrite
%s
# Read-only files
%s
%s`, x.Instructions, x.Lang, langRules[x.Lang], testFile, fileList(Files(work, x.Solution)), fileList(Files(work, x.Editor)), feedback)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"files":        filesSchema(x.Solution)["properties"].(map[string]any)["files"],
			"test_file":    map[string]any{"type": "string"},
			"requirements": map[string]any{"type": "array", "minItems": 1, "items": obj(map[string]any{"id": str(), "text": str(), "test": str()})},
		},
		"required":             []string{"files", "test_file", "requirements"},
		"additionalProperties": false,
	}
	raw, err := m.Complete(ctx, "large", model, coderSystem, prompt, schema)
	if err != nil {
		return nil, err
	}
	var out struct {
		Files        json.RawMessage `json:"files"`
		TestFile     string          `json:"test_file"`
		Requirements []reqOut        `json:"requirements"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if err := writeFiles(work, mustJSON(map[string]any{"files": out.Files}), x.Solution); err != nil {
		return nil, err
	}
	if err := writeFile(work, testFile, out.TestFile); err != nil {
		return nil, err
	}
	man := manifest.Manifest{Schema: "graybox.manifest/v1"}
	for _, r := range out.Requirements {
		man.Requirements = append(man.Requirements, manifest.Requirement{ID: r.ID, Text: r.Text, Tests: []string{r.Test}, Coverage: "full"})
	}
	if err := writeFile(work, filepath.Join(runner.Dot, "manifest.json"), string(mustJSON(man))); err != nil {
		return nil, err
	}
	return out.Requirements, writeFile(work, filepath.Join(runner.Dot, "questions.json"), `{"schema":"graybox.questions/v1","questions":[]}`)
}

var langRules = map[string]string{
	"go": `- Keep the package name and every exported signature already present; add any types or functions the exercise needs.
- Every function that needs logic has the body exactly: panic("not implemented")
- Declare every struct field and type the implementation will need now.
- Do not add imports for future code; imports are fixed automatically when bodies are filled.
- Requirement tests live in graybox_test.go, package the same as the solution: func TestREQ001_ShortName(t *testing.T) { t.Skip("graybox") }`,
	"python": `- Keep module, function and class names. Add complete type annotations to every function, method, parameter and return so pyright --strict passes.
- Every function or method that needs logic has the body exactly: raise NotImplementedError
- Requirement tests live in test_graybox.py: import unittest; class GrayboxTest(unittest.TestCase) with methods def test_REQ001_short_name(self) -> None: self.skipTest("graybox")`,
	"java": `- Keep class names and public signatures; add any classes, methods and private fields the implementation will need (default package if the stub has none).
- Every method that needs logic has the body exactly: throw new UnsupportedOperationException("not implemented");
- A class may have at most one constructor; if it needs logic its body is the same throw statement.
- Add the imports (java.util.*, etc.) the implementation will likely need.
- Requirement tests live in src/test/java/GrayboxTest.java using JUnit 5 only (org.junit.jupiter.api.Test, Assertions, Assumptions): @Test void testREQ001_shortName() { Assumptions.abort("graybox"); }`,
	"javascript": `- Keep the ES module exports and names. Add JSDoc types (@param, @returns, @typedef) to every exported function and class so tsc --checkJs --strict passes.
- Every function that needs logic has a block body exactly: throw new Error("not implemented"); (arrow functions too, with braces).
- Requirement tests live in graybox.test.mjs: import { test } from "node:test"; import assert from "node:assert/strict"; import what you test from the solution file with its .js extension; each test: test("REQ-001: short name", (t) => { t.skip("graybox"); });`,
}

// ---------- plan and bodies ----------

func askPlan(ctx context.Context, m *Meter, role, model string, x *Exercise, work string, reqs []reqOut, stubs []string, feedback string) (*manifest.Plan, error) {
	stubs = uniq(stubs)
	prompt := fmt.Sprintf(`%s

# Graybox (fixed; you will later fill only stub bodies)
%s
# Requirements
%s
# Stub symbols
%s
Map EVERY stub symbol above to the requirement ids it serves (test stubs map to their own requirement). Use the symbols exactly as written.
%s`, x.Instructions, fileList(Files(work, append(append([]string{}, x.Solution...), GrayboxTestFile[x.Lang]))), reqList(reqs), strings.Join(stubs, "\n"), feedback)
	schema := obj(map[string]any{"targets": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"symbol": map[string]any{"type": "string", "enum": stubs}, "reqs": map[string]any{"type": "array", "minItems": 1, "items": str()}})}})
	raw, err := m.Complete(ctx, role, model, coderSystem, prompt, schema)
	if err != nil {
		return nil, err
	}
	var p manifest.Plan
	return &p, json.Unmarshal(raw, &p)
}

func askBodies(ctx context.Context, m *Meter, role, model string, x *Exercise, work string, reqs []reqOut, plan *manifest.Plan, feedback string) (map[string]string, error) {
	var syms []string
	for _, t := range plan.Targets {
		syms = append(syms, t.Symbol)
	}
	sort.Strings(syms)
	prompt := fmt.Sprintf(`%s

# Graybox (signatures are fixed)
%s
# Requirements
%s
# Symbols to fill
%s
Return the body of every symbol: only the statements inside the function (no signature, no enclosing braces).
Test symbols get real assertions for their requirement. Define any helpers inside the body (nested functions or closures).
Java: use fully qualified names for classes that are not imported.
%s`, x.Instructions, fileList(Files(work, append(append([]string{}, x.Solution...), GrayboxTestFile[x.Lang]))), reqList(reqs), strings.Join(syms, "\n"), feedback)
	schema := obj(map[string]any{"bodies": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"symbol": map[string]any{"type": "string", "enum": syms}, "body": str()})}})
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
	return bodies, nil
}

func reqList(reqs []reqOut) string {
	var b strings.Builder
	for _, r := range reqs {
		fmt.Fprintf(&b, "%s: %s (test: %s)\n", r.ID, r.Text, r.Test)
	}
	return b.String()
}

// ---------- language setup ----------

func newAdapter(env *Env, lang, work string) adapter.Adapter {
	switch lang {
	case "go":
		return goadapter.New(work)
	case "python":
		return &extadapter.Adapter{Language: lang, Cmd: []string{"python3", filepath.Join(env.RepoRoot, "adapters/python/adapter.py")}, Dir: work}
	case "java":
		return &extadapter.Adapter{Language: lang, Cmd: []string{filepath.Join(env.RepoRoot, "adapters/jvm/java-adapter.sh")}, Dir: work}
	default: // javascript
		return &extadapter.Adapter{Language: lang, Cmd: []string{"node", filepath.Join(env.RepoRoot, "adapters/ts/adapter.mjs")}, Dir: work}
	}
}

func writeLangConfig(env *Env, lang, work string, solution []string) {
	switch lang {
	case "python":
		_ = writeFile(work, "pyrightconfig.json", `{"typeCheckingMode":"strict","reportUnusedImport":"warning","reportMissingModuleSource":"none","exclude":["**/*_test.py"]}`)
	case "javascript":
		include := append(append([]string{}, solution...), GrayboxTestFile[lang])
		cfg := map[string]any{"compilerOptions": map[string]any{
			"allowJs": true, "checkJs": true, "strict": true, "noEmit": true, "target": "esnext",
			"module": "esnext", "moduleResolution": "bundler", "skipLibCheck": true,
			"types": []string{"node"}, "typeRoots": []string{filepath.Join(env.RepoRoot, "adapters/ts/node_modules/@types")},
		}, "include": include}
		_ = writeFile(work, "tsconfig.json", string(mustJSON(cfg)))
		// Official spec files are hidden; keep jest config out of the typecheck.
	}
}

// ---------- JSON helpers ----------

func str() map[string]any { return map[string]any{"type": "string"} }

func obj(props map[string]any) map[string]any {
	req := make([]string, 0, len(props))
	for k := range props {
		req = append(req, k)
	}
	sort.Strings(req)
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}

func filesSchema(paths []string) map[string]any {
	return obj(map[string]any{"files": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"path": map[string]any{"type": "string", "enum": paths}, "content": str()})}})
}

func writeFiles(work string, raw json.RawMessage, allowed []string) error {
	var out struct {
		Files []struct{ Path, Content string } `json:"files"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, f := range out.Files {
		if !ok[f.Path] {
			return fmt.Errorf("refusing to write %q: not a solution file", f.Path)
		}
		if err := writeFile(work, f.Path, f.Content); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(work, rel, content string) error {
	p := filepath.Join(work, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0o644)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

// cfgLLM is replaced in tests; the default backend is the Claude CLI.
var cfgLLM = func(ctx context.Context) LLM { return &ClaudeCLI{} }
