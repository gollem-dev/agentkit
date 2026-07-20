// Command quickstart is the shortest useful agentkit program: register an
// agent, build a kernel, spawn a Process, and run a worker until it finishes.
//
// Run it from the examples module: `cd examples && go run ./quickstart`.
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
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/m-mizutani/goerr/v2"
)

const defaultPrompt = "Explain what a durable agent runtime is, in two sentences."

func main() {
	prompt := flag.String("prompt", defaultPrompt, "the prompt handed to the agent")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, *prompt); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, w io.Writer, prompt string) error {
	model, live, err := demo.NewLLM(ctx, demo.Turn{Texts: []string{
		"A durable agent runtime checkpoints an agent after every step, so a crash resumes " +
			"the work instead of restarting it. The state lives in a store rather than in one " +
			"process's memory, so any worker can pick it up.",
	}})
	if err != nil {
		return goerr.Wrap(err, "new llm")
	}
	fmt.Fprintf(w, "model:   %s\n", demo.ModelLabel(live))

	// 1. Register. The typed handle it returns is the only way to spawn, so the
	// input type is checked at compile time.
	reg := agentkit.NewRegistry()
	assistant, err := simple.Register(reg, "assistant", 1,
		simple.WithSystemPrompt("Answer concisely and concretely."))
	if err != nil {
		return goerr.Wrap(err, "register assistant")
	}

	// 2. Construct the kernel: repository, default model, registry.
	k, err := agentkit.New(memory.New(), model, reg)
	if err != nil {
		return goerr.Wrap(err, "new kernel")
	}

	// 3. Spawn. This writes a pending Process and returns; nothing executes yet.
	pid, err := assistant.Spawn(ctx, k, simple.Input{Prompt: prompt})
	if err != nil {
		return goerr.Wrap(err, "spawn")
	}
	fmt.Fprintf(w, "spawned: %s\n", pid)

	// 4. Serve. A real deployment runs this in its own process, and in as many
	// of them as it likes. Here it runs in the background just long enough for
	// this one Process to finish.
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

	// Output is whatever bytes the strategy passed to Done; agentkit never
	// parses it. simple happens to use JSON.
	var out simple.Output
	if err := json.Unmarshal(proc.Output, &out); err != nil {
		return goerr.Wrap(err, "decode output")
	}
	for _, text := range out.Texts {
		fmt.Fprintf(w, "answer:  %s\n", text)
	}
	fmt.Fprintf(w, "metrics: %v\n", proc.Metrics)
	return nil
}
