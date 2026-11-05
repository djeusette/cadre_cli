package cli

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// forwarding runs forwardSignals until the test ends, and returns its channel of signals and what it forwarded.
func forwarding(t *testing.T) (chan<- os.Signal, <-chan os.Signal) {
	t.Helper()
	sigs := make(chan os.Signal)
	forwarded := make(chan os.Signal, 2)
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go forwardSignals(sigs, done, func(sig os.Signal) { forwarded <- sig })
	return sigs, forwarded
}

func nextForwarded(t *testing.T, forwarded <-chan os.Signal) os.Signal {
	t.Helper()
	select {
	case sig := <-forwarded:
		return sig
	case <-time.After(2 * time.Second):
		t.Fatal("nothing forwarded")
		return nil
	}
}

func TestForwardSignalsForwardsEverySIGTERM(t *testing.T) {
	sigs, forwarded := forwarding(t)

	for range 2 {
		sigs <- syscall.SIGTERM
		if sig := nextForwarded(t, forwarded); sig != syscall.SIGTERM {
			t.Errorf("forwarded %v", sig)
		}
	}
}

// The SIGTERM behind the interrupt shows the interrupt was read, and dropped.
func TestForwardSignalsNeverForwardsAnInterrupt(t *testing.T) {
	sigs, forwarded := forwarding(t)

	sigs <- os.Interrupt
	sigs <- syscall.SIGTERM

	if sig := nextForwarded(t, forwarded); sig != syscall.SIGTERM {
		t.Errorf("forwarded %v", sig)
	}
}

func TestForwardSignalsReturnsWhenDoneCloses(t *testing.T) {
	done := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		forwardSignals(make(chan os.Signal), done, func(os.Signal) { t.Error("forwarded without a signal") })
		close(returned)
	}()
	close(done)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("never returned")
	}
}
