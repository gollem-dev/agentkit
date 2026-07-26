// Command tracing records what a claim actually did -- when each transition,
// tool call and LLM call started and ended -- and saves the result as JSON.
//
// The interesting part is how little of it is wiring. Three middleware cover
// what agentkit owns:
//
//   - ClaimMiddleware opens the root span and puts the trace handler in the ctx.
//     A claim is the only scope that brackets a worker's whole run on a Process,
//     so it is where a per-claim thing is opened and closed.
//   - StepMiddleware opens one span per transition.
//   - ToolCallMiddleware opens one per tool call, which is needed because
//     agentkit calls the tool directly rather than through gollem's agent loop.
//
// The fourth kind of span, llm_call, is emitted by gollem's LLM client itself:
// it reads the handler out of the ctx. That is the whole reason the handler goes
// in at the claim and nothing else is wired for it -- and it is also why running
// offline shows no llm_call span, since the offline stub is a mock session
// rather than a real client. Set the Vertex AI environment variables to see it.
//
// Timing is measured, never inferred. A Process that suspends for a day is not
// one long span: it is two claims, each timed for as long as a worker was really
// working on it. Correlating them is what the process_id label is for.
//
// The handler here writes JSON files, so the example runs on its own and leaves
// something to look at. Swapping the backend for OpenTelemetry is one line --
// gollem's otel handler implements the same interface -- but then the spans go
// to a collector instead of a directory.
//
// Run it from the examples module: `cd examples && go run ./tracing`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem"
	gtrace "github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/goerr/v2"
)

const agentName agentkit.AgentName = "librarian"

// --- the three middleware that record the trace ------------------------------

// claimTrace opens one trace per claim and saves it when the claim ends, under
// an id built from the Process id and this claim's lease token.
//
// One trace per CLAIM rather than per Process, for three reasons.
//
// It is the honest unit. A Process may be claimed many times; only a claim is a
// single continuous stretch of work on one worker.
//
// It is also the only unit a Recorder can represent. A Recorder holds one trace,
// and StartAgentExecute on a Recorder that already has one attaches a child span
// instead of starting a second trace. Sharing one across claims would therefore
// braid every concurrently claimed Process into a single tree — and agentkit
// runs claims in parallel (WithMaxConcurrent defaults to 64). So the Recorder is
// built here, inside the per-claim closure. The Repository holds no trace state
// and is built once, outside it.
//
// And it keeps the saved artifacts distinct. The file repository writes one file
// per trace id, so a per-Process id would have each claim overwrite the last.
// Naming it "<process>-<lease token>" makes the id unique (a fresh lease token
// is minted on every claim) while still saying which Process it belongs to
// without opening the file. The process id is on Metadata.Labels too, which is
// what a Repository writing to a database would key on rather than parsing the
// id apart.
func claimTrace(dir string, logger *log.Logger) agentkit.ClaimMiddleware {
	// Shared by every claim: a Repository is a destination, not trace state.
	repo := gtrace.NewFileRepository(dir)
	return func(next agentkit.ClaimHandler) agentkit.ClaimHandler {
		return func(ctx context.Context, req *agentkit.ClaimRequest) (agentkit.ClaimOutcome, error) {
			proc := req.Process
			// Per claim, never reused: see why on claimTrace.
			rec := gtrace.New(
				gtrace.WithTraceID(string(proc.ID)+"-"+proc.LeaseToken),
				gtrace.WithRepository(repo),
				gtrace.WithMetadata(gtrace.TraceMetadata{
					Strategy: string(proc.Agent),
					Labels: map[string]string{
						"process_id": string(proc.ID),
						"root_id":    string(proc.RootID),
						"worker":     proc.LeaseOwner,
					},
				}),
			)
			// gollem's LLM client looks for this and emits its own llm_call spans.
			ctx = gtrace.WithHandler(ctx, rec)
			ctx = rec.StartAgentExecute(ctx)

			var outcome agentkit.ClaimOutcome
			var err error
			// Closing the span and saving belong in a defer, not after the call. The
			// kernel recovers a panic OUTSIDE this middleware, so a panicking claim
			// never returns here -- and that is exactly the run whose trace is worth
			// having. The panic is re-raised so the kernel still sees it.
			defer func() {
				if r := recover(); r != nil {
					rec.AddEvent(ctx, "claim.panic", fmt.Sprint(r))
					rec.EndAgentExecute(ctx, fmt.Errorf("claim panicked: %v", r))
					save(ctx, rec, logger, proc.ID)
					panic(r)
				}
				rec.AddEvent(ctx, "claim.outcome", string(outcome))
				rec.EndAgentExecute(ctx, err)
				save(ctx, rec, logger, proc.ID)
			}()

			outcome, err = next(ctx, req)
			return outcome, err
		}
	}
}

