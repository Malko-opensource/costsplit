package runner_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/local/graybox/internal/adapter"
	"github.com/local/graybox/internal/decider"
	"github.com/local/graybox/internal/extadapter"
	"github.com/local/graybox/internal/goadapter"
	"github.com/local/graybox/internal/manifest"
	"github.com/local/graybox/internal/policy"
	"github.com/local/graybox/internal/runner"
)

// ---- fake decider ----

type fakeDecider struct {
	calls  int
	lastQs map[string]decider.Question
	mode   string // pass | fail_req002 | low | invalid | down | unsure
}

func (f *fakeDecider) Decide(_ context.Context, _ string, qs map[string]decider.Question) (map[string]decider.Answer, error) {
	f.calls++
	f.lastQs = qs
	if f.mode == "down" {
		return nil, fmt.Errorf("%w: 429 rate limited", decider.ErrUnavailable)
	}
	out := map[string]decider.Answer{}
	for k, q := range qs {
		choice := ""
		for _, c := range []string{"pass", "ok", "no"} {
			if _, ok := q.Criteria[c]; ok {
				choice = c
			}
		}
		conf := 0.95
		switch {
		case f.mode == "fail_req002" && k == "REQ-002" && q.Criteria["fail_logic"] != "":
			choice = "fail_logic"
		case f.mode == "low":
			conf = 0.4
		case f.mode == "invalid":
			choice = "maybe"
		case f.mode == "unsure" && k == decider_TestGaming:
			choice = "unsure"
		}
		out[k] = decider.Answer{Type: "choice", Choice: choice, Confidence: conf}
	}
	return out, nil
}

const decider_TestGaming = "test_gaming"

// ---- language cases ----

type langCase struct {
	name      string
	example   string
	adapter   func(dir string) adapter.Adapter
	symbol    string // the stub to implement
	goodBody  string
	badBody   string // parses but does not build / type-check
	breakSig  func(t *testing.T, dir string)
	available func() string // "" when runnable, else skip reason
}

