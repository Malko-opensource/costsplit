package calibrate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Malko-opensource/costsplit/internal/bench"
)

// results builds n tasks per arm; pass[arm] lists the task numbers that pass.
func results(n int, cost map[string]float64, fail map[string][]int) []bench.Result {
	var rs []bench.Result
	for arm, c := range cost {
		bad := map[int]bool{}
		for _, i := range fail[arm] {
			bad[i] = true
		}
		for i := 0; i < n; i++ {
			r := bench.Result{Exercise: fmt.Sprint("t", i), Arm: arm, Pass: !bad[i], Outcome: "pass", Info: map[string]any{}}
			r.Total.CostUSD = c
			rs = append(rs, r)
		}
	}
	return rs
}

func TestAnalyzePicksCheapestWithinTolerance(t *testing.T) {
	cost := map[string]float64{"agent-full": 0.10, "agent-low": 0.07, "agent-haiku": 0.01}
	// low loses 2 of 40 (the tolerance), haiku loses 10.
	rep := Analyze(results(40, cost, map[string][]int{"agent-low": {0, 1}, "agent-haiku": {0, 1, 2, 3, 4, 5, 6, 7, 8, 9}}), "agent-full", -1)
	if rep.Tolerance != 2 || rep.Pick != "agent-low" || rep.Tasks != 40 {
		t.Fatalf("%+v", rep)
	}
	// One more loss puts low outside the tolerance: the baseline stays.
	rep = Analyze(results(40, cost, map[string][]int{"agent-low": {0, 1, 2}, "agent-haiku": {0, 1, 2, 3}}), "agent-full", -1)
	if rep.Pick != "agent-full" {
		t.Fatalf("pick %s", rep.Pick)
	}
	if md := rep.Markdown("o/r"); !strings.Contains(md, "agent-full") || !strings.Contains(md, "허용 범위 밖") {
		t.Fatal(md)
	}
}

func TestAnalyzeSkipsTasksWithErrorsOrMissingArms(t *testing.T) {
	rs := results(10, map[string]float64{"agent-full": 0.1, "agent-low": 0.05}, nil)
	for i := range rs {
		if rs[i].Arm == "agent-low" && rs[i].Exercise == "t3" {
			rs[i].Outcome, rs[i].Pass = "error", false
		}
	}
	rep := Analyze(rs, "agent-full", -1)
	if rep.Tasks != 9 || rep.Errors != 1 || rep.Pick != "agent-low" {
		t.Fatalf("%+v", rep)
	}
}

func TestIsTest(t *testing.T) {
	for p, want := range map[string]bool{"tests/a.py": true, "src/foo_test.go": true, "pkg/test_x.py": true, "web/a.spec.ts": true,
		"src/latest.c": false, "plugins/contest/main.cpp": false, "libobs/obs.c": false} {
		if IsTest(p) != want {
			t.Errorf("%s: got %v", p, !want)
		}
	}
}
