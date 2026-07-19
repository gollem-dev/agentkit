// Package simple provides a general LLM-loop strategy: generate, run any tool
// calls, feed the results back, repeat until the model answers with no more tool
// calls. It does at most one LLM Generate per transition (checkpointing between
// rounds), which keeps replay cheap and avoids relying on intra-transition
// determinism (agentkit is at-least-once, D44).
package simple

import (
	"context"
	"encoding/json"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// Input is the launch input (required; empty prompt is rejected by Init).
type Input struct {
	Prompt string `json:"prompt"`
}

// Output is the Done output.
type Output struct {
	Texts []string `json:"texts"`
}

// Option configures the strategy.
type Option func(*config)

type config struct {
	role          agentkit.ModelRole
	systemPrompt  string
	maxIterations int
}

// WithRole sets the model role. Default: nil (the default model).
func WithRole(r agentkit.ModelRole) Option { return func(c *config) { c.role = r } }

// WithSystemPrompt sets the system prompt. Default: "".
func WithSystemPrompt(p string) Option { return func(c *config) { c.systemPrompt = p } }

// WithMaxIterations caps the number of LLM rounds. Default: 32. Exceeding it
// finalizes as Fail(strategy_error).
func WithMaxIterations(n int) Option { return func(c *config) { c.maxIterations = n } }

// Register builds and registers the simple strategy, returning a typed handle
// (the state type is hidden).
func Register(r *agentkit.Registry, name agentkit.AgentName, version int, opts ...Option) (agentkit.Agent[Input], error) {
	cfg := config{maxIterations: 32}
	for _, o := range opts {
		o(&cfg)
	}
	return agentkit.Register(r, name, version, &strategy{
		version:       version,
		role:          cfg.role,
		systemPrompt:  cfg.systemPrompt,
		maxIterations: cfg.maxIterations,
	})
}

// state is the checkpointed strategy state. It stores only serializable data
// (gollem.History JSON round-trips; FunctionCall/toolResult are plain structs).
type state struct {
	Prompt    string                `json:"prompt"`
	History   *gollem.History       `json:"history,omitempty"`
	Iteration int                   `json:"iteration"`
	Pending   []gollem.FunctionCall `json:"pending,omitempty"`   // tool calls to execute next (tools phase).
	Responses []toolResult          `json:"responses,omitempty"` // results to feed the next Generate.
}

type toolResult struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Data   map[string]any `json:"data,omitempty"`
	ErrMsg string         `json:"err,omitempty"`
}

type strategy struct {
	version       int
	role          agentkit.ModelRole
	systemPrompt  string
	maxIterations int
}

func (s *strategy) Version() int { return s.version }

func (s *strategy) Init(in Input) (state, error) {
	if in.Prompt == "" {
		return state{}, goerr.New("prompt is required")
	}
	return state{Prompt: in.Prompt}, nil
}

func (s *strategy) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	// Tools phase: execute the pending tool calls, then Continue to the next
	// Generate. Doing tools in their own transition keeps one Generate per step.
	if len(st.Pending) > 0 {
		for _, fc := range st.Pending {
			call := fc
			out, terr := sys.CallTool(ctx, call)
			tr := toolResult{ID: fc.ID, Name: fc.Name, Data: out}
			if terr != nil {
				tr.ErrMsg = terr.Error()
			}
			st.Responses = append(st.Responses, tr)
		}
		st.Pending = nil
		return st, agentkit.Continue(), nil
	}

	// Generate phase.
	if st.Iteration >= s.maxIterations {
		return st, agentkit.Fail(agentkit.FailureStrategyError, "max iterations exceeded"), nil
	}

	var input []gollem.Input
	if st.Iteration == 0 && st.History == nil {
		input = []gollem.Input{gollem.Text(st.Prompt)}
	} else {
		for _, tr := range st.Responses {
			fr := &gollem.FunctionResponse{ID: tr.ID, Name: tr.Name, Data: tr.Data}
			if tr.ErrMsg != "" {
				fr.Error = goerr.New(tr.ErrMsg)
			}
			input = append(input, fr)
		}
		st.Responses = nil
	}

	opts := []agentkit.GenerateOption{
		agentkit.WithTools(sys.Tools()...),
		agentkit.WithHistory(st.History),
	}
	if s.systemPrompt != "" {
		opts = append(opts, agentkit.WithSystemPrompt(s.systemPrompt))
	}
	if s.role != nil {
		opts = append(opts, agentkit.WithRole(s.role))
	}
	res, err := sys.Generate(ctx, input, opts...)
	if err != nil {
		return st, agentkit.Decision{}, err
	}
	st.History = res.History
	st.Iteration++

	if len(res.FunctionCalls) > 0 {
		st.Pending = st.Pending[:0]
		for _, fc := range res.FunctionCalls {
			st.Pending = append(st.Pending, *fc)
		}
		return st, agentkit.Continue(), nil
	}

	raw, err := json.Marshal(Output{Texts: res.Texts})
	if err != nil {
		return st, agentkit.Decision{}, goerr.Wrap(err, "marshal output")
	}
	return st, agentkit.Done(raw), nil
}

func (s *strategy) EncodeState(st state) ([]byte, error) { return json.Marshal(st) }

func (s *strategy) DecodeState(_ int, raw []byte) (state, error) {
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, goerr.Wrap(err, "decode state")
	}
	return st, nil
}
