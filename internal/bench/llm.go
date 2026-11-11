// Package bench runs the Aider Polyglot benchmark against three arms:
// small model alone, large model alone, and small model inside the graybox
// pipeline (large model writes the graybox, decider judges, tool recommends
// escalation). It measures pass rate and cost (spec AC-8, D-006).
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Usage is what one or more model calls cost.
type Usage struct {
	Calls        int     `json:"calls"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	// ThinkingTokens is the part of OutputTokens spent on reasoning.
	ThinkingTokens int   `json:"thinking_tokens,omitempty"`
	DurationMS     int64 `json:"duration_ms"`
}

func (u *Usage) Add(o Usage) {
	u.Calls += o.Calls
	u.CostUSD += o.CostUSD
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.ThinkingTokens += o.ThinkingTokens
	u.DurationMS += o.DurationMS
}

// LLM returns a JSON value matching schema.
type LLM interface {
	Complete(ctx context.Context, model, system, prompt string, schema map[string]any) (json.RawMessage, Usage, error)
}

// Meter wraps an LLM and attributes usage to roles (small, large, decider).
type Meter struct {
	LLM LLM
	mu  sync.Mutex
	By  map[string]*Usage
}

func NewMeter(l LLM) *Meter { return &Meter{LLM: l, By: map[string]*Usage{}} }

func (m *Meter) Complete(ctx context.Context, role, model, system, prompt string, schema map[string]any) (json.RawMessage, error) {
	out, u, err := m.LLM.Complete(ctx, model, system, prompt, schema)
	m.mu.Lock()
	if m.By[role] == nil {
		m.By[role] = &Usage{}
	}
	m.By[role].Add(u)
	m.mu.Unlock()
	return out, err
}

func (m *Meter) Total() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t Usage
	for _, u := range m.By {
		t.Add(*u)
	}
	return t
}

func (m *Meter) Snapshot() map[string]Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Usage{}
	for k, v := range m.By {
		out[k] = *v
	}
	return out
}

// ClaudeCLI calls `claude -p` headless with no tools, no user settings and
// no MCP servers, from an empty directory, so only the prompt reaches the
// model. Cost is the CLI's reported total_cost_usd.
type ClaudeCLI struct {
	Bin     string
	Timeout time.Duration
}

type cliResult struct {
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	DurationMS       int64           `json:"duration_ms"`
	Usage            struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		Details                  struct {
			ThinkingTokens int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

func (c *ClaudeCLI) Complete(ctx context.Context, model, system, prompt string, schema map[string]any) (json.RawMessage, Usage, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sch, err := json.Marshal(schema)
	if err != nil {
		return nil, Usage{}, err
	}
	dir, err := os.MkdirTemp("", "polybench-call-")
	if err != nil {
		return nil, Usage{}, err
	}
	defer os.RemoveAll(dir)
	args := []string{"-p", "--model", model, "--output-format", "json", "--json-schema", string(sch),
		"--tools", "", "--setting-sources", "", "--strict-mcp-config", "--no-session-persistence"}
	if system != "" {
		args = append(args, "--system-prompt", system)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewBufferString(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var r cliResult
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return nil, Usage{Calls: 1}, fmt.Errorf("claude cli: %v: %s %s", runErr, tail(stdout.String(), 300), tail(stderr.String(), 300))
	}
	u := Usage{Calls: 1, CostUSD: r.TotalCostUSD, DurationMS: r.DurationMS,
		InputTokens:  r.Usage.InputTokens + r.Usage.CacheCreationInputTokens + r.Usage.CacheReadInputTokens,
		OutputTokens: r.Usage.OutputTokens, ThinkingTokens: r.Usage.Details.ThinkingTokens}
	if r.IsError || len(r.StructuredOutput) == 0 || string(r.StructuredOutput) == "null" {
		return nil, u, fmt.Errorf("claude cli error: %s", tail(r.Result, 300))
	}
	return r.StructuredOutput, u, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
