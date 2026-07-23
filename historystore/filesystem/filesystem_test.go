package filesystem_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit/historystore/filesystem"
	"github.com/gollem-dev/agentkit/historystore/historytest"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
)

func TestConformance(t *testing.T) {
	historytest.Run(t, func(t *testing.T) gollem.HistoryRepository {
		dir := t.TempDir()
		repo, err := filesystem.New(dir)
		gt.NoError(t, err)
		t.Cleanup(func() {
			if err := repo.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
		return repo
	})
}

func newSessionID() string {
	return fmt.Sprintf("session-%d", time.Now().UnixNano())
}

func newHistory(t *testing.T, text string) *gollem.History {
	t.Helper()
	c, err := gollem.NewTextContent(text)
	gt.NoError(t, err)
	return &gollem.History{
		LLType:  gollem.LLMTypeOpenAI,
		Version: gollem.HistoryVersion,
		Messages: []gollem.Message{
			{Role: gollem.RoleUser, Contents: []gollem.MessageContent{c}},
		},
	}
}

// TestCloseReopenRoundTrip proves persistence across process restarts: data
// saved before Close is still readable after a fresh New on the same dir.
func TestCloseReopenRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	id := newSessionID()
	h := newHistory(t, "persisted-across-reopen")

	r1, err := filesystem.New(dir)
	gt.NoError(t, err)
	gt.NoError(t, r1.Save(ctx, id, h))
	gt.NoError(t, r1.Close())

	r2, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, r2.Close()) }()

	got, err := r2.Load(ctx, id)
	gt.NoError(t, err)
	gt.NotNil(t, got)
	gt.Value(t, got.LLType).Equal(h.LLType)
	gt.Value(t, got.Version).Equal(h.Version)
	gt.Array(t, got.Messages).Length(1)
	gt.Value(t, got.Messages[0].Role).Equal(gollem.RoleUser)
	tc, err := got.Messages[0].Contents[0].GetTextContent()
	gt.NoError(t, err)
	gt.Value(t, tc.Text).Equal("persisted-across-reopen")
}

func TestLockRejectsSecondOpen(t *testing.T) {
	dir := t.TempDir()
	r1, err := filesystem.New(dir)
	gt.NoError(t, err)

	// A second concurrent New on the same directory must fail (single-process).
	r2, err := filesystem.New(dir)
	gt.Error(t, err)
	gt.Nil(t, r2)

	gt.NoError(t, r1.Close())

	// After Close the lock is released and re-opening works.
	r3, err := filesystem.New(dir)
	gt.NoError(t, err)
	gt.NoError(t, r3.Close())
}

func TestInvalidSessionIDRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, repo.Close()) }()

	h := newHistory(t, "x")
	for _, id := range []string{"", "a/b", "a\\b", "..", "../escape", "a/../b"} {
		_, loadErr := repo.Load(ctx, id)
		gt.Error(t, loadErr).Is(filesystem.ErrInvalidSessionID)

		saveErr := repo.Save(ctx, id, h)
		gt.Error(t, saveErr).Is(filesystem.ErrInvalidSessionID)
	}
}

func TestUnmarshalErrorPropagates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, repo.Close()) }()

	// Save a History with a version that does not match gollem.HistoryVersion;
	// Load must surface gollem's version-mismatch error, not swallow it.
	id := newSessionID()
	bad := newHistory(t, "x")
	bad.Version = gollem.HistoryVersion + 999

	// Write the mismatched-version JSON directly: Save would marshal it fine
	// (marshaling never checks Version), so this exercises Load's Unmarshal path.
	gt.NoError(t, repo.Save(ctx, id, bad))

	_, err = repo.Load(ctx, id)
	gt.Error(t, err).Is(gollem.ErrHistoryVersionMismatch)
}

func TestDirSyncFailurePropagates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, repo.Close()) }()

	repo.SetDirSyncForTest(func(string) error {
		return goerr.New("injected dir fsync failure")
	})

	id := newSessionID()
	err = repo.Save(ctx, id, newHistory(t, "x"))
	gt.Error(t, err)
}
