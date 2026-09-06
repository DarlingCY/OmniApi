//go:build !windows

package tray

import "context"

// run has no GUI on non-Windows builds; it simply waits for cancellation so
// server deployments never link a desktop toolkit.
func run(ctx context.Context, _ Options) {
	<-ctx.Done()
}
