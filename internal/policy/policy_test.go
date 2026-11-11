package policy

import "testing"

func TestDecide(t *testing.T) {
	l := Limits{SmallRetries: 3, LargeRetries: 2, ConfidenceThreshold: 0.8}
	cases := []struct {
		m       Model
		attempt int
		s       Signal
		plan    bool
		want    Next
	}{
		{Small, 1, SigPass, false, Done},
		{Small, 1, SigPass, true, Proceed},
		{Small, 1, SigFail, false, RetrySmall},
		{Small, 3, SigFail, false, EscalateLarge},
		{Large, 1, SigFail, false, RetryLarge},
		{Large, 2, SigFail, false, EscalateHuman},
		{Small, 1, SigUnsure, false, EscalateLarge},
		{Large, 1, SigUnsure, false, EscalateHuman},
		{Small, 1, SigDeciderErr, false, Paused},
	}
	for _, c := range cases {
		if got := Decide(c.m, c.attempt, c.s, l, c.plan); got != c.want {
			t.Errorf("Decide(%s,%d,%s,plan=%v) = %s, want %s", c.m, c.attempt, c.s, c.plan, got, c.want)
		}
	}
}

func TestClassifyLowConfidence(t *testing.T) {
	l := Limits{ConfidenceThreshold: 0.8}
	if Classify("pass", 0.79, l) != SigUnsure || Classify("pass", 0.8, l) != SigPass || Classify("fail", 0.99, l) != SigFail {
		t.Fatal("classify")
	}
}
