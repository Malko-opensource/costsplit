// Package decider calls the lightweight decision model (D-001, D-009).
//
// Request:  {model, state, questions:{key:{type:"choice", instructions, criteria:{label:desc}}}}
// Response: {answers:{key:{type, choice, confidence, probabilities}}}
//
// Any transport error, non-2xx status, timeout, or malformed answer is reported
// as ErrUnavailable so the caller can pause the task (D-012).
package decider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnavailable means the decision model could not give a usable answer.
var ErrUnavailable = errors.New("decider unavailable")

type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type Request struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
}

type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type Response struct {
	Answers map[string]Answer `json:"answers"`
}

// Decider is the interface the runner depends on; tests use a fake.
type Decider interface {
	Decide(ctx context.Context, state string, questions map[string]Question) (map[string]Answer, error)
}

// Client is the HTTP implementation.
type Client struct {
	URL     string
	Model   string
	APIKey  string
	Timeout time.Duration
	HTTP    *http.Client
}

func (c *Client) Decide(ctx context.Context, state string, questions map[string]Question) (map[string]Answer, error) {
	body, err := json.Marshal(Request{Model: c.Model, State: state, Questions: questions})
	if err != nil {
		return nil, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%w: status %d: %s", ErrUnavailable, resp.StatusCode, truncate(string(raw), 200))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: bad json: %v", ErrUnavailable, err)
	}
	if err := Validate(questions, out.Answers); err != nil {
		return nil, err
	}
	return out.Answers, nil
}

// Validate checks that every question has an answer whose choice is one of its
// criteria labels. A violation is treated as the decider being unavailable.
func Validate(questions map[string]Question, answers map[string]Answer) error {
	for key, q := range questions {
		a, ok := answers[key]
		if !ok {
			return fmt.Errorf("%w: no answer for %q", ErrUnavailable, key)
		}
		if _, ok := q.Criteria[a.Choice]; !ok {
			return fmt.Errorf("%w: answer %q for %q is not a criteria label", ErrUnavailable, a.Choice, key)
		}
		if a.Confidence < 0 || a.Confidence > 1 {
			return fmt.Errorf("%w: confidence %v for %q out of range", ErrUnavailable, a.Confidence, key)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
