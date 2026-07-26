package agentkit_test

import (
	"encoding/json"
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

func TestAddMetrics(t *testing.T) {
	t.Run("field-wise sum without mutating inputs", func(t *testing.T) {
		a := agentkit.Metrics{LLMCalls: 2, InputTokens: 10}
		b := agentkit.Metrics{LLMCalls: 3, ToolCalls: 1}
		out := agentkit.AddMetrics(a, b)
		gt.Value(t, out.LLMCalls).Equal(int64(5))
		gt.Value(t, out.InputTokens).Equal(int64(10))
		gt.Value(t, out.ToolCalls).Equal(int64(1))
		// inputs unchanged
		gt.Value(t, a.LLMCalls).Equal(int64(2))
		gt.Value(t, b.LLMCalls).Equal(int64(3))
	})

	t.Run("every counter is summed", func(t *testing.T) {
		a := agentkit.Metrics{InputTokens: 1, OutputTokens: 2, LLMCalls: 3, ToolCalls: 4, Steps: 5, Spawns: 6}
		out := agentkit.AddMetrics(a, a)
		gt.Value(t, out).Equal(agentkit.Metrics{
			InputTokens: 2, OutputTokens: 4, LLMCalls: 6, ToolCalls: 8, Steps: 10, Spawns: 12,
		})
	})

	t.Run("zero value is the identity", func(t *testing.T) {
		a := agentkit.Metrics{Steps: 1}
		gt.Value(t, agentkit.AddMetrics(a, agentkit.Metrics{})).Equal(a)
		gt.Value(t, agentkit.AddMetrics(agentkit.Metrics{}, a)).Equal(a)
		gt.Value(t, agentkit.AddMetrics(agentkit.Metrics{}, agentkit.Metrics{})).Equal(agentkit.Metrics{})
	})
}

// The struct replaced a map[Metric]int64 whose keys these tags reproduce. A
// Repository that stores a Process or an Await as JSON — both reference
// implementations do — must keep reading what it wrote before that change, so
// the wire form is part of the contract, not an implementation detail.
func TestMetricsJSONMatchesTheFormerMapKeys(t *testing.T) {
	t.Run("marshal omits zero counters", func(t *testing.T) {
		b := gt.R1(json.Marshal(agentkit.Metrics{InputTokens: 10, LLMCalls: 3})).NoError(t)
		gt.Value(t, string(b)).Equal(`{"input_tokens":10,"llm_calls":3}`)
	})

	t.Run("marshal names every counter", func(t *testing.T) {
		b := gt.R1(json.Marshal(agentkit.Metrics{
			InputTokens: 1, OutputTokens: 2, LLMCalls: 3, ToolCalls: 4, Steps: 5, Spawns: 6,
		})).NoError(t)
		gt.Value(t, string(b)).Equal(
			`{"input_tokens":1,"output_tokens":2,"llm_calls":3,"tool_calls":4,"steps":5,"spawns":6}`)
	})

	t.Run("unmarshal reads a snapshot written as a map", func(t *testing.T) {
		var m agentkit.Metrics
		gt.NoError(t, json.Unmarshal([]byte(`{"llm_calls":3,"steps":1}`), &m))
		gt.Value(t, m).Equal(agentkit.Metrics{LLMCalls: 3, Steps: 1})
	})

	t.Run("an empty object is the zero value", func(t *testing.T) {
		var m agentkit.Metrics
		gt.NoError(t, json.Unmarshal([]byte(`{}`), &m))
		gt.Value(t, m).Equal(agentkit.Metrics{})
	})

	// A Process that never ran an effect stored a nil map, which marshals as
	// null. The struct writes {} there instead, so the bytes differ — but the
	// old form still has to load.
	t.Run("null is the zero value", func(t *testing.T) {
		var m agentkit.Metrics
		gt.NoError(t, json.Unmarshal([]byte(`null`), &m))
		gt.Value(t, m).Equal(agentkit.Metrics{})
	})

	// The map type could hold a key outside the six. The kernel never wrote one,
	// but a snapshot carrying one must not fail to load — it is dropped.
	t.Run("an unknown counter is dropped, not an error", func(t *testing.T) {
		var m agentkit.Metrics
		gt.NoError(t, json.Unmarshal([]byte(`{"llm_calls":3,"cost_micro_usd":42}`), &m))
		gt.Value(t, m).Equal(agentkit.Metrics{LLMCalls: 3})
	})
}
