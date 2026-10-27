package server

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"time"
)

type FixedUsage struct {
	Prompt     *int `json:"prompt_tokens,omitempty"`
	Completion *int `json:"completion_tokens,omitempty"`
}
type Fault struct {
	Kind        string `json:"kind,omitempty"`
	Percent     int    `json:"percent,omitempty"`
	AfterChunks int    `json:"after_chunks,omitempty"`
}
type Model struct {
	Name            string                     `json:"name"`
	Kind            string                     `json:"kind"`
	Scenario        string                     `json:"scenario"`
	FirstTokenMS    int                        `json:"first_token_ms"`
	InputMSPerToken float64                    `json:"input_ms_per_token"`
	TotalMS         int                        `json:"total_ms"`
	OutputTokens    int                        `json:"output_tokens"`
	ChunkTokens     int                        `json:"chunk_tokens"`
	Response        string                     `json:"response"`
	JSONResponse    json.RawMessage            `json:"json_response,omitempty"`
	ToolFixtures    map[string]json.RawMessage `json:"tool_fixtures,omitempty"`
	Dimensions      int                        `json:"dimensions,omitempty"`
	Seed            string                     `json:"seed"`
	FixedUsage      *FixedUsage                `json:"fixed_usage,omitempty"`
	Fault           Fault                      `json:"fault,omitempty"`
}
type Config struct {
	Listen                string  `json:"listen"`
	APIKey                string  `json:"api_key,omitempty"`
	MaxBodyBytes          int64   `json:"max_body_bytes"`
	MaxContextTokens      int     `json:"max_context_tokens"`
	DefaultOutputTokens   int     `json:"default_output_tokens"`
	MaxOutputTokens       int     `json:"max_output_tokens"`
	MaxActive             int     `json:"max_active"`
	MaxBatch              int     `json:"max_batch"`
	RequestTimeoutSeconds int     `json:"request_timeout_seconds"`
	ShutdownSeconds       int     `json:"shutdown_seconds"`
	Models                []Model `json:"models"`
}

