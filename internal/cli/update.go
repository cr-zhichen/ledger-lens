package cli

import (
	"context"
	"time"

	"ledger-lens/internal/update"
	"ledger-lens/internal/version"
)

const automaticUpdateTimeout = 2 * time.Second

type updateChecker interface {
	Check(context.Context, string) (update.Result, error)
	CheckAutomatic(context.Context, string) (update.Result, error)
	Cached(string) update.Result
}

// Run the network request alongside the command, but serialize all output in Run.
func startUpdateCheck(ctx context.Context, checker updateChecker, current string, disabled bool) func() *update.Notice {
	if disabled || !version.IsStable(current) {
		return func() *update.Notice { return nil }
	}
	cachedNotice := checker.Cached(current).Notice()
	checkCtx, cancel := context.WithTimeout(ctx, automaticUpdateTimeout)
	done := make(chan *update.Notice, 1)
	go func() {
		// A failed request can still return a previously confirmed newer release.
		result, _ := checker.CheckAutomatic(checkCtx, current)
		done <- result.Notice()
	}()
	return func() *update.Notice {
		defer cancel()
		// Prefer an already completed result, even when the main command ran longer.
		select {
		case notice := <-done:
			return notice
		default:
		}
		select {
		case notice := <-done:
			return notice
		case <-checkCtx.Done():
			return cachedNotice
		}
	}
}
