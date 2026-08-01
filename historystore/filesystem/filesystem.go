// Package filesystem is a single-process reference implementation of
// agentkit.HistoryStore backed by one JSON file per History version. Each
// Process gets its own {dir}/{pid}/ directory, and each version a
// {dir}/{pid}/{ref}.json written atomically via temp-file + fsync + rename +
// directory fsync (the rename is the commit point), mirroring the discipline in
// repository/filesystem.
//
// Versions are immutable, as the port requires: Save mints a fresh ref and
// writes a new file, never touching one it handed out earlier. Refs are UUIDv7,
// so the directory listing reads in creation order. Discard removes the file.
//
// Constraints (stated honestly): it is single-process only — a LOCK file
// (flock) makes a second New on the same directory fail — and there is no
// snapshot to reload, so every Load reads its file directly. It suits
// development, tests, and small one-shot runs. It has no reclamation policy of
// its own beyond Discard, so versions left behind by a crash between a save and
// its commit stay on disk until someone removes them.
package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gollem-dev/agentkit"
	"github.com/gollem-dev/gollem"
	"github.com/google/uuid"
	"github.com/m-mizutani/goerr/v2"
	"golang.org/x/sys/unix"
)

const lockName = "LOCK"

// Store is the filesystem-backed agentkit.HistoryStore.
type Store struct {
	mu   sync.Mutex
	dir  string
	lock *os.File

	// dirSync performs the post-rename directory fsync. It is a field so tests
	// can inject a failure.
	dirSync func(dir string) error
}

var _ agentkit.HistoryStore = (*Store)(nil)

// New opens (or creates) a filesystem Store rooted at dir. It acquires an
// exclusive flock on {dir}/LOCK; a second concurrent New on the same
// directory fails.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, goerr.Wrap(err, "create store dir", goerr.V("dir", dir))
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

	return &Store{
		dir:     dir,
		lock:    lockFile,
		dirSync: fsyncDir,
	}, nil
}

// Close releases the flock. After Close the Store must not be used; reopening
// with New on the same directory is safe (each version lives in its own file, so
// there is no snapshot to reload).
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	var errs []error
	// #nosec G115 -- see the note in New; a file descriptor cannot overflow int.
	if err := unix.Flock(int(s.lock.Fd()), unix.LOCK_UN); err != nil {
		errs = append(errs, err)
	}
	if err := s.lock.Close(); err != nil {
		errs = append(errs, err)
	}
	s.lock = nil
	if len(errs) > 0 {
		return goerr.Wrap(errors.Join(errs...), "close store", goerr.V("dir", s.dir))
	}
	return nil
}

// Save marshals h and writes it to {dir}/{pid}/{ref}.json under a fresh UUIDv7
// ref, via temp file -> fsync -> rename -> directory fsync. It never touches a
// version returned by an earlier Save.
func (s *Store) Save(ctx context.Context, pid agentkit.ProcessID, h *gollem.History) (agentkit.HistoryRef, error) {
	ref := agentkit.HistoryRef(uuid.Must(uuid.NewV7()).String())

	s.mu.Lock()
	defer s.mu.Unlock()

	procDir, path, err := s.versionPath(pid, ref)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(procDir, 0o700); err != nil {
		return "", goerr.Wrap(err, "create process dir", goerr.V("dir", procDir))
	}

	data, err := json.Marshal(h)
	if err != nil {
		return "", goerr.Wrap(err, "marshal history", goerr.V("process", pid), goerr.V("ref", ref))
	}
	if err := s.writeFile(procDir, path, data); err != nil {
		return "", err
	}
	return ref, nil
}

// Load reads {dir}/{pid}/{ref}.json. A missing file is
// agentkit.ErrHistoryVersionMissing: the caller referenced a version, so its
// absence is data loss rather than an empty conversation. Unmarshal errors
// (including gollem's Version gate) propagate wrapped, not swallowed.
func (s *Store) Load(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) (*gollem.History, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, path, err := s.versionPath(pid, ref)
	if err != nil {
		return nil, err
	}

	// #nosec G304 -- path is derived from the caller-owned store directory plus a validated pid and ref.
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// fall through
	case os.IsNotExist(err):
		return nil, goerr.Wrap(agentkit.ErrHistoryVersionMissing, "no such history version",
			goerr.V("process", pid), goerr.V("ref", ref))
	default:
		return nil, goerr.Wrap(err, "read history file", goerr.V("path", path))
	}

	var h gollem.History
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, goerr.Wrap(err, "unmarshal history", goerr.V("path", path), goerr.V("process", pid))
	}
	return &h, nil
}

// Discard removes the version's file. An unknown ref, an already-discarded one,
// and an unsafe path component are all silent no-ops: Discard is a notification
// with nothing to report back (agentkit.HistoryStore).
func (s *Store) Discard(ctx context.Context, pid agentkit.ProcessID, ref agentkit.HistoryRef) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, path, err := s.versionPath(pid, ref)
	if err != nil {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return
	}
}

// versionPath validates pid and ref and returns the process directory and the
// version file path. Both must be non-empty and free of path separators and
// "..", so neither can escape dir.
func (s *Store) versionPath(pid agentkit.ProcessID, ref agentkit.HistoryRef) (string, string, error) {
	if err := checkComponent(string(pid)); err != nil {
		return "", "", goerr.Wrap(err, "unsafe process id", goerr.V("process", pid))
	}
	if err := checkComponent(string(ref)); err != nil {
		return "", "", goerr.Wrap(err, "unsafe history ref", goerr.V("ref", ref))
	}
	procDir := filepath.Join(s.dir, string(pid))
	return procDir, filepath.Join(procDir, string(ref)+".json"), nil
}

func checkComponent(v string) error {
	if v == "" || strings.ContainsAny(v, "/\\") || strings.Contains(v, "..") {
		return ErrInvalidPathComponent
	}
	return nil
}

// writeFile writes data to path via temp file -> fsync -> rename -> directory
// fsync (the rename is the commit point). procDir is the directory holding path,
// and the one whose fsync makes the rename durable.
func (s *Store) writeFile(procDir, path string, data []byte) error {
	tmp := path + ".tmp"

	// #nosec G304 -- tmp is derived from the caller-owned store directory and a validated pid and ref.
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
	if err := s.dirSync(procDir); err != nil {
		return goerr.Wrap(err, "fsync directory", goerr.V("dir", procDir))
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
