package agentkit

import (
	"context"

	"github.com/gollem-dev/gollem"
)

// EffectContext identifies "which Process's which transition produced this
// call". It can key an audit row.
type EffectContext struct {
	ProcessID ProcessID
	RootID    ProcessID // correlation id for the whole tree (reaches down to children).
	Agent     AgentName
	StateSeq  int // transition number.
	// Attempt reports prior attempts at this transition that did not commit, so
	// a middleware can tell a replayed effect from a first one.
	Attempt AttemptInfo
}

// This file defines the five next-chains the kernel calls out through: the
// strategy boundary (Init, Step) and the effects a strategy performs (Generate,
// CallTool, SpawnChild). A middleware receives the next handler and returns a
// replacement, so it can observe, rewrite the request, or refuse by not calling
// next. Registration is on the Kernel, so one registration covers every agent —
// see the WithXMiddleware options.
//
// An effect handler may be called more than once (a retry charges twice, which
// is the truth). StepHandler may not: see StepMiddleware.
//
// A request and a result are valid only for the duration of the handler call.
// Their payloads, maps and slices are shared with the kernel and with whatever
// runs next — nothing is deep-copied, because a middleware is expected to
// rewrite them. Handing one to a goroutine or a queue that outlives the call
// means copying what you need out of it first.
//
// Two properties shape everything below and are repeated on the declarations
// they apply to, because a reader arrives at one of those, not here.
//
// There is no static type safety over the type-erased payloads. A middleware
// runs across all agents, so it does not know any particular strategy's I or S;
// nothing the compiler can check follows from that. Touching a payload is an
// opaque operation and the responsibility is the middleware's. An operation
// that needs to be statically checked is, by that fact, not a cross-cutting
// concern and belongs in the strategy. Observation is the well-served case:
// InitInput[any] / StepState[any] always succeed.
//
// Effect middleware is the outermost layer of its syscall, wrapping the Limiter
// check, tool resolution and argument validation — so a refused call reaches it
// too, and a middleware that returns without calling next stops the call before
// any of them.
//
// See ADR-0012 for why this replaced the observation-only Observer hooks.

// --- Init -------------------------------------------------------------------

// InitRequest is one call to a strategy's Init. ProcessID is already minted
// when Init runs, so an audit record can be correlated with the Process that
// the call is about to create.
//
// An Init middleware fires for both entry points: Agent[I].Spawn (Parent is
// nil) and Agent[I].SpawnChild (Parent is the spawning transition). It also
// fires on an idempotent Spawn that ends up returning an existing Process — the
// initial state is built before the idempotency key is looked up — and in that
// case the ProcessID here is discarded.
type InitRequest struct {
	ProcessID ProcessID
	Agent     AgentName
	Parent    *EffectContext

	input any // Strategy[S, I]'s I.
}

// InitInput reads the launch input as I. A middleware runs for every agent, so
// ok == false simply means "not an agent this middleware knows about" — pass
// the request to next unchanged rather than treating it as an error.
func InitInput[I any](req *InitRequest) (I, bool) {
	v, ok := req.input.(I)
	return v, ok
}

// NewInitRequest returns a shallow copy of req with the input replaced. req is
// left untouched, so an outer middleware never sees its own request change.
//
// I is not constrained to the agent's input type — a kernel middleware spans
// every agent and cannot know it. Passing the wrong type compiles and surfaces
// as ErrInvalidRequest when the binding runs. Reading with InitInput first and
// writing the value back is the form that keeps the types aligned.
func NewInitRequest[I any](req *InitRequest, input I) *InitRequest {
	cp := *req
	cp.input = input
	return &cp
}

// InitResult is the initial strategy state an Init call produced.
type InitResult struct {
	state any // Strategy[S, I]'s S.
}

// NewInitResult builds a result without calling next, i.e. it replaces the
// strategy's own Init. As with NewInitRequest, S is unchecked here and a
// mismatch surfaces as ErrInvalidRequest when the state is encoded.
func NewInitResult[S any](state S) *InitResult { return &InitResult{state: state} }

// InitState reads the produced initial state as S.
func InitState[S any](res *InitResult) (S, bool) {
	v, ok := res.state.(S)
	return v, ok
}

// InitHandler builds the initial state for a Process.
type InitHandler func(ctx context.Context, req *InitRequest) (*InitResult, error)

// InitMiddleware wraps an InitHandler.
type InitMiddleware func(next InitHandler) InitHandler

// --- Step -------------------------------------------------------------------

