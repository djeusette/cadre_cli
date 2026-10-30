package cli

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestGuardInterruptRestoresEchoAndExits130(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	exited := make(chan int, 1)
	var out bytes.Buffer
	restored := 0

	go guardInterrupt(sigs, done, func() { restored++ }, &out, func(code int) { exited <- code })
	sigs <- syscall.SIGTERM

	select {
	case code := <-exited:
		if code != 130 {
			t.Errorf("exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never exited")
	}
	if restored != 1 {
		t.Errorf("restored %d times", restored)
	}
	if out.String() != "\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestGuardInterruptStopsWhenTheReadIsDone(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	returned := make(chan struct{})
	restore := func() { t.Error("echo restored without a signal") }

	go func() {
		guardInterrupt(sigs, done, restore, &bytes.Buffer{}, func(int) { t.Error("exited without a signal") })
		close(returned)
	}()
	close(done)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("never returned")
	}
}
