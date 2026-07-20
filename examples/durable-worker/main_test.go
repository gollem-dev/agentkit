// See examples/quickstart/main_test.go for why these tests live in
// `package main` rather than a black-box `package main_test`.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/examples/internal/demo"
	"github.com/gollem-dev/agentkit/repository/filesystem"
	"github.com/m-mizutani/gt"
)

func offline(t *testing.T) {
	t.Helper()
	t.Setenv(demo.ProjectEnv, "")
	t.Setenv(demo.LocationEnv, "")
}

func TestSubmitThenWorkFinishesTheProcess(t *testing.T) {
	offline(t)

	ctx := context.Background()
	dir := t.TempDir()
	var out bytes.Buffer

	// The submitting process opens the store, writes the work, and closes it.
	pid, err := runSubmit(ctx, &out, dir, "durability", 2)
	gt.NoError(t, err)
	gt.Value(t, pid).NotEqual(agentkit.ProcessID(""))

	// A separate worker picks it up from the store alone.
	gt.NoError(t, runWork(ctx, &out, dir, pid, 0))
	gt.String(t, out.String()).Contains("status:  succeeded")

	var status bytes.Buffer
	gt.NoError(t, runStatus(ctx, &status, dir, pid))
	got := status.String()
	gt.String(t, got).Contains("status:        succeeded")
	// Two rounds plus the transition that finalizes them.
	gt.String(t, got).Contains("committed seq: 3")
	gt.String(t, got).Contains("note 1:")
	gt.String(t, got).Contains("note 2:")
}

func TestOutputCarriesOneNotePerRound(t *testing.T) {
	offline(t)

	ctx := context.Background()
	dir := t.TempDir()
	var out bytes.Buffer

	pid, err := runSubmit(ctx, &out, dir, "durability", 3)
	gt.NoError(t, err)
	gt.NoError(t, runWork(ctx, &out, dir, pid, 0))

	repo, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, repo.Close()) }()

	proc, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	var res output
	gt.NoError(t, json.Unmarshal(proc.Output, &res))
	gt.Array(t, res.Notes).Length(3)
}

func TestSubmitRejectsANonPositiveRoundCount(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	// Init runs inside Spawn, so this fails synchronously instead of creating a
	// Process that fails later.
	_, err := runSubmit(context.Background(), &out, t.TempDir(), "durability", 0)
	gt.Error(t, err)
}

func TestSubmitRejectsAnEmptyTopic(t *testing.T) {
	offline(t)

	var out bytes.Buffer
	_, err := runSubmit(context.Background(), &out, t.TempDir(), "", 1)
	gt.Error(t, err)
}

func TestStoreAllowsOnlyOneOpenAtATime(t *testing.T) {
	dir := t.TempDir()

	first, err := filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, first.Close()) }()

	// This is why submit, work and status each open and close the store rather
	// than running side by side.
	_, err = filesystem.New(dir)
	gt.Error(t, err)
}

func TestStatusReportsAnUnknownProcess(t *testing.T) {
	var out bytes.Buffer

	err := runStatus(context.Background(), &out, t.TempDir(), "no-such-process")
	gt.Error(t, err)
	gt.True(t, errors.Is(err, agentkit.ErrProcessNotFound))
}

// TestDispatchRunsTheDocumentedFlow drives the three subcommands the way the
// README tells a reader to, flag parsing included. The -crash-after path is not
// covered: it ends in os.Exit, which would take the test binary with it.
func TestDispatchRunsTheDocumentedFlow(t *testing.T) {
	offline(t)

	ctx := context.Background()
	dir := t.TempDir()

	var submitted bytes.Buffer
	gt.NoError(t, dispatch(ctx, &submitted, []string{
		"submit", "-dir", dir, "-topic", "durability", "-rounds", "2",
	}))
	pid := spawnedPID(t, submitted.String())

	var worked bytes.Buffer
	gt.NoError(t, dispatch(ctx, &worked, []string{"work", "-dir", dir, "-pid", pid}))
	gt.String(t, worked.String()).Contains("status:  succeeded")

	var status bytes.Buffer
	gt.NoError(t, dispatch(ctx, &status, []string{"status", "-dir", dir, "-pid", pid}))
	gt.String(t, status.String()).Contains("status:        succeeded")
}

// spawnedPID reads the id back out of submit's output, which is what a reader
// following the README does by hand.
func spawnedPID(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "spawned: "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no spawned id in output: %q", out)
	return ""
}

func TestDispatchRejectsUnknownInput(t *testing.T) {
	var out bytes.Buffer
	ctx := context.Background()

	gt.Error(t, dispatch(ctx, &out, nil))
	gt.Error(t, dispatch(ctx, &out, []string{"deploy"}))
	// status without -pid has nothing to look up.
	gt.Error(t, dispatch(ctx, &out, []string{"status", "-dir", t.TempDir()}))
}

