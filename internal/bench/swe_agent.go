package bench

// The agent arms: the large model runs as a Claude Code agent over the
// repository, many turns, prompt caching on, and must find the files itself
// (no file list). Its only file tools come from cmd/colamcp; the two arms
// differ only in what read_file returns.
//
//	agent-full  read_file returns the whole file
//	agent-cola  read_file returns a diet-cola view; show and the quote gate
//
// Grep and Glob are the built-in search tools in both arms. The agent cannot
// run code.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Malko-opensource/costsplit/internal/cola"
)

// Exported for cmd/colamcp.
func ColaDoc(env *SWEEnv, work, path string) (*cola.Doc, error) { return colaDoc(env, work, path) }
func ApplyEdits(env *SWEEnv, work string, raw json.RawMessage) error {
	return applyEdits(env, work, raw)
}
func StripLineNumbers(s string) string { return stripLineNumbers(s) }

const agentSystem = `You fix issues in the Python repository in the current directory.
Find the code with Grep and Glob, read files with read_file, and change them with edit. You cannot run code or tests.
Do not add or change tests. Make the smallest change that fixes the issue.
When the fix is in place, stop and reply with one line saying what you changed.`

const agentBudgetUSD = "3"

func agentBin(env *SWEEnv) string {
	if b := os.Getenv("COLAMCP_BIN"); b != "" {
		return b
	}
	return filepath.Join(env.RepoRoot, "bin", "colamcp")
}

// streamEvent is the part of `claude -p --output-format stream-json` we read.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content []struct {
			Type    string          `json:"type"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		CostUSD                  float64 `json:"costUSD"`
		InputTokens              int     `json:"inputTokens"`
		OutputTokens             int     `json:"outputTokens"`
		CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
		CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	} `json:"modelUsage"`
}

func RunSWEAgent(ctx context.Context, env *SWEEnv, cfg Config, s *SWE, arm string) (res Result) {
	start := time.Now()
	res = Result{Lang: "python", Exercise: s.ID, Arm: arm, Info: map[string]any{}, Usage: map[string]Usage{}}
	defer func() {
		res.Seconds = time.Since(start).Seconds()
		for _, u := range res.Usage {
			res.Total.Add(u)
		}
	}()
	step := func(f string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(f, a...)) }
	mode := strings.TrimPrefix(arm, "agent-")
	work, cleanup, err := s.prepare(env, arm)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer cleanup()
	side := filepath.Dir(work) // holds the task, ledger and MCP config, outside the repository
	task, ledger, mcp := filepath.Join(side, "task.md"), filepath.Join(side, "ledger.jsonl"), filepath.Join(side, "mcp.json")
	if err := os.WriteFile(task, []byte(s.Problem), 0o644); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	conf, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"cola": map[string]any{
		"command": agentBin(env),
		"args": []string{"-mode", mode, "-work", work, "-python", env.Python, "-root", env.RepoRoot,
			"-task", task, "-ledger", ledger, "-small", cfg.Small},
	}}})
	if err := os.WriteFile(mcp, conf, 0o644); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	tools := "Grep,Glob,mcp__cola__read_file,mcp__cola__edit"
	if mode == "cola" {
		tools += ",mcp__cola__show"
	}
	actx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(actx, "claude", "-p", "--model", cfg.Large, "--output-format", "stream-json", "--verbose",
		"--system-prompt", agentSystem, "--tools", "Grep,Glob", "--allowedTools", tools,
		"--mcp-config", mcp, "--strict-mcp-config", "--setting-sources", "", "--no-session-persistence",
		"--max-budget-usd", agentBudgetUSD)
	cmd.Dir = work
	cmd.Stdin = strings.NewReader("Fix this issue.\n\n# Issue\n" + s.Problem)
	// Caching on: a real agent loop re-sends its history every turn.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DISABLE_PROMPT_CACHING=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	calls := map[string]int{}
	grepChars := 0
	var final *streamEvent
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		var ev streamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_use" {
					calls[strings.TrimPrefix(c.Name, "mcp__cola__")]++
				}
			}
		case "user":
			for _, c := range ev.Message.Content {
				if c.Type == "tool_result" {
					grepChars += len(c.Content) // all tool results; MCP ones are also in the ledger
				}
			}
		case "result":
			e := ev
			final = &e
		}
	}
	if final == nil {
		res.Outcome, res.Error = "error", fmt.Sprintf("agent: %v: %s", runErr, tail(stderr.String(), 400))
		return res
	}
	for model, u := range final.ModelUsage {
		role := "agent:" + model
		if model == cfg.Large || strings.Contains(model, "opus") {
			role = "large"
		}
		cur := res.Usage[role]
		cur.Add(Usage{CostUSD: u.CostUSD, InputTokens: u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens, OutputTokens: u.OutputTokens})
		res.Usage[role] = cur
	}
	if len(final.ModelUsage) == 0 {
		res.Usage["large"] = Usage{CostUSD: final.TotalCostUSD}
	}
	if u, ok := res.Usage["large"]; ok {
		u.Calls, u.DurationMS = final.NumTurns, final.DurationMS
		res.Usage["large"] = u
	}
	res.Info["turns"] = final.NumTurns
	res.Info["subtype"] = final.Subtype
	res.Info["tool_calls"] = calls
	res.Info["tool_result_chars"] = grepChars
	res.Info["usage"] = final.Usage
	res.Hint = tail(final.Result, 400)
	if final.IsError {
		step("agent ended with %s: %s", final.Subtype, tail(final.Result, 200))
	}

	// The ledger: the small model's ranking calls, and what the agent read.
	var loc Usage
	read, viewTok, fileTok, gates, shows, edits, editsOK := map[string]bool{}, 0, 0, 0, 0, 0, 0
	if b, err := os.ReadFile(ledger); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var e struct {
				Event        string  `json:"event"`
				Path         string  `json:"path"`
				Cost         float64 `json:"cost_usd"`
				In           int     `json:"input_tokens"`
				Out          int     `json:"output_tokens"`
				FileTokens   int     `json:"file_tokens"`
				ViewTokens   int     `json:"view_tokens"`
				OK           bool    `json:"ok"`
				ResultTokens int     `json:"result_tokens"`
			}
			if json.Unmarshal([]byte(l), &e) != nil {
				continue
			}
			switch e.Event {
			case "rank":
				loc.Add(Usage{Calls: 1, CostUSD: e.Cost, InputTokens: e.In, OutputTokens: e.Out})
			case "read":
				read[e.Path] = true
				viewTok += e.ViewTokens
				fileTok += e.FileTokens
			case "gate":
				gates++
			case "show":
				shows++
				read[e.Path] = true
			case "edit":
				edits++
				if e.OK {
					editsOK++
				}
			}
		}
	}
	if loc.Calls > 0 {
		res.Usage["locate"] = loc
	}
	var files []string
	for p := range read {
		files = append(files, p)
	}
	res.Info["files_read"] = files
	res.Info["view_tokens"], res.Info["file_tokens"] = viewTok, fileTok
	res.Info["gated"], res.Info["shows"], res.Info["edits"], res.Info["edits_ok"] = gates, shows, edits, editsOK
	gold := 0
	for _, f := range s.Files {
		if read[f] {
			gold++
		}
	}
	res.Info["read_gold_files"] = gold
	res.Info["gold_files"] = len(s.Files)
	step("agent: %d turns, tools %v, %d edits applied", final.NumTurns, calls, editsOK)
	res.Pass, res.ScoreOutput = s.Score(ctx, env, work)
	res.Outcome = map[bool]string{true: "pass", false: "fail"}[res.Pass]
	return res
}
