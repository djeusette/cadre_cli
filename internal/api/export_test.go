package api

import (
	"testing"
	"time"
)

// SetTimeouts makes calls wait def, and SetTarget wait setTarget, until the test ends.
func SetTimeouts(t *testing.T, def, setTarget time.Duration) {
	t.Helper()
	oldDefault, oldSetTarget := defaultTimeout, setTargetTimeout
	defaultTimeout, setTargetTimeout = def, setTarget
	t.Cleanup(func() { defaultTimeout, setTargetTimeout = oldDefault, oldSetTarget })
}
