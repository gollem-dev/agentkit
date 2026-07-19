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
type Syscalls interface {
	// --- execution context ---
	ProcessID() ProcessID
	RootID() ProcessID
	ParentID() (ProcessID, bool)
	Agent() AgentName
	Now() time.Time // current time (the Kernel's clock; testable via WithClock). Not deterministic.

	// --- LLM (via gollem; Limiter before, Metrics after) ---
	Tools() []gollem.Tool // the tools the ToolFactory built (to declare to the LLM).
	Generate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error)

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

// GenerateOption configures a Generate. Only input is required (D26).
type GenerateOption func(*generateConfig)

type generateConfig struct {
	role         ModelRole
	history      *gollem.History
	systemPrompt string
	tools        []gollem.Tool
	schema       *gollem.Parameter
	llmOpts      []gollem.GenerateOption
}

// WithRole selects the model role. Omitting it (or nil) means the default model.
func WithRole(r ModelRole) GenerateOption {
	return func(c *generateConfig) { c.role = r }
}

// WithHistory passes prior conversation history (to gollem.WithSessionHistory).
func WithHistory(h *gollem.History) GenerateOption {
	return func(c *generateConfig) { c.history = h }
}

// WithSystemPrompt sets the system prompt (to gollem.WithSessionSystemPrompt).
func WithSystemPrompt(p string) GenerateOption {
	return func(c *generateConfig) { c.systemPrompt = p }
}

// WithTools declares tools to the LLM (execution goes through CallTool).
func WithTools(tools ...gollem.Tool) GenerateOption {
	return func(c *generateConfig) { c.tools = append(c.tools, tools...) }
}

// WithSchema requests JSON output against a schema (gollem
// WithSessionContentType(JSON) + WithSessionResponseSchema).
func WithSchema(p *gollem.Parameter) GenerateOption {
	return func(c *generateConfig) { c.schema = p }
}

// WithLLMOptions passes gollem generate options (temperature, etc.) straight
// through. Note agentkit.GenerateOption and gollem.GenerateOption are distinct
// types (package-qualified); pass-through is via this option.
func WithLLMOptions(o ...gollem.GenerateOption) GenerateOption {
	return func(c *generateConfig) { c.llmOpts = append(c.llmOpts, o...) }
}

