package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestTokenEstimator(t *testing.T) {
	for _, item := range []struct {
		text  string
		count int
	}{{"你好世界", 4}, {"abcdefgh ij", 3}, {"a b,c!", 5}, {"  \n", 0}, {"🙂", 1}} {
		if CountTokens(item.text) != item.count {
			t.Errorf("%q: %d", item.text, CountTokens(item.text))
		}
	}
}
func TestInputCostAndWaitSchedule(t *testing.T) {
	c := DefaultConfig()
	m := c.Models[0]
	m.InputMSPerToken = 2
	m.TotalMS = 0
	r := ChatRequest{Messages: []Message{{Role: "user", Content: json.RawMessage(`"你好世界"`)}}}
	p, err := planChat(c, m, r, "id")
	if err != nil {
		t.Fatal(err)
	}
	if p.first != 208*time.Millisecond || p.total != p.first {
		t.Fatal(p)
	}
	m.Fault = Fault{Kind: "429", Percent: 50}
	expected := faultSelected(m, "same")
	for i := 0; i < 100; i++ {
		if faultSelected(m, "same") != expected {
			t.Fatal("fault depends on call order")
		}
	}
}
func TestConfigValidationAndOverrides(t *testing.T) {
	t.Setenv("MOCK_LLM_LISTEN", ":9090")
	t.Setenv("MOCK_LLM_API_KEY", "test")
	c, err := LoadConfig("")
	if err != nil || c.Listen != ":9090" || c.APIKey != "test" {
		t.Fatal(c, err)
	}
	c.Models[1].Name = c.Models[0].Name
	if c.Validate() == nil {
		t.Fatal("duplicate model accepted")
	}
	c = DefaultConfig()
	c.MaxActive = 0
	if c.Validate() == nil {
		t.Fatal("invalid limits accepted")
	}
}
func TestFixedUsage(t *testing.T) {
	c := DefaultConfig()
	m := c.Models[0]
	prompt, completion := 10, 5
	m.FixedUsage = &FixedUsage{Prompt: &prompt, Completion: &completion}
	p, err := planChat(c, m, ChatRequest{Messages: []Message{{Role: "user", Content: json.RawMessage(`"long input"`)}}}, "id")
	if err != nil {
		t.Fatal(err)
	}
	u := p.usage(false)
	if u.Prompt != 10 || u.Completion != 5 || u.Total != 15 || p.prompt == 10 {
		t.Fatal(p, u)
	}
}
func TestRootCancellation(t *testing.T) {
	s, err := New(DefaultConfig(), nil, func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	s.Cancel()
	if s.root.Err() != context.Canceled || s.ready.Load() {
		t.Fatal("cancel failed")
	}
}
