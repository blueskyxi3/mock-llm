package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type chatPlan struct {
	model          Model
	prompt, output int
	units          []string
	static         string
	calls          []ToolCall
	finish         string
	first, total   time.Duration
}

func planChat(c Config, m Model, r ChatRequest, id string) (chatPlan, *APIError) {
	p := chatPlan{model: m, finish: "stop"}
	if r.N != nil && *r.N != 1 {
		return p, invalid("only n=1 is supported", "n")
	}
	if len(r.Messages) == 0 {
		return p, invalid("messages must not be empty", "messages")
	}
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, message := range r.Messages {
		switch message.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return p, invalid("unsupported message role", "messages")
		}
		content, err := messageText(message.Content)
		if err != nil {
			return p, invalid("only text message content is supported", "messages")
		}
		if message.Role == "tool" {
			if !pending[message.ToolCallID] {
				return p, invalid("unknown, missing or already answered tool_call_id", "messages")
			}
			delete(pending, message.ToolCallID)
		} else if len(pending) > 0 {
			return p, invalid("all tool calls require results before the next message", "messages")
		}
		p.prompt += CountTokens(content)
		if len(message.ToolCalls) > 0 && message.Role != "assistant" {
			return p, invalid("tool_calls require assistant role", "messages")
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" || seen[call.ID] || call.Type != "function" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
				return p, invalid("invalid assistant tool call", "messages")
			}
			seen[call.ID] = true
			pending[call.ID] = true
			p.prompt += CountTokens(call.Function.Name) + CountTokens(call.Function.Arguments)
		}
	}
	if len(pending) > 0 {
		return p, invalid("unanswered tool calls", "messages")
	}
	toolNames := map[string]bool{}
	for _, tool := range r.Tools {
		if tool.Type != "function" || tool.Function.Name == "" || toolNames[tool.Function.Name] {
			return p, invalid("tools must contain unique named functions", "tools")
		}
		toolNames[tool.Function.Name] = true
		data, _ := json.Marshal(tool)
		p.prompt += CountTokens(string(data))
	}
	if p.prompt > c.MaxContextTokens {
		return p, invalid("context limit exceeded", "messages")
	}
	limit := c.DefaultOutputTokens
	if r.MaxTokens != nil {
		limit = *r.MaxTokens
	}
	if r.MaxCompletionTokens != nil {
		limit = *r.MaxCompletionTokens
	}
	if r.MaxTokens != nil && r.MaxCompletionTokens != nil && *r.MaxTokens != *r.MaxCompletionTokens {
		return p, invalid("conflicting output limits", "max_completion_tokens")
	}
	if limit < 1 || limit > c.MaxOutputTokens {
		return p, invalid(fmt.Sprintf("output limit must be between 1 and %d", c.MaxOutputTokens), "max_completion_tokens")
	}
	choice, forced := "auto", ""
	if len(r.ToolChoice) > 0 && string(r.ToolChoice) != "null" {
		if json.Unmarshal(r.ToolChoice, &choice) != nil {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(r.ToolChoice, &obj) != nil || obj.Type != "function" || obj.Function.Name == "" {
				return p, invalid("invalid tool_choice", "tool_choice")
			}
			choice = "forced"
			forced = obj.Function.Name
		}
	}
	switch choice {
	case "auto", "none", "required", "forced":
	default:
		return p, invalid("unsupported tool_choice", "tool_choice")
	}
	if forced != "" && !toolNames[forced] {
		return p, invalid("forced tool is not supplied", "tool_choice")
	}
	if choice == "required" && len(r.Tools) == 0 {
		return p, invalid("required tool_choice needs tools", "tool_choice")
	}
	if r.ResponseFormat.Type != "" && r.ResponseFormat.Type != "text" && r.ResponseFormat.Type != "json_object" {
		return p, invalid("only text and json_object response formats are supported", "response_format")
	}
	useTools := choice == "required" || choice == "forced" || choice == "auto" && m.Scenario == "agent" && len(r.Tools) > 0 && r.Messages[len(r.Messages)-1].Role != "tool"
	if useTools {
		if r.ResponseFormat.Type == "json_object" {
			return p, invalid("json_object cannot be combined with this tool response", "response_format")
		}
		for _, tool := range r.Tools {
			if forced != "" && tool.Function.Name != forced {
				continue
			}
			args, ok := m.ToolFixtures[tool.Function.Name]
			if !ok {
				return p, invalid("no configured argument fixture for tool: "+tool.Function.Name, "tools")
			}
			p.calls = append(p.calls, ToolCall{ID: fmt.Sprintf("call_%s_%d", id, len(p.calls)), Type: "function", Function: Function{Name: tool.Function.Name, Arguments: string(args)}})
			p.output += CountTokens(tool.Function.Name) + CountTokens(string(args))
			if forced != "" || r.ParallelToolCalls != nil && !*r.ParallelToolCalls {
				break
			}
		}
		if p.output > limit {
			return p, invalid("output budget cannot fit complete tool arguments", "max_completion_tokens")
		}
		p.finish = "tool_calls"
	} else if r.ResponseFormat.Type == "json_object" {
		if len(m.JSONResponse) == 0 {
			return p, invalid("no json_response configured for model", "response_format")
		}
		p.static = string(m.JSONResponse)
		p.output = CountTokens(p.static)
		if p.output > limit {
			return p, invalid("output budget cannot fit valid JSON object", "max_completion_tokens")
		}
	} else {
		p.output = min(m.OutputTokens, limit)
		if limit < m.OutputTokens {
			p.finish = "length"
		}
		p.units = TokenUnits(m.Response)
		if len(p.units) == 0 {
			return p, invalid("configured response has no tokens", "model")
		}
	}
	p.first = time.Duration(float64(m.FirstTokenMS)*float64(time.Millisecond) + float64(p.prompt)*m.InputMSPerToken*float64(time.Millisecond))
	p.total = max(time.Duration(m.TotalMS)*time.Millisecond, p.first)
	if p.total >= c.Timeout() {
		return p, invalid("configured timing with this input exceeds request timeout", "messages")
	}
	return p, nil
}
func (p chatPlan) usage(fault bool) Usage {
	u := Usage{Prompt: p.prompt, Completion: p.output}
	if p.model.FixedUsage != nil {
		if p.model.FixedUsage.Prompt != nil {
			u.Prompt = *p.model.FixedUsage.Prompt
		}
		if p.model.FixedUsage.Completion != nil {
			u.Completion = *p.model.FixedUsage.Completion
		}
	}
	if fault {
		u.Prompt = -1
		u.Completion = -1
	}
	u.Total = u.Prompt + u.Completion
	return u
}

