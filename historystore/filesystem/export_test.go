package filesystem

// SetDirSyncForTest replaces the post-rename directory fsync step so a test
// can inject a failure and exercise that error path.
func (r *Repository) SetDirSyncForTest(fn func(dir string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dirSync = fn
}