func newGenerateConfig(opts []GenerateOption) *generateConfig {
	cfg := &generateConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

func (c *generateConfig) sessionOptions() []gollem.SessionOption {
	var opts []gollem.SessionOption
	if c.history != nil {
		opts = append(opts, gollem.WithSessionHistory(c.history))
	}
	if c.systemPrompt != "" {
		opts = append(opts, gollem.WithSessionSystemPrompt(c.systemPrompt))
	}
	if len(c.tools) > 0 {
		opts = append(opts, gollem.WithSessionTools(c.tools...))
	}
	if c.schema != nil {
		opts = append(opts, gollem.WithSessionContentType(gollem.ContentTypeJSON), gollem.WithSessionResponseSchema(c.schema))
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

	// run accumulation (committed proc.Metrics is only the committed cumulative;
	// this run's share is folded on any successful Apply, D44).
	runMetrics Metrics

	// transition buffers (folded into the commit ChangeSet).
	pendingChildren  []*Process    // SpawnChild children (atomic insert on commit, D48).
	pendingEvents    []*Event      // Emit / await.created.
	pendingSpawnDone []func(error) // Spawn observer completions (called by the worker after commit, #8).
}

func newSyscalls(k *Kernel, proc *Process, tools []gollem.Tool) *syscalls {
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
	return &syscalls{k: k, proc: proc, seq: proc.StateSeq + 1, tools: tools, toolByName: byName, awaits: awaits}
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

// notifySpawnDone calls every buffered Spawn observer completion exactly once
// with the transition outcome (nil = the child was committed; non-nil = the
// transition did not commit), then clears them so a retry does not double-fire.
func (s *syscalls) notifySpawnDone(err error) {
	for _, done := range s.pendingSpawnDone {
		done(err)
	}
	s.pendingSpawnDone = nil
}

func (s *syscalls) ec() EffectContext {
	return EffectContext{ProcessID: s.proc.ID, RootID: s.proc.RootID, Agent: s.proc.Agent, StateSeq: s.seq}
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

func (s *syscalls) Generate(ctx context.Context, input []gollem.Input, opts ...GenerateOption) (*GenerateResult, error) {
	if err := s.checkLimit(ctx); err != nil {
		return nil, err
	}
	cfg := newGenerateConfig(opts)
	done := s.observeGenerate(ctx, input, cfg.role)
	client := s.k.resolveModel(cfg.role)
	session, err := client.NewSession(ctx, cfg.sessionOptions()...)
	if err != nil {
		done(nil, err)
		return nil, goerr.Wrap(err, "new session")
	}
	resp, err := session.Generate(ctx, input, cfg.llmOpts...)
	if err != nil {
		done(nil, err)
		return nil, goerr.Wrap(err, "generate")
	}
	hist, err := session.History()
	if err != nil {
		done(nil, err)
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
	done(result, nil)
	return result, nil // not journaled: a replay re-calls the LLM (re-charge allowed, D44).
}

func (s *syscalls) CallTool(ctx context.Context, call gollem.FunctionCall) (map[string]any, error) {
	tool, ok := s.toolByName[call.Name]
	if !ok {
		return nil, goerr.Wrap(ErrToolNotFound, "unknown tool", goerr.V("tool", call.Name))
	}
	spec := tool.Spec()
	if err := spec.ValidateArgs(call.Arguments); err != nil {
		return nil, goerr.Wrap(err, "validate tool args", goerr.V("tool", call.Name))
	}
	if err := s.checkLimit(ctx); err != nil {
		return nil, err
	}
	done := s.observeToolCall(ctx, call)
	out, terr := tool.Run(ctx, call.Arguments)
	s.addMetrics(Metrics{MetricToolCalls: 1})
	done(out, terr)
	// A replay re-executes; side-effecting tools must be made idempotent by the
	// author (D44/D45). Run errors are returned to the strategy (the Process is
	// not dropped).
	return out, terr
}

func (s *syscalls) spawn(ctx context.Context, agent AgentName, input any, opts ...SpawnOption) (ProcessID, error) {
	cfg := newSpawnConfig(opts)
	if cfg.hasIdempotencyKey {
		return "", goerr.Wrap(ErrInvalidRequest, "WithIdempotencyKey is not allowed on SpawnChild (D48)")
	}
	if err := s.checkLimit(ctx); err != nil {
		return "", err
	}
	b, err := s.k.agents.binding(agent)
	if err != nil {
		return "", err
	}
	st, err := b.init(input)
	if err != nil {
		return "", err
	}
	raw, err := b.encode(st)
	if err != nil {
		return "", goerr.Wrap(err, "encode child initial state")
	}
	now := s.k.clock()
	cid := ProcessID(uuid.Must(uuid.NewV7()).String())
	child := &Process{
		ID:           cid,
		Agent:        agent,
		Status:       ProcessPending,
		Metadata:     cfg.metadata,
		State:        raw,
		StateVersion: b.version,
		ParentID:     &s.proc.ID,
		RootID:       s.proc.RootID,
		Subject:      cfg.subject,
		CreatedAt:    now,
		UpdatedAt:    now,
		Rev:          0,
	}
	s.pendingChildren = append(s.pendingChildren, child)
	s.addMetrics(Metrics{MetricSpawns: 1})
	// Observation (#8): start now (intent), completion after the transition
	// commit persists the child (the worker calls pendingSpawnDone).
	if start := s.k.observer.Spawn; start != nil {
		if done := s.safeSpawnStart(ctx, start, cid, agent); done != nil {
			s.pendingSpawnDone = append(s.pendingSpawnDone, done)
		}
	}
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

// --- observation helpers (best-effort; panics are recovered, C12) ---

// The observer receives deep copies of inputs/results so it can never mutate a
// value that execution uses (observation, not control, C12/#5).
func (s *syscalls) observeGenerate(ctx context.Context, input []gollem.Input, role ModelRole) func(*GenerateResult, error) {
	start := s.k.observer.Generate
	if start == nil {
		return func(*GenerateResult, error) {}
	}
	cp := copyInputs(input)
	var done func(*GenerateResult, error)
	s.recover("observer.Generate start", func() { done = start(ctx, s.ec(), cp, role) })
	if done == nil {
		return func(*GenerateResult, error) {}
	}
	return func(res *GenerateResult, err error) {
		rc := copyGenerateResult(res)
		s.recover("observer.Generate done", func() { done(rc, err) })
	}
}

func (s *syscalls) observeToolCall(ctx context.Context, call gollem.FunctionCall) func(map[string]any, error) {
	start := s.k.observer.ToolCall
	if start == nil {
		return func(map[string]any, error) {}
	}
	cp := call
	cp.Arguments = deepCopyMap(call.Arguments)
	var done func(map[string]any, error)
	s.recover("observer.ToolCall start", func() { done = start(ctx, s.ec(), cp) })
	if done == nil {
		return func(map[string]any, error) {}
	}
	return func(res map[string]any, err error) {
		rc := deepCopyMap(res)
		s.recover("observer.ToolCall done", func() { done(rc, err) })
	}
}

func (s *syscalls) safeSpawnStart(ctx context.Context, start func(context.Context, EffectContext, ProcessID, AgentName) func(error), cid ProcessID, agent AgentName) func(error) {
	var done func(error)
	s.recover("observer.Spawn start", func() { done = start(ctx, s.ec(), cid, agent) })
	if done == nil {
		return nil
	}
	return func(err error) { s.recover("observer.Spawn done", func() { done(err) }) }
}

func (s *syscalls) recover(where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.k.logger.Error("observer panic recovered", slog.String("where", where), slog.Any("panic", r))
		}
	}()
	fn()
}

// deepCopyValue recursively copies JSON-like values (maps/slices/primitives) so
// the observer cannot mutate nested data that execution shares.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = deepCopyValue(e)
		}
		return cp
	default:
		return v
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = deepCopyValue(v)
	}
	return cp
}

// copyInputs deep-copies the mutable parts of the LLM input slice (a
// FunctionResponse's Data map); immutable inputs (Text) are shared safely.
func copyInputs(input []gollem.Input) []gollem.Input {
	cp := make([]gollem.Input, len(input))
	for i, in := range input {
		switch v := in.(type) {
		case *gollem.FunctionResponse:
			fr := *v
			fr.Data = deepCopyMap(v.Data)
			cp[i] = &fr
		case gollem.FunctionResponse:
			v.Data = deepCopyMap(v.Data)
			cp[i] = v
		default:
			cp[i] = in
		}
	}
	return cp
}

func copyGenerateResult(r *GenerateResult) *GenerateResult {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Texts = append([]string(nil), r.Texts...)
	cp.Thoughts = append([]string(nil), r.Thoughts...)
	if r.FunctionCalls != nil {
		cp.FunctionCalls = make([]*gollem.FunctionCall, len(r.FunctionCalls))
		for i, fc := range r.FunctionCalls {
			if fc != nil {
				c := *fc
				c.Arguments = deepCopyMap(fc.Arguments)
				cp.FunctionCalls[i] = &c
			}
		}
	}
	if r.History != nil {
		cp.History = r.History.Clone()
	}
	return &cp
}
