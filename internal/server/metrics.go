package server

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

type metricKey struct{ model, scenario, outcome string }
type metricValue struct {
	count, input, output int64
	duration, first      float64
	firstCount           int64
}
type metrics struct {
	mu     sync.Mutex
	values map[metricKey]metricValue
	active int
}

func (m *metrics) activity(delta int) { m.mu.Lock(); m.active += delta; m.mu.Unlock() }
func (m *metrics) record(model, scenario, outcome string, input, output int, duration, first time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := metricKey{model, scenario, outcome}
	v := m.values[k]
	v.count++
	v.input += int64(input)
	v.output += int64(output)
	v.duration += duration.Seconds()
	if first > 0 {
		v.first += first.Seconds()
		v.firstCount++
	}
	m.values[k] = v
}
func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintf(w, "# TYPE mock_llm_active_requests gauge\nmock_llm_active_requests %d\n", m.active)
	for _, name := range []string{"requests_total", "input_tokens_total", "output_tokens_total", "duration_seconds_sum", "duration_seconds_count", "first_token_seconds_sum", "first_token_seconds_count"} {
		typ := "counter"
		if name == "duration_seconds_sum" || name == "duration_seconds_count" || name == "first_token_seconds_sum" || name == "first_token_seconds_count" {
			typ = "untyped"
		}
		fmt.Fprintf(w, "# TYPE mock_llm_%s %s\n", name, typ)
	}
	keys := make([]metricKey, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, k := range keys {
		v := m.values[k]
		labels := fmt.Sprintf("model=%q,scenario=%q,outcome=%q", k.model, k.scenario, k.outcome)
		fmt.Fprintf(w, "mock_llm_requests_total{%s} %d\nmock_llm_input_tokens_total{%s} %d\nmock_llm_output_tokens_total{%s} %d\nmock_llm_duration_seconds_sum{%s} %g\nmock_llm_duration_seconds_count{%s} %d\nmock_llm_first_token_seconds_sum{%s} %g\nmock_llm_first_token_seconds_count{%s} %d\n", labels, v.count, labels, v.input, labels, v.output, labels, v.duration, labels, v.count, labels, v.first, labels, v.firstCount)
	}
}
