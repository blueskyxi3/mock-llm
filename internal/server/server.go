package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type WaitFunc func(context.Context, time.Duration) error

func realWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type Server struct {
	config  Config
	models  map[string]Model
	slots   chan struct{}
	ready   atomic.Bool
	metrics metrics
	logger  *slog.Logger
	wait    WaitFunc
	root    context.Context
	cancel  context.CancelFunc
}

func New(c Config, log *slog.Logger, wait WaitFunc) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if wait == nil {
		wait = realWait
	}
	root, cancel := context.WithCancel(context.Background())
	s := &Server{config: c, models: map[string]Model{}, slots: make(chan struct{}, c.MaxActive), metrics: metrics{values: map[metricKey]metricValue{}}, logger: log, wait: wait, root: root, cancel: cancel}
	for _, m := range c.Models {
		s.models[m.Name] = m
	}
	s.ready.Store(true)
	return s, nil
}
func (s *Server) Drain()  { s.ready.Store(false) }
func (s *Server) Cancel() { s.Drain(); s.cancel() }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ok"})
		return
	case "/readyz":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		code := 200
		if !s.ready.Load() {
			code = 503
		}
		writeJSON(w, code, map[string]any{"ready": s.ready.Load()})
		return
	case "/metrics":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.metrics.write(w)
		return
	}
	if r.URL.Path != "/v1/models" && r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/embeddings" {
		writeError(w, 404, &APIError{"unsupported endpoint", "invalid_request_error", "", "not_found"})
		return
	}
	if s.config.APIKey != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.config.APIKey)) != 1 {
		writeError(w, 401, &APIError{"invalid API key", "authentication_error", "", "invalid_api_key"})
		s.metrics.record("_unknown", "_unknown", "authentication", 0, 0, 0, 0)
		return
	}
	if r.URL.Path == "/v1/models" {
		if r.Method != "GET" {
			writeError(w, 405, invalid("GET required", ""))
			return
		}
		items := []map[string]any{}
		for _, m := range s.config.Models {
			items = append(items, map[string]any{"id": m.Name, "object": "model", "created": 0, "owned_by": "mock-llm"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": items})
		return
	}
	if r.Method != "POST" {
		writeError(w, 405, invalid("POST required", ""))
		return
	}
	id := r.Header.Get("X-Request-ID")
	if id == "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			writeError(w, 500, &APIError{"request ID unavailable", "server_error", "", "internal_error"})
			return
		}
		id = hex.EncodeToString(b)
	}
	if len(id) > 128 || strings.ContainsAny(id, "\r\n") {
		writeError(w, 400, invalid("invalid request ID", "X-Request-ID"))
		return
	}
	// Header IDs are hashed for response IDs, keeping arbitrary caller input out of IDs.
	hash := sha256.Sum256([]byte(id))
	responseID := hex.EncodeToString(hash[:12])
	w.Header().Set("X-Request-ID", id)
	if !s.ready.Load() {
		w.Header().Set("Retry-After", "5")
		writeError(w, 503, &APIError{"service draining", "server_error", "", "draining"})
		return
	}
	select {
	case s.slots <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, 429, &APIError{"active request limit reached", "rate_limit_error", "", "concurrency_limit"})
		s.metrics.record("_unknown", "_unknown", "rejected", 0, 0, 0, 0)
		return
	}
	s.metrics.activity(1)
	defer func() { <-s.slots; s.metrics.activity(-1) }()
	ctx, cancel := context.WithTimeout(r.Context(), s.config.Timeout())
	defer cancel()
	stop := context.AfterFunc(s.root, cancel)
	defer stop()
	r = r.WithContext(ctx)
	started := time.Now()
	outcome := "invalid_request"
	model, scenario := "_unknown", "_unknown"
	input, output := 0, 0
	first := time.Duration(0)
	defer func() {
		elapsed := time.Since(started)
		s.metrics.record(model, scenario, outcome, input, output, elapsed, first)
		s.logger.Info("request completed", "request_id", id, "response_id", responseID, "model", model, "scenario", scenario, "outcome", outcome, "input_tokens", input, "output_tokens", output, "first_token_ms", float64(first)/float64(time.Millisecond), "duration_ms", float64(elapsed)/float64(time.Millisecond))
	}()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes))
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeError(w, 413, invalid("request body limit exceeded", "body"))
		} else {
			writeError(w, 400, invalid("cannot read request body", "body"))
		}
		return
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		writeError(w, 400, invalid("body must be a JSON object", "body"))
		return
	}
	for _, key := range []string{"functions", "function_call", "audio", "modalities", "prediction", "logprobs", "top_logprobs"} {
		if _, ok := object[key]; ok {
			writeError(w, 400, invalid("unsupported request field: "+key, key))
			return
		}
	}
	var name string
	if json.Unmarshal(object["model"], &name) != nil {
		writeError(w, 400, invalid("model is required", "model"))
		return
	}
	m, ok := s.models[name]
	if !ok {
		writeError(w, 404, &APIError{"unknown model", "invalid_request_error", "model", "model_not_found"})
		return
	}
	model, scenario = m.Name, m.Scenario
	if !s.ready.Load() {
		outcome = "draining"
		writeError(w, 503, &APIError{"service draining", "server_error", "", "draining"})
		return
	}
	fault := faultSelected(m, id)
	if fault && (m.Fault.Kind == "429" || m.Fault.Kind == "503") {
		code := 429
		if m.Fault.Kind == "503" {
			code = 503
		}
		w.Header().Set("Retry-After", "5")
		outcome = "injected_" + m.Fault.Kind
		writeError(w, code, &APIError{"injected supplier failure", "server_error", "", "injected_failure"})
		return
	}
	if fault && m.Fault.Kind == "timeout" {
		outcome = "timeout"
		_ = s.wait(ctx, s.config.Timeout()+time.Second)
		if ctx.Err() != context.Canceled {
			writeError(w, 504, &APIError{"injected pre-response timeout", "server_error", "", "timeout"})
		}
		return
	}
	if r.URL.Path == "/v1/embeddings" {
		if m.Kind != "embedding" {
			writeError(w, 400, invalid("chat model cannot generate embeddings", "model"))
			return
		}
		input, output, outcome = s.embeddings(w, r, body, m, started)
		return
	}
	if m.Kind != "chat" {
		writeError(w, 400, invalid("embedding model cannot generate chat", "model"))
		return
	}
	var request ChatRequest
	if json.Unmarshal(body, &request) != nil {
		writeError(w, 400, invalid("invalid chat request", "body"))
		return
	}
	plan, apiErr := planChat(s.config, m, request, responseID)
	if apiErr != nil {
		writeError(w, 400, apiErr)
		return
	}
	input = plan.prompt
	output, first, outcome = s.chat(w, r, plan, request.Stream, request.StreamOptions.IncludeUsage, responseID, started, fault)
}
func faultSelected(m Model, id string) bool {
	if m.Fault.Kind == "" {
		return false
	}
	h := sha256.Sum256([]byte(m.Seed + "\x00" + m.Name + "\x00" + id))
	v := (int(h[0])<<8 | int(h[1])) % 100
	return v < m.Fault.Percent
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, e *APIError) {
	writeJSON(w, status, map[string]any{"error": e})
}
func (s *Server) chat(w http.ResponseWriter, r *http.Request, p chatPlan, stream, includeUsage bool, id string, start time.Time, fault bool) (int, time.Duration, string) {
	if fault && (p.model.Fault.Kind == "disconnect" || p.model.Fault.Kind == "stream_error") && !stream {
		writeError(w, 400, invalid("this fault requires stream=true", "stream"))
		return 0, 0, "invalid_request"
	}
	count := p.frameCount()
	var full strings.Builder
	first := time.Duration(0)
	emitted := 0
	responseID := "chatcmpl-" + id
	created := start.Unix()
	send := func(delta map[string]any, finish any, usage *Usage, empty bool) error {
		choices := []any{}
		if !empty {
			choices = append(choices, map[string]any{"index": 0, "delta": delta, "finish_reason": finish})
		}
		obj := map[string]any{"id": responseID, "object": "chat.completion.chunk", "created": created, "model": p.model.Name, "choices": choices}
		if usage != nil {
			obj["usage"] = usage
		}
		data, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	for i := 0; i < count; i++ {
		due := p.first
		if count > 1 {
			due += (p.total - p.first) * time.Duration(i) / time.Duration(count-1)
		} else {
			due = p.total
		}
		if err := s.wait(r.Context(), time.Until(start.Add(due))); err != nil {
			if i == 0 && r.Context().Err() != context.Canceled {
				writeError(w, 504, &APIError{"request deadline exceeded", "server_error", "", "timeout"})
			}
			if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				return emitted, first, "timeout"
			}
			return emitted, first, "canceled"
		}
		if stream && i == 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			if err := send(map[string]any{"role": "assistant"}, nil, nil, false); err != nil {
				return emitted, first, "send_failure"
			}
		}
		f := p.frameAt(i)
		delta := map[string]any{}
		if len(f.calls) > 0 {
			delta["tool_calls"] = f.calls
		} else {
			delta["content"] = f.text
		}
		if first == 0 {
			first = time.Since(start)
		}
		if stream {
			if err := send(delta, nil, nil, false); err != nil {
				return emitted, first, "send_failure"
			}
		} else {
			full.WriteString(f.text)
		}
		emitted += CountTokens(f.text)
		if len(f.calls) > 0 {
			for _, call := range f.calls {
				emitted += CountTokens(call["function"].(map[string]any)["arguments"].(string))
			}
		}
		if fault && i+1 >= max(1, p.model.Fault.AfterChunks) {
			switch p.model.Fault.Kind {
			case "disconnect":
				if hijacker, ok := w.(http.Hijacker); ok {
					if conn, _, err := hijacker.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return emitted, first, "injected_disconnect"
			case "stream_error":
				data, _ := json.Marshal(map[string]any{"error": &APIError{"injected stream error", "server_error", "", "injected_stream_error"}})
				if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
					return emitted, first, "send_failure"
				}
				_ = http.NewResponseController(w).Flush()
				return emitted, first, "injected_stream_error"
			}
		}
	}
	usage := p.usage(fault && p.model.Fault.Kind == "invalid_usage")
	if stream {
		if err := send(map[string]any{}, p.finish, nil, false); err != nil {
			return emitted, first, "send_failure"
		}
		if includeUsage {
			if err := send(nil, nil, &usage, true); err != nil {
				return emitted, first, "send_failure"
			}
		}
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return emitted, first, "send_failure"
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return emitted, first, "send_failure"
		}
	} else {
		message := map[string]any{"role": "assistant", "content": full.String()}
		if len(p.calls) > 0 {
			message["content"] = nil
			message["tool_calls"] = p.calls
		}
		w.Header().Set("X-Mock-Duration-Ms", fmt.Sprintf("%.3f", float64(time.Since(start))/float64(time.Millisecond)))
		obj := map[string]any{"id": responseID, "object": "chat.completion", "created": created, "model": p.model.Name, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": p.finish}}, "usage": usage}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(obj); err != nil {
			return emitted, first, "send_failure"
		}
	}
	return p.output, first, "success"
}
