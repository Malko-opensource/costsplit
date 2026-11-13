package bench

// The agent arms: the large model runs as a Claude Code agent over the
// repository, many turns, prompt caching on, and must find the files itself
// (no file list). Its only file tools come from cmd/colamcp; the two arms
// differ only in what read_file returns.
//
//	agent-full  read_file returns the whole file
//	agent-cola  read_file returns a diet-cola view; show and the quote gate
//	agent-haiku the small model alone; agent-scout the small model explores
//	            read-only and briefs the large model
//	agent-low, agent-medium  the large model at that effort; agent-sonnet
//	            the middle model
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

const scoutSystem = `You explore the Python repository in the current directory for a colleague who will fix an issue. You cannot change files or run code.
Find the code involved with Grep, Glob and read_file. Then reply with a brief for the colleague:
- the files and functions involved, with line numbers;
- the relevant code, quoted verbatim with line numbers (only what matters: at most about 150 lines in all);
- what causes the issue, if you found it, and where the fix probably belongs.
Do not write the fix itself.`

const agentBudgetUSD = "3"

// agentMid is the middle model for agent-sonnet.
const agentMid = "claude-sonnet-5-5"

// runClaudeAgent runs one `claude -p` agent in work and returns its result
// event and tool-call counts. Caching stays on: a real agent loop re-sends
// its history every turn.
func runClaudeAgent(ctx context.Context, model, effort, system, prompt, tools, mcp, work string) (*streamEvent, map[string]int, error) {
	actx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()
	// Built-in tools are on only when the caller lists them: a no-Grep arm must not have them.
	builtin := ""
	if strings.Contains(tools, "Grep") {
		builtin = "Grep,Glob"
	}
	cmd := exec.CommandContext(actx, "claude", "-p", "--model", model, "--output-format", "stream-json", "--verbose",
		"--system-prompt", system, "--tools", builtin, "--allowedTools", tools,
		"--mcp-config", mcp, "--strict-mcp-config", "--setting-sources", "", "--no-session-persistence",
		"--max-budget-usd", agentBudgetUSD)
	if effort != "" {
		cmd.Args = append(cmd.Args, "--effort", effort)
	}
	cmd.Dir = work
	cmd.Stdin = strings.NewReader(prompt)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DISABLE_PROMPT_CACHING=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	calls := map[string]int{}
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
		case "result":
			e := ev
			final = &e
		}
	}
	if final == nil {
		return nil, calls, fmt.Errorf("agent: %v: %s", runErr, tail(stderr.String(), 400))
	}
	return final, calls, nil
}

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
	// Arm names are agent-<mode>[-nogrep][-vague]. nogrep takes Grep and Glob
	// away, so the index tools are the only way to find code. vague swaps the
	// issue for a rewrite without identifiers (scripts/bench/vague_issues.py).
	mode := strings.TrimPrefix(arm, "agent-")
	vague := strings.HasSuffix(mode, "-vague")
	mode = strings.TrimSuffix(mode, "-vague")
	noGrep := strings.HasSuffix(mode, "-nogrep")
	mode = strings.TrimSuffix(mode, "-nogrep")
	problem := s.Problem
	if vague {
		dir := os.Getenv("COSTSPLIT_VAGUE")
		if dir == "" {
			dir = filepath.Join(os.Getenv("HOME"), "bench-data", "swe", "vague")
		}
		b, err := os.ReadFile(filepath.Join(dir, s.ID+".md"))
		if err != nil {
			res.Outcome, res.Error = "error", err.Error()
			return res
		}
		problem = string(b)
	}
	work, cleanup, err := s.prepare(env, arm)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	defer cleanup()
	serverMode := mode
	if mode != "cola" {
		serverMode = "full"
	}
	side := filepath.Dir(work) // holds the task, ledger and MCP config, outside the repository
	task, ledger, mcp := filepath.Join(side, "task.md"), filepath.Join(side, "ledger.jsonl"), filepath.Join(side, "mcp.json")
	if err := os.WriteFile(task, []byte(problem), 0o644); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	serverArgs := []string{"-mode", serverMode, "-work", work, "-python", env.Python, "-root", env.RepoRoot,
		"-task", task, "-ledger", ledger, "-small", cfg.Small}
	toolsDir := os.Getenv("COSTSPLIT_TOOLS")
	if toolsDir == "" {
		toolsDir = filepath.Join(os.Getenv("HOME"), "bench-data", "tools")
	}
	cache := filepath.Join(toolsDir, "cache", s.ID)
	servers := map[string]any{}
	extraTools, extraSystem := "", ""
	prep := time.Now()
	buildIndex := func() (string, error) {
		db := filepath.Join(side, "index.db")
		out, err := exec.Command(env.Python, filepath.Join(env.RepoRoot, "adapters/python/index.py"), "build", work, db).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("index: %v: %s", err, tail(string(out), 300))
		}
		return db, nil
	}
	var prepErr error
	switch mode {
	case "db":
		// Code in a database: a SQLite index of symbols and call edges,
		// built before the agent starts, queried through extra tools.
		db, err := buildIndex()
		prepErr = err
		serverArgs = append(serverArgs, "-index", db)
		extraTools = ",mcp__cola__search_code,mcp__cola__symbol,mcp__cola__callers,mcp__cola__callees"
		extraSystem = "\nThe repository is indexed: search_code finds functions and classes, symbol shows one definition, callers and callees follow calls."
	case "embed":
		// Embeddings: every function and class embedded with a local model.
		npz := filepath.Join(cache, "embed.npz")
		py := filepath.Join(toolsDir, "embed", "bin", "python")
		if _, err := os.Stat(npz); err != nil {
			db, err := buildIndex()
			if err == nil {
				os.MkdirAll(cache, 0o755)
				var out []byte
				if out, err = exec.Command(py, filepath.Join(env.RepoRoot, "adapters/python/embed.py"), "build", work, db, npz, filepath.Join(toolsDir, "cache", "vec.db")).CombinedOutput(); err != nil {
					err = fmt.Errorf("embed: %v: %s", err, tail(string(out), 300))
				}
			}
			prepErr = err
		}
		serverArgs = append(serverArgs, "-embed", npz, "-embed-python", py)
		extraTools = ",mcp__cola__semantic_search"
		extraSystem = "\nThe repository's functions and classes are embedded: semantic_search finds code by meaning."
	case "graphify":
		// graphify's code graph (AST extraction, no LLM), served by its own MCP server.
		graph := filepath.Join(cache, "graphify", "graph.json")
		if _, err := os.Stat(graph); err != nil {
			cmd := exec.Command(filepath.Join(toolsDir, "graphify", "bin", "graphify"), "update", ".", "--no-cluster")
			cmd.Dir = work
			out, err := cmd.CombinedOutput()
			if err != nil {
				prepErr = fmt.Errorf("graphify: %v: %s", err, tail(string(out), 300))
			} else {
				os.MkdirAll(cache, 0o755)
				os.RemoveAll(filepath.Join(cache, "graphify"))
				prepErr = os.Rename(filepath.Join(work, "graphify-out"), filepath.Join(cache, "graphify"))
			}
		}
		os.RemoveAll(filepath.Join(work, "graphify-out"))
		servers["graphify"] = map[string]any{"command": filepath.Join(toolsDir, "graphify", "bin", "graphify-mcp"), "args": []string{graph}}
		extraTools = ",mcp__graphify__query_graph,mcp__graphify__get_node,mcp__graphify__get_neighbors,mcp__graphify__shortest_path,mcp__graphify__god_nodes"
		extraSystem = "\nThe repository has a knowledge graph of its code: the graphify tools query it."
	case "graft":
		// graft's wiring graph (no LLM), kept outside the repository, served by its own MCP server.
		bin, dir := filepath.Join(toolsDir, "graft", "node_modules", ".bin", "graft"), filepath.Join(side, "graft")
		cmd := exec.Command(bin, "--dir", dir, "build", work)
		cmd.Env = append(os.Environ(), "DO_NOT_TRACK=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			prepErr = fmt.Errorf("graft: %v: %s", err, tail(string(out), 300))
		}
		os.RemoveAll(filepath.Join(work, "graft"))
		servers["graft"] = map[string]any{"command": bin, "args": []string{"--dir", dir, "mcp", work}, "env": map[string]string{"DO_NOT_TRACK": "1"}}
		extraTools = ",mcp__graft__graft_find_code,mcp__graft__graft_file_api,mcp__graft__graft_trace_calls,mcp__graft__graft_find_all,mcp__graft__graft_repo_map"
		extraSystem = "\nThe repository has a context graph of its code: the graft tools query it."
	}
	if prepErr != nil {
		res.Outcome, res.Error = "error", prepErr.Error()
		return res
	}
	res.Info["prep_seconds"] = time.Since(prep).Seconds()
	servers["cola"] = map[string]any{"command": agentBin(env), "args": serverArgs}
	conf, _ := json.Marshal(map[string]any{"mcpServers": servers})
	if err := os.WriteFile(mcp, conf, 0o644); err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	tools := "Grep,Glob,mcp__cola__read_file,mcp__cola__edit"
	if mode == "cola" {
		tools += ",mcp__cola__show"
	}
	system := agentSystem
	if noGrep {
		tools = strings.TrimPrefix(tools, "Grep,Glob,")
		system = strings.Replace(system, "Find the code with Grep and Glob", "Find the code with the search tools described below", 1)
	}
	tools += extraTools
	system += extraSystem
	model, role := cfg.Large, "large"
	effort := ""
	switch mode {
	case "haiku":
		model, role = cfg.Small, "small"
	case "low", "medium":
		effort = mode
	case "sonnet":
		model, role = agentMid, "mid"
	}
	prompt := "Fix this issue.\n\n# Issue\n" + problem
	calls := map[string]int{}
	addUsage := func(ev *streamEvent, role string) {
		for m, u := range ev.ModelUsage {
			r := role
			if !strings.Contains(m, "opus") && !strings.Contains(m, "haiku") && !strings.Contains(m, "sonnet") {
				r = "agent:" + m
			}
			cur := res.Usage[r]
			cur.Add(Usage{CostUSD: u.CostUSD, InputTokens: u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens, OutputTokens: u.OutputTokens})
			res.Usage[r] = cur
		}
		if len(ev.ModelUsage) == 0 {
			cur := res.Usage[role]
			cur.Add(Usage{CostUSD: ev.TotalCostUSD})
			res.Usage[role] = cur
		}
		if cur, ok := res.Usage[role]; ok {
			cur.Calls += ev.NumTurns
			cur.DurationMS += ev.DurationMS
			res.Usage[role] = cur
		}
	}
	if mode == "scout" {
		// The small model explores read-only and writes a brief; the large
		// model starts from it with the same tools as agent-full.
		ev, sc, err := runClaudeAgent(ctx, cfg.Small, "", scoutSystem, "Find the code involved in this issue.\n\n# Issue\n"+problem,
			"Grep,Glob,mcp__cola__read_file", mcp, work)
		if err != nil {
			res.Outcome, res.Error = "error", "scout: "+err.Error()
			return res
		}
		addUsage(ev, "scout")
		for k, v := range sc {
			calls["scout:"+k] += v
		}
		res.Info["scout_turns"], res.Info["scout_usage"] = ev.NumTurns, ev.Usage
		res.Info["brief_tokens"] = len(ev.Result) / 4
		res.Info["brief"] = tail(ev.Result, 4000)
		prompt += "\n\n# Notes from a colleague who explored the repository for this issue\nThey may be incomplete or wrong; check what you rely on.\n\n" + ev.Result
	}
	final, mc, err := runClaudeAgent(ctx, model, effort, system, prompt, tools, mcp, work)
	if err != nil {
		res.Outcome, res.Error = "error", err.Error()
		return res
	}
	addUsage(final, role)
	for k, v := range mc {
		calls[k] += v
	}
	res.Info["turns"] = final.NumTurns
	res.Info["subtype"] = final.Subtype
	res.Info["tool_calls"] = calls
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
