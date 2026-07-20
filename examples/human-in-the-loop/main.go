// Command human-in-the-loop implements a Strategy that stops and asks a person
// before it acts, then resumes from the answer.
//
// The Process parks in `waiting` with a durable question on it. Nothing is held
// in memory in the meantime: the answer can arrive minutes later, from another
// process, and a different worker can pick the work up.
//
// This is a confirmation, not a security gate. Nothing stops a strategy from
// calling a tool without asking first -- a hard allow/deny decision belongs
// inside the tool's Run (see examples/tools).
//
// Run it with `go run ./examples/human-in-the-loop`, or with `-answer no`.
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
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

const (
	agentName agentkit.AgentName = "approver"

	// confirmKey names the wait. Keys are Process-local and chosen by the
	// strategy, and they are what Respond addresses.
	confirmKey agentkit.AwaitKey = "confirm"

	// approvalRequested is emitted so an operator UI can notice the question
	// without polling every Process.
	approvalRequested agentkit.EventType = "approval.requested"

	confirmDeadline = 10 * time.Minute

	defaultRequest = "delete the staging database snapshot older than 30 days"
)

type input struct {
	Request string
}

// state is what gets checkpointed between transitions. It holds no live
// references, because the Process that resumes may be on another machine.
type state struct {
	Request string `json:"request"`
	Asked   bool   `json:"asked"`
	Note    string `json:"note,omitempty"`
}

type output struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note"`
}

type strategy struct {
	deadline time.Duration
}

func (s *strategy) Version() int { return 1 }

// Init runs inside Spawn, purely, so a bad input is an error the caller gets
// synchronously rather than a Process that fails later.
func (s *strategy) Init(in input) (state, error) {
	if in.Request == "" {
		return state{}, goerr.New("request is required")
	}
	return state{Request: in.Request}, nil
}

func (s *strategy) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	if !st.Asked {
		payload := []byte("Approve: " + st.Request + " (yes/no)")
		// Emitted events are buffered and committed with the transition, so the
		// event and the wait become visible together or not at all.
		if err := sys.Emit(ctx, approvalRequested, payload); err != nil {
			return st, agentkit.Decision{}, goerr.Wrap(err, "emit approval request")
		}
		st.Asked = true
		return st, agentkit.Suspend(
			agentkit.Question(confirmKey, payload, agentkit.WithDeadline(sys.Now().Add(s.deadline))),
		), nil
	}

	// Await reads the snapshot taken when this transition started.
	aw, ok := sys.Await(confirmKey)
	if !ok {
		return st, agentkit.Decision{}, goerr.New("confirmation await missing",
			goerr.V("key", confirmKey))
	}

	switch aw.Status {
	case agentkit.AwaitOpen:
		// Reached when this transition re-runs before anyone answered. Suspend
		// with no specs re-parks the Process without duplicating the wait.
		return st, agentkit.Suspend(), nil

	case agentkit.AwaitResponded:
		approved := strings.EqualFold(strings.TrimSpace(string(aw.Response)), "yes")
		if !approved {
			st.Note = refusalNote(aw.RespondedBy)
			return done(st, output{Approved: false, Note: st.Note})
		}
		res, err := sys.Generate(ctx, []gollem.Input{
			gollem.Text("Confirm in one line that this was carried out: " + st.Request),
		})
		if err != nil {
			return st, agentkit.Decision{}, err
		}
		st.Note = strings.Join(res.Texts, " ")
		return done(st, output{Approved: true, Note: st.Note})

	case agentkit.AwaitExpired:
		st.Note = "no answer before the deadline; treated as a refusal"
		return done(st, output{Approved: false, Note: st.Note})

	default:
		return st, agentkit.Fail(agentkit.FailureStrategyError,
			"confirmation await "+string(aw.Status)), nil
	}
}

func done(st state, out output) (state, agentkit.Decision, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return st, agentkit.Decision{}, goerr.Wrap(err, "marshal output")
	}
	return st, agentkit.Done(raw), nil
}

