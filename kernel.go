package agentkit

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
)

// Kernel is the Process lifecycle API and worker loop (the proposal's Engine +
// Worker merged into one type, D5). All held state is immutable injected
// references — no cross-request state in process memory.
type Kernel struct {
	repo         Repository
	defaultModel gollem.LLMClient
	models       map[ModelRole]gollem.LLMClient
	agents       *Registry
	toolFactory  ToolFactory
	limiter      Limiter
	initMW       []InitMiddleware
	stepMW       []StepMiddleware
	generateMW   []GenerateMiddleware
	toolCallMW   []ToolCallMiddleware
	spawnMW      []SpawnMiddleware
	logger       *slog.Logger
	clock        func() time.Time
}

type kernelConfig struct {
	roleBindings []roleBinding
	toolFactory  ToolFactory
	limiter      Limiter
	initMW       []InitMiddleware
	stepMW       []StepMiddleware
	generateMW   []GenerateMiddleware
	toolCallMW   []ToolCallMiddleware
	spawnMW      []SpawnMiddleware
	logger       *slog.Logger
	clock        func() time.Time
}

type roleBinding struct {
	role   ModelRole
	client gollem.LLMClient
}

// KernelOption configures a Kernel.
type KernelOption func(*kernelConfig)

// WithModelRole assigns a client to a role (repeatable). A nil role is
// ErrInvalidConfig (the default is passed positionally to New).
func WithModelRole(role ModelRole, client gollem.LLMClient) KernelOption {
	return func(c *kernelConfig) {
		c.roleBindings = append(c.roleBindings, roleBinding{role: role, client: client})
	}
}

// WithToolFactory sets the tool factory. Default: none (no tools).
func WithToolFactory(f ToolFactory) KernelOption {
	return func(c *kernelConfig) { c.toolFactory = f }
}

// WithLimiter sets the limiter. Default: none (unlimited).
func WithLimiter(f Limiter) KernelOption {
	return func(c *kernelConfig) { c.limiter = f }
}

// WithLogger sets the logger. Default: slog.Default().
func WithLogger(l *slog.Logger) KernelOption {
	return func(c *kernelConfig) { c.logger = l }
}

// WithClock sets the clock that Now() returns (a test seam). Default: time.Now.
// Determinism is not provided (D44).
func WithClock(fn func() time.Time) KernelOption {
	return func(c *kernelConfig) { c.clock = fn }
}

// New constructs a Kernel. model is the default model (what a nil ModelRole
// resolves to); an agent runtime needs at least one model, so it is positional
// (D26/D27). Static validation (ErrInvalidConfig): repo / model / agents non-nil.
func New(repo Repository, model gollem.LLMClient, agents *Registry, opts ...KernelOption) (*Kernel, error) {
	if repo == nil {
		return nil, goerr.Wrap(ErrInvalidConfig, "repo is nil")
	}
	if model == nil {
		return nil, goerr.Wrap(ErrInvalidConfig, "model is nil")
	}
	if agents == nil {
		return nil, goerr.Wrap(ErrInvalidConfig, "agents is nil")
	}
	cfg := kernelConfig{logger: slog.Default(), clock: time.Now}
	for _, o := range opts {
		o(&cfg)
	}
	for i, mw := range cfg.initMW {
		if mw == nil {
			return nil, goerr.Wrap(ErrInvalidConfig, "nil init middleware", goerr.V("index", i))
		}
	}
	for i, mw := range cfg.stepMW {
		if mw == nil {
			return nil, goerr.Wrap(ErrInvalidConfig, "nil step middleware", goerr.V("index", i))
		}
	}
	for i, mw := range cfg.generateMW {
		if mw == nil {
			return nil, goerr.Wrap(ErrInvalidConfig, "nil generate middleware", goerr.V("index", i))
		}
	}
	for i, mw := range cfg.toolCallMW {
		if mw == nil {
			return nil, goerr.Wrap(ErrInvalidConfig, "nil tool call middleware", goerr.V("index", i))
		}
	}
	for i, mw := range cfg.spawnMW {
		if mw == nil {
			return nil, goerr.Wrap(ErrInvalidConfig, "nil spawn middleware", goerr.V("index", i))
		}
	}
	models := make(map[ModelRole]gollem.LLMClient, len(cfg.roleBindings))
	for _, rb := range cfg.roleBindings {
		if rb.role == nil {
			return nil, goerr.Wrap(ErrInvalidConfig, "WithModelRole with nil role (default is passed positionally)")
		}
		if rb.client == nil {
			return nil, goerr.Wrap(ErrInvalidConfig, "WithModelRole with nil client", goerr.V("role", rb.role.String()))
		}
		models[rb.role] = rb.client
	}
	return &Kernel{
		repo:         repo,
		defaultModel: model,
		models:       models,
		agents:       agents,
		toolFactory:  cfg.toolFactory,
		limiter:      cfg.limiter,
		initMW:       cfg.initMW,
		stepMW:       cfg.stepMW,
		generateMW:   cfg.generateMW,
		toolCallMW:   cfg.toolCallMW,
		spawnMW:      cfg.spawnMW,
		logger:       cfg.logger,
		clock:        cfg.clock,
	}, nil
}

