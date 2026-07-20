// Command middleware wraps the five points where the kernel calls out --
// Init, Step, Generate, CallTool and SpawnChild -- with one registration each.
//
// The program's output is the audit trail the middleware produced, which is the
// shortest way to see what a next-chain is good for: it is registered once on
// the Kernel and applies to every agent, including children nobody wrote code
// for at the call site.
//
// Four things worth watching for, each marked at its middleware below:
//
//   - refusing a call by not calling next (toolPolicy),
//   - rewriting a request on the way in (inputStyling),
//   - learning whether a spawned child was really persisted (spawnAudit),
//   - and the fact that a Step middleware sees the CALL, not the commit
//     (stepTrace).
//
// Run it from the examples module: `cd examples && go run ./middleware`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

const (
	coordinatorAgent agentkit.AgentName = "coordinator"
	reporterAgent    agentkit.AgentName = "reporter"

	reportsKey agentkit.AwaitKey = "reports"

	defaultTopic = "the payments service"
)

// roleCoordinator keeps the coordinator's own calls on a separate client, which
// also makes the offline script deterministic: the children run in parallel and
// would otherwise race over one script.
var roleCoordinator = agentkit.DefineModelRole("middleware.coordinator")

// --- the audit trail the middleware writes ----------------------------------

// auditLog is what every middleware below writes to. A real one would be a
// database or a tracer; the point here is only that a single registration sees
// everything, so one sink can hold it.
type auditLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *auditLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *auditLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// --- the five middlewares ---------------------------------------------------

// inputStyling rewrites a request on the way in.
//
// It runs for every agent, so it cannot know any particular strategy's input
// type: InitInput reports ok == false for an agent this middleware does not
// recognise, which means "not mine, pass it through" rather than "error". The
// coordinator's own input takes that path; the reporter children take the other.
func inputStyling(log *auditLog) agentkit.InitMiddleware {
	const house = "Answer in one sentence. "

	return func(next agentkit.InitHandler) agentkit.InitHandler {
		return func(ctx context.Context, req *agentkit.InitRequest) (*agentkit.InitResult, error) {
			in, ok := agentkit.InitInput[simple.Input](req)
			if !ok {
				log.add("init      %-11s (left alone)", req.Agent)
				return next(ctx, req)
			}
			in.Prompt = house + in.Prompt
			log.add("init      %-11s prompt prefixed with the house style", req.Agent)
			// The original request is left untouched, so an outer middleware
			// never sees its own request change under it.
			return next(ctx, agentkit.NewInitRequest(req, in))
		}
	}
}

// stepTrace records every transition.
//
// What it observes is the Step CALL. The commit happens after the handler
// returns, outside this chain, so a transition that fails to commit is re-run
// and traced again -- at-least-once execution is visible here exactly as it is.
// It must call next at most once, which is why there is no retry in here.
func stepTrace(log *auditLog) agentkit.StepMiddleware {
	return func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(ctx context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			// StepState[any] always succeeds, which is the well-served case:
			// observation needs no knowledge of the strategy's state type.
			_, ok := agentkit.StepState[any](req)

			res, err := next(ctx, req)
			if err != nil {
				log.add("step      %-11s seq=%d failed: %v", req.Effect.Agent, req.Effect.StateSeq, err)
				return res, err
			}
			log.add("step      %-11s seq=%d -> %s (state readable: %t)",
				req.Effect.Agent, req.Effect.StateSeq, agentkit.DecisionKindOf(res), ok)
			return res, nil
		}
	}
}

// generateAudit records each LLM call and what it cost.
func generateAudit(log *auditLog) agentkit.GenerateMiddleware {
	return func(next agentkit.GenerateHandler) agentkit.GenerateHandler {
		return func(ctx context.Context, req *agentkit.GenerateRequest) (*agentkit.GenerateResult, error) {
			res, err := next(ctx, req)
			if err != nil {
				log.add("generate  %-11s role=%s failed: %v", req.Effect.Agent, roleName(req.Role), err)
				return res, err
			}
			log.add("generate  %-11s role=%s tokens=%d/%d",
				req.Effect.Agent, roleName(req.Role), res.InputTokens, res.OutputTokens)
			return res, nil
		}
	}
}

