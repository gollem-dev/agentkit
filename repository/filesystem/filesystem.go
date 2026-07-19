// Package filesystem is a single-process, crash-atomic reference implementation
// of agentkit.Repository backed by a single JSON snapshot file. It holds the
// same in-memory state as the memory implementation and, on every write,
// atomically replaces state.json via temp-file + fsync + rename + directory
// fsync (the rename is the commit point, so no WAL is needed).
//
// Constraints (stated honestly): it is single-process only — a LOCK file
// (flock) makes a second New on the same directory fail — and it rewrites the
// whole snapshot on every write, so I/O is O(state size). It suits development,
// tests, and small one-shot runs; large or high-throughput deployments should
// use a database-backed Repository. Serve's WithConcurrency (in-process
// goroutine concurrency) is supported.
package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/agentkit/repository/internal/store"
	"github.com/m-mizutani/goerr/v2"
	"golang.org/x/sys/unix"
)

const (
	lockName     = "LOCK"
	stateName    = "state.json"
	stateTmpName = "state.json.tmp"
)

// Repository is the filesystem-backed agentkit.Repository.
type Repository struct {
	mu    sync.Mutex
	dir   string
	lock  *os.File
	state *store.State

	// poisoned is set after a post-rename I/O failure leaves disk and memory in
	// an indeterminate relationship; every subsequent write returns it
	// (fail-stop). Recovery is Close + New (reload state.json).
	poisoned error

	// dirSync performs the post-rename directory fsync. It is a field so tests
	// can inject a failure and exercise the poison path.
	dirSync func(dir string) error
}

var _ agentkit.Repository = (*Repository)(nil)

// New opens (or creates) a filesystem Repository rooted at dir. It acquires an
// exclusive flock on {dir}/LOCK (a second concurrent New fails), removes any
// stray uncommitted state.json.tmp, and loads state.json if present.
func New(dir string) (*Repository, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, goerr.Wrap(err, "create repository dir", goerr.V("dir", dir))
	}

	// #nosec G304 -- dir is the store directory the caller owns and chose; this is a local single-process reference store.
	lockFile, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, goerr.Wrap(err, "open lock file", goerr.V("dir", dir))
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if cerr := lockFile.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, goerr.Wrap(err, "acquire exclusive lock (directory already open?)", goerr.V("dir", dir))
	}

	// A leftover temp file is a pre-rename (uncommitted) crash; discard it.
	if err := os.Remove(filepath.Join(dir, stateTmpName)); err != nil && !os.IsNotExist(err) {
		return nil, closeWith(lockFile, goerr.Wrap(err, "remove stray temp snapshot", goerr.V("dir", dir)))
	}

	var st *store.State
	// #nosec G304 -- dir is the store directory the caller owns and chose.
	data, err := os.ReadFile(filepath.Join(dir, stateName))
	switch {
	case err == nil:
		st, err = store.Load(data)
		if err != nil {
			return nil, closeWith(lockFile, goerr.Wrap(err, "load state snapshot", goerr.V("dir", dir)))
		}
	case os.IsNotExist(err):
		st = store.NewState()
	default:
		return nil, closeWith(lockFile, goerr.Wrap(err, "read state snapshot", goerr.V("dir", dir)))
	}

	return &Repository{
		dir:     dir,
		lock:    lockFile,
		state:   st,
		dirSync: fsyncDir,
	}, nil
}

// Close releases the flock. After Close the Repository must not be used;
// reopening with New on the same directory reloads the persisted state.
func (r *Repository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return nil
	}
	var errs []error
	if err := unix.Flock(int(r.lock.Fd()), unix.LOCK_UN); err != nil {
		errs = append(errs, err)
	}
	if err := r.lock.Close(); err != nil {
		errs = append(errs, err)
	}
	r.lock = nil
	if len(errs) > 0 {
		return goerr.Wrap(errors.Join(errs...), "close repository", goerr.V("dir", r.dir))
	}
	return nil
}

// GetProcess returns the Process, or agentkit.ErrProcessNotFound.
func (r *Repository) GetProcess(ctx context.Context, pid agentkit.ProcessID) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return nil, r.poisoned
	}
	return r.state.GetProcess(pid)
}

// FindProcessByIdempotencyKey finds a Process by idempotency key.
func (r *Repository) FindProcessByIdempotencyKey(ctx context.Context, key string) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return nil, r.poisoned
	}
	return r.state.FindByIdempotencyKey(key)
}

// FindOpenProcessBySubject finds an open Process holding the subject.
func (r *Repository) FindOpenProcessBySubject(ctx context.Context, subject agentkit.SubjectRef) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return nil, r.poisoned
	}
	return r.state.FindOpenBySubject(subject)
}

