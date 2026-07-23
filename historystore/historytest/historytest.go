// Package historytest is a contract conformance suite for
// gollem.HistoryRepository implementations. The bundled memory and
// filesystem implementations call it from their tests, and external
// implementers can run it against their own implementation.
package historytest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/gt"
)

var idCounter int64

// newSessionID returns a unique session id. It combines a nanosecond
// timestamp with a process-global counter so parallel runs and same-nanosecond
// calls never collide, and so no hardcoded ids are used.
func newSessionID() string {
	n := atomic.AddInt64(&idCounter, 1)
	return fmt.Sprintf("session-%d-%d", time.Now().UnixNano(), n)
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

// Run executes the full gollem.HistoryRepository conformance suite. factory
// must return a fresh, empty HistoryRepository each time it is called.
func Run(t *testing.T, factory func(t *testing.T) gollem.HistoryRepository) {
	ctx := context.Background()

	t.Run("LoadUnsavedKeyReturnsNilNil", func(t *testing.T) {
		repo := factory(t)
		h, err := repo.Load(ctx, newSessionID())
		gt.NoError(t, err)
		gt.Nil(t, h)
	})

	t.Run("RoundTrip", func(t *testing.T) {
		repo := factory(t)
		id := newSessionID()
		want := newHistory(t, "hello", "hi there")

		gt.NoError(t, repo.Save(ctx, id, want))

		got, err := repo.Load(ctx, id)
		gt.NoError(t, err)
		assertHistoryEqual(t, want, got)
	})

	t.Run("OverwriteLastWriterWins", func(t *testing.T) {
		repo := factory(t)
		id := newSessionID()
		h1 := newHistory(t, "first-user", "first-assistant")
		h2 := newHistory(t, "second-user", "second-assistant")

		gt.NoError(t, repo.Save(ctx, id, h1))
		gt.NoError(t, repo.Save(ctx, id, h2))

		got, err := repo.Load(ctx, id)
		gt.NoError(t, err)
		assertHistoryEqual(t, h2, got)
	})

	t.Run("CloneIsolationOnSave", func(t *testing.T) {
		repo := factory(t)
		id := newSessionID()
		h := newHistory(t, "orig-user", "orig-assistant")

		gt.NoError(t, repo.Save(ctx, id, h))

		// Mutate the caller's History after Save; the stored copy must be
		// unaffected by this or any later mutation.
		h.Messages[0] = gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "mutated-user")}}
		h.Messages = append(h.Messages, gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "appended")}})

		got, err := repo.Load(ctx, id)
		gt.NoError(t, err)
		gt.Array(t, got.Messages).Length(2)
		gt.Value(t, textOf(t, got.Messages[0])).Equal("orig-user")
		gt.Value(t, textOf(t, got.Messages[1])).Equal("orig-assistant")
	})

	t.Run("CloneIsolationOnLoad", func(t *testing.T) {
		repo := factory(t)
		id := newSessionID()
		h := newHistory(t, "orig-user", "orig-assistant")
		gt.NoError(t, repo.Save(ctx, id, h))

		got, err := repo.Load(ctx, id)
		gt.NoError(t, err)
		got.Messages[0] = gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "mutated-user")}}
		got.Messages = append(got.Messages, gollem.Message{Role: gollem.RoleUser, Contents: []gollem.MessageContent{mustText(t, "appended")}})

		again, err := repo.Load(ctx, id)
		gt.NoError(t, err)
		gt.Array(t, again.Messages).Length(2)
		gt.Value(t, textOf(t, again.Messages[0])).Equal("orig-user")
		gt.Value(t, textOf(t, again.Messages[1])).Equal("orig-assistant")
	})
}
