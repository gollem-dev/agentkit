package agentkit

import (
	"context"
	"log/slog"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
)

// Syscalls is the path by which a strategy (a user program) touches the outside
// world. It runs metering (Metrics) and Limiter checks and offers spawn, wait
// reads, and event emission. The implementation (a private struct) is assembled
// by the worker per claim. The naming is an OS metaphor: ProcessID()=getpid,
// Now()=clock, SpawnChild=fork.
//
// There is no effect journal and no approval gate. Generate/CallTool simply call
// gollem and accumulate Metrics. There is no operation label either — nothing is
// journaled, so no key is needed to identify a call.
//
// Generate, CallTool and SpawnChild each run through their middleware chain
// first, if the Kernel was given one. The chain is the outermost layer: it wraps
// the Limiter check, tool resolution and argument validation, and a middleware
// that returns without calling next stops the call before any of them.
type Syscalls interface {
	// --- execution context ---
	ProcessID() ProcessID
	RootID() ProcessID
	ParentID() (ProcessID, bool)
	Agent() AgentName
	Now() time.Time // current time (the Kernel's clock; testable via WithClock). Not deterministic.
	// Attempt reports prior attempts at THIS transition that did not commit, so
	// a strategy can tell a replay from a first run before acting. A zero value
	// means this is the first attempt.
	Attempt() AttemptInfo

	// --- LLM (via gollem; Limiter before, Metrics after) ---
	Tools() []gollem.Tool // the tools the ToolFactory built (to declare to the LLM).
	Generate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error)
	// SessionGenerate runs one LLM turn as part of the Process's managed
	// conversation: the runtime carries History across calls (and, once
	// committed, across steps and workers) and injects the claim's tools, so the
	// strategy threads neither by hand. It requires the agent to have been
	// registered with WithHistoryRepository; otherwise it returns
	// ErrHistoryNotConfigured rather than silently running without persistence.
	// To manage History yourself, use the primitive Generate with WithHistory.
	// See ADR-0017.
	SessionGenerate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error)
	// SessionHistory returns the managed conversation's current history (loading
	// the stored one on first use). Requires WithHistoryRepository, else
	// ErrHistoryNotConfigured.
	SessionHistory(ctx context.Context) (*gollem.History, error)

	// --- tool execution (Limiter before, Metrics after; no approval gate) ---
	CallTool(ctx context.Context, call gollem.FunctionCall) (map[string]any, error)

	// --- subagents ---
	// Child launch is via the typed handle (agent.SpawnChild(ctx, sys, input)).
	// The child insert is buffered into the transition commit (atomic creation;
	// no orphan, no dedup). The minted ProcessID is returned immediately.
	spawn(ctx context.Context, agent AgentName, input any, opts ...SpawnOption) (ProcessID, error)

	// --- waits ---
	Await(key AwaitKey) (*Await, bool) // reads from the snapshot loaded at transition start. Declaration is via Decision.

	// --- observation ---
	Emit(ctx context.Context, typ EventType, payload []byte) error // flushed on commit. Encoding is the caller's.
	Metrics() Metrics                                              // proc.Metrics (committed) + this run's accumulation.
}

// GenerateResult is the journalable/checkpointable result of a Generate. It is
// used instead of *gollem.Response because Response.Error is not JSON
// round-trippable; History is included so the strategy can fold it into its
// checkpointed state and pass it to the next Generate.
type GenerateResult struct {
	Texts         []string               `json:"texts"`
	Thoughts      []string               `json:"thoughts,omitempty"`
	FunctionCalls []*gollem.FunctionCall `json:"function_calls,omitempty"`
	InputTokens   int                    `json:"input_tokens"`
	OutputTokens  int                    `json:"output_tokens"`
	History       *gollem.History        `json:"history"` // session history after the call (save it, pass it next time).
}

// GenerateOption configures a Generate. Only input is required (D26). The
// options fill in a GenerateRequest, which is also what Generate middleware
// sees and may rewrite.
type GenerateOption func(*GenerateRequest)

// WithRole selects the model role. Omitting it (or nil) means the default model.
func WithRole(r ModelRole) GenerateOption {
	return func(req *GenerateRequest) { req.Role = r }
}

// WithHistory passes prior conversation history (to gollem.WithSessionHistory).
func WithHistory(h *gollem.History) GenerateOption {
	return func(req *GenerateRequest) { req.History = h }
}

// WithSystemPrompt sets the system prompt (to gollem.WithSessionSystemPrompt).
func WithSystemPrompt(p string) GenerateOption {
	return func(req *GenerateRequest) { req.SystemPrompt = p }
}

// WithTools declares tools to the LLM (execution goes through CallTool).
func WithTools(tools ...gollem.Tool) GenerateOption {
	return func(req *GenerateRequest) { req.Tools = append(req.Tools, tools...) }
}