// runInit builds a Process's initial state through the Init middleware chain.
// It is shared by the application entry point (spawnFromApp) and the child one
// (syscalls.spawnBase). The kernel is in-package, so it reads and writes the
// request's type-erased payload directly; the generic helpers exist for callers
// outside it.
func (k *Kernel) runInit(ctx context.Context, b StrategyBinding, req *InitRequest) (any, error) {
	base := func(_ context.Context, r *InitRequest) (*InitResult, error) {
		st, err := b.init(r.input)
		if err != nil {
			return nil, err
		}
		return &InitResult{state: st}, nil
	}
	h := chainInit(k.initMW, base)
	if h == nil {
		return nil, goerr.Wrap(ErrInvalidConfig, "init middleware returned a nil handler")
	}
	res, err := h(ctx, req)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, goerr.Wrap(ErrInvalidConfig, "init middleware returned a nil result")
	}
	return res.state, nil
}

// resolveModel resolves a role to a client: (1) an individual WithModelRole
// binding (identity match) -> (2) the default model. Because the default is a
// required argument, a role is always resolvable; there is no runtime unresolved
// error.
func (k *Kernel) resolveModel(role ModelRole) gollem.LLMClient {
	if role != nil {
		if c, ok := k.models[role]; ok {
			return c
		}
	}
	return k.defaultModel
}

// spawnFromApp is the shared body of Agent[I].Spawn (application launch). It runs
// Init (through the Init middleware chain), encodes the initial state, applies
// idempotency/subject, then inserts the Process with a process.created event.
// Init/encode errors return synchronously.
//
// Init runs before the idempotency lookup, so an idempotent Spawn that ends up
// returning an existing Process still runs Init and its middleware, and the id
// minted for this call is discarded.
func (k *Kernel) spawnFromApp(ctx context.Context, name AgentName, input any, opts ...SpawnOption) (ProcessID, error) {
	cfg := newSpawnConfig(opts)
	b, err := k.agents.binding(name)
	if err != nil {
		return "", err
	}
	// The id is minted before Init so an Init middleware can correlate its
	// record with the Process about to be created. A failed Init just discards
	// it — nothing is persisted yet. This also matches ADR-0009, which already
	// describes minting before running the child's Init.
	pid := ProcessID(uuid.Must(uuid.NewV7()).String())
	st, err := k.runInit(ctx, b, &InitRequest{
		ProcessID: pid,
		Agent:     name,
		Parent:    nil, // application entry point.
		input:     input,
	})
	if err != nil {
		return "", err
	}
	raw, err := b.encode(st)
	if err != nil {
		return "", goerr.Wrap(err, "encode initial state")
	}

	// idempotency: return the existing ID if one matches.
	if cfg.hasIdempotencyKey {
		if existing, ferr := k.repo.FindProcessByIdempotencyKey(ctx, cfg.idempotencyKey); ferr == nil {
			return existing.ID, nil
		} else if !isNotFound(ferr) {
			return "", ferr
		}
	}
	// subject: an open Process holding it -> ErrSubjectBusy.
	if cfg.subject != nil {
		if _, ferr := k.repo.FindOpenProcessBySubject(ctx, *cfg.subject); ferr == nil {
			return "", goerr.Wrap(ErrSubjectBusy, "subject held by an open process",
				goerr.V("subject", *cfg.subject))
		} else if !isNotFound(ferr) {
			return "", ferr
		}
	}

	now := k.clock()
	proc := &Process{
		ID:             pid,
		Agent:          name,
		Status:         ProcessPending,
		Metadata:       cfg.metadata,
		State:          raw,
		StateVersion:   b.version,
		RootID:         pid,
		Subject:        cfg.subject,
		IdempotencyKey: cfg.idempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
		Rev:            0,
	}
	cs := ChangeSet{
		Processes: []*Process{proc},
		Events:    []*Event{{ProcessID: pid, Type: EventProcessCreated, At: now}},
	}
	if err := k.repo.Apply(ctx, cs); err != nil {
		// A uniqueness conflict on idempotency key means a concurrent Spawn won;
		// re-find and return the existing ID (find-then-insert race, closed by
		// the Repository's uniqueness contract).
		if errors.Is(err, ErrConflict) && cfg.hasIdempotencyKey {
			if existing, ferr := k.repo.FindProcessByIdempotencyKey(ctx, cfg.idempotencyKey); ferr == nil {
				return existing.ID, nil
			}
		}
		return "", err
	}
	return pid, nil
}

