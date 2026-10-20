package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrQueueTimeout = errors.New("queue timeout: server is busy, please try again")
)

// WorkerPool manages access to execution slots with strict occupancy tracking
type WorkerPool struct {
	mu       sync.Mutex
	notify   chan struct{}
	slots    []int
	occupied map[int]bool
}

// NewWorkerPool initializes the pool with dedicated slots (starting from slot 1)
func NewWorkerPool(maxConcurrent int) *WorkerPool {
	p := &WorkerPool{
		notify:   make(chan struct{}, maxConcurrent),
		slots:    make([]int, 0, maxConcurrent),
		occupied: make(map[int]bool),
	}

	for i := 1; i <= maxConcurrent; i++ {
		p.slots = append(p.slots, i)
		p.occupied[i] = false
	}
	return p
}

// Acquire finds the first non-occupied slot, prioritizing slot 1 whenever free
func (p *WorkerPool) Acquire(ctx context.Context, timeout time.Duration) (int, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		p.mu.Lock()
		// Always pick the lowest-indexed unoccupied slot so we never alternate to an occupied/secondary slot unnecessarily
		for _, id := range p.slots {
			if !p.occupied[id] {
				p.occupied[id] = true
				p.mu.Unlock()
				return id, nil
			}
		}
		p.mu.Unlock()

		// If all slots are currently occupied, wait until one is released or timeout expires
		select {
		case <-p.notify:
			// A slot was released, retry acquisition
		case <-timer.C:
			return -1, ErrQueueTimeout
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
}

// Release marks the slot as unoccupied and notifies waiting callers
func (p *WorkerPool) Release(slotID int) {
	p.mu.Lock()
	p.occupied[slotID] = false
	p.mu.Unlock()

	// Non-blocking notification to waiting callers
	select {
	case p.notify <- struct{}{}:
	default:
	}
}
