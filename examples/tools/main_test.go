// See examples/quickstart/main_test.go for why these tests live in
// `package main` rather than a black-box `package main_test`.
package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/m-mizutani/gt"
)

func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

func deployArgs(env string) map[string]any {
	return map[string]any{"service": "api", "version": "v1.4.2", "env": env}
}

func TestRunRefusesProductionAndDeploysStaging(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	store := newInMemoryDeployments(&out)
	gt.NoError(t, runWith(context.Background(), &out, store))

	gt.String(t, out.String()).Contains("status:  succeeded")

	// The refusal happened inside Run, so the production deploy has no effect
	// recorded, while the staging retry does.
	gt.False(t, store.applied(deploymentKey(deployArgs("prod"))))
	gt.True(t, store.applied(deploymentKey(deployArgs("staging"))))
}

func TestRefusedCallsAreStillMetered(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, runWith(context.Background(), &out, newInMemoryDeployments(&out)))

	// Two attempts: the refused prod call and the staging one. The kernel meters
	// the attempt, not the outcome.
	gt.String(t, out.String()).Contains("tool calls attempted: 2")
}

func TestDeployToolDeduplicatesOnTheKey(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	tool := newDeployTool(newInMemoryDeployments(&out))

	first, err := tool.Run(ctx, deployArgs("staging"))
	gt.NoError(t, err)
	gt.Value(t, first["deduplicated"]).Equal(false)

	// A replay re-executes the same logical operation; the key absorbs it, as
	// long as the store behind it outlived the crash.
	second, err := tool.Run(ctx, deployArgs("staging"))
	gt.NoError(t, err)
	gt.Value(t, second["deduplicated"]).Equal(true)
}

// TestDeploymentKeySeparatesDistinctTuples guards the key construction itself.
// A plain "a:b:c" join lets an argument containing a separator collide with a
// different tuple, and a colliding key silently skips a real deployment.
func TestDeploymentKeySeparatesDistinctTuples(t *testing.T) {
	left := deploymentKey(map[string]any{"service": "api:web", "version": "v1", "env": "prod"})
	right := deploymentKey(map[string]any{"service": "api", "version": "web:v1", "env": "prod"})
	gt.Value(t, left).NotEqual(right)

	// The same tuple still maps to the same key, or deduplication would never
	// trigger at all.
	gt.Value(t, deploymentKey(deployArgs("prod"))).Equal(deploymentKey(deployArgs("prod")))
}

// TestInMemoryStoreDoesNotSurviveTheProcess states the limitation out loud: a
// fresh store has no memory of what the previous one applied, which is exactly
// what happens when a worker crashes and another one replays the transition.
func TestInMemoryStoreDoesNotSurviveTheProcess(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	key := deploymentKey(deployArgs("staging"))

	before := newInMemoryDeployments(&out)
	applied, err := before.DeployOnce(ctx, key, deployArgs("staging"))
	gt.NoError(t, err)
	gt.True(t, applied)

	after := newInMemoryDeployments(&out)
	applied, err = after.DeployOnce(ctx, key, deployArgs("staging"))
	gt.NoError(t, err)
	gt.True(t, applied) // deployed a second time -- the store has to be durable.
}

func TestGuardedToolFailsClosed(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	store := newInMemoryDeployments(&out)
	tool := newDeployTool(store)
	guarded := &guardedTool{inner: tool, allow: denyProduction}

	_, err := guarded.Run(ctx, deployArgs("prod"))
	gt.Error(t, err)
	gt.False(t, store.applied(deploymentKey(deployArgs("prod"))))

	// The guard leaves the declared spec alone: it changes who may call the
	// tool, not what the model is told about it.
	gt.Value(t, guarded.Spec().Name).Equal(tool.Spec().Name)
}

func TestToolErrorsDoNotFailTheProcess(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, runWith(context.Background(), &out, newInMemoryDeployments(&out)))

	// The refused call returned an error to the strategy, which fed it back to
	// the model. A tool error is a message, not a fatal condition.
	gt.String(t, out.String()).Contains(string(agentkit.ProcessSucceeded))
}
