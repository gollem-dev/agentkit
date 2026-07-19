// Package planexec provides a plan/execute/replan strategy: an LLM planner
// decomposes the prompt into parallel tasks, each task runs as a child Process
// (any Agent[T], adapted via makeInput), the results are collected, and the
// planner decides to iterate or finalize. A summarizer produces the final
// answer. It does at most one LLM Generate per transition (checkpointing
// between phases via the Phase field), which keeps replay cheap and avoids
// relying on intra-transition determinism (agentkit is at-least-once, D44).
//
// The one deliberate exception is E24: when a plan response is not valid JSON,
// the plan transition issues a single in-transition correction Generate before
// giving up, rather than failing the whole transition and relying on backoff.
package planexec

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// TaskSpec is one task the planner emits (planexec public type).
type TaskSpec struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

// Input is planexec's own launch input (required; empty prompt is rejected by Init).
type Input struct {
	Prompt string `json:"prompt"`
}

// Output is the Done output.
type Output struct {
	Summary []string     `json:"summary"`
	Tasks   []TaskResult `json:"tasks"`
}

// TaskResult is one task's outcome. Output carries the child's Output verbatim
// on success (its interpretation is the caller's, D40).
type TaskResult struct {
	Title     string                 `json:"title"`
	Status    agentkit.ProcessStatus `json:"status"`
	ProcessID agentkit.ProcessID     `json:"process_id"`
	Output    []byte                 `json:"output,omitempty"`
}

// Roles are exported as package variables (identity binding, D32). Unbound roles
// fall back to the default model.
var (
	RolePlanner    = agentkit.DefineModelRole("planexec.planner")
	RoleSummarizer = agentkit.DefineModelRole("planexec.summarizer")
)

// Option configures the strategy.
type Option func(*config)

type config struct {
	systemPrompt     string
	maxRounds        int
	maxParallelTasks int
}

// WithSystemPrompt sets the system prompt for planner/summarizer Generates. Default: "".
func WithSystemPrompt(p string) Option { return func(c *config) { c.systemPrompt = p } }

// WithMaxRounds caps the plan/replan rounds. Default: 3. Reaching it forces finalize.
func WithMaxRounds(n int) Option { return func(c *config) { c.maxRounds = n } }

// WithMaxParallelTasks caps tasks per round (reflected as the plan schema maxItems).
// Default: 5.
func WithMaxParallelTasks(n int) Option { return func(c *config) { c.maxParallelTasks = n } }

// Register builds and registers the planexec strategy. It is generic over the
// task agent's input type T: makeInput adapts each planned TaskSpec into the
// child input T, so any Agent[T] can be a task (D50). taskAgent's zero value or
// a nil makeInput is rejected (ErrInvalidAgentDef).
func Register[T any](r *agentkit.Registry, name agentkit.AgentName, version int,
	taskAgent agentkit.Agent[T], makeInput func(TaskSpec) (T, error),
	opts ...Option) (agentkit.Agent[Input], error) {
	if taskAgent.Name() == "" || makeInput == nil {
		return agentkit.Agent[Input]{}, goerr.Wrap(agentkit.ErrInvalidAgentDef,
			"planexec: taskAgent and makeInput are required",
			goerr.V("name", name), goerr.V("hasMakeInput", makeInput != nil))
	}
	cfg := config{maxRounds: 3, maxParallelTasks: 5}
	for _, o := range opts {
		o(&cfg)
	}
	return agentkit.Register(r, name, version, &strategy[T]{
		version:          version,
		taskAgent:        taskAgent,
		makeInput:        makeInput,
		systemPrompt:     cfg.systemPrompt,
		maxRounds:        cfg.maxRounds,
		maxParallelTasks: cfg.maxParallelTasks,
	})
}

const (
	phasePlan     = "plan"
	phaseCollect  = "collect"
	phaseReplan   = "replan"
	phaseFinalize = "finalize"

	actionContinue = "continue"
	actionFinalize = "finalize"
)

// state is the checkpointed strategy state; only serializable data.
type state struct {
	Phase          string            `json:"phase"`
	Round          int               `json:"round"`
	Prompt         string            `json:"prompt"`
	Tasks          []TaskResult      `json:"tasks,omitempty"`   // accumulated across rounds.
	Current        []TaskResult      `json:"current,omitempty"` // this round's spawned tasks (Title+ProcessID, pending).
	RoundKey       agentkit.AwaitKey `json:"round_key,omitempty"`
	PlannerHistory *gollem.History   `json:"planner_history,omitempty"` // planner conversation continuity.
}

type strategy[T any] struct {
	version          int
	taskAgent        agentkit.Agent[T]
	makeInput        func(TaskSpec) (T, error)
	systemPrompt     string
	maxRounds        int
	maxParallelTasks int
}

func (s *strategy[T]) Version() int { return s.version }