// Respond delivers a response to a question await (a confirmation's yes/no is
// sent this way too). Responding to a non-question await is ErrInvalidRequest.
// Only open is accepted (ErrAwaitClosed); first-writer-wins. The kernel only
// nil-checks the payload.
func (k *Kernel) Respond(ctx context.Context, pid ProcessID, key AwaitKey, response []byte, opts ...RespondOption) error {
	if response == nil {
		return goerr.Wrap(ErrInvalidRequest, "nil response")
	}
	cfg := newRespondConfig(opts)
	for {
		proc, err := k.repo.GetProcess(ctx, pid)
		if err != nil {
			return err
		}
		if proc.Status.Terminal() {
			return goerr.Wrap(ErrProcessFinished, "process is terminal", goerr.V("status", proc.Status))
		}
		aw, err := k.findAwait(ctx, pid, key)
		if err != nil {
			return err
		}
		if aw.Kind != AwaitQuestion {
			return goerr.Wrap(ErrInvalidRequest, "respond to non-question await", goerr.V("kind", aw.Kind))
		}
		if aw.Status != AwaitOpen {
			return goerr.Wrap(ErrAwaitClosed, "await not open", goerr.V("status", aw.Status))
		}
		now := k.clock()
		aw.Response = response
		aw.Status = AwaitResponded
		aw.RespondedBy = cfg.respondedBy
		aw.RespondedAt = &now
		wake := proc.clone()
		if wake.Status == ProcessWaiting {
			wake.Status = ProcessPending
		}
		err = k.repo.Apply(ctx, ChangeSet{Processes: []*Process{wake}, Awaits: []*Await{aw}})
		if errors.Is(err, ErrConflict) {
			continue // re-read and re-judge (E19).
		}
		return err
	}
}

// Cancel requests cancellation. pending/waiting -> immediately cancelled (open
// awaits cancelled, process.finished event). running -> only sets
// cancel_requested (the worker finalizes at the next transition boundary).
// terminal -> ErrProcessFinished.
func (k *Kernel) Cancel(ctx context.Context, pid ProcessID, reason string) error {
	for {
		proc, err := k.repo.GetProcess(ctx, pid)
		if err != nil {
			return err
		}
		if proc.Status.Terminal() {
			return goerr.Wrap(ErrProcessFinished, "process is terminal", goerr.V("status", proc.Status))
		}
		if proc.Status == ProcessRunning {
			p := proc.clone()
			p.CancelRequested = true
			p.CancelReason = reason
			if err := k.repo.Apply(ctx, ChangeSet{Processes: []*Process{p}}); errors.Is(err, ErrConflict) {
				continue
			} else {
				return err
			}
		}
		// pending/waiting: finalize as cancelled now. This is an EXTERNAL caller,
		// so it uses externalFence: a conflict is propagated, and the loop re-reads
		// and re-decides (the Process may have just been claimed and be running, in
		// which case the next iteration sets CancelRequested) (#1).
		if err := k.finalize(ctx, proc, cancelledWith(reason), externalFence, nil); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
}

// GetProcess returns the Process (read-through to the Repository).
func (k *Kernel) GetProcess(ctx context.Context, pid ProcessID) (*Process, error) {
	return k.repo.GetProcess(ctx, pid)
}

// ListAwaits returns the Process's awaits.
func (k *Kernel) ListAwaits(ctx context.Context, pid ProcessID) ([]*Await, error) {
	return k.repo.ListAwaits(ctx, pid)
}

// ListEvents returns the Process's events in append order.
func (k *Kernel) ListEvents(ctx context.Context, pid ProcessID) ([]*Event, error) {
	return k.repo.ListEvents(ctx, pid)
}

// findAwait loads a single await by key. Absent -> ErrAwaitNotFound.
func (k *Kernel) findAwait(ctx context.Context, pid ProcessID, key AwaitKey) (*Await, error) {
	awaits, err := k.repo.ListAwaits(ctx, pid)
	if err != nil {
		return nil, err
	}
	for _, aw := range awaits {
		if aw.Key == key {
			return aw, nil
		}
	}
	return nil, goerr.Wrap(ErrAwaitNotFound, "no such await", goerr.V("key", key))
}

// isNotFound reports whether err is ErrProcessNotFound.
func isNotFound(err error) bool { return errors.Is(err, ErrProcessNotFound) }