func roleName(r agentkit.ModelRole) string {
	if r == nil {
		return "default"
	}
	return r.String()
}

// toolPolicy refuses a call by not calling next.
//
// Being the outermost layer of the syscall, it wraps the Limiter check, tool
// resolution and argument validation -- so returning here stops the call before
// any of them, and a call refused further in is still visible to it.
//
// This is a chokepoint, not a gate: it sees calls made through
// Syscalls.CallTool, and a strategy holding a gollem.Tool value can call Run on
// it directly. Enforcement that must not be bypassable belongs inside Run --
// see examples/tools.
func toolPolicy(log *auditLog, allowed map[string]bool) agentkit.ToolCallMiddleware {
	return func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			if !allowed[req.Call.Name] {
				log.add("tool      %-11s %s DENIED by policy", req.Effect.Agent, req.Call.Name)
				return nil, goerr.New("tool refused by policy", goerr.V("tool", req.Call.Name))
			}
			out, err := next(ctx, req)
			if err != nil {
				log.add("tool      %-11s %s failed: %v", req.Effect.Agent, req.Call.Name, err)
				return out, err
			}
			log.add("tool      %-11s %s ok", req.Effect.Agent, req.Call.Name)
			return out, nil
		}
	}
}

// spawnAudit records children, and whether they were actually persisted.
//
// SpawnChild only buffers a child into the transition's commit, so "the call
// returned an id" is not "a child exists". OnCommit closes that gap. It is
// registered AFTER a successful next on purpose: registering before would fire
// with nil whenever the transition committed, even for a spawn that failed and
// produced no child at all.
func spawnAudit(log *auditLog) agentkit.SpawnMiddleware {
	return func(next agentkit.SpawnHandler) agentkit.SpawnHandler {
		return func(ctx context.Context, req *agentkit.SpawnRequest) (agentkit.ProcessID, error) {
			pid, err := next(ctx, req)
			if err != nil {
				log.add("spawn     %-11s refused: %v", req.Agent, err)
				return pid, err
			}
			agent := req.Agent
			req.OnCommit(func(commitErr error) {
				if commitErr != nil {
					log.add("spawn     %-11s buffered but NOT persisted: %v", agent, commitErr)
					return
				}
				log.add("spawn     %-11s persisted with the transition", agent)
			})
			return pid, nil
		}
	}
}

// --- the strategy that triggers each hook once ------------------------------

type input struct {
	Topic string
}

// state is deliberately plain. This strategy exists to reach each of the five
// hooks in a fixed order, not to be interesting in itself.
type state struct {
	Topic    string               `json:"topic"`
	Phase    string               `json:"phase"`
	Notes    []string             `json:"notes,omitempty"`
	Children []agentkit.ProcessID `json:"children,omitempty"`
}

type output struct {
	Notes   []string `json:"notes"`
	Reports []string `json:"reports"`
}

const (
	phaseSurvey  = "survey"
	phaseSpawn   = "spawn"
	phaseCollect = "collect"
)

type coordinator struct {
	reporter agentkit.Agent[simple.Input]
	fanout   int
}

func (s *coordinator) Version() int { return 1 }

func (s *coordinator) Init(in input) (state, error) {
	if in.Topic == "" {
		return state{}, goerr.New("topic is required")
	}
	return state{Topic: in.Topic, Phase: phaseSurvey}, nil
}

