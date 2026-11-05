//go:build !linux

package intake

// limitDataSegment is a no-op off Linux: the worker relies on the Go soft
// memory limit, the timeout and process isolation alone.
func limitDataSegment(int64) error { return nil }
