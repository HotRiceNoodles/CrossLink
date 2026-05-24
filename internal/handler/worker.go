package handler

import (
	"github.com/crosslink/internal/worker"
)

// UsageWorkers is a package-level worker pool for async usage logging.
// Set once during app initialization via SetUsageWorkers.
var UsageWorkers *worker.Pool

// SetUsageWorkers initializes the package-level usage worker pool.
func SetUsageWorkers(p *worker.Pool) {
	UsageWorkers = p
}

// submitUsage submits a task to the worker pool, falling back to a goroutine if the pool is nil.
func submitUsage(fn func()) {
	if UsageWorkers != nil {
		UsageWorkers.Submit(fn)
		return
	}
	go fn()
}