// save writes the trace out. A failure here must never fail the claim: returning
// it would put the Process back for a retry it does not need, which is a strange
// way for a tracing layer to behave.
func save(ctx context.Context, rec *gtrace.Recorder, logger *log.Logger, pid agentkit.ProcessID) {
	if err := rec.Finish(ctx); err != nil {
		logger.Printf("trace save failed: process=%s error=%v", pid, err)
	}
}

// stepTrace times one transition.
//
// gollem's handler has no generic span kind, so a transition is recorded as a
// child agent span -- the nearest fit it offers, and what trace.AsChildAgent
// uses for the same purpose.
func stepTrace() agentkit.StepMiddleware {
	return func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(ctx context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			h := gtrace.HandlerFrom(ctx)
			if h == nil {
				return next(ctx, req) // no claim middleware installed one.
			}
			ctx = h.StartChildAgent(ctx, fmt.Sprintf("transition %d", req.Effect.StateSeq))
			var err error
			defer func() { h.EndChildAgent(ctx, err) }() // closed even if next panics.
			res, err := next(ctx, req)
			return res, err
		}
	}
}

// toolTrace times one tool call. agentkit runs tools itself, so nothing else
// would record these.
func toolTrace() agentkit.ToolCallMiddleware {
	return func(next agentkit.ToolCallHandler) agentkit.ToolCallHandler {
		return func(ctx context.Context, req *agentkit.ToolCallRequest) (map[string]any, error) {
			h := gtrace.HandlerFrom(ctx)
			if h == nil {
				return next(ctx, req)
			}
			ctx = h.StartToolExec(ctx, req.Call.Name, req.Call.Arguments)
			var out map[string]any
			var err error
			defer func() { h.EndToolExec(ctx, out, err) }() // closed even if the tool panics.
			out, err = next(ctx, req)
			return out, err
		}
	}
}

// --- a tool worth timing -----------------------------------------------------

type lookupTool struct{}

func (lookupTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "lookup",
		Description: "Look up a book by title.",
		Parameters: map[string]*gollem.Parameter{
			"title": {
				Type:        gollem.TypeString,
				Description: "The title to look up.",
				Required:    true,
			},
		},
	}
}

func (lookupTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	title, _ := args["title"].(string)
	// Slow enough that the span has a duration worth reading, which is the point
	// of measuring rather than inferring.
	select {
	case <-time.After(25 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return map[string]any{"title": title, "shelf": "B12", "available": true}, nil
}

// --- reading the saved trace back --------------------------------------------

// loadTraces reads every trace file the run wrote, so the program can show that
// the save really happened rather than just claiming it did.
func loadTraces(dir string) ([]*gtrace.Trace, error) {
	// Both the listing and the reads go through an os.Root, so a name cannot walk
	// out of the directory we were given.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, goerr.Wrap(err, "open trace directory", goerr.V("dir", dir))
	}
	defer func() { _ = root.Close() }()

	self, err := root.Open(".")
	if err != nil {
		return nil, goerr.Wrap(err, "open trace directory for listing", goerr.V("dir", dir))
	}
	entries, err := self.ReadDir(-1)
	_ = self.Close()
	if err != nil {
		return nil, goerr.Wrap(err, "list trace files", goerr.V("dir", dir))
	}
	traces := make([]*gtrace.Trace, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		tr, rerr := readTrace(root, e.Name())
		if rerr != nil {
			return nil, rerr
		}
		traces = append(traces, tr)
	}
	return traces, nil
}

func readTrace(root *os.Root, name string) (*gtrace.Trace, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, goerr.Wrap(err, "open trace file", goerr.V("file", name))
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, goerr.Wrap(err, "read trace file", goerr.V("file", name))
	}
	var tr gtrace.Trace
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, goerr.Wrap(err, "decode trace file", goerr.V("file", name))
	}
	return &tr, nil
}

