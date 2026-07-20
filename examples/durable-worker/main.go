// Command durable-worker separates submitting work from doing it, and survives
// the worker dying in the middle.
//
//	go run ./examples/durable-worker submit -topic "durability"
//	go run ./examples/durable-worker work -pid <id> -crash-after 2   # dies
//	go run ./examples/durable-worker status -pid <id>                # partial progress
//	go run ./examples/durable-worker work -pid <id>                  # resumes, finishes
//
// The crash happens after the LLM call but before the transition commits, so
// that round runs again on resume. That is what at-least-once means in
// practice, and why a tool with an external side effect has to be idempotent.
//
// The filesystem repository holds an exclusive lock on its directory, so only
// one of these commands can run at a time. It is a single-process reference
// implementation; running real workers on more than one host needs a Repository
// of your own (see docs/persistence.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/filesystem"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

const (
	agentName agentkit.AgentName = "researcher"

	defaultTopic  = "why durable execution matters for LLM agents"
	defaultRounds = 3

	// Short enough that the resume demo does not sit idle for a minute. A real
	// deployment sets the lease well above the time one transition takes.
	demoLease        = 5 * time.Second
	demoPollInterval = 200 * time.Millisecond
)

type input struct {
	Topic  string
	Rounds int
}

// state is the checkpoint. Done counts committed rounds only, which is exactly
// why a crashed round repeats.
type state struct {
	Topic string   `json:"topic"`
	Total int      `json:"total"`
	Done  int      `json:"done"`
	Notes []string `json:"notes,omitempty"`
}

type output struct {
	Notes []string `json:"notes"`
}

type strategy struct {
	crashAfter int // 0 disables the simulated crash.
	log        io.Writer
}

func (s *strategy) Version() int { return 1 }

func (s *strategy) Init(in input) (state, error) {
	if in.Topic == "" {
		return state{}, goerr.New("topic is required")
	}
	if in.Rounds < 1 {
		return state{}, goerr.New("rounds must be positive", goerr.V("rounds", in.Rounds))
	}
	return state{Topic: in.Topic, Total: in.Rounds}, nil
}

