package filesystem_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/historystore/filesystem"
	"github.com/gollem-dev/agentkit/historystore/historytest"
	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
)

func TestConformance(t *testing.T) {
	historytest.Run(t, func(t *testing.T) agentkit.HistoryStore {
		dir := t.TempDir()
		store, err := filesystem.New(dir)
		gt.NoError(t, err)
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
		return store
	})
}

func newPID() agentkit.ProcessID {
	return agentkit.ProcessID(fmt.Sprintf("proc-%d", time.Now().UnixNano()))
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

// TestCloseReopenRoundTrip proves persistence across process restarts: a version
// saved before Close is still readable through its ref after a fresh New on the
// same dir.
func TestCloseReopenRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pid := newPID()
	h := newHistory(t, "persisted-across-reopen")

	s1, err := filesystem.New(dir)
	gt.NoError(t, err)
	ref, err := s1.Save(ctx, pid, h)
	gt.NoError(t, err)
	gt.NoError(t, s1.Close())

	s2, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, s2.Close()) }()

	got, err := s2.Load(ctx, pid, ref)
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
	s1, err := filesystem.New(dir)
	gt.NoError(t, err)

	// A second concurrent New on the same directory must fail (single-process).
	s2, err := filesystem.New(dir)
	gt.Error(t, err)
	gt.Nil(t, s2)

	gt.NoError(t, s1.Close())

	// After Close the lock is released and re-opening works.
	s3, err := filesystem.New(dir)
	gt.NoError(t, err)
	gt.NoError(t, s3.Close())
}

func TestInvalidPathComponentRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, store.Close()) }()

	unsafe := []string{"", "a/b", "a\\b", "..", "../escape", "a/../b"}
	h := newHistory(t, "x")

	// An unsafe ProcessID is rejected by both Save and Load.
	for _, bad := range unsafe {
		_, saveErr := store.Save(ctx, agentkit.ProcessID(bad), h)
		gt.Error(t, saveErr).Is(filesystem.ErrInvalidPathComponent)

		_, loadErr := store.Load(ctx, agentkit.ProcessID(bad), agentkit.HistoryRef("ref"))
		gt.Error(t, loadErr).Is(filesystem.ErrInvalidPathComponent)
	}

	// An unsafe HistoryRef cannot reach Save (the store mints refs itself), but a
	// caller can pass one to Load.
	for _, bad := range unsafe {
		_, loadErr := store.Load(ctx, newPID(), agentkit.HistoryRef(bad))
		gt.Error(t, loadErr).Is(filesystem.ErrInvalidPathComponent)
	}
}

func TestUnmarshalErrorPropagates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, store.Close()) }()

	// Save a History with a version that does not match gollem.HistoryVersion;
	// Load must surface gollem's version-mismatch error, not swallow it.
	// Save marshals it fine (marshaling never checks Version), so this exercises
	// Load's Unmarshal path.
	bad := newHistory(t, "x")
	bad.Version = gollem.HistoryVersion + 999

	pid := newPID()
	ref, err := store.Save(ctx, pid, bad)
	gt.NoError(t, err)

	_, err = store.Load(ctx, pid, ref)
	gt.Error(t, err).Is(gollem.ErrHistoryVersionMismatch)
}

func TestDirSyncFailurePropagates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, store.Close()) }()

	store.SetDirSyncForTest(func(string) error {
		return goerr.New("injected dir fsync failure")
	})

	_, err = store.Save(ctx, newPID(), newHistory(t, "x"))
	gt.Error(t, err)
}
