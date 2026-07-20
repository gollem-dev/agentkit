// Command tools shows the two obligations agentkit hands to whoever writes a
// tool: idempotency, because a transition can run more than once, and
// authorization, because the kernel has no approval gate.
//
// The scripted run has the model try a production deploy first. The policy
// refuses it inside Run, the error goes back to the model as a function
// response, and the model retries against staging.
//
// What this example can show is how to derive the idempotency key. What it
// cannot show is the half that has to survive a crash: the store behind that
// key lives in memory here, so a replay from a different process would deploy
// twice. See the deployments interface below.
//
// Run it from the examples module: `cd examples && go run ./tools`.
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

// deployments is where the deduplication actually has to happen: somewhere that
// outlives the worker. A replay after a crash comes from a different process, so
// anything the tool remembers in its own memory is already gone by then.
//
// In a real system this is the deploy API's own idempotency support, or a row in
// a database with a unique constraint on the key.
type deployments interface {
	// DeployOnce applies the deployment unless key was applied before, and
	// reports whether this call is the one that did it.
	DeployOnce(ctx context.Context, key string, args map[string]any) (applied bool, err error)
}

// inMemoryDeployments stands in for that store so the example runs on its own.
//
// It is emphatically NOT what makes the tool idempotent: it dies with the
// process, so a replay after a real crash would deploy a second time. Only the
// key is the example here; the store behind it is your infrastructure's job.
type inMemoryDeployments struct {
	mu   sync.Mutex
	done map[string]bool
	log  io.Writer
}

func newInMemoryDeployments(w io.Writer) *inMemoryDeployments {
	return &inMemoryDeployments{done: map[string]bool{}, log: w}
}

func (d *inMemoryDeployments) DeployOnce(_ context.Context, key string, _ map[string]any) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done[key] {
		fmt.Fprintf(d.log, "  deploy: %s already applied, skipping\n", key)
		return false, nil
	}
	d.done[key] = true
	fmt.Fprintf(d.log, "  deploy: %s applied\n", key)
	return true, nil
}

// applied reports whether a deployment with this key actually happened.
func (d *inMemoryDeployments) applied(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.done[key]
}

// deployTool turns a tool call into one logical deployment.
//
// agentkit deliberately does not hand tools a framework-generated key: under
// non-deterministic replay a positional one would name a different logical
// operation than the original. The key has to come from the meaning of the
// arguments, which only the tool author knows.
type deployTool struct {
	store deployments
}

func newDeployTool(store deployments) *deployTool {
	return &deployTool{store: store}
}

// deploymentKey names the logical operation. The values are quoted rather than
// pasted together, because a plain "a:b:c" join lets an argument containing a
// separator collide with a different tuple -- and a colliding key silently
// skips a deployment that should have happened.
func deploymentKey(args map[string]any) string {
	return fmt.Sprintf("deploy:%q:%q:%q", args["service"], args["version"], args["env"])
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

func (t *deployTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	key := deploymentKey(args)
	applied, err := t.store.DeployOnce(ctx, key, args)
	if err != nil {
		return nil, goerr.Wrap(err, "deploy", goerr.V("key", key))
	}
	return map[string]any{"deployed": true, "deduplicated": !applied}, nil
}

// guardedTool refuses a call before the inner tool can reach its side effect.
//
// This is where a hard allow/deny decision belongs. A strategy can also ask a
// human first, but that is a confirmation: nothing stops a buggy or manipulated
// strategy from calling the tool without asking. Run is the only path to the
// effect, so it is the only place a refusal cannot be routed around.
//
// A ToolCallMiddleware can refuse a call too, and being registered once for
// every agent it is the better home for a policy that spans them. It is still a
// chokepoint rather than a gate: it sees calls made through Syscalls.CallTool,
// and a strategy holding a gollem.Tool value can call Run on it directly. What
// must not be bypassable stays here.
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
	return runWith(ctx, w, newInMemoryDeployments(w))
}

func runWith(ctx context.Context, w io.Writer, store *inMemoryDeployments) error {
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
		return []gollem.Tool{
			&guardedTool{inner: newDeployTool(store), allow: denyProduction},
		}, nil
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
	fmt.Fprintf(w, "prod deploy applied:  %t\n",
		store.applied(deploymentKey(map[string]any{"service": "api", "version": "v1.4.2", "env": "prod"})))
	fmt.Fprintf(w, "staging deploy applied: %t\n",
		store.applied(deploymentKey(map[string]any{"service": "api", "version": "v1.4.2", "env": "staging"})))
	return nil
}