// The crash path ends in os.Exit, so it cannot be driven in-process: it would
// take the test binary with it. These two run it for real, by re-executing this
// same test binary as a child that is expected to die.
const (
	crashDirEnv = "AGENTKIT_EXAMPLE_CRASH_DIR"
	crashPIDEnv = "AGENTKIT_EXAMPLE_CRASH_PID"
)

// TestCrashHelper is not a test. It is the body of the child process, and it
// only runs when the parent below re-executes this binary with the environment
// set. Left to itself in a normal run it skips.
func TestCrashHelper(t *testing.T) {
	dir, ok := os.LookupEnv(crashDirEnv)
	if !ok {
		t.Skip(crashDirEnv + " is not set; this is the crash helper, not a test")
	}
	pid := agentkit.ProcessID(os.Getenv(crashPIDEnv))

	// Writes to stdout so the parent can see how far it got before dying.
	// Expected to call os.Exit(1) partway through and never return.
	err := runWork(context.Background(), os.Stdout, dir, pid, 2)
	t.Fatalf("the worker was supposed to crash, but returned: %v", err)
}

// TestCrashResumesFromTheLastCommit is the walkthrough the README documents,
// executed rather than described: submit, crash mid-round, inspect, resume.
func TestCrashResumesFromTheLastCommit(t *testing.T) {
	offline(t)

	ctx := context.Background()
	dir := t.TempDir()
	const rounds = 4

	var out bytes.Buffer
	pid, err := runSubmit(ctx, &out, dir, "durability", rounds)
	gt.NoError(t, err)

	// #nosec G204 -- os.Args[0] is this test binary, and the arguments are
	// constants; nothing here comes from outside the test.
	child := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
	child.Env = append(os.Environ(),
		crashDirEnv+"="+dir,
		crashPIDEnv+"="+string(pid),
		// The child gets its own environment, so stub mode has to be forced
		// again here: t.Setenv does not cross a process boundary.
		demo.ProjectEnv+"=",
		demo.LocationEnv+"=",
	)
	childOut, err := child.CombinedOutput()

	// The worker died where it was told to, taking the uncommitted round with it.
	var exitErr *exec.ExitError
	gt.True(t, errors.As(err, &exitErr))
	gt.Number(t, exitErr.ExitCode()).Equal(1)
	gt.String(t, string(childOut)).Contains("simulated crash")

	// The store is reopenable: the lock the dead process held was released by
	// the OS, not by any deferred Close that never ran.
	repo, err := filesystem.New(dir)
	gt.NoError(t, err)

	proc, err := repo.GetProcess(ctx, pid)
	gt.NoError(t, err)

	// One round committed, not two. The crashed round's LLM call happened and
	// was thrown away with the transition -- which is exactly why it has to run
	// again, and why a side-effecting tool has to be idempotent.
	gt.Value(t, proc.Status).Equal(agentkit.ProcessRunning)
	gt.Number(t, proc.StateSeq).Equal(1)

	var st state
	gt.NoError(t, json.Unmarshal(proc.State, &st))
	gt.Array(t, st.Notes).Length(1)
	gt.Number(t, st.Done).Equal(1)

	// Nothing can claim it yet: the dead worker's lease has not run out.
	gt.NotNil(t, proc.LeaseUntil)
	gt.True(t, proc.LeaseUntil.After(time.Now()))
	gt.NoError(t, repo.Close())

	// Resuming waits out the remaining lease and finishes the work, including
	// the round that was lost.
	gt.NoError(t, runWork(ctx, &out, dir, pid, 0))

	repo, err = filesystem.New(dir)
	gt.NoError(t, err)
	defer func() { gt.NoError(t, repo.Close()) }()

	proc, err = repo.GetProcess(ctx, pid)
	gt.NoError(t, err)
	gt.Value(t, proc.Status).Equal(agentkit.ProcessSucceeded)

	var res output
	gt.NoError(t, json.Unmarshal(proc.Output, &res))
	gt.Array(t, res.Notes).Length(rounds)
}

func TestStateRoundTrips(t *testing.T) {
	s := &strategy{}

	raw, err := s.EncodeState(state{Topic: "t", Total: 2, Done: 1, Notes: []string{"n"}})
	gt.NoError(t, err)

	got, err := s.DecodeState(s.Version(), raw)
	gt.NoError(t, err)
	gt.Value(t, got).Equal(state{Topic: "t", Total: 2, Done: 1, Notes: []string{"n"}})
}
