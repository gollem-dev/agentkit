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
		a := agentkit.Metrics{
			InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: 3, CacheCreationInputTokens: 4,
			LLMCalls: 5, ToolCalls: 6, Steps: 7, Spawns: 8,
		}
		out := agentkit.AddMetrics(a, a)
		gt.Value(t, out).Equal(agentkit.Metrics{
			InputTokens: 2, OutputTokens: 4, CacheReadInputTokens: 6, CacheCreationInputTokens: 8,
			LLMCalls: 10, ToolCalls: 12, Steps: 14, Spawns: 16,
		})
	})

	t.Run("zero value is the identity", func(t *testing.T) {
		a := agentkit.Metrics{Steps: 1}
		gt.Value(t, agentkit.AddMetrics(a, agentkit.Metrics{})).Equal(a)
		gt.Value(t, agentkit.AddMetrics(agentkit.Metrics{}, a)).Equal(a)
		gt.Value(t, agentkit.AddMetrics(agentkit.Metrics{}, agentkit.Metrics{})).Equal(agentkit.Metrics{})
	})

	t.Run("caller-defined counters are summed key-wise", func(t *testing.T) {
		fetched := agentkit.DefineMetricKey("test.docs_fetched")
		credits := agentkit.DefineMetricKey("test.credits")
		a := agentkit.Metrics{}.WithCount(fetched, 2)
		b := agentkit.Metrics{}.WithCount(fetched, 3).WithCount(credits, 7)

		out := agentkit.AddMetrics(a, b)
		gt.Value(t, out.Count(fetched)).Equal(int64(5))
		gt.Value(t, out.Count(credits)).Equal(int64(7))
		// inputs unchanged
		gt.Value(t, a.Count(fetched)).Equal(int64(2))
		gt.Value(t, b.Count(credits)).Equal(int64(7))
	})

	// Empty must be nil rather than a zero-length map, or two Metrics carrying
	// the same numbers stop being DeepEqual depending on how each was built.
	t.Run("a sum with no caller-defined counter equals the plain zero value", func(t *testing.T) {
		k := agentkit.DefineMetricKey("test.transient")
		emptied := agentkit.Metrics{Steps: 1}.WithCount(k, 4).WithCount(k, 0)
		gt.Value(t, emptied).Equal(agentkit.Metrics{Steps: 1})
		gt.Value(t, agentkit.AddMetrics(emptied, agentkit.Metrics{})).Equal(agentkit.Metrics{Steps: 1})
	})
}

func TestMetricKey(t *testing.T) {
	// Identity is the name, not the pointer: a Repository rebuilds a key from
	// the name it stored, and that key has to match the one the caller holds.
	t.Run("the same name is the same key", func(t *testing.T) {
		a := agentkit.DefineMetricKey("test.same")
		b := agentkit.DefineMetricKey("test.same")
		gt.Value(t, a).Equal(b)
		gt.Value(t, a.String()).Equal("test.same")

		m := agentkit.Metrics{}.WithCount(a, 1)
		m = m.WithCount(b, m.Count(b)+1)
		gt.Value(t, m.Count(a)).Equal(int64(2))
		gt.Value(t, m.Counters()).Equal(map[agentkit.MetricKey]int64{a: 2})
	})

	t.Run("different names are different keys", func(t *testing.T) {
		a := agentkit.DefineMetricKey("test.a")
		b := agentkit.DefineMetricKey("test.b")
		m := agentkit.Metrics{}.WithCount(a, 1).WithCount(b, 2)
		gt.Value(t, m.Count(a)).Equal(int64(1))
		gt.Value(t, m.Count(b)).Equal(int64(2))
	})

	t.Run("an empty name is not a key", func(t *testing.T) {
		gt.Value(t, agentkit.DefineMetricKey("")).Nil()
	})
}

