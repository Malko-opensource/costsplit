// Package runner wires the language-neutral core: it validates plans, applies
// bodies through an adapter, runs mechanical checks first, calls the decider
// only when everything passed, and recommends the next action (D-008, D-015).
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Malko-opensource/costsplit/internal/adapter"
	"github.com/Malko-opensource/costsplit/internal/decider"
	"github.com/Malko-opensource/costsplit/internal/ledger"
	"github.com/Malko-opensource/costsplit/internal/manifest"
	"github.com/Malko-opensource/costsplit/internal/policy"
	"github.com/Malko-opensource/costsplit/internal/verdict"
)

const Dot = ".graybox"

type Runner struct {
	Dir    string
	A      adapter.Adapter
	D      decider.Decider
	Limits policy.Limits
	Now    func() time.Time
}

type Result struct {
	Gate          string                    `json:"gate"`
	Task          string                    `json:"task,omitempty"`
	Model         string                    `json:"model,omitempty"`
	Attempt       int                       `json:"attempt,omitempty"`
	Checks        []adapter.Check           `json:"checks,omitempty"`
	DeciderCalled bool                      `json:"decider_called"`
	Questions     []string                  `json:"questions,omitempty"`
	Answers       map[string]decider.Answer `json:"answers,omitempty"`
	Outcome       *verdict.Outcome          `json:"outcome,omitempty"`
	Next          policy.Next               `json:"next"`
	Feedback      []string                  `json:"feedback,omitempty"`
	Error         string                    `json:"error,omitempty"`
}

func (r *Runner) path(name string) string { return filepath.Join(r.Dir, Dot, name) }

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// ---- lock (human approval of the graybox, D-007) ----

// Lock records the approved public surface in api.lock. It refuses when the
// language preconditions fail (D-019), when a blocking question is open, or
// when the graybox itself does not pass its checks.
func (r *Runner) Lock() (Result, error) {
	res := Result{Gate: "lock"}
	pre := r.A.Preconditions()
	res.Checks = append(res.Checks, pre...)
	m, err := manifest.LoadManifest(r.path("manifest.json"))
	if err != nil {
		res.Checks = append(res.Checks, fail("manifest", err.Error()))
	}
	q, err := manifest.LoadQuestions(r.path("questions.json"))
	if err != nil {
		res.Checks = append(res.Checks, fail("questions", err.Error()))
	} else if b := q.Blocking(); len(b) > 0 {
		res.Checks = append(res.Checks, fail("blocking_questions", strings.Join(b, ", ")))
	}
	if m != nil {
		res.Checks = append(res.Checks, r.syncCheck(m))
	}
	res.Checks = append(res.Checks, r.A.Checks()...)
	if !adapter.AllPass(res.Checks) {
		res.Error = "graybox not approvable: " + failedNames(res.Checks)
		return res, errors.New(res.Error)
	}
	snap, err := r.A.APISnapshot()
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(r.path("api.lock"), []byte(strings.Join(snap, "\n")+"\n"), 0o644); err != nil {
		return res, err
	}
	res.Next = "locked"
	return res, nil
}

// ---- plan gate ----

