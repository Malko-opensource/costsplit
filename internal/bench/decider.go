package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/local/graybox/internal/decider"
)

// LLMDecider stands in for the lightweight decision model: a small model
// forced to answer only with one criteria label and a confidence per
// question. It cannot return real token probabilities; confidence is
// self-reported. Any call failure surfaces as decider.ErrUnavailable so the
// runner pauses (D-012).
type LLMDecider struct {
	Meter *Meter
	Model string
}

const deciderSystem = "You are a strict classifier. For every question, pick exactly one label from its criteria and give your confidence from 0 to 1. Judge only from the state you are given. Do not explain."

func (d *LLMDecider) Decide(ctx context.Context, state string, qs map[string]decider.Question) (map[string]decider.Answer, error) {
	keys := make([]string, 0, len(qs))
	for k := range qs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Model APIs only accept [a-zA-Z0-9_.-] in schema property names, while
	// question keys are symbols ("x.js#test:REQ-001 ..."). Use q1..qN on the
	// wire and map back.
	alias := map[string]string{}
	wire := make([]string, len(keys))
	for i, k := range keys {
		wire[i] = fmt.Sprintf("q%d", i+1)
		alias[wire[i]] = k
	}
	props := map[string]any{}
	var b strings.Builder
	b.WriteString("STATE:\n")
	b.WriteString(state)
	b.WriteString("\nQUESTIONS:\n")
	for i, k := range keys {
		q := qs[k]
		labels := make([]string, 0, len(q.Criteria))
		for l := range q.Criteria {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		fmt.Fprintf(&b, "- %s (%s): %s\n", wire[i], k, q.Instructions)
		for _, l := range labels {
			fmt.Fprintf(&b, "    %s: %s\n", l, q.Criteria[l])
		}
		props[wire[i]] = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"choice":     map[string]any{"type": "string", "enum": labels},
				"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			},
			"required":             []string{"choice", "confidence"},
			"additionalProperties": false,
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "required": wire, "additionalProperties": false}

	raw, err := d.Meter.Complete(ctx, "decider", d.Model, deciderSystem, b.String(), schema)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", decider.ErrUnavailable, err)
	}
	var got map[string]struct {
		Choice     string  `json:"choice"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("%w: %v", decider.ErrUnavailable, err)
	}
	out := map[string]decider.Answer{}
	for w, a := range got {
		k, ok := alias[w]
		if !ok {
			continue
		}
		out[k] = decider.Answer{Type: "choice", Choice: a.Choice, Confidence: a.Confidence}
	}
	return out, nil
}
