// Command semaphore limits how many Processes run at once per key, using
// WithSemaphoreKey at registration and WithSemaphore at spawn.
//
// Run it from the examples module: `cd examples && go run ./semaphore`.
// Without Vertex AI configured it answers from a script, so it works offline.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

// semaphoreKey is declared once, at registration. Every Process of this agent
// runs under it, and each spawn says which INSTANCE of it — which document —
// it belongs to.
const semaphoreKey = "document"

// input is what a spawn supplies: which document to work on and what to ask
// about it. The document id becomes the semaphore value.
type input struct {
	Doc    string `json:"doc"`
	Prompt string `json:"prompt"`
}

type state struct {
	Doc    string `json:"doc"`
	Prompt string `json:"prompt"`
}

type output struct {
	Doc  string `json:"doc"`
	Text string `json:"text"`
}

// editor is a one-transition strategy: generate, then finish. It is written out
// by hand rather than reusing strategy/simple because a bundled strategy has no
// way to pass a RegisterOption through, and WithSemaphoreKey is one.
//
// work is how long each Step takes. Offline the stub answers instantly, and
// Steps that return immediately never overlap even when the runtime is willing
// to run them at once — so there would be nothing for the concurrency
// measurement below to observe. A live model supplies its own latency.
type editor struct{ work time.Duration }

func (editor) Version() int { return 1 }

func (editor) Init(in input) (state, error) {
	if in.Doc == "" {
		return state{}, goerr.New("doc is required")
	}
	return state{Doc: in.Doc, Prompt: in.Prompt}, nil
}

func (editor) Limit(context.Context, *agentkit.Process, agentkit.Metrics) agentkit.LimitDecision {
	return agentkit.LimitPass()
}

func (e editor) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision[output], error) {
	res, err := sys.Generate(ctx, []gollem.Input{gollem.Text(st.Prompt)})
	if err != nil {
		return st, agentkit.Decision[output]{}, goerr.Wrap(err, "generate")
	}
	// Stand-in for real work, so the concurrency measurement has a window to see.
	// A Step must not block for long in a real agent: it holds the claim's lease
	// the whole time.
	if e.work > 0 {
		select {
		case <-ctx.Done():
			return st, agentkit.Decision[output]{}, ctx.Err()
		case <-time.After(e.work):
		}
	}
	text := ""
	if len(res.Texts) > 0 {
		text = res.Texts[0]
	}
	return st, agentkit.Done(output{Doc: st.Doc, Text: text}), nil
}

func (editor) EncodeState(st state) ([]byte, error) { return json.Marshal(st) }

func (editor) DecodeState(_ int, raw []byte) (state, error) {
	var st state
	err := json.Unmarshal(raw, &st)
	return st, err
}

func (editor) EncodeOutput(out output) ([]byte, error) { return json.Marshal(out) }

// peak measures how many Steps ran at once, per document. The count goes up on
// the way into Step and down on the way out, so what it reports is observed
// rather than inferred.
type peak struct {
	mu   sync.Mutex
	live map[string]int
	max  map[string]int
	all  int
	// allMax is the peak across every document at once, which is what shows that
	// different values really do run in parallel.
	allMax int
}

func newPeak() *peak {
	return &peak{live: map[string]int{}, max: map[string]int{}}
}

func (p *peak) middleware() agentkit.StepMiddleware {
	return func(next agentkit.StepHandler) agentkit.StepHandler {
		return func(ctx context.Context, req *agentkit.StepRequest) (*agentkit.StepResult, error) {
			doc := ""
			if req.Process.Semaphore != nil {
				doc = req.Process.Semaphore.Value
			}
			p.enter(doc)
			defer p.exit(doc)
			return next(ctx, req)
		}
	}
}

func (p *peak) enter(doc string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live[doc]++
	p.all++
	if p.live[doc] > p.max[doc] {
		p.max[doc] = p.live[doc]
	}
	if p.all > p.allMax {
		p.allMax = p.all
	}
}

func (p *peak) exit(doc string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live[doc]--
	p.all--
}

func (p *peak) get(doc string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max[doc]
}

func (p *peak) overall() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allMax
}