func (r *Runner) Plan(ctx context.Context, task string, model policy.Model, p *manifest.Plan) (Result, error) {
	res := Result{Gate: "plan", Task: task, Model: string(model)}
	led := ledger.Ledger{Path: r.path("decisions.jsonl")}
	n, err := led.Attempts(task, "plan", string(model))
	if err != nil {
		return res, err
	}
	res.Attempt = n + 1

	m, err := manifest.LoadManifest(r.path("manifest.json"))
	if err != nil {
		return res, err
	}
	if _, err := os.Stat(r.path("api.lock")); err != nil {
		return res, fmt.Errorf("graybox is not approved yet: run lock first")
	}

	// Mechanical plan check before any decider call (D-010, AC-2).
	var probs []string
	targets := map[string][]string{}
	for _, t := range p.Targets {
		sym, err := r.A.Resolve(t.Symbol)
		switch {
		case err != nil:
			probs = append(probs, fmt.Sprintf("%s: %v", t.Symbol, err))
		case !sym.IsStub:
			probs = append(probs, fmt.Sprintf("%s: not a stub", t.Symbol))
		}
		if len(t.Reqs) == 0 {
			probs = append(probs, fmt.Sprintf("%s: no requirements", t.Symbol))
		}
		for _, id := range t.Reqs {
			if _, ok := m.Req(id); !ok {
				probs = append(probs, fmt.Sprintf("%s: %s not in manifest", t.Symbol, id))
			}
		}
		targets[t.Symbol] = t.Reqs
	}
	if len(probs) > 0 {
		res.Checks = []adapter.Check{fail("plan_targets", strings.Join(probs, "; "))}
		res.Feedback = probs
		res.Next = policy.Decide(model, res.Attempt, policy.SigFail, r.Limits, true)
		return res, r.log(led, res, "")
	}
	res.Checks = []adapter.Check{{Name: "plan_targets", Status: adapter.CheckPass}}

	qs := verdict.PlanQuestions(targets)
	state := planState(m, p, r.A)
	r.ask(ctx, &res, qs, state, true)
	if res.Next == policy.Proceed {
		b, _ := json.MarshalIndent(p, "", "  ")
		if err := os.WriteFile(r.path("plan.json"), b, 0o644); err != nil {
			return res, err
		}
	}
	return res, r.log(led, res, "")
}

// ---- done gate ----

// Apply inserts a body through the adapter, runs mechanical checks, and only
// when all pass asks the decider (D-008, D-014, D-016).
func (r *Runner) Apply(ctx context.Context, task string, model policy.Model, symbol, body string) (Result, error) {
	res := Result{Gate: "done", Task: task, Model: string(model)}
	led := ledger.Ledger{Path: r.path("decisions.jsonl")}
	n, err := led.Attempts(task, "done", string(model))
	if err != nil {
		return res, err
	}
	res.Attempt = n + 1

	p, err := manifest.LoadPlan(r.path("plan.json"))
	if err != nil {
		return res, fmt.Errorf("no accepted plan: %w", err)
	}
	var reqs []string
	for _, t := range p.Targets {
		if t.Symbol == symbol {
			reqs = t.Reqs
		}
	}
	if reqs == nil {
		return res, fmt.Errorf("%s is not in the accepted plan", symbol)
	}
	m, err := manifest.LoadManifest(r.path("manifest.json"))
	if err != nil {
		return res, err
	}

	if err := r.A.Apply(symbol, body); err != nil {
		res.Checks = []adapter.Check{fail("apply", err.Error())}
	} else {
		res.Checks = r.mechanical(m)
	}
	r.writeVerify(res.Checks)
	if !adapter.AllPass(res.Checks) {
		res.Feedback = failDetails(res.Checks)
		res.Next = policy.Decide(model, res.Attempt, policy.SigFail, r.Limits, false)
		return res, r.log(led, res, symbol)
	}

	qs := verdict.DoneQuestions(reqs)
	r.ask(ctx, &res, qs, doneState(m, reqs, symbol, body), false)
	return res, r.log(led, res, symbol)
}

