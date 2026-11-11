// Package ledger appends one line per attempt to .graybox/decisions.jsonl and
// counts attempts so the tool can recommend escalation (D-015).
package ledger

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

type Entry struct {
	Time    time.Time       `json:"time"`
	Task    string          `json:"task"`
	Gate    string          `json:"gate"` // plan | done
	Model   string          `json:"model"`
	Attempt int             `json:"attempt"`
	Symbol  string          `json:"symbol,omitempty"`
	Checks  any             `json:"checks,omitempty"`
	Decider bool            `json:"decider_called"`
	Answers any             `json:"answers,omitempty"`
	Outcome any             `json:"outcome,omitempty"`
	Next    string          `json:"next"`
	Error   string          `json:"error,omitempty"`
	Extra   json.RawMessage `json:"extra,omitempty"`
}

type Ledger struct{ Path string }

func (l Ledger) Append(e Entry) error {
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func (l Ledger) Entries() ([]Entry, error) {
	f, err := os.Open(l.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Attempts counts earlier attempts for a task/gate/model. Paused attempts do
// not count: they were not the model's fault (D-012).
func (l Ledger) Attempts(task, gate, model string) (int, error) {
	es, err := l.Entries()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range es {
		if e.Task == task && e.Gate == gate && e.Model == model && e.Next != "paused" {
			n++
		}
	}
	return n, nil
}
