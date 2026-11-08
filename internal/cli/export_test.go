package cli

import (
	"testing"
	"time"
)

// SetVercelWhoamiTimeout makes cadre wait d for vercel whoami until the test ends.
func SetVercelWhoamiTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := vercelWhoamiTimeout
	vercelWhoamiTimeout = d
	t.Cleanup(func() { vercelWhoamiTimeout = old })
}
