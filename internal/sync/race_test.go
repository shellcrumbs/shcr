package sync

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Trigger and EnableTriggers touch the same field, and Run calls EnableTriggers
// itself — so a caller who starts Run in a goroutine and triggers from another
// is racing, whatever the daemon happens to do today.
func TestTriggeringWhileRunStartsIsSafe(t *testing.T) {
	e := &Engine{Enabled: func() bool { return false }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = e.Run(ctx, DefaultLoopConfig()) }()
	go func() {
		defer wg.Done()
		for range 200 {
			e.Trigger(TriggerCommand)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()
}
