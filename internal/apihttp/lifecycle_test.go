package apihttp

import (
	"context"
	"testing"
	"time"
)

// TestServerLifecycle covers Server.Start / UIServer.Start: bind, then shut down
// cleanly on context cancellation (ListenAndServe → ErrServerClosed → nil).
func TestServerLifecycle(t *testing.T) {
	s := fullServer(t)
	s.Addr = "127.0.0.1:0"
	ui := &UIServer{Addr: "127.0.0.1:0", Secrets: s.Secrets}

	for name, start := range map[string]func(context.Context) error{
		"api": s.Start,
		"ui":  ui.Start,
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- start(ctx) }()
		time.Sleep(150 * time.Millisecond) // let it bind
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s Start returned %v", name, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s Start did not return after cancel", name)
		}
	}
}