func refusalNote(by string) string {
	if by == "" {
		return "refused"
	}
	return "refused by " + by
}

func (s *strategy) EncodeState(st state) ([]byte, error) { return json.Marshal(st) }

func (s *strategy) DecodeState(_ int, raw []byte) (state, error) {
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, goerr.Wrap(err, "decode state")
	}
	return st, nil
}

func main() {
	request := flag.String("request", defaultRequest, "the action to ask about")
	answer := flag.String("answer", "yes", `the operator's answer ("yes" or anything else)`)
	flag.Parse()

	if err := run(context.Background(), os.Stdout, *request, *answer); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, w io.Writer, request, answer string) error {
	model, live, err := demo.NewLLM(ctx, demo.Turn{
		Texts: []string{"Done. The snapshot has been deleted."},
	})
	if err != nil {
		return goerr.Wrap(err, "new llm")
	}
	fmt.Fprintf(w, "model:    %s\n", demo.ModelLabel(live))

	reg := agentkit.NewRegistry()
	approver, err := agentkit.Register(reg, agentName, 1, &strategy{deadline: confirmDeadline})
	if err != nil {
		return goerr.Wrap(err, "register approver")
	}

	k, err := agentkit.New(memory.New(), model, reg)
	if err != nil {
		return goerr.Wrap(err, "new kernel")
	}

	pid, err := approver.Spawn(ctx, k, input{Request: request})
	if err != nil {
		return goerr.Wrap(err, "spawn")
	}

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- k.Serve(serveCtx, agentkit.WithPollInterval(20*time.Millisecond))
	}()
	defer func() { stop(); <-served }()

	// The Process parks itself; the operator side finds it by looking for open
	// questions rather than by holding a handle to anything.
	if _, err := demo.WaitProcess(ctx, k, pid, demo.Waiting, 30*time.Second); err != nil {
		return goerr.Wrap(err, "wait for the question")
	}
	if err := printOpenQuestions(ctx, k, w, pid); err != nil {
		return err
	}

	// Only an open await accepts a response, and the first responder wins: a
	// second Respond for the same key gets ErrAwaitClosed.
	fmt.Fprintf(w, "answer:   %s\n", answer)
	if err := k.Respond(ctx, pid, confirmKey, []byte(answer),
		agentkit.WithRespondedBy("user:alice")); err != nil {
		return goerr.Wrap(err, "respond")
	}

	proc, err := demo.WaitProcess(ctx, k, pid, demo.Terminal, time.Minute)
	if err != nil {
		return goerr.Wrap(err, "wait for the process")
	}

	fmt.Fprintf(w, "status:   %s\n", proc.Status)
	if proc.Status != agentkit.ProcessSucceeded {
		return goerr.New("process did not succeed",
			goerr.V("status", proc.Status), goerr.V("failure", proc.Failure))
	}

	var out output
	if err := json.Unmarshal(proc.Output, &out); err != nil {
		return goerr.Wrap(err, "decode output")
	}
	fmt.Fprintf(w, "approved: %t\n", out.Approved)
	fmt.Fprintf(w, "note:     %s\n", out.Note)

	return printEvents(ctx, k, w, pid)
}

func printOpenQuestions(ctx context.Context, k *agentkit.Kernel, w io.Writer, pid agentkit.ProcessID) error {
	awaits, err := k.ListAwaits(ctx, pid)
	if err != nil {
		return goerr.Wrap(err, "list awaits")
	}
	for _, aw := range awaits {
		if aw.Kind == agentkit.AwaitQuestion && aw.Status == agentkit.AwaitOpen {
			fmt.Fprintf(w, "question: %s\n", string(aw.Question))
		}
	}
	return nil
}

func printEvents(ctx context.Context, k *agentkit.Kernel, w io.Writer, pid agentkit.ProcessID) error {
	events, err := k.ListEvents(ctx, pid)
	if err != nil {
		return goerr.Wrap(err, "list events")
	}
	fmt.Fprintln(w, "events:")
	for _, ev := range events {
		fmt.Fprintf(w, "  %s\n", ev.Type)
	}
	return nil
}
