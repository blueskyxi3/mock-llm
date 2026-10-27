package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mock-llm/internal/server"
)

func fastWait(ctx context.Context, _ time.Duration) error { return ctx.Err() }
func setup(t *testing.T, change func(*server.Config), wait server.WaitFunc) (*server.Server, *httptest.Server) {
	t.Helper()
	c := server.DefaultConfig()
	if change != nil {
		change(&c)
	}
	service, err := server.New(c, nil, wait)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(service)
	t.Cleanup(func() { service.Cancel(); h.Close() })
	return service, h
}
func request(t *testing.T, h *httptest.Server, path string, value any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(value)
	resp, err := http.Post(h.URL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, result
}
func chat(model string) map[string]any {
	return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "你好，请解释 billing。"}}}
}
func TestChatAndUsage(t *testing.T) {
	_, h := setup(t, nil, fastWait)
	r := chat("mock-chat-long")
	r["max_completion_tokens"] = 16
	status, obj := request(t, h, "/v1/chat/completions", r)
	if status != 200 {
		t.Fatal(status, obj)
	}
	choice := obj["choices"].([]any)[0].(map[string]any)
	content := choice["message"].(map[string]any)["content"].(string)
	usage := obj["usage"].(map[string]any)
	if choice["finish_reason"] != "length" || int(usage["completion_tokens"].(float64)) != 16 || server.CountTokens(content) != 16 {
		t.Fatal(obj)
	}
	if obj["object"] != "chat.completion" || obj["model"] != "mock-chat-long" {
		t.Fatal(obj)
	}
	r = chat("mock-chat-short")
	r["response_format"] = map[string]any{"type": "json_object"}
	status, obj = request(t, h, "/v1/chat/completions", r)
	if status != 200 {
		t.Fatal(obj)
	}
	content = obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if !json.Valid([]byte(content)) {
		t.Fatal(content)
	}
}
func TestInputValidation(t *testing.T) {
	_, h := setup(t, func(c *server.Config) { c.MaxContextTokens = 1000 }, fastWait)
	cases := []struct {
		name   string
		modify func(map[string]any)
		status int
	}{
		{"unknown_model", func(r map[string]any) { r["model"] = "missing" }, 404},
		{"empty_messages", func(r map[string]any) { r["messages"] = []any{} }, 400},
		{"image", func(r map[string]any) {
			r["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url"}}}}
		}, 400},
		{"long_input", func(r map[string]any) {
			r["messages"] = []any{map[string]any{"role": "user", "content": strings.Repeat("中", 1001)}}
		}, 400},
		{"max_tokens", func(r map[string]any) { r["max_tokens"] = 0 }, 400},
		{"conflict", func(r map[string]any) { r["max_tokens"] = 10; r["max_completion_tokens"] = 11 }, 400},
		{"n", func(r map[string]any) { r["n"] = 2 }, 400},
		{"schema", func(r map[string]any) { r["response_format"] = map[string]any{"type": "json_schema"} }, 400},
		{"legacy_functions", func(r map[string]any) { r["functions"] = []any{} }, 400},
		{"tool_result", func(r map[string]any) {
			r["messages"] = []any{map[string]any{"role": "tool", "content": "5", "tool_call_id": "unknown"}}
		}, 400},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			r := chat("mock-chat-short")
			test.modify(r)
			status, obj := request(t, h, "/v1/chat/completions", r)
			if status != test.status || obj["error"] == nil {
				t.Fatal(status, obj)
			}
		})
	}
}
func readSSE(t *testing.T, resp *http.Response) ([]map[string]any, bool) {
	t.Helper()
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	events := []map[string]any{}
	done := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) != nil {
			t.Fatal(line)
		}
		events = append(events, obj)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events, done
}
func TestStreamingEquivalence(t *testing.T) {
	_, h := setup(t, nil, fastWait)
	for _, model := range []string{"mock-chat-short", "mock-chat-long", "mock-rag"} {
		t.Run(model, func(t *testing.T) {
			r := chat(model)
			_, normal := request(t, h, "/v1/chat/completions", r)
			r["stream"] = true
			r["stream_options"] = map[string]bool{"include_usage": true}
			data, _ := json.Marshal(r)
			resp, err := http.Post(h.URL+"/v1/chat/completions", "application/json", bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			events, done := readSSE(t, resp)
			if !done {
				t.Fatal("missing DONE")
			}
			content := ""
			id := ""
			var usage map[string]any
			finish := ""
			for _, event := range events {
				if id == "" {
					id = event["id"].(string)
				}
				if event["id"] != id {
					t.Fatal("unstable ID")
				}
				if u, ok := event["usage"].(map[string]any); ok {
					usage = u
				}
				for _, item := range event["choices"].([]any) {
					choice := item.(map[string]any)
					delta := choice["delta"].(map[string]any)
					if text, ok := delta["content"].(string); ok {
						content += text
					}
					if f, ok := choice["finish_reason"].(string); ok {
						finish = f
					}
				}
			}
			expected := normal["choices"].([]any)[0].(map[string]any)
			if content != expected["message"].(map[string]any)["content"] || finish != expected["finish_reason"] || fmt.Sprint(usage) != fmt.Sprint(normal["usage"]) {
				t.Fatal("stream differs from normal", events)
			}
		})
	}
}
func agentTools() []any {
	return []any{map[string]any{"type": "function", "function": map[string]any{"name": "search_docs", "parameters": map[string]any{"type": "object"}}}, map[string]any{"type": "function", "function": map[string]any{"name": "calculate", "parameters": map[string]any{"type": "object"}}}}
}
func TestAgentRoundTrip(t *testing.T) {
	_, h := setup(t, nil, fastWait)
	r := chat("mock-agent")
	r["tools"] = agentTools()
	status, obj := request(t, h, "/v1/chat/completions", r)
	if status != 200 {
		t.Fatal(obj)
	}
	assistant := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatal(obj)
	}
	messages := r["messages"].([]any)
	messages = append(messages, assistant)
	for _, call := range calls {
		item := call.(map[string]any)
		if !json.Valid([]byte(item["function"].(map[string]any)["arguments"].(string))) {
			t.Fatal(item)
		}
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item["id"], "content": "local fixture result"})
	}
	r["messages"] = messages
	status, obj = request(t, h, "/v1/chat/completions", r)
	if status != 200 || obj["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatal(status, obj)
	}
	r = chat("mock-agent")
	r["tools"] = agentTools()
	r["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": "calculate"}}
	status, obj = request(t, h, "/v1/chat/completions", r)
	if status != 200 || len(obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)) != 1 {
		t.Fatal(obj)
	}
	r["tool_choice"] = "none"
	status, obj = request(t, h, "/v1/chat/completions", r)
	if status != 200 || obj["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatal(obj)
	}
}
func TestAgentStreamingArguments(t *testing.T) {
	_, h := setup(t, nil, fastWait)
	r := chat("mock-agent")
	r["tools"] = agentTools()
	r["stream"] = true
	data, _ := json.Marshal(r)
	resp, err := http.Post(h.URL+"/v1/chat/completions", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	events, done := readSSE(t, resp)
	if !done {
		t.Fatal("missing DONE")
	}
	args := map[int]string{}
	names := map[int]string{}
	for _, event := range events {
		for _, choice := range event["choices"].([]any) {
			delta := choice.(map[string]any)["delta"].(map[string]any)
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, call := range calls {
					item := call.(map[string]any)
					index := int(item["index"].(float64))
					function := item["function"].(map[string]any)
					args[index] += function["arguments"].(string)
					if name, ok := function["name"].(string); ok {
						names[index] = name
					}
				}
			}
		}
	}
	if len(args) != 2 {
		t.Fatal(args)
	}
	for index, arg := range args {
		if names[index] == "" || !json.Valid([]byte(arg)) {
			t.Fatal(names, args)
		}
	}
}
func TestEmbeddings(t *testing.T) {
	_, h := setup(t, nil, fastWait)
	r := map[string]any{"model": "mock-embedding", "input": []string{"你好", "hello"}, "dimensions": 32}
	status, obj := request(t, h, "/v1/embeddings", r)
	if status != 200 {
		t.Fatal(obj)
	}
	items := obj["data"].([]any)
	if len(items) != 2 {
		t.Fatal(obj)
	}
	vector := items[0].(map[string]any)["embedding"].([]any)
	norm := 0.0
	for _, value := range vector {
		norm += value.(float64) * value.(float64)
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatal(norm)
	}
	_, repeat := request(t, h, "/v1/embeddings", r)
	if fmt.Sprint(obj) != fmt.Sprint(repeat) {
		t.Fatal("not deterministic")
	}
	r["encoding_format"] = "base64"
	_, encoded := request(t, h, "/v1/embeddings", r)
	decoded, err := base64.StdEncoding.DecodeString(encoded["data"].([]any)[0].(map[string]any)["embedding"].(string))
	if err != nil || len(decoded) != 128 {
		t.Fatal(err)
	}
	for i, v := range vector {
		if math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(decoded[i*4:])))-v.(float64)) > 1e-6 {
			t.Fatal("encoding differs")
		}
	}
	for _, input := range []any{[]int{1, 2, 3}, [][]int{{1, 2}, {3, 4}}} {
		r["input"] = input
		status, obj = request(t, h, "/v1/embeddings", r)
		if status != 200 {
			t.Fatal(obj)
		}
	}
	for _, bad := range []any{[]int{-1}, []any{"hello", []int{1, 2}}, []string{}, ""} {
		r["input"] = bad
		status, _ = request(t, h, "/v1/embeddings", r)
		if status != 400 {
			t.Fatal(bad, status)
		}
	}
	r["input"] = "ok"
	r["dimensions"] = 3073
	status, _ = request(t, h, "/v1/embeddings", r)
	if status != 400 {
		t.Fatal(status)
	}
}
func TestAuthBodyAndReadiness(t *testing.T) {
	s, h := setup(t, func(c *server.Config) { c.APIKey = "local-test"; c.MaxBodyBytes = 256 }, fastWait)
	resp, err := http.Get(h.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", h.URL+"/v1/chat/completions", strings.NewReader(strings.Repeat("x", 300)))
	req.Header.Set("Authorization", "Bearer local-test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatal(resp.StatusCode)
	}
	s.Drain()
	resp, err = http.Get(h.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
	resp, err = http.Get(h.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}
func TestConcurrencyCancellationAndMetrics(t *testing.T) {
	entered := make(chan struct{}, 1)
	wait := func(ctx context.Context, _ time.Duration) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	_, h := setup(t, func(c *server.Config) { c.MaxActive = 1 }, wait)
	ctx, cancel := context.WithCancel(context.Background())
	data, _ := json.Marshal(chat("mock-chat-short"))
	req, _ := http.NewRequestWithContext(ctx, "POST", h.URL+"/v1/chat/completions", bytes.NewReader(data))
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	resp, err := http.Get(h.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("full request slots changed readiness")
	}
	status, _ := request(t, h, "/v1/chat/completions", chat("mock-chat-short"))
	if status != 429 {
		t.Fatal(status)
	}
	cancel()
	<-done
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get(h.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(body), "mock_llm_active_requests 0") && strings.Contains(string(body), `outcome="canceled"`) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cancellation did not release slot")
}
func TestConcurrentRequests(t *testing.T) {
	_, h := setup(t, nil, fastWait)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := request(t, h, "/v1/chat/completions", chat("mock-chat-short"))
			if status != 200 {
				t.Errorf("status %d", status)
			}
		}()
	}
	wg.Wait()
}
func TestFaultsAndDeterminism(t *testing.T) {
	for _, kind := range []string{"429", "503", "stream_error", "invalid_usage"} {
		t.Run(kind, func(t *testing.T) {
			_, h := setup(t, func(c *server.Config) { c.Models[0].Fault = server.Fault{Kind: kind, Percent: 100, AfterChunks: 1} }, fastWait)
			r := chat("mock-chat-short")
			if kind == "429" || kind == "503" {
				status, _ := request(t, h, "/v1/chat/completions", r)
				if fmt.Sprint(status) != kind {
					t.Fatal(status)
				}
				return
			}
			r["stream"] = true
			r["stream_options"] = map[string]bool{"include_usage": true}
			data, _ := json.Marshal(r)
			resp, err := http.Post(h.URL+"/v1/chat/completions", "application/json", bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			events, done := readSSE(t, resp)
			if kind == "stream_error" {
				if done || events[len(events)-1]["error"] == nil {
					t.Fatal(events)
				}
			} else if !done || events[len(events)-1]["usage"].(map[string]any)["prompt_tokens"] != float64(-1) {
				t.Fatal(events)
			}
		})
	}
}
func TestConfiguredDurationRealTime(t *testing.T) {
	if testing.Short() {
		t.Skip("real timing test")
	}
	_, h := setup(t, nil, nil)
	for _, test := range []struct {
		model    string
		duration time.Duration
	}{{"mock-chat-short", time.Second}, {"mock-chat-long", 10 * time.Second}, {"mock-stream-10s", 10 * time.Second}, {"mock-stream-30s", 30 * time.Second}, {"mock-stream-60s", 60 * time.Second}} {
		t.Run(test.model, func(t *testing.T) {
			r := chat(test.model)
			r["stream"] = true
			start := time.Now()
			data, _ := json.Marshal(r)
			resp, err := http.Post(h.URL+"/v1/chat/completions", "application/json", bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if first := time.Since(start); first < 150*time.Millisecond || first > 2*time.Second {
				t.Fatal("unexpected first byte", first)
			}
			_, done := readSSE(t, resp)
			elapsed := time.Since(start)
			if !done || elapsed < test.duration-100*time.Millisecond || elapsed > test.duration+2*time.Second {
				t.Fatal("unexpected duration", elapsed)
			}
		})
	}
}

func TestDisconnectAndTimeout(t *testing.T) {
	t.Run("disconnect", func(t *testing.T) {
		_, h := setup(t, func(c *server.Config) {
			c.Models[0].Fault = server.Fault{Kind: "disconnect", Percent: 100, AfterChunks: 1}
		}, fastWait)
		r := chat("mock-chat-short")
		r["stream"] = true
		data, _ := json.Marshal(r)
		resp, err := http.Post(h.URL+"/v1/chat/completions", "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err == nil || strings.Contains(string(body), "[DONE]") {
			t.Fatal("disconnect did not truncate transport", err, string(body))
		}
	})
	t.Run("timeout", func(t *testing.T) {
		_, h := setup(t, func(c *server.Config) {
			c.RequestTimeoutSeconds = 1
			for i := range c.Models {
				c.Models[i].TotalMS = 0
				c.Models[i].FirstTokenMS = 0
			}
			c.Models[0].Fault = server.Fault{Kind: "timeout", Percent: 100}
		}, nil)
		start := time.Now()
		status, obj := request(t, h, "/v1/chat/completions", chat("mock-chat-short"))
		if status != 504 || time.Since(start) < 900*time.Millisecond {
			t.Fatal(status, obj)
		}
	})
}
func TestDrainPreservesAcceptedRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	wait := func(ctx context.Context, _ time.Duration) error {
		once.Do(func() { close(entered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	s, h := setup(t, nil, wait)
	done := make(chan int, 1)
	go func() { status, _ := request(t, h, "/v1/chat/completions", chat("mock-chat-short")); done <- status }()
	<-entered
	s.Drain()
	status, _ := request(t, h, "/v1/chat/completions", chat("mock-chat-short"))
	if status != 503 {
		t.Fatal(status)
	}
	close(release)
	if status = <-done; status != 200 {
		t.Fatal("accepted request canceled during drain", status)
	}
}