func (s *strategy[T]) Init(in Input) (state, error) {
	if in.Prompt == "" {
		return state{}, goerr.New("prompt is required")
	}
	return state{Phase: phasePlan, Round: 1, Prompt: in.Prompt}, nil
}

func (s *strategy[T]) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	switch st.Phase {
	case phasePlan:
		return s.stepPlan(ctx, sys, st)
	case phaseCollect:
		return s.stepCollect(sys, st)
	case phaseReplan:
		return s.stepReplan(ctx, sys, st)
	case phaseFinalize:
		return s.stepFinalize(ctx, sys, st)
	default:
		return st, agentkit.Decision{}, goerr.New("unknown phase", goerr.V("phase", st.Phase))
	}
}

type planResult struct {
	Tasks []TaskSpec `json:"tasks"`
}

// stepPlan generates the task list, spawns a child per task, and suspends on the
// children. E24: one in-transition correction Generate on a JSON parse failure.
func (s *strategy[T]) stepPlan(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	input := []gollem.Input{gollem.Text(s.planPrompt(st))}
	opts := s.plannerOptions(st, s.planSchema())

	res, err := sys.Generate(ctx, input, opts...)
	if err != nil {
		return st, agentkit.Decision{}, err
	}
	var pr planResult
	if perr := parseJSON(res.Texts, &pr); perr != nil {
		// E24: one correction attempt in the same transition, then Fail. We do not
		// return the parse error (which would trigger backoff/retry_exhausted); the
		// spec wants a deterministic strategy_error after a single correction.
		corr := append([]gollem.Input{}, input...)
		corr = append(corr, gollem.Text("Your previous response was not valid JSON matching the required schema. Respond ONLY with the JSON object, no prose."))
		res2, err2 := sys.Generate(ctx, corr, opts...)
		if err2 != nil {
			return st, agentkit.Decision{}, err2
		}
		if perr2 := parseJSON(res2.Texts, &pr); perr2 != nil {
			return st, agentkit.Fail(agentkit.FailureStrategyError, "plan JSON parse failed after correction: "+perr2.Error()), nil
		}
		res = res2
	}
	st.PlannerHistory = res.History

	if len(pr.Tasks) == 0 {
		// Nothing to run this round; let the planner decide continue/finalize.
		st.Phase = phaseReplan
		return st, agentkit.Continue(), nil
	}

	var ids []agentkit.ProcessID
	st.Current = st.Current[:0]
	for _, spec := range pr.Tasks {
		in, merr := s.makeInput(spec)
		if merr != nil {
			return st, agentkit.Fail(agentkit.FailureStrategyError, "makeInput: "+merr.Error()), nil
		}
		// E23: an unregistered task agent yields ErrUnknownAgent here; surface it as
		// a deterministic strategy_error rather than a retry loop.
		id, serr := s.taskAgent.SpawnChild(ctx, sys, in)
		if serr != nil {
			return st, agentkit.Fail(agentkit.FailureStrategyError, "spawn task: "+serr.Error()), nil
		}
		ids = append(ids, id)
		st.Current = append(st.Current, TaskResult{Title: spec.Title, ProcessID: id, Status: agentkit.ProcessPending})
	}
	st.RoundKey = agentkit.AwaitKey(fmt.Sprintf("tasks:%d", st.Round))
	st.Phase = phaseCollect
	return st, agentkit.Suspend(agentkit.WaitChildren(st.RoundKey, ids...)), nil
}

// stepCollect folds the finished children into the accumulated task results.
func (s *strategy[T]) stepCollect(sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	aw, ok := sys.Await(st.RoundKey)
	if !ok || aw.Status != agentkit.AwaitResponded {
		return st, agentkit.Decision{}, goerr.New("children await not responded", goerr.V("key", st.RoundKey))
	}
	byID := make(map[agentkit.ProcessID]agentkit.ChildResult, len(aw.Results))
	for _, r := range aw.Results {
		byID[r.ProcessID] = r
	}
	for _, tr := range st.Current {
		if r, found := byID[tr.ProcessID]; found {
			tr.Status = r.Status
			tr.Output = r.Output
		}
		st.Tasks = append(st.Tasks, tr)
	}
	st.Current = nil
	st.Phase = phaseReplan
	return st, agentkit.Continue(), nil
}

type replanResult struct {
	Action string `json:"action"`
}

// stepReplan asks the planner whether to iterate or finalize.
func (s *strategy[T]) stepReplan(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	input := []gollem.Input{gollem.Text(s.replanPrompt(st))}
	opts := s.plannerOptions(st, replanSchema())

	res, err := sys.Generate(ctx, input, opts...)
	if err != nil {
		return st, agentkit.Decision{}, err
	}
	st.PlannerHistory = res.History

	var rr replanResult
	if perr := parseJSON(res.Texts, &rr); perr != nil {
		// A malformed replan decision cannot safely spawn another round, so finalize
		// with what we have. This is a terminal, in-strategy choice (not a framework
		// deviation) and keeps a flaky planner from hanging the Process.
		rr.Action = actionFinalize
	}
	if rr.Action == actionFinalize || st.Round >= s.maxRounds {
		st.Phase = phaseFinalize
		return st, agentkit.Continue(), nil
	}
	st.Round++
	st.Phase = phasePlan
	return st, agentkit.Continue(), nil
}

