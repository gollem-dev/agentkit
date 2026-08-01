package filesystem

// SetDirSyncForTest replaces the post-rename directory fsync step so a test
// can inject a failure and exercise that error path.
func (s *Store) SetDirSyncForTest(fn func(dir string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirSync = fn
}
