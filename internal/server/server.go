package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/routing"
	"github.com/omniapi/omni-api/internal/store"
	"github.com/omniapi/omni-api/internal/upstream"
)

// Options configures the gateway HTTP handler.
type Options struct {
	Store  *store.Store
	Assets fs.FS
}

// Server exposes the admin API, the proxy entrypoints and the console assets.
type Server struct {
	store       *store.Store
	assets      fs.FS
	router      *routing.Router
	upstream    *upstream.Client
	mux         *http.ServeMux
	runtimeLogs runtimeLogBuffer
}

// New builds the gateway handler.
func New(options Options) *Server {
	server := &Server{
		store:    options.Store,
		assets:   options.Assets,
		router:   routing.NewRouter(),
		upstream: upstream.NewClient(60 * time.Second),
		mux:      http.NewServeMux(),
	}
	server.routes()
	server.LogRuntime("info", "", "网关已初始化", "上游单次请求超时为 60 秒；运行日志保留本次启动的最近 1000 条")
	return server
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.mux.ServeHTTP(writer, request)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/settings", s.admin(s.getSettings))
	s.mux.HandleFunc("PUT /api/v1/settings", s.admin(s.putSettings))
	s.mux.HandleFunc("GET /api/v1/providers", s.admin(s.listProviders))
	s.mux.HandleFunc("POST /api/v1/providers", s.admin(s.createProvider))
	s.mux.HandleFunc("POST /api/v1/providers/models:sync", s.admin(s.syncModels))
	s.mux.HandleFunc("PUT /api/v1/providers/{id}", s.admin(s.updateProvider))
	s.mux.HandleFunc("DELETE /api/v1/providers/{id}", s.admin(s.deleteProvider))
	s.mux.HandleFunc("GET /api/v1/request-logs", s.admin(s.listRequestLogs))
	s.mux.HandleFunc("GET /api/v1/runtime-logs", s.admin(s.listRuntimeLogs))
	s.mux.HandleFunc("DELETE /api/v1/request-logs", s.admin(s.clearRequestLogs))
	s.mux.HandleFunc("GET /api/v1/access-keys", s.admin(s.listAccessKeys))
	s.mux.HandleFunc("GET /api/v1/access-keys/{id}", s.admin(s.getAccessKey))
	s.mux.HandleFunc("POST /api/v1/access-keys", s.admin(s.createAccessKey))
	s.mux.HandleFunc("PUT /api/v1/access-keys/{id}", s.admin(s.updateAccessKey))
	s.mux.HandleFunc("DELETE /api/v1/access-keys/{id}", s.admin(s.deleteAccessKey))
	s.mux.HandleFunc("GET /v1/models", s.listExposedModels)
	s.mux.HandleFunc("POST /v1/chat/completions", s.proxy(kindChat))
	s.mux.HandleFunc("POST /v1/responses", s.proxy(kindResponses))
	s.mux.HandleFunc("POST /v1/messages", s.proxy(kindMessages))
	s.mux.HandleFunc("GET /", s.console)
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{"message": message}})
}

func decodeBody(request *http.Request, target any) error {
	raw, err := io.ReadAll(io.LimitReader(request.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return errors.New("request body is empty")
	}
	return json.Unmarshal(raw, target)
}

func bearer(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("authorization"))
	if strings.HasPrefix(value, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	}
	return ""
}

func proxyToken(request *http.Request) string {
	if token := bearer(request); token != "" {
		return token
	}
	if token := strings.TrimSpace(request.Header.Get("x-api-key")); token != "" {
		return token
	}
	return strings.TrimSpace(request.Header.Get("api-key"))
}

// admin allows open access until an admin token is configured, so the console
// can bootstrap itself on first run.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		expected := s.store.Get().Settings.AdminToken
		if expected == "" || bearer(request) == expected {
			next(writer, request)
			return
		}
		writeError(writer, http.StatusUnauthorized, "Unauthorized")
	}
}

func (s *Server) publicSettings() model.PublicSettings {
	settings := s.store.Get().Settings
	return model.PublicSettings{
		HasAdminToken: settings.AdminToken != "",
	}
}

func (s *Server) getSettings(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, s.publicSettings())
}

func (s *Server) putSettings(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		AdminToken *string `json:"adminToken"`
	}
	if err := decodeBody(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "Request body must be a JSON object")
		return
	}
	if err := s.store.UpdateSettings(body.AdminToken, nil); err != nil {
		writeError(writer, http.StatusInternalServerError, "Settings could not be saved")
		return
	}
	writeJSON(writer, http.StatusOK, s.publicSettings())
}