func (s *coordinator) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[output], error) {
	switch st.Phase {
	case phaseSurvey:
		res, err := sys.Generate(ctx,
			[]gollem.Input{gollem.Text("What should we check about " + st.Topic + "?")},
			agentkit.WithRole(roleCoordinator))
		if err != nil {
			return st, agentkit.Decision[output]{}, err
		}
		st.Notes = append(st.Notes, res.Texts...)

		// One call the policy allows and one it refuses. A refused call comes
		// back as an error to the strategy, exactly like a failing tool: it is
		// a message, not a fatal condition.
		for _, call := range []gollem.FunctionCall{
			{ID: "call-1", Name: "lookup", Arguments: map[string]any{"key": "open-incidents"}},
			{ID: "call-2", Name: "purge", Arguments: map[string]any{"key": "open-incidents"}},
		} {
			out, terr := sys.CallTool(ctx, call)
			if terr != nil {
				st.Notes = append(st.Notes, call.Name+": "+terr.Error())
				continue
			}
			st.Notes = append(st.Notes, fmt.Sprintf("%s: %v", call.Name, out["value"]))
		}

		st.Phase = phaseSpawn
		return st, agentkit.Continue[output](), nil

	case phaseSpawn:
		st.Children = st.Children[:0]
		for i := 0; i < s.fanout; i++ {
			pid, err := s.reporter.SpawnChild(ctx, sys, simple.Input{
				Prompt: fmt.Sprintf("Write report %d about %s.", i+1, st.Topic),
			})
			if err != nil {
				return st, agentkit.Fail[output](agentkit.FailureStrategyError, "spawn reporter: "+err.Error()), nil
			}
			st.Children = append(st.Children, pid)
		}
		st.Phase = phaseCollect
		return st, agentkit.Suspend[output](agentkit.WaitChildren(reportsKey, st.Children...)), nil

	case phaseCollect:
		aw, ok := sys.Await(reportsKey)
		if !ok || aw.Status != agentkit.AwaitResponded {
			return st, agentkit.Decision[output]{}, goerr.New("children await not responded",
				goerr.V("key", reportsKey))
		}
		var reports []string
		for _, r := range aw.Results {
			if r.Status != agentkit.ProcessSucceeded {
				reports = append(reports, string(r.Status))
				continue
			}
			var childOut simple.Output
			if err := json.Unmarshal(r.Output, &childOut); err != nil {
				return st, agentkit.Decision[output]{}, goerr.Wrap(err, "decode child output")
			}
			reports = append(reports, strings.Join(childOut.Texts, " "))
		}
		return st, agentkit.Done(output{Notes: st.Notes, Reports: reports}), nil

	default:
		return st, agentkit.Decision[output]{}, goerr.New("unknown phase", goerr.V("phase", st.Phase))
	}
}

func (s *coordinator) EncodeState(st state) ([]byte, error) { return json.Marshal(st) }

func (s *coordinator) EncodeOutput(out output) ([]byte, error) { return json.Marshal(out) }

func (s *coordinator) DecodeState(_ int, raw []byte) (state, error) {
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, goerr.Wrap(err, "decode state")
	}
	return st, nil
}

// --- the tools the policy decides about -------------------------------------

type lookupTool struct {
	name string
	mu   *sync.Mutex
	ran  map[string]bool
}

func newLookupTool(name string, ran map[string]bool, mu *sync.Mutex) *lookupTool {
	return &lookupTool{name: name, mu: mu, ran: ran}
}

func (t *lookupTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        t.name,
		Description: "Read a value from the operations store.",
		Parameters: map[string]*gollem.Parameter{
			"key": {Type: gollem.TypeString, Description: "The key to read.", Required: true},
		},
	}
}

func (t *lookupTool) Run(_ context.Context, args map[string]any) (map[string]any, error) {
	t.mu.Lock()
	t.ran[t.name] = true
	t.mu.Unlock()
	return map[string]any{"value": fmt.Sprintf("%v has 3 entries", args["key"])}, nil
}

// --- wiring -----------------------------------------------------------------

func main() {
	topic := flag.String("topic", defaultTopic, "what the coordinator should look into")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, *topic); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, w io.Writer, topic string) error {
	_, err := runWith(ctx, w, topic)
	return err
}

