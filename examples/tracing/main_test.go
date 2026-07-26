// See examples/quickstart/main_test.go for why these tests live in
// `package main` rather than a black-box `package main_test`.
package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	gtrace "github.com/gollem-dev/gollem/trace"
	"github.com/m-mizutani/gt"
)

func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

// walk visits every span in the tree, so a test can look for one by kind or name
// without knowing where the recorder nested it.
func walk(s *gtrace.Span, visit func(*gtrace.Span)) {
	if s == nil {
		return
	}
	visit(s)
	for _, c := range s.Children {
		walk(c, visit)
	}
}

func runOffline(t *testing.T) (*bytes.Buffer, []*gtrace.Trace) {
	t.Helper()
	offline(t)
	dir := t.TempDir()
	var out bytes.Buffer
	gt.NoError(t, run(context.Background(), &out, dir))

	traces, err := loadTraces(dir)
	gt.NoError(t, err)
	return &out, traces
}

func TestRunSavesATraceForTheClaim(t *testing.T) {
	out, traces := runOffline(t)

	gt.String(t, out.String()).Contains("status:  succeeded")
	gt.Number(t, len(traces)).GreaterOrEqual(1)

	tr := traces[0]
	gt.NotNil(t, tr.RootSpan)
	// The claim is the root, and it is labelled with what a later reader needs to
	// correlate this trace with the other claims of the same Process.
	gt.Value(t, tr.RootSpan.Kind).Equal(gtrace.SpanKindAgentExecute)
	gt.String(t, tr.Metadata.Labels["process_id"]).NotEqual("")
	gt.String(t, tr.Metadata.Labels["worker"]).NotEqual("")
	gt.Value(t, tr.Metadata.Strategy).Equal(string(agentName))
}

func TestTraceHoldsTransitionAndToolSpans(t *testing.T) {
	_, traces := runOffline(t)

	var transitions, tools, events int
	var outcome string
	for _, tr := range traces {
		walk(tr.RootSpan, func(s *gtrace.Span) {
			switch {
			case s.Kind == gtrace.SpanKindToolExec:
				tools++
			case s.Kind == gtrace.SpanKindEvent:
				events++
				if s.Event != nil {
					outcome = s.Name
				}
			case s != tr.RootSpan && s.Kind == gtrace.SpanKindAgentExecute:
				transitions++
			}
		})
	}
	// The script runs a tool call, so there is at least one of each.
	gt.Number(t, transitions).GreaterOrEqual(1)
	gt.Number(t, tools).GreaterOrEqual(1)
	gt.Number(t, events).GreaterOrEqual(1)
	gt.Value(t, outcome).Equal("claim.outcome")
}

// The point of the example is that the times are measured, not inferred.
func TestTraceDurationsAreMeasured(t *testing.T) {
	_, traces := runOffline(t)

	var checked int
	for _, tr := range traces {
		walk(tr.RootSpan, func(s *gtrace.Span) {
			checked++
			gt.Number(t, int64(s.Duration)).GreaterOrEqual(0)
			gt.False(t, s.EndedAt.Before(s.StartedAt))
			// The root brackets everything under it.
			gt.False(t, s.StartedAt.Before(tr.RootSpan.StartedAt))
		})
		// The lookup tool sleeps, so its span cannot be a zero-width guess.
		var toolTime time.Duration
		walk(tr.RootSpan, func(s *gtrace.Span) {
			if s.Kind == gtrace.SpanKindToolExec {
				toolTime = s.Duration
			}
		})
		if toolTime > 0 {
			gt.Number(t, int64(toolTime)).GreaterOrEqual(int64(20 * time.Millisecond))
		}
	}
	gt.Number(t, checked).GreaterOrEqual(3)
}

// Offline there is no llm_call span, because those come from gollem's real LLM
// client reading the handler out of the ctx -- the stub is a mock session. The
// example says so in its output; this pins that it stays true.
func TestOfflineRunHasNoLLMSpan(t *testing.T) {
	out, traces := runOffline(t)

	for _, tr := range traces {
		walk(tr.RootSpan, func(s *gtrace.Span) {
			gt.Value(t, s.Kind).NotEqual(gtrace.SpanKindLLMCall)
		})
	}
	gt.String(t, out.String()).Contains("No llm_call span above")
}

// The run whose trace matters most is the one that blew up, and the kernel
// recovers a claim panic OUTSIDE the middleware -- so the middleware never gets
// a normal return. Ending the spans and saving from a defer is what keeps that
// case readable.
func TestTraceIsSavedWhenTheClaimPanics(t *testing.T) {
	offline(t)
	dir := t.TempDir()

	// Drive claimTrace directly with a handler that panics, which is what a
	// panicking ToolFactory or strategy looks like from here.
	mw := claimTrace(dir, log.New(io.Discard, "", 0))
	handler := mw(func(context.Context, *agentkit.ClaimRequest) (agentkit.ClaimOutcome, error) {
		panic("claim exploded")
	})

	req := &agentkit.ClaimRequest{Process: &agentkit.Process{
		ID: "p-1", RootID: "p-1", Agent: agentName, LeaseToken: "lease-1", LeaseOwner: "w1",
	}}
	func() {
		// The middleware re-raises so the kernel still sees the failure.
		defer func() { gt.NotNil(t, recover()) }()
		_, _ = handler(context.Background(), req)
	}()

	traces, err := loadTraces(dir)
	gt.NoError(t, err)
	gt.Number(t, len(traces)).Equal(1)

	tr := traces[0]
	gt.NotNil(t, tr.RootSpan)
	// The root span is closed, and the panic is on the record.
	gt.False(t, tr.RootSpan.EndedAt.IsZero())
	gt.Value(t, tr.RootSpan.Status).Equal(gtrace.SpanStatusError)
	var sawPanic bool
	walk(tr.RootSpan, func(s *gtrace.Span) {
		if s.Name == "claim.panic" {
			sawPanic = true
		}
	})
	gt.True(t, sawPanic)
}