func printSpan(w io.Writer, s *gtrace.Span, depth int) {
	if s == nil {
		return
	}
	label := s.Name
	if s.Kind == gtrace.SpanKindToolExec || s.Kind == gtrace.SpanKindLLMCall {
		label = string(s.Kind) + " " + s.Name
	}
	fmt.Fprintf(w, "  %s%-34s %6.1fms\n",
		strings.Repeat("  ", depth), label, float64(s.Duration)/float64(time.Millisecond))
	for _, c := range s.Children {
		printSpan(w, c, depth+1)
	}
}

// --- wiring ------------------------------------------------------------------

func run(ctx context.Context, w io.Writer, traceDir string) error {
	// The script: look the book up, then answer. Two LLM turns, one tool call.
	model, live, err := demo.NewLLM(ctx,
		demo.Turn{FunctionCalls: []*gollem.FunctionCall{{
			ID:        "call-1",
			Name:      "lookup",
			Arguments: map[string]any{"title": "The Mezzanine"},
		}}},
		demo.Turn{Texts: []string{"The Mezzanine is on shelf B12 and available."}},
	)
	if err != nil {
		return goerr.Wrap(err, "new llm")
	}
	fmt.Fprintf(w, "model:   %s\n", demo.ModelLabel(live))
	fmt.Fprintf(w, "traces:  %s\n", traceDir)

	toolFactory := func(_ context.Context, proc *agentkit.Process) ([]gollem.Tool, error) {
		if proc.Agent != agentName {
			return nil, nil
		}
		return []gollem.Tool{lookupTool{}}, nil
	}

	reg := agentkit.NewRegistry()
	bot, err := simple.Register(reg, agentName, 1,
		simple.WithSystemPrompt("You answer questions about books."))
	if err != nil {
		return goerr.Wrap(err, "register librarian")
	}

	logger := log.New(w, "", 0)
	k, err := agentkit.New(memory.New(), model, reg,
		agentkit.WithToolFactory(toolFactory),
		agentkit.WithClaimMiddleware(claimTrace(traceDir, logger)),
		agentkit.WithStepMiddleware(stepTrace()),
		agentkit.WithToolCallMiddleware(toolTrace()),
	)
	if err != nil {
		return goerr.Wrap(err, "new kernel")
	}

	pid, err := bot.Spawn(ctx, k, simple.Input{Prompt: "Where is The Mezzanine?"})
	if err != nil {
		return goerr.Wrap(err, "spawn")
	}

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- k.Serve(serveCtx, agentkit.WithPollInterval(20*time.Millisecond))
	}()

	proc, waitErr := demo.WaitProcess(ctx, k, pid, demo.Terminal, time.Minute)
	stop()
	<-served
	if waitErr != nil {
		return goerr.Wrap(waitErr, "wait for the process")
	}
	fmt.Fprintf(w, "status:  %s\n", proc.Status)
	if proc.Status != agentkit.ProcessSucceeded {
		return goerr.New("process did not succeed",
			goerr.V("status", proc.Status), goerr.V("failure", proc.Failure))
	}

	traces, err := loadTraces(traceDir)
	if err != nil {
		return err
	}
	if len(traces) == 0 {
		return goerr.New("no trace was saved", goerr.V("dir", traceDir))
	}
	for _, tr := range traces {
		fmt.Fprintf(w, "\ntrace %s  (process %s, worker %s)\n",
			tr.TraceID, tr.Metadata.Labels["process_id"], tr.Metadata.Labels["worker"])
		printSpan(w, tr.RootSpan, 0)
	}
	if !live {
		fmt.Fprint(w, "\nNo llm_call span above: those come from gollem's LLM client,\n"+
			"and the offline stub is a mock session. Set the Vertex AI env vars to see them.\n")
	}
	return nil
}

func main() {
	dir := flag.String("out", "", "directory to write trace JSON into (default: a temp dir)")
	flag.Parse()

	traceDir := *dir
	if traceDir == "" {
		tmp, err := os.MkdirTemp("", "agentkit-traces-")
		if err != nil {
			log.Fatalf("tracing: %+v", err)
		}
		traceDir = tmp
	}
	if err := run(context.Background(), os.Stdout, traceDir); err != nil {
		log.Fatalf("tracing: %+v", err)
	}
}
