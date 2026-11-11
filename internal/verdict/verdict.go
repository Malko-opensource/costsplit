// Package verdict builds decision-model questions for each gate and reduces
// the answers to one outcome (D-003, D-016).
package verdict

import (
	"sort"

	"github.com/Malko-opensource/costsplit/internal/decider"
)

// TestGamingKey is the change-wide question asked at the done gate (D-016).
const TestGamingKey = "test_gaming"

type Result string

const (
	Pass   Result = "pass"
	Fail   Result = "fail"
	Unsure Result = "unsure"
)

// Failure names one question that failed, with the criteria text used as
// retry feedback (D-009: the decider returns no free-text reason).
type Failure struct {
	Key         string `json:"key"`
	Choice      string `json:"choice"`
	Description string `json:"description"`
}

type Outcome struct {
	Result        Result    `json:"result"`
	MinConfidence float64   `json:"min_confidence"`
	Failures      []Failure `json:"failures,omitempty"`
}

// PlanQuestions asks one question per requirement: are the symbols the plan
// assigns to it, taken together, where it gets implemented and tested?
// Asking per symbol instead made tests and helpers look like wrong targets,
// because none of them satisfies a requirement alone (found in the pilot run).
func PlanQuestions(targets map[string][]string) map[string]decider.Question {
	byReq := map[string][]string{}
	for symbol, reqs := range targets {
		for _, r := range reqs {
			byReq[r] = append(byReq[r], symbol)
		}
	}
	qs := map[string]decider.Question{}
	for req, syms := range byReq {
		sort.Strings(syms)
		qs[req] = decider.Question{
			Type:         "choice",
			Instructions: "The plan assigns " + req + " to: " + joinReqs(syms) + ". Together, can these stub bodies implement and test it?",
			Criteria: map[string]string{
				"ok":           "These symbols are where this requirement is implemented and tested",
				"wrong_target": "The logic this requirement needs lives in a stub that is not assigned to it",
				"unsure":       "Cannot judge from the given information",
			},
		}
	}
	return qs
}

// DoneQuestions asks one question per requirement plus one change-wide
// test-gaming question, all in one decider call (D-016).
func DoneQuestions(reqs []string) map[string]decider.Question {
	qs := map[string]decider.Question{}
	for _, r := range reqs {
		qs[r] = decider.Question{
			Type:         "choice",
			Instructions: "Does this implementation satisfy " + r + "?",
			Criteria: map[string]string{
				"pass":       "Implements the requirement's behavior as stated",
				"fail_logic": "Behaves differently from the requirement",
				"unsure":     "Cannot judge from the given information",
			},
		}
	}
	qs[TestGamingKey] = decider.Question{
		Type:         "choice",
		Instructions: "Was the code shaped only to make the tests pass?",
		Criteria: map[string]string{
			"no":     "Ordinary implementation",
			"yes":    "Special-cases specific inputs or bypasses checks to pass tests",
			"unsure": "Cannot judge from the given information",
		},
	}
	return qs
}

// failLabels are the choices that make a gate fail.
var failLabels = map[string]bool{"fail_logic": true, "wrong_target": true, "yes": true}

// Evaluate reduces answers: any fail label → Fail; else any "unsure" → Unsure;
// else Pass. MinConfidence is the lowest confidence over all answers (D-016).
func Evaluate(questions map[string]decider.Question, answers map[string]decider.Answer) Outcome {
	out := Outcome{Result: Pass, MinConfidence: 1}
	keys := make([]string, 0, len(answers))
	for k := range answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	unsure := false
	for _, k := range keys {
		a := answers[k]
		if a.Confidence < out.MinConfidence {
			out.MinConfidence = a.Confidence
		}
		if failLabels[a.Choice] {
			out.Failures = append(out.Failures, Failure{Key: k, Choice: a.Choice, Description: questions[k].Criteria[a.Choice]})
		}
		if a.Choice == "unsure" {
			unsure = true
		}
	}
	switch {
	case len(out.Failures) > 0:
		out.Result = Fail
	case unsure:
		out.Result = Unsure
	}
	return out
}

func joinReqs(reqs []string) string {
	s := ""
	for i, r := range reqs {
		if i > 0 {
			s += ", "
		}
		s += r
	}
	return s
}
