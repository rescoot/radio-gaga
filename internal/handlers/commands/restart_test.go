package commands

import (
	"testing"
	"time"
)

func TestHandleRestartCommand(t *testing.T) {
	origGrace, origFunc := restartGrace, restartFunc
	defer func() { restartGrace, restartFunc = origGrace, origFunc }()

	called := make(chan struct{}, 1)
	restartGrace = 5 * time.Millisecond
	restartFunc = func() error { called <- struct{}{}; return nil }

	if err := HandleRestartCommand(); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("restart was not triggered after the grace period")
	}
}
