// Command fanout runs a planner that breaks a goal into tasks, executes each
// task as its own child Process in parallel, and summarizes the results.
//
// Two things are worth watching. Children are inserted as part of the parent's
// transition commit, so a crash never leaves an orphan or a duplicate. And the
// planner and the summarizer are bound to named model roles while the task
// workers use the kernel's default model, which is how you point a stronger
// model at planning and a cheaper one at the tasks.
//
// Run it from the examples module: `cd examples && go run ./fanout`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/planexec"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/m-mizutani/goerr/v2"
)

const (
	plannerAgent agentkit.AgentName = "researcher"
	taskAgent    agentkit.AgentName = "task-worker"

	defaultGoal = "Evaluate whether to adopt a durable agent runtime."

	// defaultMaxLLMCalls bounds one Process, not the whole tree -- see budget.
	defaultMaxLLMCalls = 12
)

func main() {
	goal := flag.String("goal", defaultGoal, "what the planner should decompose")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, *goal); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, w io.Writer, goal string) error {
	return runWith(ctx, w, goal, defaultMaxLLMCalls)
}

// newFanout wires the models, the two agents and the kernel.
//
// Three clients: one bound to planexec.RolePlanner, one to RoleSummarizer, and
// one passed positionally to New as the default that everything else -- here,
// the task workers -- resolves to. Offline that separation also keeps the
// scripts deterministic, since children run in parallel and a single shared
// script would be replayed in an unpredictable order.
func newFanout(ctx context.Context, w io.Writer, maxLLMCalls int) (*agentkit.Kernel, agentkit.Agent[planexec.Input], error) {
	var none agentkit.Agent[planexec.Input]

	plannerModel, live, err := demo.NewLLM(ctx,
		demo.Turn{Texts: []string{`{"tasks":[` +
			`{"title":"Prior art","prompt":"Which runtimes already solve durable execution?"},` +
			`{"title":"Trade-offs","prompt":"What does checkpointing every transition cost?"},` +
			`{"title":"Risks","prompt":"Where does at-least-once execution hurt?"}` +
			`]}`}},
		demo.Turn{Texts: []string{`{"action":"finalize"}`}},
	)
	if err != nil {
		return nil, none, goerr.Wrap(err, "new planner llm")
	}
	fmt.Fprintf(w, "model:   %s\n", demo.ModelLabel(live))

	summarizerModel, _, err := demo.NewLLM(ctx, demo.Turn{Texts: []string{
		"Durable execution is worth adopting where a rerun is cheaper than a lost run.",
	}})
	if err != nil {
		return nil, none, goerr.Wrap(err, "new summarizer llm")
	}

	taskModel, _, err := demo.NewLLM(ctx, demo.Turn{Texts: []string{
		"Checkpoint after every transition and let any worker resume the work.",
	}})
	if err != nil {
		return nil, none, goerr.Wrap(err, "new task llm")
	}

	// budget stops a Process that keeps calling the model. The metrics it sees
	// are that Process's own spend plus every child that has finished, counted
	// once each, so on a fan-out parent this is a budget for the subtree rather
	// than for the planner's own calls. A child's own limit binds it while it
	// runs; the parent learns what a child cost when that child terminates.
	//
	// The consequence to design around: a parent can pass this check, spawn
	// children that spend the rest of the budget, and then trip on its very next
	// transition. That is the limiter working -- the tree really did spend it --
	// but it means a parent's recorded LLMCalls can end up above the cap.
	//
	// The comparison is >= because the limiter runs *before* the call it is
	// deciding about: at maxLLMCalls calls already spent, the next one is the
	// one over the line.
	//
	// LimitStop takes the reason as a string rather than an error: the kernel is
	// what turns it into ErrLimitExceeded or a Failure message, so a limiter
	// never has to decide which error type to raise.
	//
	// The budget is registered per agent, because Limit is a Strategy method: the
	// same closure goes to both agents here, but nothing forces that -- the
	// planner and the task workers could each carry their own.
	budget := func(_ context.Context, proc *agentkit.Process, m agentkit.Metrics) agentkit.LimitDecision {
		if m.LLMCalls >= int64(maxLLMCalls) {
			return agentkit.LimitStop(fmt.Sprintf(
				"llm call budget exhausted: process %s spent %d of %d",
				proc.ID, m.LLMCalls, maxLLMCalls))
		}
		return agentkit.LimitPass()
	}

	reg := agentkit.NewRegistry()
	worker, err := simple.Register(reg, taskAgent, 1,
		simple.WithSystemPrompt("Answer the task in two sentences."),
		simple.WithLimiter(budget))
	if err != nil {
		return nil, none, goerr.Wrap(err, "register task worker")
	}

	// planexec is generic over the task agent's input type: makeInput adapts a
	// planned task into whatever that agent takes, so any Agent[T] can be one.
	researcher, err := planexec.Register(reg, plannerAgent, 1, worker,
		func(spec planexec.TaskSpec) (simple.Input, error) {
			return simple.Input{Prompt: spec.Prompt}, nil
		},
		planexec.WithMaxParallelTasks(3),
		planexec.WithMaxRounds(2),
		planexec.WithLimiter(budget),
	)
	if err != nil {
		return nil, none, goerr.Wrap(err, "register researcher")
	}

	k, err := agentkit.New(memory.New(), taskModel, reg,
		agentkit.WithModelRole(planexec.RolePlanner, plannerModel),
		agentkit.WithModelRole(planexec.RoleSummarizer, summarizerModel),
	)
	if err != nil {
		return nil, none, goerr.Wrap(err, "new kernel")
	}
	return k, researcher, nil
}