// runWith returns which tools actually ran, so a test can check that a refused
// call never reached one.
func runWith(ctx context.Context, w io.Writer, topic string) (map[string]bool, error) {
	coordinatorModel, live, err := demo.NewLLM(ctx, demo.Turn{
		Texts: []string{"Check the error rate, the queue depth and the last deploy."},
	})
	if err != nil {
		return nil, goerr.Wrap(err, "new coordinator llm")
	}
	reporterModel, _, err := demo.NewLLM(ctx, demo.Turn{
		Texts: []string{"Nothing anomalous in this slice."},
	})
	if err != nil {
		return nil, goerr.Wrap(err, "new reporter llm")
	}
	fmt.Fprintf(w, "model: %s\n\n", demo.ModelLabel(live))

	var mu sync.Mutex
	ran := map[string]bool{}
	tools := []gollem.Tool{
		newLookupTool("lookup", ran, &mu),
		newLookupTool("purge", ran, &mu),
	}

	reg := agentkit.NewRegistry()
	reporter, err := simple.Register(reg, reporterAgent, 1)
	if err != nil {
		return nil, goerr.Wrap(err, "register reporter")
	}
	coordinatorAg, err := agentkit.Register(reg, coordinatorAgent, 1,
		&coordinator{reporter: reporter, fanout: 2})
	if err != nil {
		return nil, goerr.Wrap(err, "register coordinator")
	}

	trail := &auditLog{}

	// One registration per hook, covering every agent. The first registered is
	// the outermost.
	k, err := agentkit.New(memory.New(), reporterModel, reg,
		agentkit.WithModelRole(roleCoordinator, coordinatorModel),
		agentkit.WithToolFactory(func(_ context.Context, proc *agentkit.Process) ([]gollem.Tool, error) {
			if proc.Agent != coordinatorAgent {
				return nil, nil
			}
			return tools, nil
		}),
		agentkit.WithInitMiddleware(inputStyling(trail)),
		agentkit.WithStepMiddleware(stepTrace(trail)),
		agentkit.WithGenerateMiddleware(generateAudit(trail)),
		agentkit.WithToolCallMiddleware(toolPolicy(trail, map[string]bool{"lookup": true})),
		agentkit.WithSpawnMiddleware(spawnAudit(trail)),
	)
	if err != nil {
		return nil, goerr.Wrap(err, "new kernel")
	}

	pid, err := coordinatorAg.Spawn(ctx, k, input{Topic: topic})
	if err != nil {
		return nil, goerr.Wrap(err, "spawn")
	}

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- k.Serve(serveCtx,
			agentkit.WithConcurrency(4),
			agentkit.WithPollInterval(20*time.Millisecond))
	}()

	proc, waitErr := demo.WaitProcess(ctx, k, pid, demo.Terminal, time.Minute)
	stop()
	<-served
	if waitErr != nil {
		return nil, goerr.Wrap(waitErr, "wait for the process")
	}

	fmt.Fprintln(w, "audit trail")
	for _, line := range trail.snapshot() {
		fmt.Fprintf(w, "  %s\n", line)
	}

	fmt.Fprintf(w, "\nstatus: %s\n", proc.Status)
	if proc.Status != agentkit.ProcessSucceeded {
		return ran, goerr.New("process did not succeed",
			goerr.V("status", proc.Status), goerr.V("failure", proc.Failure))
	}

	var out output
	if err := json.Unmarshal(proc.Output, &out); err != nil {
		return ran, goerr.Wrap(err, "decode output")
	}
	for _, note := range out.Notes {
		fmt.Fprintf(w, "note:   %s\n", note)
	}
	for _, report := range out.Reports {
		fmt.Fprintf(w, "report: %s\n", report)
	}

	mu.Lock()
	defer mu.Unlock()
	snapshot := map[string]bool{}
	for name, done := range ran {
		snapshot[name] = done
	}
	return snapshot, nil
}
