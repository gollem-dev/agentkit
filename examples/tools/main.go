// Command tools shows the two obligations agentkit hands to whoever writes a
// tool: idempotency, because a transition can run more than once, and
// authorization, because the kernel has no approval gate.
//
// The scripted run has the model try a production deploy first. The policy
// refuses it inside Run, the error goes back to the model as a function
// response, and the model retries against staging.
//
// Run it with `go run ./examples/tools`.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/memory"
	"github.com/gollem-dev/agentkit/strategy/simple"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
)

const agentName agentkit.AgentName = "release-bot"

// deployTool performs at most one deployment per idempotency key.
//
// agentkit deliberately does not hand tools a framework-generated key: under
// non-deterministic replay a positional one would name a different logical
// operation than the original. The key has to come from the meaning of the
// arguments, which only the tool author knows.
type deployTool struct {
	mu   sync.Mutex
	done map[string]bool
	log  io.Writer
}

func newDeployTool(w io.Writer) *deployTool {
	return &deployTool{done: map[string]bool{}, log: w}
}

func (t *deployTool) Spec() gollem.ToolSpec {
	return gollem.ToolSpec{
		Name:        "deploy",
		Description: "Deploy a service version to an environment.",
		Parameters: map[string]*gollem.Parameter{
			"service": {
				Type:        gollem.TypeString,
				Description: "The service to deploy.",
				Required:    true,
			},
			"version": {
				Type:        gollem.TypeString,
				Description: "The version tag to deploy.",
				Required:    true,
			},
			"env": {
				Type:        gollem.TypeString,
				Description: "The target environment.",
				Required:    true,
				Enum:        []string{"staging", "prod"},
			},
		},
	}
}

func (t *deployTool) Run(_ context.Context, args map[string]any) (map[string]any, error) {
	key := fmt.Sprintf("deploy:%v:%v:%v", args["service"], args["version"], args["env"])

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done[key] {
		fmt.Fprintf(t.log, "  deploy: %s already applied, skipping\n", key)
		return map[string]any{"deployed": true, "deduplicated": true}, nil
	}
	t.done[key] = true
	fmt.Fprintf(t.log, "  deploy: %s applied\n", key)
	return map[string]any{"deployed": true, "deduplicated": false}, nil
}

// applied reports whether a deployment with this key actually happened.
func (t *deployTool) applied(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done[key]
}

// guardedTool refuses a call before the inner tool can reach its side effect.
//
// This is where a hard allow/deny decision belongs. A strategy can also ask a
// human first, but that is a confirmation: nothing stops a buggy or manipulated
// strategy from calling the tool without asking. Run is the only path to the
// effect, so it is the only place a refusal cannot be routed around.
type guardedTool struct {
	inner gollem.Tool
	allow func(name string, args map[string]any) error
}

// Spec returns the inner spec unchanged: the guard alters who may call the
// tool, not what the model is told about it.
func (t *guardedTool) Spec() gollem.ToolSpec { return t.inner.Spec() }

func (t *guardedTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	if err := t.allow(t.inner.Spec().Name, args); err != nil {
		return nil, goerr.Wrap(err, "authorization denied")
	}
	return t.inner.Run(ctx, args)
}

// denyProduction is the policy: staging is self-service, production is not.
func denyProduction(name string, args map[string]any) error {
	if args["env"] == "prod" {
		return goerr.New("production deploys require a human operator",
			goerr.V("tool", name), goerr.V("service", args["service"]))
	}
	return nil
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, w io.Writer) error {
	return runWith(ctx, w, newDeployTool(w))
}

func runWith(ctx context.Context, w io.Writer, deploy *deployTool) error {
	// The script: ask for prod (refused), retry on staging (applied), answer.
	model, live, err := demo.NewLLM(ctx,
		demo.Turn{FunctionCalls: []*gollem.FunctionCall{{
			ID:   "call-1",
			Name: "deploy",
			Arguments: map[string]any{
				"service": "api", "version": "v1.4.2", "env": "prod",
			},
		}}},
		demo.Turn{FunctionCalls: []*gollem.FunctionCall{{
			ID:   "call-2",
			Name: "deploy",
			Arguments: map[string]any{
				"service": "api", "version": "v1.4.2", "env": "staging",
			},
		}}},
		demo.Turn{Texts: []string{
			"Deployed api v1.4.2 to staging. The production deploy was refused by policy.",
		}},
	)
	if err != nil {
		return goerr.Wrap(err, "new llm")
	}
	fmt.Fprintf(w, "model:   %s\n", demo.ModelLabel(live))

	// The factory runs once per claim and receives the Process, so the agent
	// kind is the selector. Wrapping here means every call is checked, because
	// the strategy can only reach the tool through what this returns.
	toolFactory := func(_ context.Context, proc *agentkit.Process) ([]gollem.Tool, error) {
		if proc.Agent != agentName {
			return nil, nil
		}
		return []gollem.Tool{&guardedTool{inner: deploy, allow: denyProduction}}, nil
	}

	reg := agentkit.NewRegistry()
	bot, err := simple.Register(reg, agentName, 1,
		simple.WithSystemPrompt("You deploy services. Prefer staging."))
	if err != nil {
		return goerr.Wrap(err, "register release-bot")
	}

	k, err := agentkit.New(memory.New(), model, reg, agentkit.WithToolFactory(toolFactory))
	if err != nil {
		return goerr.Wrap(err, "new kernel")
	}

	pid, err := bot.Spawn(ctx, k, simple.Input{Prompt: "Ship api v1.4.2."})
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

	var out simple.Output
	if err := json.Unmarshal(proc.Output, &out); err != nil {
		return goerr.Wrap(err, "decode output")
	}
	for _, text := range out.Texts {
		fmt.Fprintf(w, "answer:  %s\n", text)
	}

	// Both calls are metered, including the one the policy refused: the kernel
	// counts the attempt, not the outcome.
	fmt.Fprintf(w, "tool calls attempted: %d\n", proc.Metrics[agentkit.MetricToolCalls])
	fmt.Fprintf(w, "prod deploy applied:  %t\n", deploy.applied("deploy:api:v1.4.2:prod"))
	fmt.Fprintf(w, "staging deploy applied: %t\n", deploy.applied("deploy:api:v1.4.2:staging"))
	return nil
}