func (s *Server) listProviders(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, s.store.Get().Public().Providers)
}

func trimModels(models []model.ProviderModel) {
	for i := range models {
		models[i].UpstreamModel = strings.TrimSpace(models[i].UpstreamModel)
		models[i].Alias = strings.TrimSpace(models[i].Alias)
	}
}

func (s *Server) createProvider(writer http.ResponseWriter, request *http.Request) {
	var provider model.Provider
	if err := decodeBody(request, &provider); err != nil {
		writeError(writer, http.StatusBadRequest, "Request body must be a JSON object")
		return
	}
	if provider.ID == "" || provider.BaseURL == "" || !provider.Protocol.Valid() {
		writeError(writer, http.StatusBadRequest, "id, baseUrl and protocol are required")
		return
	}
	trimModels(provider.Models)
	config := s.store.Get()
	for _, existing := range config.Providers {
		if existing.ID == provider.ID {
			writeError(writer, http.StatusConflict, "Provider id already exists")
			return
		}
	}
	config.Providers = append(config.Providers, provider)
	if err := s.store.Replace(config); err != nil {
		writeError(writer, http.StatusInternalServerError, "Provider could not be saved")
		return
	}
	provider.APIKey = ""
	writeJSON(writer, http.StatusCreated, provider)
}

func (s *Server) updateProvider(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	var body struct {
		model.Provider
		APIKey *string `json:"apiKey"`
	}
	if err := decodeBody(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "Request body must be a JSON object")
		return
	}
	config := s.store.Get()
	index := -1
	for i, existing := range config.Providers {
		if existing.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		writeError(writer, http.StatusNotFound, "Provider not found")
		return
	}
	replacement := body.Provider
	replacement.ID = id
	trimModels(replacement.Models)
	if body.APIKey == nil {
		replacement.APIKey = config.Providers[index].APIKey
	} else {
		replacement.APIKey = *body.APIKey
	}
	config.Providers[index] = replacement
	if err := s.store.Replace(config); err != nil {
		writeError(writer, http.StatusInternalServerError, "Provider could not be saved")
		return
	}
	replacement.APIKey = ""
	writeJSON(writer, http.StatusOK, replacement)
}