// ApplyAll inserts several bodies as one attempt: every symbol must be in the
// accepted plan, mechanical checks run once, and the done gate judges all
// requirements of the plan in one decider call (D-016).
func (r *Runner) ApplyAll(ctx context.Context, task string, model policy.Model, bodies map[string]string) (Result, error) {
	res := Result{Gate: "done", Task: task, Model: string(model)}
	led := ledger.Ledger{Path: r.path("decisions.jsonl")}
	n, err := led.Attempts(task, "done", string(model))
	if err != nil {
		return res, err
	}
	res.Attempt = n + 1

	p, err := manifest.LoadPlan(r.path("plan.json"))
	if err != nil {
		return res, fmt.Errorf("no accepted plan: %w", err)
	}
	inPlan := map[string]bool{}
	seen := map[string]bool{}
	var reqs []string
	for _, t := range p.Targets {
		inPlan[t.Symbol] = true
		for _, id := range t.Reqs {
			if !seen[id] {
				seen[id] = true
				reqs = append(reqs, id)
			}
		}
	}
	sort.Strings(reqs)
	m, err := manifest.LoadManifest(r.path("manifest.json"))
	if err != nil {
		return res, err
	}

	symbols := make([]string, 0, len(bodies))
	for s := range bodies {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	var applyErrs []string
	for _, s := range symbols {
		if !inPlan[s] {
			applyErrs = append(applyErrs, s+": not in the accepted plan")
			continue
		}
		if err := r.A.Apply(s, bodies[s]); err != nil {
			applyErrs = append(applyErrs, s+": "+err.Error())
		}
	}
	if len(applyErrs) > 0 {
		res.Checks = []adapter.Check{fail("apply", strings.Join(applyErrs, "; "))}
	} else {
		res.Checks = r.mechanical(m)
	}
	r.writeVerify(res.Checks)
	if !adapter.AllPass(res.Checks) {
		res.Feedback = failDetails(res.Checks)
		res.Next = policy.Decide(model, res.Attempt, policy.SigFail, r.Limits, false)
		return res, r.log(led, res, strings.Join(symbols, ","))
	}

	var b strings.Builder
	for _, id := range reqs {
		req, _ := m.Req(id)
		fmt.Fprintf(&b, "%s: %s\n", id, req.Text)
	}
	for _, s := range symbols {
		fmt.Fprintf(&b, "--- changed: %s ---\n%s\n", s, bodies[s])
	}
	b.WriteString("--- mechanical checks: all pass ---\n")
	r.ask(ctx, &res, verdict.DoneQuestions(reqs), b.String(), false)
	return res, r.log(led, res, strings.Join(symbols, ","))
}

// Check runs the mechanical checks only; it is how direct edits are caught
// (D-014, AC-4).
func (r *Runner) Check() (Result, error) {
	res := Result{Gate: "check"}
	m, err := manifest.LoadManifest(r.path("manifest.json"))
	if err != nil {
		return res, err
	}
	res.Checks = r.mechanical(m)
	r.writeVerify(res.Checks)
	if adapter.AllPass(res.Checks) {
		res.Next = "ok"
	} else {
		res.Next = policy.RetrySmall
		res.Feedback = failDetails(res.Checks)
	}
	return res, nil
}

// ---- internals ----

func (r *Runner) ask(ctx context.Context, res *Result, qs map[string]decider.Question, state string, planGate bool) {
	for k := range qs {
		res.Questions = append(res.Questions, k)
	}
	sort.Strings(res.Questions)
	res.DeciderCalled = true
	answers, err := r.D.Decide(ctx, state, qs)
	if err != nil {
		res.Error = err.Error()
		res.Next = policy.Paused
		return
	}
	if err := decider.Validate(qs, answers); err != nil { // fakes too
		res.Error = err.Error()
		res.Next = policy.Paused
		return
	}
	res.Answers = answers
	out := verdict.Evaluate(qs, answers)
	res.Outcome = &out
	for _, f := range out.Failures {
		res.Feedback = append(res.Feedback, fmt.Sprintf("%s: %s", f.Key, f.Description))
	}
	sig := policy.Classify(string(out.Result), out.MinConfidence, r.Limits)
	switch {
	case planGate && r.Limits.AdvisoryPlan:
		sig = policy.SigPass
	case !planGate && r.Limits.TestsDecideDone:
		sig = policy.SigPass
		if a, ok := answers[verdict.TestGamingKey]; ok && a.Choice == "yes" {
			sig = policy.SigFail
		}
	}
	res.Next = policy.Decide(policy.Model(res.Model), res.Attempt, sig, r.Limits, planGate)
}

func (r *Runner) mechanical(m *manifest.Manifest) []adapter.Check {
	cs := []adapter.Check{r.boundaryCheck(), r.syncCheck(m)}
	return append(cs, r.A.Checks()...)
}

func (r *Runner) boundaryCheck() adapter.Check {
	lock, err := os.ReadFile(r.path("api.lock"))
	if err != nil {
		return fail("boundary", "api.lock missing: graybox not approved")
	}
	snap, err := r.A.APISnapshot()
	if err != nil {
		return fail("boundary", err.Error())
	}
	want := lines(string(lock))
	got := map[string]bool{}
	for _, s := range snap {
		got[s] = true
	}
	var diff []string
	for _, w := range want {
		if !got[w] {
			diff = append(diff, "- "+w)
		}
		delete(got, w)
	}
	for g := range got {
		diff = append(diff, "+ "+g)
	}
	if len(diff) > 0 {
		sort.Strings(diff)
		return fail("boundary", "public API or graybox data changed since approval:\n"+strings.Join(diff, "\n"))
	}
	return adapter.Check{Name: "boundary", Status: adapter.CheckPass}
}

func (r *Runner) syncCheck(m *manifest.Manifest) adapter.Check {
	tests, err := r.A.ListTests()
	if err != nil {
		return fail("manifest_sync", err.Error())
	}
	if p := m.SyncProblems(tests); len(p) > 0 {
		return fail("manifest_sync", strings.Join(p, "; "))
	}
	return adapter.Check{Name: "manifest_sync", Status: adapter.CheckPass}
}

func (r *Runner) writeVerify(cs []adapter.Check) {
	b, _ := json.MarshalIndent(map[string]any{"time": r.now(), "checks": cs}, "", "  ")
	_ = os.WriteFile(r.path("verify.json"), b, 0o644)
}

func (r *Runner) log(led ledger.Ledger, res Result, symbol string) error {
	return led.Append(ledger.Entry{
		Time: r.now(), Task: res.Task, Gate: res.Gate, Model: res.Model, Attempt: res.Attempt,
		Symbol: symbol, Checks: res.Checks, Decider: res.DeciderCalled, Answers: res.Answers,
		Outcome: res.Outcome, Next: string(res.Next), Error: res.Error,
	})
}

func planState(m *manifest.Manifest, p *manifest.Plan, a adapter.Adapter) string {
	var b strings.Builder
	for _, t := range p.Targets {
		sym, _ := a.Resolve(t.Symbol)
		fmt.Fprintf(&b, "--- target %s ---\n%s\n", t.Symbol, sym.Signature)
		for _, id := range t.Reqs {
			req, _ := m.Req(id)
			fmt.Fprintf(&b, "%s: %s\n", id, req.Text)
		}
	}
	return b.String()
}

func doneState(m *manifest.Manifest, reqs []string, symbol, body string) string {
	var b strings.Builder
	for _, id := range reqs {
		req, _ := m.Req(id)
		fmt.Fprintf(&b, "%s: %s\n", id, req.Text)
	}
	fmt.Fprintf(&b, "--- changed: %s ---\n%s\n--- mechanical checks: all pass ---\n", symbol, body)
	return b.String()
}

func fail(name, detail string) adapter.Check {
	return adapter.Check{Name: name, Status: adapter.CheckFail, Detail: detail}
}

func failDetails(cs []adapter.Check) []string {
	var out []string
	for _, c := range cs {
		if c.Status == adapter.CheckFail {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return out
}

func failedNames(cs []adapter.Check) string {
	var out []string
	for _, c := range cs {
		if c.Status == adapter.CheckFail {
			out = append(out, c.Name)
		}
	}
	return strings.Join(out, ", ")
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
