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

func TestRunRefusesProductionAndDeploysStaging(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	deploy := newDeployTool(&out)
	gt.NoError(t, runWith(context.Background(), &out, deploy))

	got := out.String()
	gt.String(t, got).Contains("status:  succeeded")

	// The refusal happened inside Run, so the production deploy has no effect
	// recorded, while the staging retry does.
	gt.False(t, deploy.applied("deploy:api:v1.4.2:prod"))
	gt.True(t, deploy.applied("deploy:api:v1.4.2:staging"))
}

func TestRefusedCallsAreStillMetered(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, runWith(context.Background(), &out, newDeployTool(&out)))

	// Two attempts: the refused prod call and the staging one. The kernel meters
	// the attempt, not the outcome.
	gt.String(t, out.String()).Contains("tool calls attempted: 2")
}

func TestDeployToolIsIdempotent(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	deploy := newDeployTool(&out)
	args := map[string]any{"service": "api", "version": "v1.4.2", "env": "staging"}

	first, err := deploy.Run(ctx, args)
	gt.NoError(t, err)
	gt.Value(t, first["deduplicated"]).Equal(false)

	// A replay re-executes the same logical operation; the key absorbs it.
	second, err := deploy.Run(ctx, args)
	gt.NoError(t, err)
	gt.Value(t, second["deduplicated"]).Equal(true)
}

func TestGuardedToolFailsClosed(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	deploy := newDeployTool(&out)
	guarded := &guardedTool{inner: deploy, allow: denyProduction}

	_, err := guarded.Run(ctx, map[string]any{"service": "api", "version": "v1", "env": "prod"})
	gt.Error(t, err)
	gt.False(t, deploy.applied("deploy:api:v1:prod"))

	// The guard leaves the declared spec alone: it changes who may call the
	// tool, not what the model is told about it.
	gt.Value(t, guarded.Spec().Name).Equal(deploy.Spec().Name)
}

func TestToolErrorsDoNotFailTheProcess(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	gt.NoError(t, runWith(context.Background(), &out, newDeployTool(&out)))

	// The refused call returned an error to the strategy, which fed it back to
	// the model. A tool error is a message, not a fatal condition.
	gt.String(t, out.String()).Contains(string(agentkit.ProcessSucceeded))
}
