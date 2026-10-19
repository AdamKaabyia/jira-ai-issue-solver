package scanner

import "context"

// ScanOnce exposes a single scan cycle for testing without the
// polling goroutine. Eliminates timing-based test synchronization.
func (s *TriageLabelScanner) ScanOnce(ctx context.Context) {
	s.scan(ctx)
}
