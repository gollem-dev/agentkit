// Package historytest is a contract conformance suite for
// agentkit.HistoryStore implementations. The bundled memory and filesystem
// implementations call it from their tests, and external implementers can run
// it against their own implementation.
//
// The property the whole design rests on is OlderRefSurvivesNewerSave: a
// version must stay readable through its ref after later versions are saved. A
// store that overwrites in place passes everything else and still breaks the
// rollback guarantee (ADR-0017).
package historytest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
)

var idCounter int64

// newPID returns a unique ProcessID. It combines a nanosecond timestamp with a
// process-global counter so parallel runs and same-nanosecond calls never
// collide, and so no hardcoded ids are used.
func newPID() agentkit.ProcessID {
	n := atomic.AddInt64(&idCounter, 1)
	return agentkit.ProcessID(fmt.Sprintf("proc-%d-%d", time.Now().UnixNano(), n))
}

// mustText builds a text MessageContent, failing the test if it errors (it
// cannot, absent a json.Marshal failure on a plain string, but the error is
// still checked rather than ignored).
func mustText(t *testing.T, text string) gollem.MessageContent {
	t.Helper()
	c, err := gollem.NewTextContent(text)
	gt.NoError(t, err)
	return c
}

// newHistory builds a minimal, valid gollem.History: current HistoryVersion,
// LLMTypeOpenAI, and one user message followed by one assistant message, each
// holding a single text content.
func newHistory(t *testing.T, userText, assistantText string) *gollem.History {
	t.Helper()
	return &gollem.History{
		LLType:  gollem.LLMTypeOpenAI,
		Version: gollem.HistoryVersion,
		Messages: []gollem.Message{
			{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, userText)}},
			{Role: gollem.RoleAssistant, Contents: []gollem.MessageContent{mustText(t, assistantText)}},
		},
	}
}

// textOf extracts the text of a message's first content, failing the test if
// it is not text content.
func textOf(t *testing.T, m gollem.Message) string {
	t.Helper()
	gt.Array(t, m.Contents).Length(1)
	tc, err := m.Contents[0].GetTextContent()
	gt.NoError(t, err)
	return tc.Text
}

// assertHistoryEqual compares two Histories on LLType, Version, message count,
// and each message's Role and text content — real fields, not just non-nil.
func assertHistoryEqual(t *testing.T, want, got *gollem.History) {
	t.Helper()
	gt.NotNil(t, got)
	gt.Value(t, got.LLType).Equal(want.LLType)
	gt.Value(t, got.Version).Equal(want.Version)
	gt.Array(t, got.Messages).Length(len(want.Messages))
	for i := range want.Messages {
		gt.Value(t, got.Messages[i].Role).Equal(want.Messages[i].Role)
		gt.Value(t, textOf(t, got.Messages[i])).Equal(textOf(t, want.Messages[i]))
	}
}