func (s *Server) deleteProvider(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	config := s.store.Get()
	remaining := make([]model.Provider, 0, len(config.Providers))
	for _, existing := range config.Providers {
		if existing.ID != id {
			remaining = append(remaining, existing)
		}
	}
	if len(remaining) == len(config.Providers) {
		writeError(writer, http.StatusNotFound, "Provider not found")
		return
	}
	config.Providers = remaining
	if err := s.store.Replace(config); err != nil {
		writeError(writer, http.StatusInternalServerError, "Provider could not be removed")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncModels(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ProviderID string         `json:"providerId"`
		Protocol   model.Protocol `json:"protocol"`
		BaseURL    string         `json:"baseUrl"`
		APIKey     string         `json:"apiKey"`
	}
	if err := decodeBody(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "Request body must be a JSON object")
		return
	}
	if !body.Protocol.Valid() || strings.TrimSpace(body.BaseURL) == "" {
		writeError(writer, http.StatusBadRequest, "protocol and baseUrl are required")
		return
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey == "" && body.ProviderID != "" {
		apiKey = s.store.ProviderAPIKey(body.ProviderID)
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	models, err := s.upstream.DiscoverModels(ctx, body.Protocol, body.BaseURL, apiKey)
	if err != nil {
		status := http.StatusBadGateway
		message := "Unable to synchronize upstream models"
		var failure *upstream.Failure
		if errors.As(err, &failure) {
			if failure.Status >= 400 && failure.Status <= 599 {
				status = failure.Status
			}
			if failure.Message != "" {
				message = failure.Message
			}
		}
		writeError(writer, status, message)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) proxy(kind string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		requestID := newRequestID()
		startedAt := time.Now()
		writer.Header().Set("x-request-id", requestID)
		s.LogRuntime("info", requestID, "收到外部请求", fmt.Sprintf("method=%s path=%s client=%s contentLength=%d", request.Method, request.URL.Path, request.RemoteAddr, request.ContentLength))
		if !s.proxyAuthorized(request) {
			writeError(writer, http.StatusUnauthorized, "Unauthorized")
			s.recordRequest(request, model.RequestLog{
				ID: requestID, StartedAt: startedAt, Inbound: kind, Status: http.StatusUnauthorized,
				Error: "Unauthorized", DurationMs: elapsedMs(startedAt),
			})
			return
		}
		var body map[string]any
		stopReading := s.trackWaiting(requestID, "仍在读取客户端请求体", "检查客户端是否完成发送")
		bodyErr := decodeBody(request, &body)
		stopReading()
		if bodyErr != nil {
			writer.Header().Set("x-request-id", requestID)
			writeError(writer, http.StatusBadRequest, "Request body must be a JSON object")
			s.recordRequest(request, model.RequestLog{
				ID: requestID, StartedAt: startedAt, Inbound: kind, Status: http.StatusBadRequest,
				Error: "Request body must be a JSON object", DurationMs: elapsedMs(startedAt),
			})
			return
		}
		internal := normalize(kind, body)
		internal.RequestID = requestID
		s.LogRuntime("info", requestID, "请求解析完成", fmt.Sprintf("model=%s stream=%t", internal.Model, internal.Stream))
		exposed, found := s.store.Get().ExposedModel(internal.Model)
		if !found {
			writer.Header().Set("x-request-id", requestID)
			writeJSON(writer, http.StatusNotFound, map[string]any{
				"error": map[string]any{"message": "Model not found", "code": "model_not_found"},
			})
			s.recordRequest(request, model.RequestLog{
				ID: requestID, StartedAt: startedAt, Inbound: kind, ExposedModel: internal.Model,
				Stream: internal.Stream, Status: http.StatusNotFound, Error: "Model not found",
				DurationMs: elapsedMs(startedAt),
			})
			return
		}
		attempts := 0
		selected := model.Target{}
		result, err := s.router.Execute(request.Context(), exposed,
			func(ctx context.Context, target model.Target) (*upstream.Result, error) {
				attempts++
				selected = target
				detail := targetDetail(target, attempts)
				s.LogRuntime("info", requestID, "开始上游转发", detail)
				stopWaiting := s.trackWaiting(requestID, "仍在等待上游响应或首个事件", detail)
				attemptStarted := time.Now()
				result, callErr := s.upstream.Call(ctx, target.Provider, target.Model, internal)
				stopWaiting()
				if callErr != nil {
					status := http.StatusBadGateway
					var failure *upstream.Failure
					if errors.As(callErr, &failure) {
						status = failure.Status
					}
					s.LogRuntime("error", requestID, "上游转发失败", fmt.Sprintf("%s status=%d durationMs=%d error=%s", detail, status, elapsedMs(attemptStarted), upstream.Diagnostic(callErr)))
					if routing.IsSwitchableStatus(status) && attempts < len(exposed.Targets) {
						s.LogRuntime("warn", requestID, "准备切换下一个上游", fmt.Sprintf("failedAttempt=%d", attempts))
					}
				} else {
					s.LogRuntime("info", requestID, "上游响应就绪", fmt.Sprintf("%s status=%d durationMs=%d stream=%t", detail, result.Status, elapsedMs(attemptStarted), result.Stream != nil))
				}
				return result, callErr
			})
		if err != nil {
			status := http.StatusBadGateway
			message := "Upstream request failed"
			var failed *routing.AllProvidersFailedError
			if errors.As(err, &failed) {
				if failed.Last.Status >= 400 && failed.Last.Status <= 599 {
					status = failed.Last.Status
				}
				if failed.Last.Message != "" {
					message = failed.Last.Message
				}
			}
			writer.Header().Set("x-request-id", requestID)
			writeJSON(writer, status, map[string]any{
				"error": map[string]any{"message": message, "code": "all_providers_failed", "requestId": requestID},
			})
			s.recordRequest(request, model.RequestLog{
				ID: requestID, StartedAt: startedAt, Inbound: kind, ExposedModel: internal.Model,
				UpstreamModel: selected.Model.UpstreamModel, ProviderID: selected.Provider.ID,
				ProviderName: selected.Provider.Name, Stream: internal.Stream, Status: status,
				Attempts: attempts, Error: message, DurationMs: elapsedMs(startedAt),
			})
			return
		}
		if internal.Stream && result.Stream != nil {
			s.LogRuntime("info", requestID, "开始向客户端发送流式响应", "已收到首个事件")
			stopStreaming := s.trackWaiting(requestID, "流式请求仍未结束", targetDetail(selected, attempts))
			firstTokenMs, durationMs := writeStream(writer, kind, internal.Model, result.Stream, requestID, startedAt, internal)
			stopStreaming()
			status, streamError := http.StatusOK, ""
			if err := result.Stream.Err(); err != nil {
				status, streamError = http.StatusBadGateway, upstream.Diagnostic(err)
			}
			s.recordRequest(request, model.RequestLog{
				ID: requestID, StartedAt: startedAt, Inbound: kind, ExposedModel: internal.Model,
				UpstreamModel: selected.Model.UpstreamModel, ProviderID: selected.Provider.ID,
				ProviderName: selected.Provider.Name, Stream: true, Status: status, Error: streamError,
				Attempts: attempts, Usage: result.Stream.Usage(), FirstTokenMs: firstTokenMs, DurationMs: durationMs,
			})
			return
		}
		writer.Header().Set("x-request-id", requestID)
		writeJSON(writer, http.StatusOK, encode(kind, internal.Model, result, requestID, startedAt, internal))
		durationMs := elapsedMs(startedAt)
		s.recordRequest(request, model.RequestLog{
			ID: requestID, StartedAt: startedAt, Inbound: kind, ExposedModel: internal.Model,
			UpstreamModel: selected.Model.UpstreamModel, ProviderID: selected.Provider.ID,
			ProviderName: selected.Provider.Name, Status: http.StatusOK, Attempts: attempts,
			Usage: result.Usage, FirstTokenMs: durationMs, DurationMs: durationMs,
		})
	}
}

func (s *Server) recordRequest(request *http.Request, entry model.RequestLog) {
	entry.ClientAddress = request.RemoteAddr
	if forwarded := strings.TrimSpace(strings.Split(request.Header.Get("x-forwarded-for"), ",")[0]); forwarded != "" {
		entry.ClientAddress = forwarded
	}
	level, message := "info", "请求完成"
	if entry.Status >= 400 {
		level, message = "error", "请求失败"
	}
	s.LogRuntime(level, entry.ID, message, fmt.Sprintf("status=%d model=%s provider=%s attempts=%d firstTokenMs=%d durationMs=%d error=%s", entry.Status, entry.ExposedModel, entry.ProviderName, entry.Attempts, entry.FirstTokenMs, entry.DurationMs, entry.Error))
	if err := s.store.AddRequestLog(entry); err != nil {
		s.LogRuntime("error", entry.ID, "请求监控记录保存失败", err.Error())
	}
}

func elapsedMs(startedAt time.Time) int {
	return int(time.Since(startedAt).Milliseconds())
}

func (s *Server) listRequestLogs(writer http.ResponseWriter, request *http.Request) {
	filter, err := requestLogFilter(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	entries, total, summary, err := s.store.RequestLogsWithSummary(queryInt(request, "limit", 50), queryInt(request, "offset", 0), filter)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "Request logs could not be loaded")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": entries, "total": total, "summary": summary})
}

func requestLogFilter(request *http.Request) (store.RequestLogFilter, error) {
	query := request.URL.Query()
	filter := store.RequestLogFilter{
		ExposedModel: strings.TrimSpace(query.Get("exposedModel")), UpstreamModel: strings.TrimSpace(query.Get("upstreamModel")),
		ProviderID: strings.TrimSpace(query.Get("providerId")),
	}
	for name, target := range map[string]**time.Time{"startedAfter": &filter.StartedAfter, "startedBefore": &filter.StartedBefore} {
		value := strings.TrimSpace(query.Get(name))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return filter, fmt.Errorf("%s must be RFC3339", name)
		}
		*target = &parsed
	}
	if filter.StartedAfter != nil && filter.StartedBefore != nil && filter.StartedAfter.After(*filter.StartedBefore) {
		return filter, errors.New("startedAfter must not be after startedBefore")
	}
	return filter, nil
}

func queryInt(request *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(request.URL.Query().Get(name))
	if err != nil {
		return fallback
	}
	return value
}

func (s *Server) clearRequestLogs(writer http.ResponseWriter, _ *http.Request) {
	if err := s.store.ClearRequestLogs(); err != nil {
		writeError(writer, http.StatusInternalServerError, "Request logs could not be cleared")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) proxyAuthorized(request *http.Request) bool {
	provided := proxyToken(request)
	if provided == "" {
		return false
	}
	expected := s.store.Get().Settings.ProxyToken
	return expected != "" && provided == expected || s.store.HasAccessKey(provided)
}

func publicAccessKey(accessKey model.AccessKey) model.AccessKey {
	accessKey.Prefix = maskedSecret(accessKey.Secret)
	accessKey.Secret = ""
	return accessKey
}

func maskedSecret(secretValue string) string {
	characters := []rune(secretValue)
	if len(characters) < 2 {
		return "••••••••"
	}
	return string(characters[0]) + "••••••••" + string(characters[len(characters)-1])
}

func accessKeyPrefix(secretValue string) string {
	characters := []rune(secretValue)
	if len(characters) > 10 {
		characters = characters[:10]
	}
	return string(characters)
}

func (s *Server) listAccessKeys(writer http.ResponseWriter, _ *http.Request) {
	keys := s.store.Get().AccessKeys
	if keys == nil {
		keys = []model.AccessKey{}
	}
	for index := range keys {
		keys[index] = publicAccessKey(keys[index])
	}
	writeJSON(writer, http.StatusOK, keys)
}

func (s *Server) getAccessKey(writer http.ResponseWriter, request *http.Request) {
	for _, accessKey := range s.store.Get().AccessKeys {
		if accessKey.ID == request.PathValue("id") {
			writeJSON(writer, http.StatusOK, accessKey)
			return
		}
	}
	writeError(writer, http.StatusNotFound, "Access key not found")
}

func (s *Server) createAccessKey(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name   string  `json:"name"`
		Secret *string `json:"secret"`
	}
	if err := decodeBody(request, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeError(writer, http.StatusBadRequest, "name is required")
		return
	}
	secretValue := ""
	if body.Secret == nil {
		secretValue = "oa_" + newRequestID()
	} else {
		secretValue = strings.TrimSpace(*body.Secret)
		if secretValue == "" {
			writeError(writer, http.StatusBadRequest, "secret is required")
			return
		}
	}
	accessKey := model.AccessKey{
		ID: newRequestID(), Name: strings.TrimSpace(body.Name), Secret: secretValue,
		Prefix: accessKeyPrefix(secretValue), Enabled: true, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.AddAccessKey(accessKey); err != nil {
		if errors.Is(err, store.ErrAccessKeyExists) {
			writeError(writer, http.StatusConflict, "Access key already exists")
			return
		}
		writeError(writer, http.StatusInternalServerError, "Access key could not be saved")
		return
	}
	writeJSON(writer, http.StatusCreated, accessKey)
}

func (s *Server) updateAccessKey(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeBody(request, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeError(writer, http.StatusBadRequest, "name is required")
		return
	}
	found, err := s.store.UpdateAccessKey(request.PathValue("id"), strings.TrimSpace(body.Name), body.Enabled)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "Access key could not be updated")
		return
	}
	if !found {
		writeError(writer, http.StatusNotFound, "Access key not found")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAccessKey(writer http.ResponseWriter, request *http.Request) {
	found, err := s.store.DeleteAccessKey(request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "Access key could not be deleted")
		return
	}
	if !found {
		writeError(writer, http.StatusNotFound, "Access key not found")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) listExposedModels(writer http.ResponseWriter, request *http.Request) {
	if !s.proxyAuthorized(request) {
		writeError(writer, http.StatusUnauthorized, "Unauthorized")
		return
	}
	exposedModels := s.store.Get().ExposedModels()
	names := make([]string, 0, len(exposedModels))
	for _, exposed := range exposedModels {
		names = append(names, exposed.Name)
	}
	sort.Strings(names)
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{"id": name, "object": "model"})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) console(writer http.ResponseWriter, request *http.Request) {
	if s.assets == nil {
		if request.URL.Path != "/" {
			writeError(writer, http.StatusNotFound, "Not found")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "service": "omni-api"})
		return
	}
	name := strings.TrimPrefix(request.URL.Path, "/")
	if name == "" {
		serveAsset(writer, request, s.assets, "index.html")
		return
	}
	if _, err := fs.Stat(s.assets, name); err != nil {
		serveAsset(writer, request, s.assets, "index.html")
		return
	}
	http.FileServer(http.FS(s.assets)).ServeHTTP(writer, request)
}

func serveAsset(writer http.ResponseWriter, request *http.Request, assets fs.FS, name string) {
	file, err := assets.Open(name)
	if err != nil {
		writeError(writer, http.StatusNotFound, "Console assets are not available")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "Console assets could not be read")
		return
	}
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(content)
	_ = request
}
