package routing

import (
	"context"
	"fmt"
	"sync"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/upstream"
)

// Failure is the last upstream failure observed while routing.
type Failure struct {
	Status  int
	Message string
}

// AllProvidersFailedError reports that every candidate target was exhausted.
type AllProvidersFailedError struct {
	Last Failure
}

// Error implements error.
func (e *AllProvidersFailedError) Error() string { return "all_providers_failed" }

// CallFunc performs one upstream attempt against a resolved target.
type CallFunc func(ctx context.Context, target model.Target) (*upstream.Result, error)

// IsSwitchableStatus reports whether a status justifies trying the next provider.
func IsSwitchableStatus(status int) bool {
	switch status {
	case 0, 401, 403, 404, 408, 429:
		return true
	}
	return status >= 500
}

// Router walks the targets of an exposed model round-robin with failover.
type Router struct {
	mutex  sync.Mutex
	cursor map[string]int
}

// NewRouter creates an empty router.
func NewRouter() *Router {
	return &Router{cursor: make(map[string]int)}
}

func (r *Router) advance(exposedName string, length int) int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	start := r.cursor[exposedName]
	if start >= length {
		start = 0
	}
	r.cursor[exposedName] = (start + 1) % length
	return start
}

// Execute tries each target once, starting from the rotating cursor.
func (r *Router) Execute(ctx context.Context, exposed model.ExposedModel, call CallFunc) (*upstream.Result, error) {
	noTargets := Failure{Status: 503, Message: "No enabled provider models"}
	if len(exposed.Targets) == 0 {
		return nil, &AllProvidersFailedError{Last: noTargets}
	}

	start := r.advance(exposed.Name, len(exposed.Targets))
	last := noTargets

	for offset := 0; offset < len(exposed.Targets); offset++ {
		target := exposed.Targets[(start+offset)%len(exposed.Targets)]
		result, err := call(ctx, target)
		if err != nil {
			status := 502
			message := "Upstream request failed"
			if failure, isFailure := err.(*upstream.Failure); isFailure {
				if failure.Status != 0 {
					status = failure.Status
				}
				if failure.Message != "" {
					message = failure.Message
				}
			}
			last = Failure{Status: status, Message: message}
			if !IsSwitchableStatus(status) {
				return nil, &AllProvidersFailedError{Last: last}
			}
			continue
		}
		if result.Status >= 200 && result.Status < 300 {
			return result, nil
		}
		last = Failure{Status: result.Status, Message: fmt.Sprintf("Upstream returned %d", result.Status)}
		if !IsSwitchableStatus(result.Status) {
			return nil, &AllProvidersFailedError{Last: last}
		}
	}

	return nil, &AllProvidersFailedError{Last: last}
}