func TestMetricsCounters(t *testing.T) {
	docs := agentkit.DefineMetricKey("test.docs")

	t.Run("an unset counter reads zero", func(t *testing.T) {
		gt.Value(t, agentkit.Metrics{}.Count(docs)).Equal(int64(0))
		gt.Value(t, agentkit.Metrics{}.Count(nil)).Equal(int64(0))
		gt.Value(t, agentkit.Metrics{}.Counters()).Nil()
	})

	t.Run("WithCount sets rather than adds", func(t *testing.T) {
		m := agentkit.Metrics{}.WithCount(docs, 5).WithCount(docs, 2)
		gt.Value(t, m.Count(docs)).Equal(int64(2))
	})

	t.Run("WithCount zero drops the counter", func(t *testing.T) {
		m := agentkit.Metrics{}.WithCount(docs, 5).WithCount(docs, 0)
		gt.Value(t, m.Counters()).Nil()
		gt.Value(t, m).Equal(agentkit.Metrics{})
	})

	t.Run("WithCount with a nil key changes nothing", func(t *testing.T) {
		m := agentkit.Metrics{}.WithCount(docs, 5)
		gt.Value(t, m.WithCount(nil, 9)).Equal(m)
	})

	t.Run("WithCount does not mutate the receiver", func(t *testing.T) {
		before := agentkit.Metrics{}.WithCount(docs, 5)
		_ = before.WithCount(agentkit.DefineMetricKey("test.other"), 1)
		gt.Value(t, before.Counters()).Equal(map[agentkit.MetricKey]int64{docs: 5})
	})

	t.Run("Counters returns a copy", func(t *testing.T) {
		m := agentkit.Metrics{}.WithCount(docs, 5)
		got := m.Counters()
		got[docs] = 999
		delete(got, docs)
		gt.Value(t, m.Count(docs)).Equal(int64(5))
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
			InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: 3, CacheCreationInputTokens: 4,
			LLMCalls: 5, ToolCalls: 6, Steps: 7, Spawns: 8,
		})).NoError(t)
		gt.Value(t, string(b)).Equal(
			`{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4,` +
				`"llm_calls":5,"tool_calls":6,"steps":7,"spawns":8}`)
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

// Caller-defined counters are persisted like the kernel's own, so their wire
// form is part of the Repository contract too.
func TestMetricsJSONCarriesCallerDefinedCounters(t *testing.T) {
	docs := agentkit.DefineMetricKey("test.docs_fetched")

	t.Run("marshal nests them under custom", func(t *testing.T) {
		b := gt.R1(json.Marshal(agentkit.Metrics{LLMCalls: 3}.WithCount(docs, 7))).NoError(t)
		gt.Value(t, string(b)).Equal(`{"llm_calls":3,"custom":{"test.docs_fetched":7}}`)
	})

	t.Run("marshal omits custom when there is none", func(t *testing.T) {
		b := gt.R1(json.Marshal(agentkit.Metrics{LLMCalls: 3})).NoError(t)
		gt.Value(t, string(b)).Equal(`{"llm_calls":3}`)
	})

	t.Run("round trip", func(t *testing.T) {
		want := agentkit.Metrics{InputTokens: 10}.WithCount(docs, 7).
			WithCount(agentkit.DefineMetricKey("test.credits"), 2)
		b := gt.R1(json.Marshal(want)).NoError(t)

		var got agentkit.Metrics
		gt.NoError(t, json.Unmarshal(b, &got))
		gt.Value(t, got).Equal(want)
	})

	// A snapshot written before caller-defined counters existed has no custom
	// key at all, and must load as "none" rather than failing.
	t.Run("a snapshot without custom reads as none", func(t *testing.T) {
		var m agentkit.Metrics
		gt.NoError(t, json.Unmarshal([]byte(`{"llm_calls":3}`), &m))
		gt.Value(t, m.Count(docs)).Equal(int64(0))
		gt.Value(t, m).Equal(agentkit.Metrics{LLMCalls: 3})
	})

	// No key a caller can hold addresses the empty name, so an entry under it
	// would only be an entry nothing can read.
	t.Run("an empty name is dropped", func(t *testing.T) {
		var m agentkit.Metrics
		gt.NoError(t, json.Unmarshal([]byte(`{"custom":{"":4,"test.docs_fetched":7}}`), &m))
		gt.Value(t, m).Equal(agentkit.Metrics{}.WithCount(docs, 7))
	})
}