// StepRequest is one call to a strategy's Step, between DecodeState and
// EncodeState.
//
// What a Step middleware observes is the Step CALL, not the transition's
// commit: the commit happens after the handler returns, outside this chain. A
// transition that fails to commit is re-run from the last checkpoint, so the
// middleware is called again — the at-least-once execution model is visible
// here exactly as it is.
type StepRequest struct {
	Effect EffectContext

	// Process is a copy taken for this transition, so writing to it changes
	// nothing that gets committed. It is a snapshot for deciding and recording,
	// not a way to edit the row.
	Process *Process

	// Sys is the syscall surface handed to Step. It may be replaced; Syscalls is
	// sealed against outside implementations but can be wrapped by embedding it
	// (type counting struct{ agentkit.Syscalls }) and overriding what you need.
	Sys Syscalls

	state any // the decoded S.
}

// StepState reads the transition's input state as S. As with InitInput,
// ok == false means the request belongs to another agent.
func StepState[S any](req *StepRequest) (S, bool) {
	v, ok := req.state.(S)
	return v, ok
}

// NewStepRequest returns a shallow copy of req with the state replaced. req is
// left untouched. S is unchecked here (see NewInitRequest); a mismatch surfaces
// as ErrInvalidRequest when the strategy's Step is called.
func NewStepRequest[S any](req *StepRequest, state S) *StepRequest {
	cp := *req
	cp.state = state
	return &cp
}

// StepResult is what one Step produced: the next state and the Decision.
//
// Both are type-erased for the same reason: a kernel middleware runs across
// every agent and knows neither S nor the strategy's output type O. Read them
// with ResultState and ResultDecision, and build a replacement with
// NewStepResult.
type StepResult struct {
	state any // S.
	dec   decision
}

// NewStepResult builds a result without calling next, i.e. it decides the
// transition in place of the strategy. S and O are unchecked here (see
// NewInitRequest); a mismatch surfaces as ErrInvalidRequest when the state is
// encoded or the Done output is.
func NewStepResult[S, O any](state S, dec Decision[O]) *StepResult {
	return &StepResult{state: state, dec: dec.erase()}
}

// ResultState reads the post-transition state as S.
func ResultState[S any](res *StepResult) (S, bool) {
	v, ok := res.state.(S)
	return v, ok
}

// ResultDecision reads the Decision as Decision[O]. ok == false means the
// result belongs to an agent whose output type is not O — for every kind, not
// only for a Done.
//
// Unlike StepState and friends, O must be the agent's exact output type:
// Decision carries its own type witness, so ResultDecision[any] does NOT match
// an agent whose O is something else. To branch on what a transition decided
// without naming O, use DecisionKindOf.
func ResultDecision[O any](res *StepResult) (Decision[O], bool) {
	return restore[O](res.dec)
}

// DecisionKindOf reports what a result decided without naming O, for a
// middleware that only needs to branch on continue/suspend/done/fail.
func DecisionKindOf(res *StepResult) DecisionKind { return res.dec.kind }

// StepHandler runs one transition of a strategy.
type StepHandler func(ctx context.Context, req *StepRequest) (*StepResult, error)

// StepMiddleware wraps a StepHandler.
//
// Unlike the effect middleware, it must call next AT MOST ONCE. A Step's side
// effects — spawned children, emitted events, metrics — accumulate per
// transition rather than per call, so a second attempt would commit the first
// attempt's effects together with its own state and Decision. The second call
// returns ErrInvalidRequest rather than doing that quietly.
//
// To decide a transition without running the strategy, do not call next at all
// and return a StepResult built with NewStepResult.
type StepMiddleware func(next StepHandler) StepHandler

// --- Generate ---------------------------------------------------------------

// GenerateRequest carries everything one Generate needs. Every field is a
// concrete type, so a middleware assigns to them directly. Effect is filled in
// by the kernel and is not read back from here.
type GenerateRequest struct {
	Effect       EffectContext
	Input        []gollem.Input
	Role         ModelRole
	History      *gollem.History
	SystemPrompt string
	Tools        []gollem.Tool
	Schema       *gollem.Parameter
	LLMOptions   []gollem.GenerateOption
}

// GenerateHandler performs one LLM call.
type GenerateHandler func(ctx context.Context, req *GenerateRequest) (*GenerateResult, error)

// GenerateMiddleware wraps a GenerateHandler.
type GenerateMiddleware func(next GenerateHandler) GenerateHandler

// --- CallTool ---------------------------------------------------------------

// ToolCallRequest carries one tool call, before the tool is resolved. Rewriting
// Call.Name selects a different tool; rewriting Call.Arguments changes what is
// validated and executed.
type ToolCallRequest struct {
	Effect EffectContext
	Call   gollem.FunctionCall
}

// ToolCallHandler executes one tool call.
type ToolCallHandler func(ctx context.Context, req *ToolCallRequest) (map[string]any, error)

