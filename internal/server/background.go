// background.go - Cancellation and joining for service-owned goroutines.
package server

import (
	"context"
	"sync"
)

// backgroundTasks prevents new goroutines once stopping begins and joins owners.
type backgroundTasks struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

func newBackgroundTasks() *backgroundTasks {
	ctx, cancel := context.WithCancel(context.Background())
	return &backgroundTasks{ctx: ctx, cancel: cancel}
}

func (tasks *backgroundTasks) run(fn func()) bool {
	if !tasks.begin() {
		return false
	}
	go func() {
		defer tasks.wg.Done()
		fn()
	}()
	return true
}

func (tasks *backgroundTasks) begin() bool {
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if tasks.stopped {
		return false
	}
	tasks.wg.Add(1)
	return true
}

func (tasks *backgroundTasks) stop() {
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	tasks.stopped = true
	tasks.cancel()
}

func (tasks *backgroundTasks) close() {
	tasks.stop()
	tasks.wg.Wait()
}