// WithSchema requests JSON output against a schema (gollem
// WithSessionContentType(JSON) + WithSessionResponseSchema).
func WithSchema(p *gollem.Parameter) GenerateOption {
	return func(req *GenerateRequest) { req.Schema = p }
}

// WithLLMOptions passes gollem generate options (temperature, etc.) straight
// through. Note agentkit.GenerateOption and gollem.GenerateOption are distinct
// types (package-qualified); pass-through is via this option.
func WithLLMOptions(o ...gollem.GenerateOption) GenerateOption {
	return func(req *GenerateRequest) { req.LLMOptions = append(req.LLMOptions, o...) }
}

func (r *GenerateRequest) sessionOptions() []gollem.SessionOption {
	var opts []gollem.SessionOption
	if r.History != nil {
		opts = append(opts, gollem.WithSessionHistory(r.History))
	}
	if r.SystemPrompt != "" {
		opts = append(opts, gollem.WithSessionSystemPrompt(r.SystemPrompt))
	}
	if len(r.Tools) > 0 {
		opts = append(opts, gollem.WithSessionTools(r.Tools...))
	}
	if r.Schema != nil {
		opts = append(opts, gollem.WithSessionContentType(gollem.ContentTypeJSON), gollem.WithSessionResponseSchema(r.Schema))
	}
	return opts
}

// SpawnOption configures a Spawn / SpawnChild.
type SpawnOption func(*spawnConfig)

type spawnConfig struct {
	idempotencyKey    string
	hasIdempotencyKey bool
	subject           *SubjectRef
	metadata          map[string]string
}

// WithIdempotencyKey makes Spawn return the existing Process's ID if one already
// matches (not an error). Not usable on SpawnChild (child creation is buffered
// into the transition commit and has no dedup); specifying it there is ErrInvalidRequest.
func WithIdempotencyKey(key string) SpawnOption {
	return func(c *spawnConfig) { c.idempotencyKey = key; c.hasIdempotencyKey = true }
}

// WithSubject sets a turn-lock subject. An open Process holding the same subject
// makes Spawn return ErrSubjectBusy.
func WithSubject(ref SubjectRef) SpawnOption {
	return func(c *spawnConfig) { c.subject = &ref }
}

// WithMetadata sets Process.Metadata (infrastructure-facing scope for ToolFactory;
// not a credential — derive it server-side from a validated principal).
func WithMetadata(m map[string]string) SpawnOption {
	return func(c *spawnConfig) { c.metadata = m }
}

