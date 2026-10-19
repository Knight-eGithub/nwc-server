package main

import (
	"context"
	"errors"
	"time"
)

var (
	ErrQueueTimeout = errors.New("queue timeout: server is busy, please try again")
)

// WorkerPool manages access to execution slots (Box 0 and Box 1)
type WorkerPool struct {
	availableSlots chan int
}

// NewWorkerPool initializes the pool with 2 slots (0 and 1)
func NewWorkerPool(maxConcurrent int) *WorkerPool {
	pool := &WorkerPool{
		availableSlots: make(chan int, maxConcurrent),
	}
	for i := 0; i < maxConcurrent; i++ {
		pool.availableSlots <- i
	}
	return pool
}

// Acquire waits for an available slot with a timeout
func (p *WorkerPool) Acquire(ctx context.Context, timeout time.Duration) (int, error) {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case slotID := <-p.availableSlots:
		return slotID, nil
	case <-ctxWithTimeout.Done():
		return -1, ErrQueueTimeout
	}
}

// Release returns the slot back to the pool so the next queued request can run
func (p *WorkerPool) Release(slotID int) {
	p.availableSlots <- slotID
}