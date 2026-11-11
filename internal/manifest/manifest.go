// Package manifest loads the agent-written JSON files (manifest.json,
// questions.json) and the plan, and cross-checks them against the code.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
)

type Requirement struct {
	ID           string   `json:"id"`
	Text         string   `json:"text"`
	Tests        []string `json:"tests"`
	Coverage     string   `json:"coverage"`
	WhyUncovered string   `json:"why_uncovered,omitempty"`
}

type Manifest struct {
	Schema       string        `json:"schema"`
	Requirements []Requirement `json:"requirements"`
}

type Question struct {
	ID         string   `json:"id"`
	Question   string   `json:"question"`
	Blocking   bool     `json:"blocking"`
	Affects    []string `json:"affects"`
	Assumption *string  `json:"assumption"`
}

type Questions struct {
	Schema    string     `json:"schema"`
	Questions []Question `json:"questions"`
}

type Target struct {
	Symbol string   `json:"symbol"`
	Reqs   []string `json:"reqs"`
}

type Plan struct {
	Targets []Target `json:"targets"`
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func LoadManifest(path string) (*Manifest, error) {
	var m Manifest
	if err := readJSON(path, &m); err != nil {
		return nil, err
	}
	return &m, m.Validate()
}

func LoadQuestions(path string) (*Questions, error) {
	var q Questions
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &Questions{Schema: "graybox.questions/v1"}, nil
	}
	if err := readJSON(path, &q); err != nil {
		return nil, err
	}
	return &q, q.Validate()
}

func LoadPlan(path string) (*Plan, error) {
	var p Plan
	if err := readJSON(path, &p); err != nil {
		return nil, err
	}
	if len(p.Targets) == 0 {
		return nil, fmt.Errorf("plan has no targets")
	}
	return &p, nil
}

func (m *Manifest) Validate() error {
	if m.Schema != "graybox.manifest/v1" {
		return fmt.Errorf("manifest schema must be graybox.manifest/v1, got %q", m.Schema)
	}
	seen := map[string]bool{}
	for _, r := range m.Requirements {
		if r.ID == "" || seen[r.ID] {
			return fmt.Errorf("requirement id empty or duplicated: %q", r.ID)
		}
		seen[r.ID] = true
		switch r.Coverage {
		case "full", "partial":
		case "none":
			if r.WhyUncovered == "" {
				return fmt.Errorf("%s: coverage none requires why_uncovered", r.ID)
			}
		default:
			return fmt.Errorf("%s: coverage must be full|partial|none", r.ID)
		}
	}
	return nil
}

func (q *Questions) Validate() error {
	if q.Schema != "graybox.questions/v1" {
		return fmt.Errorf("questions schema must be graybox.questions/v1, got %q", q.Schema)
	}
	for _, x := range q.Questions {
		if !x.Blocking && (x.Assumption == nil || *x.Assumption == "") {
			return fmt.Errorf("%s: non-blocking question requires an assumption", x.ID)
		}
	}
	return nil
}

func (q *Questions) Blocking() []string {
	var ids []string
	for _, x := range q.Questions {
		if x.Blocking {
			ids = append(ids, x.ID)
		}
	}
	return ids
}

func (m *Manifest) Req(id string) (Requirement, bool) {
	for _, r := range m.Requirements {
		if r.ID == id {
			return r, true
		}
	}
	return Requirement{}, false
}

var reqInName = regexp.MustCompile(`REQ-?(\d+)`)

// SyncProblems compares manifest test names with the tests the adapter lists:
// every manifest test must exist, and every test naming a REQ must be in the
// manifest.
func (m *Manifest) SyncProblems(tests []string) []string {
	have := map[string]bool{}
	for _, t := range tests {
		have[t] = true
	}
	listed := map[string]bool{}
	var probs []string
	for _, r := range m.Requirements {
		for _, t := range r.Tests {
			listed[t] = true
			if !have[t] {
				probs = append(probs, fmt.Sprintf("%s: test %q not found in code", r.ID, t))
			}
		}
	}
	for _, t := range tests {
		if reqInName.MatchString(t) && !listed[t] {
			probs = append(probs, fmt.Sprintf("test %q names a requirement but is not in manifest", t))
		}
	}
	sort.Strings(probs)
	return probs
}
