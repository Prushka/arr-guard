package probe

// SnapshotChangedError indicates that a fresh read/probe may resolve a changed
// subtitle snapshot. It never authorizes replaying a subsequent mutation.
type SnapshotChangedError struct{ error }

func (e *SnapshotChangedError) Unwrap() error { return e.error }

func snapshotChanged(err error) error { return &SnapshotChangedError{err} }