func (s *strategy) Step(ctx context.Context, sys agentkit.Syscalls, st state) (state, agentkit.Decision, error) {
	if st.Done >= st.Total {
		raw, err := json.Marshal(output{Notes: st.Notes})
		if err != nil {
			return st, agentkit.Decision{}, goerr.Wrap(err, "marshal output")
		}
		return st, agentkit.Done(raw), nil
	}

	res, err := sys.Generate(ctx, []gollem.Input{gollem.Text(fmt.Sprintf(
		"Round %d of %d on %q: give one short observation.", st.Done+1, st.Total, st.Topic))})
	if err != nil {
		return st, agentkit.Decision{}, err
	}
	st.Notes = append(st.Notes, strings.Join(res.Texts, " "))
	st.Done++
	fmt.Fprintf(s.log, "  round %d/%d done\n", st.Done, st.Total)

	if s.crashAfter > 0 && st.Done == s.crashAfter {
		fmt.Fprintf(s.log,
			"simulated crash: exiting after the LLM call for round %d, before it commits\n", st.Done)
		os.Exit(1)
	}

	// One transition per round: the checkpoint between them is what makes the
	// work resumable.
	return st, agentkit.Continue(), nil
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
	if err := dispatch(context.Background(), os.Stdout, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func defaultDir() string {
	return filepath.Join(os.TempDir(), "agentkit-durable-worker-example")
}

func dispatch(ctx context.Context, w io.Writer, args []string) error {
	if len(args) == 0 {
		return goerr.New("usage: durable-worker <submit|work|status> [flags]")
	}

	switch args[0] {
	case "submit":
		fs := flag.NewFlagSet("submit", flag.ContinueOnError)
		dir := fs.String("dir", defaultDir(), "store directory")
		topic := fs.String("topic", defaultTopic, "what the agent should work on")
		rounds := fs.Int("rounds", defaultRounds, "how many checkpointed rounds to run")
		if err := fs.Parse(args[1:]); err != nil {
			return goerr.Wrap(err, "parse submit flags")
		}
		_, err := runSubmit(ctx, w, *dir, *topic, *rounds)
		return err

	case "work":
		fs := flag.NewFlagSet("work", flag.ContinueOnError)
		dir := fs.String("dir", defaultDir(), "store directory")
		pid := fs.String("pid", "", "stop once this process finishes; empty serves until interrupted")
		crashAfter := fs.Int("crash-after", 0, "exit before committing this round (0 disables)")
		if err := fs.Parse(args[1:]); err != nil {
			return goerr.Wrap(err, "parse work flags")
		}
		return runWork(ctx, w, *dir, agentkit.ProcessID(*pid), *crashAfter)

	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		dir := fs.String("dir", defaultDir(), "store directory")
		pid := fs.String("pid", "", "the process to inspect")
		if err := fs.Parse(args[1:]); err != nil {
			return goerr.Wrap(err, "parse status flags")
		}
		if *pid == "" {
			return goerr.New("status requires -pid")
		}
		return runStatus(ctx, w, *dir, agentkit.ProcessID(*pid))

	default:
		return goerr.New("unknown subcommand", goerr.V("subcommand", args[0]))
	}
}

// newKernel opens the store and builds a kernel over it. The caller owns the
// returned close function; the store's lock is held until it runs.
func newKernel(ctx context.Context, w io.Writer, dir string, s *strategy) (*agentkit.Kernel, agentkit.Agent[input], func() error, error) {
	model, live, err := demo.NewLLM(ctx, demo.Turn{Texts: []string{
		"Durable execution means the work survives the worker, not just the request.",
	}})
	if err != nil {
		return nil, agentkit.Agent[input]{}, nil, goerr.Wrap(err, "new llm")
	}
	fmt.Fprintf(w, "model:   %s\n", demo.ModelLabel(live))

	repo, err := filesystem.New(dir)
	if err != nil {
		return nil, agentkit.Agent[input]{}, nil, goerr.Wrap(err, "open store", goerr.V("dir", dir))
	}

	reg := agentkit.NewRegistry()
	researcher, err := agentkit.Register(reg, agentName, 1, s)
	if err != nil {
		return nil, agentkit.Agent[input]{}, nil, errors.Join(err, repo.Close())
	}

	k, err := agentkit.New(repo, model, reg)
	if err != nil {
		return nil, agentkit.Agent[input]{}, nil, errors.Join(err, repo.Close())
	}
	return k, researcher, repo.Close, nil
}

// runSubmit writes the work down and walks away. Nothing executes here: the
// process that accepts a request need not be the one that runs it, or even be
// alive at the same time.
func runSubmit(ctx context.Context, w io.Writer, dir, topic string, rounds int) (pid agentkit.ProcessID, err error) {
	k, researcher, closeStore, err := newKernel(ctx, w, dir, &strategy{log: io.Discard})
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := closeStore(); cerr != nil && err == nil {
			err = goerr.Wrap(cerr, "close store")
		}
	}()

	// Init runs inside Spawn, so a bad input fails right here rather than
	// becoming a Process that fails later.
	pid, err = researcher.Spawn(ctx, k, input{Topic: topic, Rounds: rounds})
	if err != nil {
		return "", goerr.Wrap(err, "spawn")
	}

	fmt.Fprintf(w, "store:   %s\n", dir)
	fmt.Fprintf(w, "spawned: %s\n", pid)
	return pid, nil
}

// runWork claims whatever is runnable in the store. Passing -pid only makes the
// demo terminate; a real worker does not know which processes it will get.
func runWork(ctx context.Context, w io.Writer, dir string, pid agentkit.ProcessID, crashAfter int) (err error) {
	k, _, closeStore, err := newKernel(ctx, w, dir, &strategy{crashAfter: crashAfter, log: w})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeStore(); cerr != nil && err == nil {
			err = goerr.Wrap(cerr, "close store")
		}
	}()

	serveOpts := []agentkit.ServeOption{
		agentkit.WithLease(demoLease),
		agentkit.WithPollInterval(demoPollInterval),
	}

	if pid == "" {
		sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt)
		defer stopSignals()
		fmt.Fprintln(w, "serving until interrupted")
		if serveErr := k.Serve(sigCtx, serveOpts...); serveErr != nil && !errors.Is(serveErr, context.Canceled) {
			return goerr.Wrap(serveErr, "serve")
		}
		return nil
	}

	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- k.Serve(serveCtx, serveOpts...) }()

	// A process left running by a dead worker only becomes claimable once its
	// lease expires. The lease was set when the dead worker claimed it, so what
	// is left to wait out is whatever remains of it -- at most demoLease, and
	// nothing at all if enough time has already passed.
	proc, waitErr := demo.WaitProcess(ctx, k, pid, demo.Terminal, 2*time.Minute)
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
	return nil
}

// runStatus reads the store without running anything, so it needs no model.
func runStatus(ctx context.Context, w io.Writer, dir string, pid agentkit.ProcessID) (err error) {
	repo, err := filesystem.New(dir)
	if err != nil {
		return goerr.Wrap(err, "open store", goerr.V("dir", dir))
	}
	defer func() {
		if cerr := repo.Close(); cerr != nil && err == nil {
			err = goerr.Wrap(cerr, "close store")
		}
	}()

	proc, err := repo.GetProcess(ctx, pid)
	if err != nil {
		return goerr.Wrap(err, "get process", goerr.V("pid", pid))
	}

	fmt.Fprintf(w, "status:        %s\n", proc.Status)
	fmt.Fprintf(w, "committed seq: %d\n", proc.StateSeq)
	fmt.Fprintf(w, "step attempts: %d\n", proc.StepAttempts)
	fmt.Fprintf(w, "metrics:       %v\n", proc.Metrics)
	if proc.Failure != nil {
		fmt.Fprintf(w, "failure:       %s %s\n", proc.Failure.Code, proc.Failure.Message)
	}
	if proc.Status == agentkit.ProcessSucceeded {
		var out output
		if err := json.Unmarshal(proc.Output, &out); err != nil {
			return goerr.Wrap(err, "decode output")
		}
		for i, note := range out.Notes {
			fmt.Fprintf(w, "note %d:        %s\n", i+1, note)
		}
	}
	return nil
}
