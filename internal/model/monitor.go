package model

import "time"

// Usage counts the tokens one upstream call consumed. CachedTokens is always
// normalized to be a subset of InputTokens so the cache rate stays comparable
// across protocols. CacheCreationTokens and ReasoningTokens are reported
// separately because providers include them in their total input or output.
type Usage struct {
	InputTokens         int `json:"inputTokens"`
	OutputTokens        int `json:"outputTokens"`
	CachedTokens        int `json:"cachedTokens"`
	CacheCreationTokens int `json:"cacheCreationTokens"`
	ReasoningTokens     int `json:"reasoningTokens"`
}

// Merge keeps the highest counter seen so far. Streaming providers split usage
// across several events and report cumulative totals, so the largest value of
// each counter is the final one.
func (u *Usage) Merge(other Usage) {
	if other.InputTokens > u.InputTokens {
		u.InputTokens = other.InputTokens
	}
	if other.OutputTokens > u.OutputTokens {
		u.OutputTokens = other.OutputTokens
	}
	if other.CachedTokens > u.CachedTokens {
		u.CachedTokens = other.CachedTokens
	}
	if other.CacheCreationTokens > u.CacheCreationTokens {
		u.CacheCreationTokens = other.CacheCreationTokens
	}
	if other.ReasoningTokens > u.ReasoningTokens {
		u.ReasoningTokens = other.ReasoningTokens
	}
}

// Empty reports whether the upstream reported no usage at all.
func (u Usage) Empty() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedTokens == 0 &&
		u.CacheCreationTokens == 0 && u.ReasoningTokens == 0
}

// RequestLogSummary reports aggregated metrics for a set of request logs.
type RequestLogSummary struct {
	TotalRequests   int     `json:"totalRequests"`
	TotalTokens     int64   `json:"totalTokens"`
	SuccessRate     float64 `json:"successRate"`
	AverageDuration int     `json:"averageDuration"`
}

// RequestLog is one recorded proxy request.
type RequestLog struct {
	ID            string    `json:"id"`
	StartedAt     time.Time `json:"startedAt"`
	Inbound       string    `json:"inbound"`
	ExposedModel  string    `json:"exposedModel"`
	UpstreamModel string    `json:"upstreamModel"`
	ProviderID    string    `json:"providerId"`
	ProviderName  string    `json:"providerName"`
	ClientAddress string    `json:"clientAddress"`
	Stream        bool      `json:"stream"`
	Status        int       `json:"status"`
	Attempts      int       `json:"attempts"`
	Error         string    `json:"error,omitempty"`
	Usage         Usage     `json:"usage"`
	FirstTokenMs  int       `json:"firstTokenMs"`
	DurationMs    int       `json:"durationMs"`
}
