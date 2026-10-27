package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

type embeddingInput struct {
	key    string
	tokens int
}

func parseInputs(raw json.RawMessage) ([]embeddingInput, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, fmt.Errorf("input is empty")
		}
		return []embeddingInput{{s, CountTokens(s)}}, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil, fmt.Errorf("input must be text, text array or token IDs")
	}
	var first int
	if json.Unmarshal(items[0], &first) == nil {
		ids := []int{}
		if json.Unmarshal(raw, &ids) != nil {
			return nil, fmt.Errorf("invalid token IDs")
		}
		for _, id := range ids {
			if id < 0 {
				return nil, fmt.Errorf("negative token ID")
			}
		}
		canonical, _ := json.Marshal(ids)
		return []embeddingInput{{string(canonical), len(ids)}}, nil
	}
	out := []embeddingInput{}
	for _, item := range items {
		if json.Unmarshal(item, &s) == nil {
			if s == "" {
				return nil, fmt.Errorf("input is empty")
			}
			out = append(out, embeddingInput{s, CountTokens(s)})
			continue
		}
		var ids []int
		if json.Unmarshal(item, &ids) != nil || len(ids) == 0 {
			return nil, fmt.Errorf("mixed or invalid embedding inputs")
		}
		for _, id := range ids {
			if id < 0 {
				return nil, fmt.Errorf("negative token ID")
			}
		}
		var key any = ids
		bytes, _ := json.Marshal(key)
		out = append(out, embeddingInput{string(bytes), len(ids)})
	}
	// Do not silently accept mixed strings and token arrays.
	var stringsOnly []string
	if json.Unmarshal(raw, &stringsOnly) != nil {
		var tokenOnly [][]int
		if json.Unmarshal(raw, &tokenOnly) != nil {
			return nil, fmt.Errorf("mixed embedding input types")
		}
	}
	return out, nil
}
func embeddingVector(key, seed, model string, dimension int) []float32 {
	out := make([]float32, dimension)
	sum := 0.0
	for i := 0; i < dimension; i += 8 {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", seed, model, key, i/8)))
		for j := 0; j < 8 && i+j < dimension; j++ {
			value := float64(binary.LittleEndian.Uint32(h[j*4:j*4+4]))/float64(math.MaxUint32)*2 - 1
			out[i+j] = float32(value)
			sum += value * value
		}
	}
	norm := math.Sqrt(sum)
	for i := range out {
		out[i] = float32(float64(out[i]) / norm)
	}
	return out
}
func (s *Server) embeddings(w http.ResponseWriter, r *http.Request, body []byte, m Model, start time.Time) (int, int, string) {
	var request struct {
		Input      json.RawMessage `json:"input"`
		Dimensions *int            `json:"dimensions"`
		Encoding   string          `json:"encoding_format"`
	}
	if json.Unmarshal(body, &request) != nil {
		writeError(w, 400, invalid("invalid embedding request", "body"))
		return 0, 0, "invalid_request"
	}
	inputs, err := parseInputs(request.Input)
	if err != nil {
		writeError(w, 400, invalid(err.Error(), "input"))
		return 0, 0, "invalid_request"
	}
	if len(inputs) > s.config.MaxBatch {
		writeError(w, 400, invalid("batch limit exceeded", "input"))
		return 0, 0, "invalid_request"
	}
	dims := m.Dimensions
	if request.Dimensions != nil {
		dims = *request.Dimensions
	}
	if dims < 1 || dims > 3072 {
		writeError(w, 400, invalid("dimensions must be between 1 and 3072", "dimensions"))
		return 0, 0, "invalid_request"
	}
	if request.Encoding != "" && request.Encoding != "float" && request.Encoding != "base64" {
		writeError(w, 400, invalid("unsupported encoding format", "encoding_format"))
		return 0, 0, "invalid_request"
	}
	total := 0
	for _, input := range inputs {
		total += input.tokens
	}
	if total > s.config.MaxContextTokens {
		writeError(w, 400, invalid("context limit exceeded", "input"))
		return 0, 0, "invalid_request"
	}
	delay := time.Duration(float64(m.TotalMS)*float64(time.Millisecond) + float64(total)*m.InputMSPerToken*float64(time.Millisecond))
	if err = s.wait(r.Context(), time.Until(start.Add(delay))); err != nil {
		if r.Context().Err() == context.Canceled {
			return total, 0, "canceled"
		}
		writeError(w, 504, &APIError{"embedding deadline exceeded", "server_error", "", "timeout"})
		return total, 0, "timeout"
	}
	items := make([]any, 0, len(inputs))
	for i, input := range inputs {
		if r.Context().Err() != nil {
			return total, 0, "canceled"
		}
		vector := embeddingVector(input.key, m.Seed, m.Name, dims)
		var value any = vector
		if request.Encoding == "base64" {
			bytes := make([]byte, dims*4)
			for j, v := range vector {
				binary.LittleEndian.PutUint32(bytes[j*4:], math.Float32bits(v))
			}
			value = base64.StdEncoding.EncodeToString(bytes)
		}
		items = append(items, map[string]any{"object": "embedding", "index": i, "embedding": value})
	}
	usage := map[string]int{"prompt_tokens": total, "total_tokens": total}
	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items, "model": m.Name, "usage": usage}); err != nil {
		return total, 0, "send_failure"
	}
	return total, 0, "success"
}
