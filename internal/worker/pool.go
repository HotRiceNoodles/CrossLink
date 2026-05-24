package worker

import (
	"context"
	"log/slog"
	"sync"
)

type Pool struct {
	ch chan func()
	wg sync.WaitGroup
}

func NewPool(workerCount int, bufferSize int) *Pool {
	p := &Pool{
		ch: make(chan func(), bufferSize),
	}
	for i := 0; i < workerCount; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for fn := range p.ch {
				func() {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("worker task panicked", "error", r)
						}
					}()
					fn()
				}()
			}
		}()
	}
	return p
}

func (p *Pool) Submit(fn func()) {
	select {
	case p.ch <- fn:
	default:
		slog.Warn("worker pool full, dropping task")
	}
}

func (p *Pool) Shutdown(ctx context.Context) {
	close(p.ch)
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("worker pool shutdown timeout, some tasks may be dropped")
	}
}
