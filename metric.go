package agentkit

import "context"

// Metric is a measurement key. The Kernel measures usage; a Limiter decides
// whether to continue.
type Metric string

const (
	MetricInputTokens  Metric = "input_tokens"
	MetricOutputTokens Metric = "output_tokens"
	MetricLLMCalls     Metric = "llm_calls"
	MetricToolCalls    Metric = "tool_calls"
	MetricSteps        Metric = "steps"
	MetricSpawns       Metric = "spawns"
)

// Metrics is a set of measurements.
type Metrics map[Metric]int64

// add returns the element-wise sum of a and b without mutating either. A nil
// map is treated as empty.
func addMetrics(a, b Metrics) Metrics {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(Metrics, len(a)+len(b))
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

// Limiter decides whether a Process may continue. Measurement (Metrics) is the
// Kernel's job; the decision logic is the caller's closure. It is invoked just
// before each LLM/tool call and at transition boundaries. A nil return means
// continue; a non-nil return means stop — before-call it surfaces as
// ErrLimitExceeded to the strategy, at a transition boundary it finalizes the
// Process as failed(limit_exceeded) with the returned error's message.
//
// metrics is a snapshot of "committed cumulative (proc.Metrics) + what this run
// has accumulated so far". A nil Limiter means unlimited.
type Limiter func(ctx context.Context, proc *Process, metrics Metrics) error