type frame struct {
	text  string
	calls []map[string]any
}

func (p chatPlan) frameCount() int {
	if len(p.calls) > 0 {
		n := 1
		for _, call := range p.calls {
			n = max(n, (len([]rune(call.Function.Arguments))+p.model.ChunkTokens-1)/p.model.ChunkTokens)
		}
		return n
	}
	if p.static != "" {
		return max(1, (len(TokenUnits(p.static))+p.model.ChunkTokens-1)/p.model.ChunkTokens)
	}
	return (p.output + p.model.ChunkTokens - 1) / p.model.ChunkTokens
}
func (p chatPlan) frameAt(i int) frame {
	if len(p.calls) > 0 {
		f := frame{}
		for index, call := range p.calls {
			args := []rune(call.Function.Arguments)
			start := i * p.model.ChunkTokens
			if start >= len(args) {
				continue
			}
			entry := map[string]any{"index": index, "function": map[string]any{"arguments": string(args[start:min(len(args), start+p.model.ChunkTokens)])}}
			if i == 0 {
				entry["id"] = call.ID
				entry["type"] = "function"
				entry["function"].(map[string]any)["name"] = call.Function.Name
			}
			f.calls = append(f.calls, entry)
		}
		return f
	}
	if p.static != "" {
		units := TokenUnits(p.static)
		return frame{text: strings.Join(units[i*p.model.ChunkTokens:min(len(units), (i+1)*p.model.ChunkTokens)], "")}
	}
	return frame{text: outputChunk(p.units, i*p.model.ChunkTokens, min(p.output, (i+1)*p.model.ChunkTokens))}
}