func repoRoot(t *testing.T) string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func copyDir(t *testing.T, src string) string {
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "__pycache__" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "node_modules")); err == nil {
		if err := os.Symlink(filepath.Join(src, "node_modules"), filepath.Join(dst, "node_modules")); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func replaceInFile(t *testing.T, path, old, new string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s: %q not found", path, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cases(t *testing.T) []langCase {
	root := repoRoot(t)
	return []langCase{
		{
			name:    "go",
			example: filepath.Join(root, "examples/go/order"),
			adapter: func(dir string) adapter.Adapter { return goadapter.New(dir) },
			symbol:  "order.(*Service).Apply",
			goodBody: `o, err := s.repo.Get(ctx, id)
if err != nil {
	return Order{}, err
}
next, ok := Transitions[o.State][e]
if !ok {
	return o, ErrInvalidTransition
}
o.State = next
if err := s.repo.Save(ctx, o); err != nil {
	return Order{}, err
}
return o, nil`,
			badBody: `return undefinedThing, nil`,
			breakSig: func(t *testing.T, dir string) {
				replaceInFile(t, filepath.Join(dir, "service.go"), "e Event) (Order, error)", "e Event, force bool) (Order, error)")
			},
			available: func() string { return "" },
		},
		{
			name:    "python",
			example: filepath.Join(root, "examples/python/order"),
			adapter: func(dir string) adapter.Adapter {
				return &extadapter.Adapter{Language: "python", Cmd: []string{"python3", filepath.Join(root, "adapters/python/adapter.py")}, Dir: dir}
			},
			symbol: "order.service:Service.apply",
			goodBody: `order = self._repo.get(order_id)
nxt = TRANSITIONS.get(order.state, {}).get(event)
if nxt is None:
    raise InvalidTransition(f"{order.state} -> {event}")
updated = Order(id=order.id, state=nxt)
self._repo.save(updated)
return updated`,
			badBody: `return self._repo.get(order_id).no_such_attribute`,
			breakSig: func(t *testing.T, dir string) {
				replaceInFile(t, filepath.Join(dir, "order/service.py"), "event: Event) -> Order:", "event: Event, force: bool = False) -> Order:")
			},
			available: func() string { return pyrightPath(root) },
		},
		{
			name:    "ts",
			example: filepath.Join(root, "examples/ts/order"),
			adapter: func(dir string) adapter.Adapter {
				return &extadapter.Adapter{Language: "ts", Cmd: []string{"node", filepath.Join(root, "adapters/ts/adapter.mjs")}, Dir: dir}
			},
			symbol: "src/service.ts#Service.apply",
			goodBody: `const order = await this.repo.get(id);
const next = transitions[order.state]?.[event];
if (next === undefined) {
  throw new InvalidTransitionError(order.state, event);
}
const updated: Order = { ...order, state: next };
await this.repo.save(updated);
return updated;`,
			badBody: `return this.repo.noSuchMethod(id);`,
			breakSig: func(t *testing.T, dir string) {
				replaceInFile(t, filepath.Join(dir, "src/service.ts"), "event: Event): Promise<Order>", "event: Event, force?: boolean): Promise<Order>")
			},
			available: func() string {
				if _, err := os.Stat(filepath.Join(root, "adapters/ts/node_modules/typescript")); err != nil {
					return "adapters/ts: run npm install"
				}
				return ""
			},
		},
		{
			name:    "java",
			example: filepath.Join(root, "examples/java/order"),
			adapter: func(dir string) adapter.Adapter {
				return &extadapter.Adapter{Language: "java", Cmd: []string{filepath.Join(root, "adapters/jvm/java-adapter.sh")}, Dir: dir}
			},
			symbol: "com.acme.order.Service#apply",
			goodBody: `Order order = repo.get(id);
var row = Transitions.TABLE.get(order.state());
State next = row == null ? null : row.get(event);
if (next == null) {
    throw new InvalidTransitionException(order.state(), event);
}
Order updated = new Order(order.id(), next);
repo.save(updated);
return updated;`,
			badBody: `return repo.noSuchMethod(id);`,
			breakSig: func(t *testing.T, dir string) {
				replaceInFile(t, filepath.Join(dir, "src/main/java/com/acme/order/Service.java"), "apply(String id, Event event)", "apply(String id, Event event, boolean force)")
			},
			available: jvmAvailable(false),
		},
		{
			name:    "kotlin",
			example: filepath.Join(root, "examples/kotlin/order"),
			adapter: func(dir string) adapter.Adapter {
				return &extadapter.Adapter{Language: "kotlin", Cmd: []string{filepath.Join(root, "adapters/jvm/kotlin-adapter.sh")}, Dir: dir}
			},
			symbol: "com.acme.order.Service#apply",
			goodBody: `val order = repo.get(id)
val next = TRANSITIONS[order.state]?.get(event)
    ?: throw InvalidTransitionException(order.state, event)
val updated = order.copy(state = next)
repo.save(updated)
return updated`,
			badBody: `return repo.noSuchMethod(id)`,
			breakSig: func(t *testing.T, dir string) {
				replaceInFile(t, filepath.Join(dir, "src/main/kotlin/com/acme/order/Service.kt"), "fun apply(id: String, event: Event): Order", "fun apply(id: String, event: Event, force: Boolean = false): Order")
			},
			available: jvmAvailable(true),
		},
	}
}

func jvmAvailable(kotlin bool) func() string {
	return func() string {
		java := "/opt/homebrew/opt/openjdk/bin/java"
		if h := os.Getenv("JAVA_HOME"); h != "" {
			java = filepath.Join(h, "bin/java")
		}
		if _, err := os.Stat(java); err != nil {
			return "JDK not installed"
		}
		if kotlin {
			if _, err := exec.LookPath("kotlinc"); err != nil && os.Getenv("KOTLIN_HOME") == "" {
				return "kotlinc not installed"
			}
		}
		return ""
	}
}

func pyrightPath(root string) string {
	if _, err := os.Stat(filepath.Join(root, "adapters/python/node_modules/.bin/pyright")); err != nil {
		if _, err := exec.LookPath("pyright"); err != nil {
			return "pyright not installed (adapters/python: npm install)"
		}
	}
	return ""
}

var limits = policy.Limits{SmallRetries: 2, LargeRetries: 1, ConfidenceThreshold: 0.8}

func setup(t *testing.T, lc langCase, mode string) (*runner.Runner, *fakeDecider, string) {
	if why := lc.available(); why != "" {
		t.Skip(why)
	}
	dir := copyDir(t, lc.example)
	fd := &fakeDecider{mode: mode}
	r := &runner.Runner{Dir: dir, A: lc.adapter(dir), D: fd, Limits: limits}
	if res, err := r.Lock(); err != nil {
		t.Fatalf("lock: %v\n%+v", err, res.Checks)
	}
	return r, fd, dir
}

func plan(t *testing.T, r *runner.Runner, lc langCase) {
	p := &manifest.Plan{Targets: []manifest.Target{{Symbol: lc.symbol, Reqs: []string{"REQ-001", "REQ-002", "REQ-003"}}}}
	res, err := r.Plan(context.Background(), "T1", policy.Small, p)
	if err != nil || res.Next != policy.Proceed {
		t.Fatalf("plan: next=%s err=%v %+v", res.Next, err, res)
	}
}

func failed(cs []adapter.Check) string {
	var out []string
	for _, c := range cs {
		if c.Status == adapter.CheckFail {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestAcceptance(t *testing.T) {
	ctx := context.Background()
	for _, lc := range cases(t) {
		lc := lc
		t.Run(lc.name, func(t *testing.T) {
			t.Run("AC2_bad_plan_rejected_without_decider", func(t *testing.T) {
				r, fd, _ := setup(t, lc, "pass")
				p := &manifest.Plan{Targets: []manifest.Target{{Symbol: lc.symbol + "Nope", Reqs: []string{"REQ-999"}}}}
				res, err := r.Plan(ctx, "T1", policy.Small, p)
				if err != nil {
					t.Fatal(err)
				}
				if fd.calls != 0 || res.Next != policy.RetrySmall {
					t.Fatalf("calls=%d next=%s", fd.calls, res.Next)
				}
			})

			t.Run("AC1_AC7_check_failure_skips_decider_then_escalates", func(t *testing.T) {
				r, fd, _ := setup(t, lc, "pass")
				plan(t, r, lc)
				before := fd.calls
				res, _ := r.Apply(ctx, "T1", policy.Small, lc.symbol, lc.badBody)
				if fd.calls != before || res.Next != policy.RetrySmall {
					t.Fatalf("attempt1 calls=%d next=%s failed=%s", fd.calls-before, res.Next, failed(res.Checks))
				}
				res, _ = r.Apply(ctx, "T1", policy.Small, lc.symbol, lc.badBody)
				if res.Next != policy.EscalateLarge {
					t.Fatalf("attempt2 next=%s", res.Next)
				}
				res, _ = r.Apply(ctx, "T1", policy.Large, lc.symbol, lc.badBody)
				if res.Next != policy.EscalateHuman {
					t.Fatalf("large attempt next=%s", res.Next)
				}
			})

			t.Run("AC3_AC9_good_body_done_and_questions", func(t *testing.T) {
				r, fd, _ := setup(t, lc, "pass")
				plan(t, r, lc)
				res, err := r.Apply(ctx, "T1", policy.Small, lc.symbol, lc.goodBody)
				if err != nil || res.Next != policy.Done {
					t.Fatalf("next=%s err=%v failed=%s checks=%+v", res.Next, err, failed(res.Checks), res.Checks)
				}
				var keys []string
				for k := range fd.lastQs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				if strings.Join(keys, ",") != "REQ-001,REQ-002,REQ-003,test_gaming" {
					t.Fatalf("done questions = %v", keys)
				}
				if c, _ := r.Check(); c.Next != "ok" {
					t.Fatalf("check after apply: %s", failed(c.Checks))
				}
			})

			t.Run("decider_fail_gives_feedback", func(t *testing.T) {
				r, _, _ := setup(t, lc, "fail_req002")
				plan(t, r, lc)
				res, _ := r.Apply(ctx, "T1", policy.Small, lc.symbol, lc.goodBody)
				if res.Next != policy.RetrySmall || len(res.Feedback) != 1 || !strings.HasPrefix(res.Feedback[0], "REQ-002") {
					t.Fatalf("next=%s feedback=%v", res.Next, res.Feedback)
				}
			})

			t.Run("AC6_low_confidence_escalates", func(t *testing.T) {
				r, _, _ := setup(t, lc, "pass")
				plan(t, r, lc)
				r.D.(*fakeDecider).mode = "low"
				res, _ := r.Apply(ctx, "T1", policy.Small, lc.symbol, lc.goodBody)
				if res.Next != policy.EscalateLarge {
					t.Fatalf("next=%s", res.Next)
				}
			})

			t.Run("unsure_escalates", func(t *testing.T) {
				r, _, _ := setup(t, lc, "pass")
				plan(t, r, lc)
				r.D.(*fakeDecider).mode = "unsure"
				res, _ := r.Apply(ctx, "T1", policy.Small, lc.symbol, lc.goodBody)
				if res.Next != policy.EscalateLarge {
					t.Fatalf("next=%s", res.Next)
				}
			})

			t.Run("AC5_decider_invalid_or_down_pauses", func(t *testing.T) {
				for _, mode := range []string{"invalid", "down"} {
					r, _, _ := setup(t, lc, "pass")
					plan(t, r, lc)
					r.D.(*fakeDecider).mode = mode
					res, _ := r.Apply(ctx, "T1", policy.Small, lc.symbol, lc.goodBody)
					if res.Next != policy.Paused || res.Outcome != nil {
						t.Fatalf("%s: next=%s", mode, res.Next)
					}
				}
			})

			t.Run("AC4_direct_signature_edit_caught", func(t *testing.T) {
				r, _, dir := setup(t, lc, "pass")
				lc.breakSig(t, dir)
				res, _ := r.Check()
				var boundary adapter.Check
				for _, c := range res.Checks {
					if c.Name == "boundary" {
						boundary = c
					}
				}
				if boundary.Status != adapter.CheckFail {
					t.Fatalf("boundary=%+v", boundary)
				}
			})
		})
	}
}

// AC-11: a weakly typed language without strict type checking cannot be locked
// (D-019).
func TestStrictTypingRequired(t *testing.T) {
	for _, lc := range cases(t) {
		lc := lc
		if lc.name != "python" && lc.name != "ts" {
			continue // statically typed: nothing to switch off
		}
		t.Run(lc.name, func(t *testing.T) {
			if why := lc.available(); why != "" {
				t.Skip(why)
			}
			dir := copyDir(t, lc.example)
			switch lc.name {
			case "python":
				replaceInFile(t, filepath.Join(dir, "pyrightconfig.json"), `"strict"`, `"basic"`)
			case "ts":
				replaceInFile(t, filepath.Join(dir, "tsconfig.json"), `"strict": true`, `"strict": false`)
			}
			r := &runner.Runner{Dir: dir, A: lc.adapter(dir), D: &fakeDecider{mode: "pass"}, Limits: limits}
			res, err := r.Lock()
			if err == nil || !strings.Contains(res.Error, "strict_typing") {
				t.Fatalf("lock should be refused: err=%v res=%+v", err, res.Error)
			}
		})
	}
}