func newSpawnConfig(opts []SpawnOption) *spawnConfig {
	cfg := &spawnConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// RespondOption configures a Respond.
type RespondOption func(*respondConfig)

type respondConfig struct {
	respondedBy string
}

// WithRespondedBy records the responder (Await.RespondedBy) for audit. Optional.
func WithRespondedBy(id string) RespondOption {
	return func(c *respondConfig) { c.respondedBy = id }
}

func newRespondConfig(opts []RespondOption) *respondConfig {
	cfg := &respondConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// syscalls is the per-claim Syscalls implementation the worker assembles. There
// is one write per transition (the commit); there is no immediate effect write
// (no journal, D44). Run-time side effects (metrics, child creation, events)
// accumulate in buffers folded into the commit ChangeSet.
type syscalls struct {
	k          *Kernel
	proc       *Process
	seq        int // = proc.StateSeq + 1
	tools      []gollem.Tool
	toolByName map[string]gollem.Tool
	awaits     map[AwaitKey]*Await

	// hist is the claim-scoped committed History holder (shared across a claim's
	// transitions). sessWorking/sessStarted/sessDirty are this transition's
	// managed-conversation state (SessionGenerate/SessionHistory). See session.go
	// / ADR-0017.
	hist        *historyState
	sessWorking *gollem.History
	sessStarted bool
	sessDirty   bool

	// run accumulation (committed proc.Metrics is only the committed cumulative;
	// this run's share is folded on any successful Apply, D44).
	runMetrics Metrics

	// transition buffers (folded into the commit ChangeSet).
	pendingChildren  []*Process    // SpawnChild children (atomic insert on commit, D48).
	pendingEvents    []*Event      // Emit / await.created.
	pendingSpawnDone []func(error) // SpawnRequest.OnCommit callbacks (called by the worker after commit, #8).
}

func newSyscalls(k *Kernel, proc *Process, tools []gollem.Tool, hist *historyState) *syscalls {
	byName := make(map[string]gollem.Tool, len(tools))
	for _, t := range tools {
		byName[t.Spec().Name] = t
	}
	awaits := map[AwaitKey]*Await{}
	if list, err := k.repo.ListAwaits(context.Background(), proc.ID); err == nil {
		for _, aw := range list {
			awaits[aw.Key] = aw
		}
	}
	return &syscalls{k: k, proc: proc, seq: proc.StateSeq + 1, tools: tools, toolByName: byName, awaits: awaits, hist: hist}
}

func (s *syscalls) ProcessID() ProcessID { return s.proc.ID }
func (s *syscalls) RootID() ProcessID    { return s.proc.RootID }
func (s *syscalls) Agent() AgentName     { return s.proc.Agent }
func (s *syscalls) Now() time.Time       { return s.k.clock() }
func (s *syscalls) Tools() []gollem.Tool { return s.tools }

func (s *syscalls) ParentID() (ProcessID, bool) {
	if s.proc.ParentID == nil {
		return "", false
	}
	return *s.proc.ParentID, true
}

func (s *syscalls) Metrics() Metrics { return addMetrics(s.proc.Metrics, s.runMetrics) }

func (s *syscalls) addMetrics(m Metrics) { s.runMetrics = addMetrics(s.runMetrics, m) }

// notifySpawnDone calls every buffered OnCommit callback exactly once
// with the transition outcome (nil = the child was committed; non-nil = the
// transition did not commit), then clears them so a retry does not double-fire.
func (s *syscalls) notifySpawnDone(err error) {
	for _, done := range s.pendingSpawnDone {
		done(err)
	}
	s.pendingSpawnDone = nil
}

func (s *syscalls) Attempt() AttemptInfo {
	return AttemptInfo{Errors: s.proc.StepAttempts, UncleanReclaims: s.proc.UncleanReclaims}
}

func (s *syscalls) ec() EffectContext {
	return EffectContext{ProcessID: s.proc.ID, RootID: s.proc.RootID, Agent: s.proc.Agent,
		StateSeq: s.seq, Attempt: s.Attempt()}
}

// checkLimit runs the Limiter with the live snapshot (committed + this run).
func (s *syscalls) checkLimit(ctx context.Context) error {
	if s.k.limiter == nil {
		return nil
	}
	if err := s.k.limiter(ctx, s.proc, s.Metrics()); err != nil {
		return goerr.Wrap(ErrLimitExceeded, err.Error())
	}
	return nil
}

// Generate runs the middleware chain around generateBase. The chain is the
// outermost layer: a middleware sees calls the Limiter later refuses, and one
// that returns without calling next consumes neither quota nor metrics.
func (s *syscalls) Generate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error) {
	req := &GenerateRequest{Effect: s.ec(), Input: input}
	for _, o := range opts {
		o(req)
	}
	h := chainGenerate(s.k.generateMW, s.generateBase)
	if h == nil {
		return nil, goerr.Wrap(ErrInvalidConfig, "generate middleware returned a nil handler")
	}
	return h(ctx, req)
}

func (s *syscalls) generateBase(ctx context.Context, req *GenerateRequest) (*GenerateResult, error) {
	if err := s.checkLimit(ctx); err != nil {
		return nil, err
	}
	client := s.k.resolveModel(req.Role)
	session, err := client.NewSession(ctx, req.sessionOptions()...)
	if err != nil {
		return nil, goerr.Wrap(err, "new session")
	}
	resp, err := session.Generate(ctx, req.Input, req.LLMOptions...)
	if err != nil {
		return nil, goerr.Wrap(err, "generate")
	}
	hist, err := session.History()
	if err != nil {
		return nil, goerr.Wrap(err, "session history")
	}
	result := &GenerateResult{
		Texts:         resp.Texts,
		Thoughts:      resp.Thoughts,
		FunctionCalls: resp.FunctionCalls,
		InputTokens:   resp.InputToken,
		OutputTokens:  resp.OutputToken,
		History:       hist,
	}
	s.addMetrics(Metrics{
		MetricInputTokens:  int64(resp.InputToken),
		MetricOutputTokens: int64(resp.OutputToken),
		MetricLLMCalls:     1,
	})
	return result, nil // not journaled: a replay re-calls the LLM (re-charge allowed, D44).
}

func (s *syscalls) CallTool(ctx context.Context, call gollem.FunctionCall) (map[string]any, error) {
	req := &ToolCallRequest{Effect: s.ec(), Call: call}
	h := chainToolCall(s.k.toolCallMW, s.toolCallBase)
	if h == nil {
		return nil, goerr.Wrap(ErrInvalidConfig, "tool call middleware returned a nil handler")
	}
	return h(ctx, req)
}

func (s *syscalls) toolCallBase(ctx context.Context, req *ToolCallRequest) (map[string]any, error) {
	tool, ok := s.toolByName[req.Call.Name]
	if !ok {
		return nil, goerr.Wrap(ErrToolNotFound, "unknown tool", goerr.V("tool", req.Call.Name))
	}
	spec := tool.Spec()
	if err := spec.ValidateArgs(req.Call.Arguments); err != nil {
		return nil, goerr.Wrap(err, "validate tool args", goerr.V("tool", req.Call.Name))
	}
	if err := s.checkLimit(ctx); err != nil {
		return nil, err
	}
	out, terr := tool.Run(ctx, req.Call.Arguments)
	s.addMetrics(Metrics{MetricToolCalls: 1})
	// A replay re-executes; side-effecting tools must be made idempotent by the
	// author (D44/D45). Run errors are returned to the strategy (the Process is
	// not dropped).
	return out, terr
}

func (s *syscalls) spawn(ctx context.Context, agent AgentName, input any, opts ...SpawnOption) (ProcessID, error) {
	cfg := newSpawnConfig(opts)
	// A static misuse, rejected before any middleware sees the request.
	if cfg.hasIdempotencyKey {
		return "", goerr.Wrap(ErrInvalidRequest, "WithIdempotencyKey is not allowed on SpawnChild (D48)")
	}
	req := &SpawnRequest{
		Effect:   s.ec(),
		Agent:    agent,
		Metadata: cfg.metadata,
		Subject:  cfg.subject,
		OnCommit: s.registerSpawnCommit,
		input:    input,
	}
	h := chainSpawn(s.k.spawnMW, s.spawnBase)
	if h == nil {
		return "", goerr.Wrap(ErrInvalidConfig, "spawn middleware returned a nil handler")
	}
	return h(ctx, req)
}

// registerSpawnCommit buffers fn to be called once with this transition's
// commit outcome. It runs after the commit, outside the transition, so a panic
// there cannot become a transition error — it is recovered and logged instead,
// or it would take the worker down.
func (s *syscalls) registerSpawnCommit(fn func(error)) {
	if fn == nil {
		return
	}
	s.pendingSpawnDone = append(s.pendingSpawnDone, func(err error) {
		s.recover("spawn OnCommit", func() { fn(err) })
	})
}

func (s *syscalls) spawnBase(ctx context.Context, req *SpawnRequest) (ProcessID, error) {
	if err := s.checkLimit(ctx); err != nil {
		return "", err
	}
	b, err := s.k.agents.binding(req.Agent)
	if err != nil {
		return "", err
	}
	// Minted before Init so the Init middleware can name the child (D-K/ADR-0009).
	cid := ProcessID(uuid.Must(uuid.NewV7()).String())
	// The parent context comes from the live transition, not from req.Effect: a
	// middleware may have overwritten that field, and the child's recorded
	// lineage must match its actual ParentID/RootID.
	parent := s.ec()
	st, err := s.k.runInit(ctx, b, &InitRequest{
		ProcessID: cid,
		Agent:     req.Agent,
		Parent:    &parent,
		input:     req.input,
	})
	if err != nil {
		return "", err
	}
	raw, err := b.encode(st)
	if err != nil {
		return "", goerr.Wrap(err, "encode child initial state")
	}
	now := s.k.clock()
	s.pendingChildren = append(s.pendingChildren, &Process{
		ID:           cid,
		Agent:        req.Agent,
		Status:       ProcessPending,
		Metadata:     req.Metadata,
		State:        raw,
		StateVersion: b.version,
		ParentID:     &s.proc.ID,
		RootID:       s.proc.RootID,
		Subject:      req.Subject,
		CreatedAt:    now,
		UpdatedAt:    now,
		Rev:          0,
	})
	s.addMetrics(Metrics{MetricSpawns: 1})
	return cid, nil
}

func (s *syscalls) Await(key AwaitKey) (*Await, bool) {
	aw, ok := s.awaits[key]
	return aw, ok
}

func (s *syscalls) Emit(_ context.Context, typ EventType, payload []byte) error {
	if payload == nil {
		return goerr.Wrap(ErrInvalidRequest, "nil event payload")
	}
	s.pendingEvents = append(s.pendingEvents, &Event{
		ProcessID: s.proc.ID,
		Type:      typ,
		Payload:   payload,
		At:        s.k.clock(),
	})
	return nil
}

// recover runs fn and turns a panic into a log line. It exists for the one
// callback the kernel invokes outside a transition — a SpawnRequest.OnCommit
// function, which runs on the worker goroutine after the commit, where a panic
// would otherwise kill the worker. Middleware itself is NOT wrapped: it runs
// inside the transition, where the worker's own recovery turns a panic into a
// transition error, which is the right outcome for a failing control layer.
func (s *syscalls) recover(where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.k.logger.Error("panic recovered", slog.String("where", where), slog.Any("panic", r))
		}
	}()
	fn()
}
