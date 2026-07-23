// Package filesystem is a single-process reference implementation of
// gollem.HistoryRepository backed by one JSON blob file per session. Each
// session gets its own {dir}/{sessionID}.json, written atomically via
// temp-file + fsync + rename + directory fsync (the rename is the commit
// point), mirroring the discipline in repository/filesystem.
//
// Constraints (stated honestly): it is single-process only — a LOCK file
// (flock) makes a second New on the same directory fail — and unlike
// repository/filesystem there is no single snapshot to reload, so each
// session's history is read directly from its own file on every Load. It
// suits development, tests, and small one-shot runs.
package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gollem-dev/gollem"
	"github.com/m-mizutani/goerr/v2"
	"golang.org/x/sys/unix"
)

const lockName = "LOCK"

// Repository is the filesystem-backed gollem.HistoryRepository.
type Repository struct {
	mu   sync.Mutex
	dir  string
	lock *os.File

	// dirSync performs the post-rename directory fsync. It is a field so tests
	// can inject a failure.
	dirSync func(dir string) error
}

var _ gollem.HistoryRepository = (*Repository)(nil)

// New opens (or creates) a filesystem Repository rooted at dir. It acquires an
// exclusive flock on {dir}/LOCK; a second concurrent New on the same
// directory fails.
func New(dir string) (*Repository, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, goerr.Wrap(err, "create repository dir", goerr.V("dir", dir))
	}

	// #nosec G304 -- dir is the store directory the caller owns and chose; this is a local single-process reference store.
	lockFile, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, goerr.Wrap(err, "open lock file", goerr.V("dir", dir))
	}
	// #nosec G115 -- Fd() returns a file descriptor as a uintptr and the syscall
	// wrapper takes an int; a descriptor is small and this cannot overflow.
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if cerr := lockFile.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, goerr.Wrap(err, "acquire exclusive lock (directory already open?)", goerr.V("dir", dir))
	}

	return &Repository{
		dir:     dir,
		lock:    lockFile,
		dirSync: fsyncDir,
	}, nil
}

// Close releases the flock. After Close the Repository must not be used;
// reopening with New on the same directory is safe (each session's history
// lives in its own file, so there is no snapshot to reload).
func (r *Repository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return nil
	}
	var errs []error
	// #nosec G115 -- see the note in New; a file descriptor cannot overflow int.
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

// Load reads {dir}/{sessionID}.json and unmarshals it into a *gollem.History.
// If the file does not exist it returns (nil, nil), the HistoryRepository
// contract for an unsaved session ID. Unmarshal errors (including gollem's
// Version gate) propagate wrapped, not swallowed.
func (r *Repository) Load(ctx context.Context, sessionID string) (*gollem.History, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	path, err := r.sessionPath(sessionID)
	if err != nil {
		return nil, err
	}

	// #nosec G304 -- path is derived from the caller-owned store directory plus a validated sessionID.
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// fall through
	case os.IsNotExist(err):
		return nil, nil
	default:
		return nil, goerr.Wrap(err, "read history file", goerr.V("path", path))
	}

	var h gollem.History
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, goerr.Wrap(err, "unmarshal history", goerr.V("path", path), goerr.V("sessionID", sessionID))
	}
	return &h, nil
}

// Save marshals history and atomically writes it to {dir}/{sessionID}.json
// via temp file -> fsync -> rename -> directory fsync. A previous file under
// the same sessionID is overwritten (last-writer-wins).
func (r *Repository) Save(ctx context.Context, sessionID string, history *gollem.History) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path, err := r.sessionPath(sessionID)
	if err != nil {
		return err
	}

	data, err := json.Marshal(history)
	if err != nil {
		return goerr.Wrap(err, "marshal history", goerr.V("sessionID", sessionID))
	}

	return r.writeFile(path, data)
}

// sessionPath validates sessionID and returns {dir}/{sessionID}.json.
// sessionID must be a non-empty string with no path separator and no "..",
// so it can never escape dir.
func (r *Repository) sessionPath(sessionID string) (string, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\") || strings.Contains(sessionID, "..") {
		return "", goerr.Wrap(ErrInvalidSessionID, "unsafe session id", goerr.V("sessionID", sessionID))
	}
	return filepath.Join(r.dir, sessionID+".json"), nil
}

// writeFile writes data to path via temp file -> fsync -> rename -> directory
// fsync (the rename is the commit point).
func (r *Repository) writeFile(path string, data []byte) error {
	tmp := path + ".tmp"

	// #nosec G304 -- tmp is derived from the caller-owned store directory and a validated sessionID.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return goerr.Wrap(err, "open temp history file", goerr.V("path", tmp))
	}
	if _, err := f.Write(data); err != nil {
		return removeAndClose(f, tmp, goerr.Wrap(err, "write temp history file"))
	}
	if err := f.Sync(); err != nil {
		return removeAndClose(f, tmp, goerr.Wrap(err, "fsync temp history file"))
	}
	if err := f.Close(); err != nil {
		if rerr := os.Remove(tmp); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return goerr.Wrap(err, "close temp history file")
	}
	if err := os.Rename(tmp, path); err != nil {
		if rerr := os.Remove(tmp); rerr != nil && !os.IsNotExist(rerr) {
			err = errors.Join(err, rerr)
		}
		return goerr.Wrap(err, "rename temp history file")
	}
	// Commit point passed: the rename is durable in the page cache; the
	// directory fsync below only guarantees it survives a crash.
	if err := r.dirSync(r.dir); err != nil {
		return goerr.Wrap(err, "fsync directory", goerr.V("dir", r.dir))
	}
	return nil
}

// removeAndClose removes the temp file and closes f, joining any cleanup
// errors into cause.
func removeAndClose(f *os.File, tmp string, cause error) error {
	if cerr := f.Close(); cerr != nil {
		cause = errors.Join(cause, cerr)
	}
	if rerr := os.Remove(tmp); rerr != nil && !os.IsNotExist(rerr) {
		cause = errors.Join(cause, rerr)
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
