package vault

import (
	"context"
	"runtime"
	"sync"
)

// defaultWorkers returns the number of workers to use for parallel operations.
// Capped at 8 to avoid overwhelming the filesystem.
func defaultWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		return 8
	}
	return n
}

// workerPool runs fn jobs on workers goroutines, collecting results in
// submission order. It stops dispatching on the first error but drains
// in-flight work before returning.
func workerPool[J any, R any](ctx context.Context, jobs []J, workers int, fn func(context.Context, J) (R, error)) ([]R, error) {
	if workers <= 0 {
		workers = defaultWorkers()
	}
	type indexed struct {
		idx int
		val R
		err error
	}

	jobCh := make(chan struct {
		idx int
		job J
	}, len(jobs))
	for i, j := range jobs {
		jobCh <- struct {
			idx int
			job J
		}{i, j}
	}
	close(jobCh)

	resultCh := make(chan indexed, len(jobs))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobCh {
				if ctx.Err() != nil {
					resultCh <- indexed{idx: item.idx, err: ctx.Err()}
					continue
				}
				val, err := fn(ctx, item.job)
				resultCh <- indexed{idx: item.idx, val: val, err: err}
			}
		}()
	}
	wg.Wait()
	close(resultCh)

	results := make([]R, len(jobs))
	var firstErr error
	for r := range resultCh {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		results[r.idx] = r.val
	}
	return results, firstErr
}
