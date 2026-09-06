package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/omniapi/omni-api/internal/model"
)

const runtimeLogCapacity = 1000

type runtimeLog struct {
	ID        uint64    `json:"id"`
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	RequestID string    `json:"requestId,omitempty"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail,omitempty"`
}

type runtimeLogBuffer struct {
	mutex      sync.Mutex
	generation string
	next       uint64
	items      []runtimeLog
}

func (b *runtimeLogBuffer) add(entry runtimeLog) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.next++
	entry.ID = b.next
	entry.Time = time.Now().UTC()
	if len(b.items) == runtimeLogCapacity {
		copy(b.items, b.items[1:])
		b.items = b.items[:len(b.items)-1]
	}
	b.items = append(b.items, entry)
}

// LogRuntime records a diagnostic event without retaining configured secrets.
func (s *Server) LogRuntime(level, requestID, message, detail string) {
	config := s.store.Get()
	secrets := []string{config.Settings.AdminToken, config.Settings.ProxyToken}
	for _, provider := range config.Providers {
		secrets = append(secrets, provider.APIKey)
	}
	for _, key := range config.AccessKeys {
		secrets = append(secrets, key.Secret)
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
			detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
		}
	}
	// Bound individual upstream diagnostics as well as the number of entries.
	if len(detail) > 4096 {
		detail = detail[:4096] + "…"
	}
	s.runtimeLogs.add(runtimeLog{Level: level, RequestID: requestID, Message: message, Detail: detail})
}

func (s *Server) listRuntimeLogs(writer http.ResponseWriter, request *http.Request) {
	s.runtimeLogs.mutex.Lock()
	afterID := uint64(0)
	if value := request.URL.Query().Get("afterId"); value != "" {
		if parsed, err := strconv.ParseUint(value, 10, 64); err == nil {
			afterID = parsed
		}
	}
	items := make([]runtimeLog, 0, len(s.runtimeLogs.items))
	for _, item := range s.runtimeLogs.items {
		if item.ID > afterID {
			items = append(items, item)
		}
	}
	generation := s.runtimeLogs.generation
	s.runtimeLogs.mutex.Unlock()
	writer.Header().Set("cache-control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": items, "capacity": runtimeLogCapacity, "generation": generation,
	})
}

func targetDetail(target model.Target, attempt int) string {
	// Credentials and query parameters in provider URLs are deliberately omitted.
	address := "invalid URL"
	if parsed, err := url.Parse(target.Provider.BaseURL); err == nil {
		address = parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
	}
	return fmt.Sprintf("attempt=%d provider=%s protocol=%s model=%s endpoint=%s", attempt,
		target.Provider.Name, target.Provider.Protocol, target.Model.UpstreamModel, address)
}

// trackWaiting makes stalled calls visible while they are still running.
// The returned stop function joins the reporter before a completion is logged.
func (s *Server) trackWaiting(requestID, stage, detail string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.LogRuntime("warn", requestID, stage, fmt.Sprintf("%s elapsed=%ds", detail, int(time.Since(started).Seconds())))
			}
		}
	}()
	return func() { close(stop); <-done }
}
