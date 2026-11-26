// Package aichecktest is a fake Jev and Anthropic API for tests of the AI
// checks: one HTTP server answering both, scripted per model, recording
// every request it got.
package aichecktest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Answer scripts one model's reply. Status other than 0 or 200 answers that
// HTTP status with Body. For Jev, Noul is the noul (nil: no noul in the
// answer); for Claude, Verdict, Confidence and Observed are the answer
// (Text, when set, replaces the JSON text) and Refusal stops it.
type Answer struct {
	Status     int
	Body       string
	Noul       *float64
	Verdict    string
	Confidence float64
	Observed   string
	Text       string
	Refusal    bool
	// Tokens charged.
	InputTokens, OutputTokens int
}

// Request is one request the server got.
type Request struct {
	Path   string
	Model  string
	Header http.Header
	Body   map[string]any
}

// Server is the fake.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	answers  map[string]Answer
	requests []Request
}

// New starts a fake answering the models in answers; an unscripted model
// answers 500.
func New(t testing.TB, answers map[string]Answer) *Server {
	s := &Server{answers: answers}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// JevURL and AnthropicURL are the fake's two endpoints.
func (s *Server) JevURL() string       { return s.URL + "/v1/systemone" }
func (s *Server) AnthropicURL() string { return s.URL + "/v1/messages" }

// Set replaces one model's script.
func (s *Server) Set(model string, a Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[model] = a
}

// Requests returns what the server got, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Models returns the model of each request, in order.
func (s *Server) Models() []string {
	var out []string
	for _, r := range s.Requests() {
		out = append(out, r.Model)
	}
	return out
}

// Noul is a pointer to v, for Answer.Noul.
func Noul(v float64) *float64 { return &v }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	model, _ := body["model"].(string)
	s.mu.Lock()
	s.requests = append(s.requests, Request{Path: r.URL.Path, Model: model, Header: r.Header.Clone(), Body: body})
	a, ok := s.answers[model]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"unscripted model"}`)
		return
	}
	if a.Status != 0 && a.Status != http.StatusOK {
		w.WriteHeader(a.Status)
		_, _ = io.WriteString(w, a.Body)
		return
	}
	if a.Body != "" {
		_, _ = io.WriteString(w, a.Body)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/systemone") {
		answer := map[string]any{"type": "noul"}
		if a.Noul != nil {
			answer["noul"] = *a.Noul
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   model,
			"answers": map[string]any{"assertion": answer},
			"usage":   map[string]any{"input_tokens": a.InputTokens, "output_tokens": 1},
		})
		return
	}
	text := a.Text
	if text == "" {
		b, _ := json.Marshal(map[string]any{"verdict": a.Verdict, "confidence": a.Confidence, "observed": a.Observed})
		text = string(b)
	}
	stop := "end_turn"
	if a.Refusal {
		stop = "refusal"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":       model,
		"stop_reason": stop,
		"content":     []any{map[string]any{"type": "thinking", "thinking": ""}, map[string]any{"type": "text", "text": text}},
		"usage":       map[string]any{"input_tokens": a.InputTokens, "output_tokens": a.OutputTokens},
	})
}
