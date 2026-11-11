package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CascadeOpt describes one "small model first" policy. The large model never
// writes code here: it can only add a short hint.
type CascadeOpt struct {
	SmallTries int  // small-model attempts, each fed the failing test output
	Plan       bool // the small model writes pseudocode before the files (its own pre-design)
	ShowTests  bool // the official tests are in the prompt from the first attempt (a visible contract)
	HintTries  int  // 0: stop after the small tries. Else the large model writes one hint and the small model gets this many more attempts
}

var cascadeArm = regexp.MustCompile(`^c(\d+)(p?)(t?)(?:h(\d+))?$`)

// ParseCascadeArm reads arm names like c5 (five small tries), c5p (with
// pseudocode first) and c2h2 (two small tries, a hint, two more tries).
func ParseCascadeArm(arm string) (CascadeOpt, bool) {
	m := cascadeArm.FindStringSubmatch(arm)
	if m == nil {
		return CascadeOpt{}, false
	}
	o := CascadeOpt{Plan: m[2] == "p", ShowTests: m[3] == "t"}
	o.SmallTries, _ = strconv.Atoi(m[1])
	if m[4] != "" {
		o.HintTries, _ = strconv.Atoi(m[4])
	}
	return o, o.SmallTries > 0
}

// RunCascade stops at the first attempt that passes the official tests and
// records the stage ("small" or "hint") and the attempt number.
func RunCascade(ctx context.Context, env *Env, cfg Config, x *Exercise, arm string, opt CascadeOpt) (res Result) {
	start := time.Now()
	res = Result{Lang: x.Lang, Exercise: x.Name, Arm: arm, Outcome: "fail"}
	meter := NewMeter(cfgLLM(ctx))
	defer func() {
		res.Usage, res.Total, res.Seconds = meter.Snapshot(), meter.Total(), time.Since(start).Seconds()
	}()
	work, err := os.MkdirTemp("", "polybench-"+arm+"-")
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer os.RemoveAll(work)
	defer func() {
		if os.Getenv("POLYBENCH_KEEP_FINAL") != "" {
			res.Final = Files(work, x.Solution)
		}
	}()
	if err := x.Prepare(work); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	step := func(f string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(f, a...)) }

	schema := filesSchema(x.Solution)
	planRule := ""
	if opt.Plan {
		// "approach" sorts before "files", so it is also generated first.
		schema = obj(map[string]any{"approach": str(), "files": schema["properties"].(map[string]any)["files"]})
		planRule = "Before the files, write `approach`: pseudocode for your solution in at most 15 lines, naming each edge case the instructions require. On a retry, start it with what the failing tests show is wrong.\n"
	}
	readOnly := fileList(Files(work, x.Editor))
	if opt.ShowTests {
		readOnly += "# Tests your code must pass (read-only)\n" + fileList(Files(x.Dir, x.Test))
	}
	testOut, try := "", 0
	attempt := func(stage, hint string) bool {
		try++
		feedback := ""
		if testOut != "" {
			feedback = "\n# Your previous answer failed these tests\n" + tail(testOut, 3000) + "\nFix the code.\n"
		}
		if hint != "" {
			feedback += "\n# Hint from a senior engineer who read your attempt\n" + hint + "\n"
		}
		prompt := fmt.Sprintf("%s\n\n# Files you must edit\n%s\n# Read-only files\n%s\n%s\n%sReturn the complete new contents of every file you must edit.",
			x.Instructions, fileList(Files(work, x.Solution)), readOnly, feedback, planRule)
		raw, err := meter.Complete(ctx, "small", cfg.Small, coderSystem, prompt, schema)
		if err != nil {
			step("%s try %d: model error: %v", stage, try, err)
			return false
		}
		note := ""
		if opt.Plan {
			a, f := strings.Index(string(raw), `"approach"`), strings.Index(string(raw), `"files"`)
			note = fmt.Sprintf(" (approach first: %v)", a >= 0 && a < f)
		}
		if err := writeFiles(work, raw, x.Solution); err != nil {
			step("%s try %d: %v", stage, try, err)
			return false
		}
		ok, out := x.Score(ctx, env, work)
		res.ScoreOutput = tail(out, 1500)
		if ok {
			res.Pass, res.Outcome, res.Stage, res.Try = true, "pass", stage, try
			step("%s try %d: pass%s", stage, try, note)
			return true
		}
		testOut = out
		step("%s try %d: fail%s", stage, try, note)
		return false
	}

	for i := 0; i < opt.SmallTries; i++ {
		if attempt("small", "") {
			return res
		}
	}
	if opt.HintTries == 0 {
		return res
	}
	hint, err := askHint(ctx, meter, cfg.Large, x, work, testOut)
	if err != nil {
		step("hint: model error: %v", err)
		return res
	}
	res.Hint = hint
	step("hint: %d words", len(strings.Fields(hint)))
	for i := 0; i < opt.HintTries; i++ {
		if attempt("hint", hint) {
			return res
		}
	}
	return res
}

// askHint has the large model read the failed attempt and say what to change
// without writing the code: its expensive output stays short.
func askHint(ctx context.Context, m *Meter, model string, x *Exercise, work, testOut string) (string, error) {
	prompt := fmt.Sprintf(`%s

# Files (a junior engineer's attempt; it fails)
%s
# Read-only files
%s
# Failing test output
%s

Write a hint for the junior engineer: what is wrong and how to fix it, as precise notes or short pseudocode. Do NOT write the solution code. At most 120 words.`,
		x.Instructions, fileList(Files(work, x.Solution)), fileList(Files(work, x.Editor)), tail(testOut, 3000))
	raw, err := m.Complete(ctx, "hint", model, coderSystem, prompt, obj(map[string]any{"hint": str()}))
	if err != nil {
		return "", err
	}
	var out struct {
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Hint, nil
}