// ClaimNextProcess atomically claims one runnable Process and persists the
// claim. No target -> (nil, nil).
func (r *Repository) ClaimNextProcess(ctx context.Context, workerID string, leaseUntil time.Time, now time.Time) (*agentkit.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return nil, r.poisoned
	}
	claimed, next, err := r.state.ClaimNext(workerID, leaseUntil, now)
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return nil, nil
	}
	if err := r.commit(next); err != nil {
		return nil, err
	}
	return claimed, nil
}

// ListAwaits returns all awaits of a Process.
func (r *Repository) ListAwaits(ctx context.Context, pid agentkit.ProcessID) ([]*agentkit.Await, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return nil, r.poisoned
	}
	return r.state.ListAwaits(pid), nil
}

// ListEvents returns a Process's events in append order.
func (r *Repository) ListEvents(ctx context.Context, pid agentkit.ProcessID) ([]*agentkit.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return nil, r.poisoned
	}
	return r.state.ListEvents(pid), nil
}

// Apply applies a ChangeSet atomically and persists it. On any precondition
// failure it writes nothing and returns agentkit.ErrConflict.
func (r *Repository) Apply(ctx context.Context, cs agentkit.ChangeSet) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.poisoned != nil {
		return r.poisoned
	}
	next, err := r.state.After(cs)
	if err != nil {
		return err // ErrConflict: memory and disk untouched.
	}
	return r.commit(next)
}

// commit persists next and advances in-memory state. It must be called with the
// mutex held and poisoned already checked. It enforces the post-rename error
// contract: a pre-rename failure leaves state untouched (safe to retry); a
// post-rename failure advances state, poisons the Repository, and returns
// agentkit.ErrRepositoryIndeterminate.
func (r *Repository) commit(next *store.State) error {
	if err := r.writeSnapshot(next); err != nil {
		var pr *postRenameError
		if errors.As(err, &pr) {
			r.state = next
			r.poisoned = goerr.Wrap(agentkit.ErrRepositoryIndeterminate,
				"post-rename fsync failed; repository fail-stopped, reopen to recover",
				goerr.V("dir", r.dir))
			return r.poisoned
		}
		return err
	}
	r.state = next
	return nil
}

// postRenameError marks a failure that occurred after the atomic rename
// committed, so disk holds the new state while memory has not yet advanced.
type postRenameError struct{ err error }

func (e *postRenameError) Error() string { return e.err.Error() }
func (e *postRenameError) Unwrap() error { return e.err }

// writeSnapshot writes next to a temp file, fsyncs and renames it onto
// state.json (the commit point), then fsyncs the directory. A directory fsync
// failure is returned as *postRenameError.
func (r *Repository) writeSnapshot(next *store.State) error {
	data, err := next.Marshal()
	if err != nil {
		return goerr.Wrap(err, "marshal snapshot")
	}
	tmp := filepath.Join(r.dir, stateTmpName)
	final := filepath.Join(r.dir, stateName)

	// #nosec G304 -- tmp is derived from the caller-owned store directory.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return goerr.Wrap(err, "open temp snapshot", goerr.V("path", tmp))
	}
	if _, err := f.Write(data); err != nil {
		return removeAndClose(f, tmp, goerr.Wrap(err, "write temp snapshot"))
	}
	if err := f.Sync(); err != nil {
		return removeAndClose(f, tmp, goerr.Wrap(err, "fsync temp snapshot"))
	}
	if err := f.Close(); err != nil {
		if rerr := os.Remove(tmp); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return goerr.Wrap(err, "close temp snapshot")
	}
	if err := os.Rename(tmp, final); err != nil {
		if rerr := os.Remove(tmp); rerr != nil && !os.IsNotExist(rerr) {
			err = errors.Join(err, rerr)
		}
		return goerr.Wrap(err, "rename temp snapshot")
	}
	// Commit point passed: from here a failure is post-rename.
	if err := r.dirSync(r.dir); err != nil {
		return &postRenameError{err: goerr.Wrap(err, "fsync directory", goerr.V("dir", r.dir))}
	}
	return nil
}

// removeAndClose removes the temp file and closes f, joining any cleanup errors
// into cause.
func removeAndClose(f *os.File, tmp string, cause error) error {
	if cerr := f.Close(); cerr != nil {
		cause = errors.Join(cause, cerr)
	}
	if rerr := os.Remove(tmp); rerr != nil && !os.IsNotExist(rerr) {
		cause = errors.Join(cause, rerr)
	}
	return cause
}

// closeWith closes the lock file (joining any error) while returning cause; used
// on New's error paths.
func closeWith(lockFile *os.File, cause error) error {
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_UN); err != nil {
		cause = errors.Join(cause, err)
	}
	if err := lockFile.Close(); err != nil {
		cause = errors.Join(cause, err)
	}
	return cause
}

// fsyncDir fsyncs a directory so a rename within it is durable.
func fsyncDir(dir string) error {
	// #nosec G304 -- dir is the caller-owned store directory (opened to fsync it).
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		if cerr := d.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return err
	}
	return d.Close()
}
