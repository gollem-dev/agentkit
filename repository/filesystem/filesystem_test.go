package filesystem_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/filesystem"
	"github.com/gollem-dev/agentkit/repository/repotest"
	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
)

func TestConformance(t *testing.T) {
	repotest.Run(t, func(t *testing.T) agentkit.Repository {
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

func mkProc(pid agentkit.ProcessID) *agentkit.Process {
	now := time.Now()
	return &agentkit.Process{
		ID:        pid,
		Agent:     "fs-test",
		Status:    agentkit.ProcessPending,
		RootID:    pid,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func newPID(t *testing.T) agentkit.ProcessID {
	t.Helper()
	return agentkit.ProcessID(time.Now().Format("proc-150405.000000000"))
}

func TestSnapshotRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pid := agentkit.ProcessID("proc-recovery-" + time.Now().Format("150405.000000000"))

	r1, err := filesystem.New(dir)
	gt.NoError(t, err)
	gt.NoError(t, r1.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))
	gt.NoError(t, r1.Close())

	// Reopen: the persisted state is reloaded.
	r2, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, r2.Close()) }()
	got, err := r2.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, got.ID).Equal(pid)
	gt.Value(t, got.Rev).Equal(int64(1))
}

func TestLeftoverTempIgnored(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pid := agentkit.ProcessID("proc-tmp-" + time.Now().Format("150405.000000000"))

	r1, err := filesystem.New(dir)
	gt.NoError(t, err)
	gt.NoError(t, r1.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))
	gt.NoError(t, r1.Close())

	// Simulate a pre-rename crash: a stray uncommitted temp file.
	tmpPath := filepath.Join(dir, "state.json.tmp")
	gt.NoError(t, os.WriteFile(tmpPath, []byte("garbage-not-json"), 0o600))

	r2, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, r2.Close()) }()

	// The stray temp is ignored and deleted; committed state survives.
	_, statErr := os.Stat(tmpPath)
	gt.Bool(t, os.IsNotExist(statErr)).True()
	got, err := r2.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, got.ID).Equal(pid)
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

func TestPostRenamePoisonsRepository(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pid := agentkit.ProcessID("proc-poison-" + time.Now().Format("150405.000000000"))

	r1, err := filesystem.New(dir)
	gt.NoError(t, err)

	// Inject a post-rename (directory fsync) failure.
	r1.SetDirSyncForTest(func(string) error {
		return goerr.New("injected dir fsync failure")
	})

	err = r1.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}})
	gt.Error(t, err).Is(agentkit.ErrRepositoryIndeterminate)

	// Fail-stop: every subsequent write returns the same error.
	err = r1.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(agentkit.ProcessID("other-" + string(pid)))}})
	gt.Error(t, err).Is(agentkit.ErrRepositoryIndeterminate)
	_, err = r1.ClaimNextProcess(ctx, "w", time.Now().Add(time.Hour), time.Now())
	gt.Error(t, err).Is(agentkit.ErrRepositoryIndeterminate)
	gt.NoError(t, r1.Close())

	// The rename committed before the fsync failure, so the data is on disk.
	// Recovery is Close + New, which reloads state.json.
	r2, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, r2.Close()) }()
	got, err := r2.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, got.ID).Equal(pid)
}

func TestPersistedClaimSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pid := newPID(t)

	r1, err := filesystem.New(dir)
	gt.NoError(t, err)
	gt.NoError(t, r1.Apply(ctx, agentkit.ChangeSet{Processes: []*agentkit.Process{mkProc(pid)}}))
	now := time.Now()
	claimed, err := r1.ClaimNextProcess(ctx, "w", now.Add(time.Hour), now)
	gt.NoError(t, err)
	gt.NotNil(t, claimed)
	gt.NoError(t, r1.Close())

	// The claim (status=running, lease, new Rev) was persisted.
	r2, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, r2.Close()) }()
	got, err := r2.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, got.Status).Equal(agentkit.ProcessRunning)
	gt.Value(t, got.LeaseToken).Equal(claimed.LeaseToken)
}
