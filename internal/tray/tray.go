package tray

import "context"

// Options configures the tray integration.
type Options struct {
	// ConsoleURL resolves the configuration URL at click time so it always
	// carries the current admin token.
	ConsoleURL func() string
}

// Run shows the tray icon and blocks until the user quits or ctx is done.
// Non-Windows builds have no tray, so Run waits for cancellation instead.
func Run(ctx context.Context, options Options) {
	run(ctx, options)
}