// ToolCallMiddleware wraps a ToolCallHandler.
//
// It can refuse a call fail-closed, and unlike an observation hook it runs
// before the tool does. It is a real chokepoint for calls made through
// Syscalls.CallTool — but not the only path to a tool: a strategy holding a
// gollem.Tool value can call Run on it directly. Enforcement still belongs
// inside Run; this is not an authorization gate.
type ToolCallMiddleware func(next ToolCallHandler) ToolCallHandler

// --- SpawnChild -------------------------------------------------------------

// SpawnRequest is one SpawnChild. The launch options are resolved into fields
// so a middleware can read them; WithIdempotencyKey is rejected before the
// chain runs because it is never valid on a child.
type SpawnRequest struct {
	Effect   EffectContext
	Agent    AgentName
	Metadata map[string]string
	Subject  *SubjectRef

	// OnCommit registers fn to be called exactly once with this TRANSITION's
	// commit outcome: nil when the transition committed, non-nil when it did
	// not.
	//
	// The scope is the transition, not the child, and the distinction is not
	// pedantic: registering before calling next means fn still fires with nil if
	// next failed but the transition went on to commit anyway — no child exists
	// in that case. Register after a successful next to bind fn to a child that
	// was really buffered.
	//
	// fn runs after the commit, outside the transition, so a panic in it cannot
	// become a transition error; it is recovered and logged. A nil fn is
	// ignored.
	OnCommit func(fn func(err error))

	input any // the child strategy's I.
}

// SpawnInput reads the child's launch input as I.
func SpawnInput[I any](req *SpawnRequest) (I, bool) {
	v, ok := req.input.(I)
	return v, ok
}

// NewSpawnRequest returns a shallow copy of req with the input replaced. req is
// left untouched. I is unchecked here (see NewInitRequest); a mismatch surfaces
// as ErrInvalidRequest when the child's Init runs.
func NewSpawnRequest[I any](req *SpawnRequest, input I) *SpawnRequest {
	cp := *req
	cp.input = input
	return &cp
}

// SpawnHandler buffers one child Process into the transition commit and returns
// its freshly minted id. The child is not persisted until the transition
// commits.
type SpawnHandler func(ctx context.Context, req *SpawnRequest) (ProcessID, error)

// SpawnMiddleware wraps a SpawnHandler.
type SpawnMiddleware func(next SpawnHandler) SpawnHandler

// --- registration -----------------------------------------------------------

// WithInitMiddleware adds Init middleware. Repeatable; the first registered is
// the outermost. A nil element makes New return ErrInvalidConfig.
func WithInitMiddleware(mw ...InitMiddleware) KernelOption {
	return func(c *kernelConfig) { c.initMW = append(c.initMW, mw...) }
}

// WithStepMiddleware adds Step middleware (same ordering and nil rules).
func WithStepMiddleware(mw ...StepMiddleware) KernelOption {
	return func(c *kernelConfig) { c.stepMW = append(c.stepMW, mw...) }
}

// WithGenerateMiddleware adds Generate middleware (same ordering and nil rules).
func WithGenerateMiddleware(mw ...GenerateMiddleware) KernelOption {
	return func(c *kernelConfig) { c.generateMW = append(c.generateMW, mw...) }
}

// WithToolCallMiddleware adds CallTool middleware (same ordering and nil rules).
func WithToolCallMiddleware(mw ...ToolCallMiddleware) KernelOption {
	return func(c *kernelConfig) { c.toolCallMW = append(c.toolCallMW, mw...) }
}

// WithSpawnMiddleware adds SpawnChild middleware (same ordering and nil rules).
func WithSpawnMiddleware(mw ...SpawnMiddleware) KernelOption {
	return func(c *kernelConfig) { c.spawnMW = append(c.spawnMW, mw...) }
}

// --- chaining ---------------------------------------------------------------

// The five function types are distinct, so a single generic chain helper cannot
// express them; each is the same loop. A middleware that returns a nil handler
// yields nil, which the call site reports as ErrInvalidConfig rather than
// letting it panic.

func chainInit(mws []InitMiddleware, base InitHandler) InitHandler {
	h := base
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
		if h == nil {
			return nil
		}
	}
	return h
}

func chainStep(mws []StepMiddleware, base StepHandler) StepHandler {
	h := base
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
		if h == nil {
			return nil
		}
	}
	return h
}

func chainGenerate(mws []GenerateMiddleware, base GenerateHandler) GenerateHandler {
	h := base
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
		if h == nil {
			return nil
		}
	}
	return h
}

func chainToolCall(mws []ToolCallMiddleware, base ToolCallHandler) ToolCallHandler {
	h := base
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
		if h == nil {
			return nil
		}
	}
	return h
}

func chainSpawn(mws []SpawnMiddleware, base SpawnHandler) SpawnHandler {
	h := base
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
		if h == nil {
			return nil
		}
	}
	return h
}
