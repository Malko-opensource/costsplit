package decider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var qs = map[string]Question{
	"desk": {Type: "choice", Instructions: "which team?", Criteria: map[string]string{"a": "A", "b": "B"}},
}

func server(t *testing.T, h http.HandlerFunc) *Client {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return &Client{URL: s.URL, Model: "m", APIKey: "k", Timeout: 200 * time.Millisecond}
}

func TestDecideSendsRequestAndParsesAnswer(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "m" || req.State != "s" || req.Questions["desk"].Type != "choice" {
			t.Errorf("request = %+v", req)
		}
		_, _ = w.Write([]byte(`{"answers":{"desk":{"type":"choice","choice":"a","confidence":0.9,"probabilities":{"a":0.9,"b":0.1}}}}`))
	})
	a, err := c.Decide(context.Background(), "s", qs)
	if err != nil || a["desk"].Choice != "a" || a["desk"].Confidence != 0.9 {
		t.Fatalf("a=%+v err=%v", a, err)
	}
}

func TestDecideUnavailable(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate_limited": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"bad_json":     func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`nope`)) },
		"not_a_label": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"answers":{"desk":{"type":"choice","choice":"zzz","confidence":0.9}}}`))
		},
		"missing_answer": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"answers":{}}`)) },
		"timeout":        func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := server(t, h).Decide(context.Background(), "s", qs)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
		})
	}
}
