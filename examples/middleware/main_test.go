// See examples/quickstart/main_test.go for why these tests live in
// `package main` rather than a black-box `package main_test`.
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/m-mizutani/gt"
)

func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

// execute runs the example and returns its output plus which tools really ran.
func execute(t *testing.T) (string, map[string]bool) {
	t.Helper()
	offline(t)

	var out bytes.Buffer
	ran, err := runWith(context.Background(), &out, "the payments service")
	gt.NoError(t, err)
	return out.String(), ran
}

func TestRunSucceedsAndReportsEveryHook(t *testing.T) {
	got, _ := execute(t)

	gt.String(t, got).Contains("status: succeeded")
	// One registration per hook, and every one of them fired.
	for _, hook := range []string{"init", "step", "generate", "tool", "spawn"} {
		gt.String(t, got).Contains(hook)
	}
}

func TestPolicyRefusesWithoutRunningTheTool(t *testing.T) {
	got, ran := execute(t)

	gt.String(t, got).Contains("purge DENIED by policy")
	gt.String(t, got).Contains("lookup ok")

	// The refusal returned before next, so the tool itself never ran.
	gt.False(t, ran["purge"])
	gt.True(t, ran["lookup"])

	// The strategy saw the refusal as an error and carried on, rather than the
	// Process failing over it.
	gt.String(t, got).Contains("note:   purge: ")
}

func TestInitMiddlewareRewritesOnlyTheAgentItKnows(t *testing.T) {
	got, _ := execute(t)

	// The reporter's input is a simple.Input, so it gets the house style.
	gt.String(t, got).Contains("init      reporter    prompt prefixed")
	// The coordinator's own input is a different type: not this middleware's
	// business, passed through untouched.
	gt.String(t, got).Contains("init      coordinator (left alone)")
}

func TestSpawnMiddlewareLearnsTheChildWasPersisted(t *testing.T) {
	got, _ := execute(t)

	// Two children, each confirmed by its OnCommit callback after the
	// transition committed.
	gt.Number(t, strings.Count(got, "persisted with the transition")).Equal(2)
	gt.String(t, got).NotContains("NOT persisted")
}

func TestStepMiddlewareSeesEveryTransitionAndItsDecision(t *testing.T) {
	got, _ := execute(t)

	// The coordinator's three phases: survey and spawn continue/suspend, the
	// last one finishes.
	gt.String(t, got).Contains("step      coordinator seq=1 -> continue")
	gt.String(t, got).Contains("step      coordinator seq=2 -> suspend")
	gt.String(t, got).Contains("step      coordinator seq=3 -> done")

	// StepState[any] always succeeds, whatever agent the transition belongs to.
	gt.String(t, got).NotContains("state readable: false")
}

func TestGenerateMiddlewareRecordsTheRole(t *testing.T) {
	got, _ := execute(t)

	// The coordinator is bound to a named role; the reporters fall through to
	// the kernel's default model.
	gt.String(t, got).Contains("role=middleware.coordinator")
	gt.String(t, got).Contains("role=default")
}

func TestInitRejectsAnEmptyTopic(t *testing.T) {
	s := &coordinator{}

	_, err := s.Init(input{})
	gt.Error(t, err)

	st, err := s.Init(input{Topic: "payments"})
	gt.NoError(t, err)
	gt.Value(t, st.Phase).Equal(phaseSurvey)
}

func TestStateRoundTrips(t *testing.T) {
	s := &coordinator{}

	raw, err := s.EncodeState(state{Topic: "t", Phase: phaseSpawn, Notes: []string{"n"}})
	gt.NoError(t, err)

	got, err := s.DecodeState(s.Version(), raw)
	gt.NoError(t, err)
	gt.Value(t, got).Equal(state{Topic: "t", Phase: phaseSpawn, Notes: []string{"n"}})
}
