package agentkit_test

import (
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/m-mizutani/gt"
)

func TestAddMetrics(t *testing.T) {
	t.Run("element-wise sum without mutating inputs", func(t *testing.T) {
		a := agentkit.Metrics{agentkit.MetricLLMCalls: 2, agentkit.MetricInputTokens: 10}
		b := agentkit.Metrics{agentkit.MetricLLMCalls: 3, agentkit.MetricToolCalls: 1}
		out := agentkit.AddMetrics(a, b)
		gt.Value(t, out[agentkit.MetricLLMCalls]).Equal(int64(5))
		gt.Value(t, out[agentkit.MetricInputTokens]).Equal(int64(10))
		gt.Value(t, out[agentkit.MetricToolCalls]).Equal(int64(1))
		// inputs unchanged
		gt.Value(t, a[agentkit.MetricLLMCalls]).Equal(int64(2))
		gt.Value(t, b[agentkit.MetricLLMCalls]).Equal(int64(3))
	})

	t.Run("both empty yields nil", func(t *testing.T) {
		gt.Nil(t, agentkit.AddMetrics(nil, nil))
		gt.Nil(t, agentkit.AddMetrics(agentkit.Metrics{}, nil))
	})

	t.Run("nil operand treated as empty", func(t *testing.T) {
		out := agentkit.AddMetrics(agentkit.Metrics{agentkit.MetricSteps: 1}, nil)
		gt.Value(t, out[agentkit.MetricSteps]).Equal(int64(1))
	})
}