func main() {
	docs := flag.Int("docs", 2, "how many documents to work on")
	perDoc := flag.Int("per-doc", 3, "how many processes to spawn per document")
	work := flag.Duration("work", 50*time.Millisecond, "how long each Step takes, so concurrency is observable offline")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, *docs, *perDoc, *work); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, w io.Writer, docs, perDoc int, work time.Duration) error {
	if docs < 1 || perDoc < 1 {
		return goerr.New("docs and per-doc must both be at least 1",
			goerr.V("docs", docs), goerr.V("per_doc", perDoc))
	}

	turns := make([]demo.Turn, 0, docs*perDoc)
	for range docs * perDoc {
		turns = append(turns, demo.Turn{Texts: []string{"Tightened the wording."}})
	}
	model, live, err := demo.NewLLM(ctx, turns...)
	if err != nil {
		return goerr.Wrap(err, "new llm")
	}
	fmt.Fprintf(w, "model:   %s\n\n", demo.ModelLabel(live))

	// One slot per document: Processes working on the same document run one at a
	// time. The slot count lives HERE and not on the spawns below, so two spawns
	// cannot disagree about what the limit is.
	reg := agentkit.NewRegistry()
	ag, err := agentkit.Register(reg, "editor", 1, editor{work: work},
		agentkit.WithSemaphoreKey[output](semaphoreKey, 1))
	if err != nil {
		return goerr.Wrap(err, "register editor")
	}

	measured := newPeak()
	k, err := agentkit.New(memory.New(), model, reg,
		agentkit.WithStepMiddleware(measured.middleware()))
	if err != nil {
		return goerr.Wrap(err, "new kernel")
	}

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		// One poll loop drives its claims one after another, so a single-loop
		// worker would serialize everything and say nothing about the semaphore.
		// The point here is that DIFFERENT documents may run at once, so the
		// worker is given room for one claim per document.
		served <- k.Serve(serveCtx,
			agentkit.WithPollInterval(20*time.Millisecond),
			agentkit.WithPollConcurrency(docs))
	}()
	defer func() { stop(); <-served }()

	docIDs := make([]string, 0, docs)
	for i := range docs {
		docIDs = append(docIDs, fmt.Sprintf("doc-%c", 'a'+i))
	}

	// A spawn against a full document SUCCEEDS. The Process is written as pending
	// and waits for a slot; nothing is refused and nothing blocks here.
	var pids []agentkit.ProcessID
	for _, doc := range docIDs {
		for range perDoc {
			pid, serr := ag.Spawn(ctx, k, input{Doc: doc, Prompt: "Tighten the opening paragraph."},
				agentkit.WithSemaphore(semaphoreKey, doc))
			if serr != nil {
				return goerr.Wrap(serr, "spawn", goerr.V("doc", doc))
			}
			pids = append(pids, pid)
		}
	}
	fmt.Fprintf(w, "spawned %d processes across %d documents, %d each\n", len(pids), docs, perDoc)

	// The backlog is visible: Spawn never refuses, so this is what a caller
	// throttles on. It is an observation, not a reservation — a slot reported free
	// may be taken before you act on it.
	for _, doc := range docIDs {
		st, serr := k.GetSemaphoreStatus(ctx, semaphoreKey, doc)
		if serr != nil {
			return goerr.Wrap(serr, "semaphore status", goerr.V("doc", doc))
		}
		fmt.Fprintf(w, "  %s  held=%d waiting=%d slots=%d\n", doc, st.Held, st.Waiting, st.Slots)
	}

	for _, pid := range pids {
		proc, werr := demo.WaitProcess(ctx, k, pid, demo.Terminal, time.Minute)
		if werr != nil {
			return goerr.Wrap(werr, "wait for the process", goerr.V("process", pid))
		}
		if proc.Status != agentkit.ProcessSucceeded {
			return goerr.New("process did not succeed",
				goerr.V("process", pid), goerr.V("status", proc.Status), goerr.V("failure", proc.Failure))
		}
	}

	// Deliberately NOT printed: the order the documents were worked in. Slot
	// acquisition order is not guaranteed — eager dispatch can overtake an older
	// waiting row — so printing an order would read as a promise the runtime does
	// not make, and would make this output non-reproducible.
	fmt.Fprintf(w, "\npeak concurrent Step per document\n")
	for _, doc := range docIDs {
		fmt.Fprintf(w, "  %s  %d   (slots 1)\n", doc, measured.get(doc))
	}
	fmt.Fprintf(w, "peak concurrent Step overall  %d\n", measured.overall())

	fmt.Fprintf(w, "\nall %d processes succeeded\n", len(pids))
	for _, doc := range docIDs {
		st, serr := k.GetSemaphoreStatus(ctx, semaphoreKey, doc)
		if serr != nil {
			return goerr.Wrap(serr, "semaphore status", goerr.V("doc", doc))
		}
		fmt.Fprintf(w, "  %s  held=%d waiting=%d slots=%d\n", doc, st.Held, st.Waiting, st.Slots)
	}
	return nil
}