func DefaultConfig() Config {
	c := Config{Listen: ":8080", MaxBodyBytes: 16 << 20, MaxContextTokens: 128 * 1024, DefaultOutputTokens: 256, MaxOutputTokens: 8192, MaxActive: 4096, MaxBatch: 128, RequestTimeoutSeconds: 120, ShutdownSeconds: 30}
	for _, item := range []struct {
		name, scenario string
		total, output  int
	}{
		{"mock-chat-short", "chat", 1000, 128}, {"mock-chat-long", "chat", 10000, 2048}, {"mock-stream-10s", "chat", 10000, 512}, {"mock-stream-30s", "chat", 30000, 512}, {"mock-stream-60s", "chat", 60000, 512}, {"mock-rag", "rag", 1000, 128}, {"mock-agent", "agent", 1000, 128},
	} {
		m := Model{Name: item.name, Kind: "chat", Scenario: item.scenario, FirstTokenMS: 200, TotalMS: item.total, OutputTokens: item.output, ChunkTokens: 8, Response: "这是一段可复现的模拟回答，用于测试模型代理、流式输出和账务链路。\n", JSONResponse: json.RawMessage(`{"answer":"可复现的模拟回答","mock":true}`), Seed: "mock-llm-v1"}
		if m.Scenario == "rag" {
			m.Response = "根据检索片段，AI Hub 在请求开始前预扣，完成后提交账务事件并结算。[doc-1] SQL 和 Redis 通过凭证补偿保持最终一致。[doc-2]\n"
		}
		if m.Scenario == "agent" {
			m.Response = "根据工具返回的结果，本轮任务已经完成。这是确定性夹具回答，不执行真实工具。\n"
			m.ToolFixtures = map[string]json.RawMessage{"search_docs": json.RawMessage(`{"query":"AI Hub billing"}`), "calculate": json.RawMessage(`{"expression":"2+3"}`)}
		}
		c.Models = append(c.Models, m)
	}
	c.Models = append(c.Models, Model{Name: "mock-embedding", Kind: "embedding", Scenario: "embedding", Dimensions: 1536, Seed: "mock-llm-v1"})
	return c
}
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err = json.Unmarshal(data, &c); err != nil {
			return c, err
		}
	}
	if v, ok := os.LookupEnv("MOCK_LLM_LISTEN"); ok {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("MOCK_LLM_API_KEY"); ok {
		c.APIKey = v
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.Listen == "" || c.MaxBodyBytes < 1 || c.MaxContextTokens < 1 || c.DefaultOutputTokens < 1 || c.DefaultOutputTokens > c.MaxOutputTokens || c.MaxActive < 1 || c.MaxBatch < 1 || c.MaxBatch > 128 || c.MaxOutputTokens > 8192 || c.RequestTimeoutSeconds < 1 || c.ShutdownSeconds < 1 || len(c.Models) == 0 {
		return fmt.Errorf("invalid server limits")
	}
	names := map[string]bool{}
	for _, m := range c.Models {
		if !regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`).MatchString(m.Name) || names[m.Name] {
			return fmt.Errorf("empty or duplicate model name: %q", m.Name)
		}
		names[m.Name] = true
		if m.Kind != "chat" && m.Kind != "embedding" {
			return fmt.Errorf("invalid model kind: %s", m.Name)
		}
		if m.Kind == "chat" && (m.Scenario != "chat" && m.Scenario != "rag" && m.Scenario != "agent") {
			return fmt.Errorf("invalid scenario: %s", m.Name)
		}
		if m.FirstTokenMS < 0 || m.TotalMS < 0 || m.InputMSPerToken < 0 || math.IsNaN(m.InputMSPerToken) || math.IsInf(m.InputMSPerToken, 0) || m.InputMSPerToken > float64(c.RequestTimeoutSeconds*1000) || m.FirstTokenMS > c.RequestTimeoutSeconds*1000 || m.TotalMS > c.RequestTimeoutSeconds*1000 {
			return fmt.Errorf("invalid timing: %s", m.Name)
		}
		if m.Kind == "chat" && (m.OutputTokens < 1 || m.OutputTokens > c.MaxOutputTokens || m.ChunkTokens < 1 || m.Response == "") {
			return fmt.Errorf("invalid chat output: %s", m.Name)
		}
		if m.Kind == "embedding" && (m.Dimensions < 1 || m.Dimensions > 3072) {
			return fmt.Errorf("invalid dimensions: %s", m.Name)
		}
		if len(m.JSONResponse) > 0 {
			var obj map[string]any
			if json.Unmarshal(m.JSONResponse, &obj) != nil || obj == nil {
				return fmt.Errorf("json_response must be an object: %s", m.Name)
			}
		}
		for name, args := range m.ToolFixtures {
			var obj map[string]any
			if name == "" || json.Unmarshal(args, &obj) != nil || obj == nil {
				return fmt.Errorf("invalid tool fixture: %s", name)
			}
		}
		switch m.Fault.Kind {
		case "", "429", "503", "timeout", "disconnect", "stream_error", "invalid_usage":
		default:
			return fmt.Errorf("unknown fault: %s", m.Fault.Kind)
		}
		if m.Kind == "embedding" && m.Fault.Kind != "" && m.Fault.Kind != "429" && m.Fault.Kind != "503" && m.Fault.Kind != "timeout" {
			return fmt.Errorf("fault requires a chat model: %s", m.Name)
		}
		if m.FixedUsage != nil && ((m.FixedUsage.Prompt != nil && *m.FixedUsage.Prompt < 0) || (m.FixedUsage.Completion != nil && *m.FixedUsage.Completion < 0)) {
			return fmt.Errorf("negative fixed usage requires invalid_usage fault: %s", m.Name)
		}
		if m.Fault.Percent < 0 || m.Fault.Percent > 100 || (m.Fault.Kind != "" && m.Fault.Percent == 0) || m.Fault.AfterChunks < 0 {
			return fmt.Errorf("invalid fault rate: %s", m.Name)
		}
	}
	return nil
}
func (c Config) Timeout() time.Duration { return time.Duration(c.RequestTimeoutSeconds) * time.Second }
