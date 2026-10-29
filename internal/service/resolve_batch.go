package service

import (
	"context"
	"sync"
)

// MaxResolveBatch bounds the items of one ResolveBatch call.
const MaxResolveBatch = 100

// resolveParallel is how many probes a batch runs at once.
const resolveParallel = 4

// ResolveOutcome is the result of one item of ResolveBatch.
type ResolveOutcome struct {
	Result *ResolveResult
	Err    error
}

// ResolveBatch probes several sources, a few at a time, and answers one
// outcome per input in the same order; one failing source does not stop the
// others.
func (s *TaskService) ResolveBatch(ctx context.Context, in []ResolveInput) []ResolveOutcome {
	out := make([]ResolveOutcome, len(in))
	var wg sync.WaitGroup
	sem := make(chan struct{}, resolveParallel)
	for i := range in {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			res, err := s.Resolve(ctx, in[i])
			out[i] = ResolveOutcome{Result: res, Err: err}
		}()
	}
	wg.Wait()
	return out
}
