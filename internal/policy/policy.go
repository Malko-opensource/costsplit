// Package policy computes the recommended next action (D-011, D-012, D-015).
// The tool only recommends; the coding agent decides whether to follow.
package policy

type Next string

const (
	Proceed       Next = "proceed" // plan gate passed: go implement
	Done          Next = "done"    // done gate passed
	RetrySmall    Next = "retry_small"
	RetryLarge    Next = "retry_large"
	EscalateLarge Next = "escalate_large"
	EscalateHuman Next = "escalate_human"
	Paused        Next = "paused" // decider unavailable (D-012)
)

// Limits are deliberately required config: the spec leaves the values open
// (Q-007, Q-012).
type Limits struct {
	SmallRetries        int     `json:"small_retries"`
	LargeRetries        int     `json:"large_retries"`
	ConfidenceThreshold float64 `json:"confidence_threshold"`
}

type Model string

const (
	Small Model = "small"
	Large Model = "large"
)

// Signal summarizes one attempt.
type Signal string

const (
	SigPass       Signal = "pass"
	SigFail       Signal = "fail"   // mechanical check failed or decider said fail
	SigUnsure     Signal = "unsure" // decider said unsure, or pass below threshold
	SigDeciderErr Signal = "decider_error"
)

// Classify turns a decider pass into unsure when confidence is low (D-011).
func Classify(result string, minConfidence float64, l Limits) Signal {
	switch result {
	case "fail":
		return SigFail
	case "unsure":
		return SigUnsure
	}
	if minConfidence < l.ConfidenceThreshold {
		return SigUnsure
	}
	return SigPass
}

// Decide returns the next action. attempts counts this attempt (1 = first).
// planGate selects Proceed instead of Done on success.
func Decide(m Model, attempts int, s Signal, l Limits, planGate bool) Next {
	switch s {
	case SigDeciderErr:
		return Paused
	case SigPass:
		if planGate {
			return Proceed
		}
		return Done
	case SigUnsure:
		return escalate(m)
	}
	// SigFail
	if m == Small {
		if attempts >= l.SmallRetries {
			return EscalateLarge
		}
		return RetrySmall
	}
	if attempts >= l.LargeRetries {
		return EscalateHuman
	}
	return RetryLarge
}

func escalate(m Model) Next {
	if m == Small {
		return EscalateLarge
	}
	return EscalateHuman
}