// stepFinalize summarizes the accumulated results into the Done output.
func (s *strategy[T]) stepFinalize(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	input := []gollem.Input{gollem.Text(s.finalizePrompt(st))}
	opts := []agentkit.GenerateOption{agentkit.WithRole(RoleSummarizer)}
	if s.systemPrompt != "" {
		opts = append(opts, agentkit.WithSystemPrompt(s.systemPrompt))
	}
	res, err := sys.Generate(ctx, input, opts...)
	if err != nil {
		return st, agentkit.Decision{}, err
	}
	raw, merr := json.Marshal(Output{Summary: res.Texts, Tasks: st.Tasks})
	if merr != nil {
		return st, agentkit.Decision{}, goerr.Wrap(merr, "marshal output")
	}
	return st, agentkit.Done(raw), nil
}

func (s *strategy[T]) plannerOptions(st state, schema *gollem.Parameter) []agentkit.GenerateOption {
	opts := []agentkit.GenerateOption{agentkit.WithRole(RolePlanner), agentkit.WithSchema(schema)}
	if s.systemPrompt != "" {
		opts = append(opts, agentkit.WithSystemPrompt(s.systemPrompt))
	}
	if st.PlannerHistory != nil {
		opts = append(opts, agentkit.WithHistory(st.PlannerHistory))
	}
	return opts
}

func (s *strategy[T]) planPrompt(st state) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n\n", st.Prompt)
	if len(st.Tasks) > 0 {
		b.WriteString("Work already completed in previous rounds:\n")
		b.WriteString(renderTasks(st.Tasks))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Break the remaining work into at most %d independent tasks that can run in parallel. "+
		"Respond as JSON matching the schema. If nothing remains, return an empty task list.", s.maxParallelTasks)
	return b.String()
}

func (s *strategy[T]) replanPrompt(st state) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n\n", st.Prompt)
	b.WriteString("Results so far:\n")
	b.WriteString(renderTasks(st.Tasks))
	fmt.Fprintf(&b, "\nRound %d of %d. Decide whether more task rounds are needed. "+
		"Respond as JSON with action \"continue\" or \"finalize\".", st.Round, s.maxRounds)
	return b.String()
}

func (s *strategy[T]) finalizePrompt(st state) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n\n", st.Prompt)
	b.WriteString("Task results:\n")
	b.WriteString(renderTasks(st.Tasks))
	b.WriteString("\nWrite a concise final answer for the goal based on these results.")
	return b.String()
}

func renderTasks(tasks []TaskResult) string {
	var b strings.Builder
	for i, t := range tasks {
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, t.Status, t.Title)
		if len(t.Output) > 0 {
			fmt.Fprintf(&b, " -> %s", string(t.Output))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *strategy[T]) planSchema() *gollem.Parameter {
	maxItems := s.maxParallelTasks
	return &gollem.Parameter{
		Type:     gollem.TypeObject,
		Required: true,
		Properties: map[string]*gollem.Parameter{
			"tasks": {
				Type:        gollem.TypeArray,
				Description: "Independent tasks to execute in parallel.",
				Required:    true,
				MaxItems:    &maxItems,
				Items: &gollem.Parameter{
					Type:     gollem.TypeObject,
					Required: true,
					Properties: map[string]*gollem.Parameter{
						"title":  {Type: gollem.TypeString, Description: "A short task title.", Required: true},
						"prompt": {Type: gollem.TypeString, Description: "The prompt handed to the task agent.", Required: true},
					},
				},
			},
		},
	}
}

func replanSchema() *gollem.Parameter {
	return &gollem.Parameter{
		Type:     gollem.TypeObject,
		Required: true,
		Properties: map[string]*gollem.Parameter{
			"action": {
				Type:        gollem.TypeString,
				Description: "Whether to run another task round or finalize.",
				Required:    true,
				Enum:        []string{actionContinue, actionFinalize},
			},
		},
	}
}

func parseJSON(texts []string, v any) error {
	joined := strings.TrimSpace(strings.Join(texts, ""))
	if joined == "" {
		return goerr.New("empty structured response")
	}
	if err := json.Unmarshal([]byte(joined), v); err != nil {
		return goerr.Wrap(err, "unmarshal structured response")
	}
	return nil
}

func (s *strategy[T]) EncodeState(st state) ([]byte, error) { return json.Marshal(st) }

func (s *strategy[T]) DecodeState(_ int, raw []byte) (state, error) {
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, goerr.Wrap(err, "decode state")
	}
	return st, nil
}