// Run executes the full agentkit.HistoryStore conformance suite. factory must
// return a fresh, empty HistoryStore each time it is called.
func Run(t *testing.T, factory func(t *testing.T) agentkit.HistoryStore) {
	ctx := context.Background()

	t.Run("SaveReturnsNonEmptyRef", func(t *testing.T) {
		store := factory(t)
		ref, err := store.Save(ctx, newPID(), newHistory(t, "hello", "hi there"))
		gt.NoError(t, err)
		gt.Value(t, ref).NotEqual(agentkit.HistoryRef(""))
	})

	t.Run("RoundTripByRef", func(t *testing.T) {
		store := factory(t)
		pid := newPID()
		want := newHistory(t, "hello", "hi there")

		ref, err := store.Save(ctx, pid, want)
		gt.NoError(t, err)

		got, err := store.Load(ctx, pid, ref)
		gt.NoError(t, err)
		assertHistoryEqual(t, want, got)
	})

	// The core contract: a ref keeps naming the content it was returned for. A
	// store that overwrites the previous version fails here, and only here.
	t.Run("OlderRefSurvivesNewerSave", func(t *testing.T) {
		store := factory(t)
		pid := newPID()
		h1 := newHistory(t, "first-user", "first-assistant")
		h2 := newHistory(t, "second-user", "second-assistant")

		ref1, err := store.Save(ctx, pid, h1)
		gt.NoError(t, err)
		ref2, err := store.Save(ctx, pid, h2)
		gt.NoError(t, err)
		gt.Value(t, ref1).NotEqual(ref2)

		got1, err := store.Load(ctx, pid, ref1)
		gt.NoError(t, err)
		assertHistoryEqual(t, h1, got1)

		got2, err := store.Load(ctx, pid, ref2)
		gt.NoError(t, err)
		assertHistoryEqual(t, h2, got2)
	})

	t.Run("LoadUnknownRefErrors", func(t *testing.T) {
		store := factory(t)
		pid := newPID()
		ref, err := store.Save(ctx, pid, newHistory(t, "u", "a"))
		gt.NoError(t, err)

		// A ref this store never returned, and a real ref under another process.
		_, err = store.Load(ctx, pid, agentkit.HistoryRef("no-such-ref"))
		gt.Error(t, err).Is(agentkit.ErrHistoryVersionMissing)

		_, err = store.Load(ctx, newPID(), ref)
		gt.Error(t, err).Is(agentkit.ErrHistoryVersionMissing)
	})

	t.Run("DiscardIsIdempotent", func(t *testing.T) {
		store := factory(t)
		pid := newPID()
		ref, err := store.Save(ctx, pid, newHistory(t, "u", "a"))
		gt.NoError(t, err)

		// Discarding twice, an unknown ref, and an unknown process must all be
		// accepted silently: the caller has no outcome to react to.
		store.Discard(ctx, pid, ref)
		store.Discard(ctx, pid, ref)
		store.Discard(ctx, pid, agentkit.HistoryRef("no-such-ref"))
		store.Discard(ctx, newPID(), ref)
	})

	t.Run("DiscardDoesNotAffectOtherRefs", func(t *testing.T) {
		store := factory(t)
		pid := newPID()
		h1 := newHistory(t, "first-user", "first-assistant")
		h2 := newHistory(t, "second-user", "second-assistant")

		ref1, err := store.Save(ctx, pid, h1)
		gt.NoError(t, err)
		ref2, err := store.Save(ctx, pid, h2)
		gt.NoError(t, err)

		store.Discard(ctx, pid, ref1)

		got, err := store.Load(ctx, pid, ref2)
		gt.NoError(t, err)
		assertHistoryEqual(t, h2, got)
	})

	t.Run("DistinctProcessesAreIsolated", func(t *testing.T) {
		store := factory(t)
		pidA, pidB := newPID(), newPID()
		hA := newHistory(t, "a-user", "a-assistant")
		hB := newHistory(t, "b-user", "b-assistant")

		refA, err := store.Save(ctx, pidA, hA)
		gt.NoError(t, err)
		refB, err := store.Save(ctx, pidB, hB)
		gt.NoError(t, err)

		gotA, err := store.Load(ctx, pidA, refA)
		gt.NoError(t, err)
		assertHistoryEqual(t, hA, gotA)

		gotB, err := store.Load(ctx, pidB, refB)
		gt.NoError(t, err)
		assertHistoryEqual(t, hB, gotB)

		// Discarding one process's version leaves the other's alone.
		store.Discard(ctx, pidA, refA)
		gotB, err = store.Load(ctx, pidB, refB)
		gt.NoError(t, err)
		assertHistoryEqual(t, hB, gotB)
	})

	t.Run("CloneIsolationOnSave", func(t *testing.T) {
		store := factory(t)
		pid := newPID()
		h := newHistory(t, "orig-user", "orig-assistant")

		ref, err := store.Save(ctx, pid, h)
		gt.NoError(t, err)

		// Mutate the caller's History after Save; the stored copy must be
		// unaffected by this or any later mutation.
		h.Messages[0] = gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "mutated-user")}}
		h.Messages = append(h.Messages, gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "appended")}})

		got, err := store.Load(ctx, pid, ref)
		gt.NoError(t, err)
		gt.Array(t, got.Messages).Length(2)
		gt.Value(t, textOf(t, got.Messages[0])).Equal("orig-user")
		gt.Value(t, textOf(t, got.Messages[1])).Equal("orig-assistant")
	})

	t.Run("CloneIsolationOnLoad", func(t *testing.T) {
		store := factory(t)
		pid := newPID()
		h := newHistory(t, "orig-user", "orig-assistant")
		ref, err := store.Save(ctx, pid, h)
		gt.NoError(t, err)

		got, err := store.Load(ctx, pid, ref)
		gt.NoError(t, err)
		got.Messages[0] = gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "mutated-user")}}
		got.Messages = append(got.Messages, gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "appended")}})

		again, err := store.Load(ctx, pid, ref)
		gt.NoError(t, err)
		gt.Array(t, again.Messages).Length(2)
		gt.Value(t, textOf(t, again.Messages[0])).Equal("orig-user")
		gt.Value(t, textOf(t, again.Messages[1])).Equal("orig-assistant")
	})
}