func runWith(ctx context.Context, w io.Writer, goal string, maxLLMCalls int) error {
	k, researcher, err := newFanout(ctx, w, maxLLMCalls)
	if err != nil {
		return err
	}

	pid, err := researcher.Spawn(ctx, k, planexec.Input{Prompt: goal})
	if err != nil {
		return goerr.Wrap(err, "spawn")
	}
	fmt.Fprintf(w, "spawned: %s\n", pid)

	// Children are ordinary Processes, so parallelism is just more workers.
	serveCtx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- k.Serve(serveCtx,
			agentkit.WithPollConcurrency(4),
			agentkit.WithPollInterval(20*time.Millisecond))
	}()

	proc, waitErr := demo.WaitProcess(ctx, k, pid, demo.Terminal, 2*time.Minute)
	stop()
	<-served
	if waitErr != nil {
		return goerr.Wrap(waitErr, "wait for the process")
	}

	fmt.Fprintf(w, "status:  %s\n", proc.Status)
	if proc.Status != agentkit.ProcessSucceeded {
		if proc.Failure != nil {
			fmt.Fprintf(w, "failure: %s: %s\n", proc.Failure.Code, proc.Failure.Message)
		}
		return goerr.New("process did not succeed",
			goerr.V("status", proc.Status), goerr.V("failure", proc.Failure))
	}

	var out planexec.Output
	if err := json.Unmarshal(proc.Output, &out); err != nil {
		return goerr.Wrap(err, "decode output")
	}
	for _, text := range out.Summary {
		fmt.Fprintf(w, "summary: %s\n", text)
	}
	return printTasks(ctx, k, w, out.Tasks)
}

// printTasks shows each task's own Process. planexec carries the child's Output
// through verbatim; reading it means knowing what the task agent produced,
// which the spawner does and the kernel does not.
func printTasks(ctx context.Context, k *agentkit.Kernel, w io.Writer, tasks []planexec.TaskResult) error {
	for i, task := range tasks {
		fmt.Fprintf(w, "task %d:  [%s] %s\n", i+1, task.Status, task.Title)

		child, err := k.GetProcess(ctx, task.ProcessID)
		if err != nil {
			return goerr.Wrap(err, "get child process", goerr.V("pid", task.ProcessID))
		}
		fmt.Fprintf(w, "         root=%s\n", child.RootID)

		if child.Status != agentkit.ProcessSucceeded {
			continue
		}
		var childOut simple.Output
		if err := json.Unmarshal(child.Output, &childOut); err != nil {
			return goerr.Wrap(err, "decode child output", goerr.V("pid", task.ProcessID))
		}
		for _, text := range childOut.Texts {
			fmt.Fprintf(w, "         %s\n", text)
		}
	}
	return nil
}
