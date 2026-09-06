package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/secret"
)

// ErrAccessKeyExists is returned when a credential is already registered.
var ErrAccessKeyExists = errors.New("access key already exists")

// Store keeps the gateway configuration in memory and in SQLite.
type Store struct {
	path   string
	codec  secret.Codec
	mutex  sync.RWMutex
	config model.Config
}

// New creates a store backed by a SQLite database and a secret codec.
func New(path string, codec secret.Codec) *Store {
	return &Store{path: path, codec: codec}
}

// Load initializes the database and loads its configuration into memory.
func (s *Store) Load() error {
	database, err := s.open()
	if err != nil {
		return err
	}
	defer database.Close()

	if err := initialize(database); err != nil {
		return err
	}
	configured, err := hasConfiguration(database)
	if err != nil {
		return err
	}
	if !configured {
		if err := s.migrateJSON(database); err != nil {
			return err
		}
	}
	stored, err := loadConfig(database, s.codec)
	if err != nil {
		return err
	}
	if stored.Settings.ProxyToken != "" {
		secretValue := stored.Settings.ProxyToken
		stored.Settings.ProxyToken = ""
		exists := false
		for _, accessKey := range stored.AccessKeys {
			if accessKey.Secret == secretValue {
				exists = true
				break
			}
		}
		if !exists {
			stored.AccessKeys = append(stored.AccessKeys, model.AccessKey{
				ID: "legacy-proxy-token", Name: "默认代理密钥", Secret: secretValue,
				Prefix: accessKeyPrefix(secretValue), Enabled: true, CreatedAt: time.Now().UTC(),
			})
		}
		if err := replaceConfig(database, s.codec, stored); err != nil {
			return err
		}
	}
	s.mutex.Lock()
	s.config = stored
	s.mutex.Unlock()
	return nil
}

// Get returns a deep copy of the current configuration.
func (s *Store) Get() model.Config {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.config.Clone()
}

// Replace persists a full configuration atomically.
func (s *Store) Replace(config model.Config) error {
	database, err := s.open()
	if err != nil {
		return err
	}
	defer database.Close()
	if err := initialize(database); err != nil {
		return err
	}
	if err := replaceConfig(database, s.codec, config); err != nil {
		return err
	}
	s.mutex.Lock()
	s.config = config.Clone()
	s.mutex.Unlock()
	return nil
}

// UpdateSettings merges provided tokens, leaving nil fields untouched.
func (s *Store) UpdateSettings(adminToken, proxyToken *string) error {
	config := s.Get()
	if adminToken != nil {
		config.Settings.AdminToken = *adminToken
	}
	if proxyToken != nil {
		config.Settings.ProxyToken = *proxyToken
	}
	return s.Replace(config)
}

// ProviderAPIKey returns the stored key for a provider id.
func (s *Store) ProviderAPIKey(providerID string) string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	for _, provider := range s.config.Providers {
		if provider.ID == providerID {
			return provider.APIKey
		}
	}
	return ""
}

// AddAccessKey persists a newly generated external credential.
func (s *Store) AddAccessKey(accessKey model.AccessKey) error {
	config := s.Get()
	for _, existing := range config.AccessKeys {
		if existing.Secret == accessKey.Secret {
			return ErrAccessKeyExists
		}
	}
	config.AccessKeys = append(config.AccessKeys, accessKey)
	return s.Replace(config)
}

func accessKeyPrefix(secretValue string) string {
	characters := []rune(secretValue)
	if len(characters) > 10 {
		characters = characters[:10]
	}
	return string(characters)
}

// UpdateAccessKey changes the display name and enabled state of an access key.
func (s *Store) UpdateAccessKey(id, name string, enabled bool) (bool, error) {
	config := s.Get()
	for index := range config.AccessKeys {
		if config.AccessKeys[index].ID != id {
			continue
		}
		config.AccessKeys[index].Name = name
		config.AccessKeys[index].Enabled = enabled
		return true, s.Replace(config)
	}
	return false, nil
}

// DeleteAccessKey removes an external credential.
func (s *Store) DeleteAccessKey(id string) (bool, error) {
	config := s.Get()
	for index := range config.AccessKeys {
		if config.AccessKeys[index].ID != id {
			continue
		}
		config.AccessKeys = append(config.AccessKeys[:index], config.AccessKeys[index+1:]...)
		return true, s.Replace(config)
	}
	return false, nil
}

// HasAccessKey reports whether secret is an enabled external credential.
func (s *Store) HasAccessKey(secret string) bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	for _, accessKey := range s.config.AccessKeys {
		if accessKey.Enabled && accessKey.Secret == secret {
			return true
		}
	}
	return false
}

// AddRequestLog records request metadata and aggregate usage without storing
// prompts, responses or credentials.
func (s *Store) AddRequestLog(entry model.RequestLog) error {
	database, err := s.open()
	if err != nil {
		return err
	}
	defer database.Close()
	if err := initialize(database); err != nil {
		return err
	}
	_, err = database.Exec(`INSERT INTO request_logs (
		id, started_at, inbound, exposed_model, upstream_model, provider_id, provider_name,
		client_address, stream, status, attempts, error, input_tokens, output_tokens,
		cached_tokens, cache_creation_tokens, reasoning_tokens, first_token_ms, duration_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.StartedAt.UTC().Format(time.RFC3339Nano), entry.Inbound, entry.ExposedModel,
		entry.UpstreamModel, entry.ProviderID, entry.ProviderName, entry.ClientAddress, entry.Stream,
		entry.Status, entry.Attempts, entry.Error, entry.Usage.InputTokens, entry.Usage.OutputTokens,
		entry.Usage.CachedTokens, entry.Usage.CacheCreationTokens, entry.Usage.ReasoningTokens,
		entry.FirstTokenMs, entry.DurationMs)
	if err != nil {
		return err
	}
	_, err = database.Exec(`DELETE FROM request_logs WHERE id IN (
		SELECT id FROM request_logs ORDER BY started_at DESC LIMIT -1 OFFSET 10000
	)`)
	return err
}

// RequestLogFilter narrows monitoring history before pagination.
type RequestLogFilter struct {
	StartedAfter  *time.Time
	StartedBefore *time.Time
	ExposedModel  string
	UpstreamModel string
	ProviderID    string
}

// RequestLogs returns newest matching entries first and the filtered row count.
func (s *Store) RequestLogs(limit, offset int, filter ...RequestLogFilter) ([]model.RequestLog, int, error) {
	entries, total, _, err := s.RequestLogsWithSummary(limit, offset, filter...)
	return entries, total, err
}

// RequestLogsWithSummary returns newest matching entries, the total count, and aggregated metrics for the filter.
func (s *Store) RequestLogsWithSummary(limit, offset int, filter ...RequestLogFilter) ([]model.RequestLog, int, model.RequestLogSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	database, err := s.open()
	if err != nil {
		return nil, 0, model.RequestLogSummary{}, err
	}
	defer database.Close()
	if err := initialize(database); err != nil {
		return nil, 0, model.RequestLogSummary{}, err
	}
	where, arguments := requestLogWhere(filter)
	var total int
	var totalTokens int64
	var successCount int
	var avgDuration float64
	if err := database.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(input_tokens + output_tokens), 0),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(AVG(CASE WHEN status >= 200 AND status < 300 THEN duration_ms ELSE NULL END), 0)
		FROM request_logs`+where, arguments...).Scan(&total, &totalTokens, &successCount, &avgDuration); err != nil {
		return nil, 0, model.RequestLogSummary{}, err
	}
	var successRate float64
	if total > 0 {
		successRate = math.Round(float64(successCount)/float64(total)*1000) / 10
	}
	summary := model.RequestLogSummary{
		TotalRequests:   total,
		TotalTokens:     totalTokens,
		SuccessRate:     successRate,
		AverageDuration: int(math.Round(avgDuration)),
	}
	rows, err := database.Query(`SELECT id, started_at, inbound, exposed_model, upstream_model,
		provider_id, provider_name, client_address, stream, status, attempts, error, input_tokens,
		output_tokens, cached_tokens, cache_creation_tokens, reasoning_tokens, first_token_ms, duration_ms
		FROM request_logs`+where+` ORDER BY started_at DESC LIMIT ? OFFSET ?`, append(arguments, limit, offset)...)
	if err != nil {
		return nil, 0, summary, err
	}
	defer rows.Close()
	entries := make([]model.RequestLog, 0, limit)
	for rows.Next() {
		var entry model.RequestLog
		var startedAt string
		if err := rows.Scan(&entry.ID, &startedAt, &entry.Inbound, &entry.ExposedModel,
			&entry.UpstreamModel, &entry.ProviderID, &entry.ProviderName, &entry.ClientAddress,
			&entry.Stream, &entry.Status, &entry.Attempts, &entry.Error, &entry.Usage.InputTokens,
			&entry.Usage.OutputTokens, &entry.Usage.CachedTokens, &entry.Usage.CacheCreationTokens,
			&entry.Usage.ReasoningTokens, &entry.FirstTokenMs, &entry.DurationMs); err != nil {
			return nil, 0, summary, err
		}
		entry.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			return nil, 0, summary, err
		}
		entries = append(entries, entry)
	}
	return entries, total, summary, rows.Err()
}

func requestLogWhere(filters []RequestLogFilter) (string, []any) {
	if len(filters) == 0 {
		return "", nil
	}
	filter := filters[0]
	conditions := make([]string, 0, 5)
	arguments := make([]any, 0, 5)
	if filter.StartedAfter != nil {
		conditions = append(conditions, "started_at >= ?")
		arguments = append(arguments, filter.StartedAfter.UTC().Format(time.RFC3339Nano))
	}
	if filter.StartedBefore != nil {
		conditions = append(conditions, "started_at <= ?")
		arguments = append(arguments, filter.StartedBefore.UTC().Format(time.RFC3339Nano))
	}
	for column, value := range map[string]string{
		"exposed_model": filter.ExposedModel, "upstream_model": filter.UpstreamModel, "provider_id": filter.ProviderID,
	} {
		if value != "" {
			conditions = append(conditions, column+" = ?")
			arguments = append(arguments, value)
		}
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), arguments
}

// ClearRequestLogs removes all monitoring history.
func (s *Store) ClearRequestLogs() error {
	database, err := s.open()
	if err != nil {
		return err
	}
	defer database.Close()
	if err := initialize(database); err != nil {
		return err
	}
	_, err = database.Exec(`DELETE FROM request_logs`)
	return err
}

func (s *Store) open() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", s.path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
}

func initialize(database *sql.DB) error {
	_, err := database.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			admin_token TEXT NOT NULL,
			proxy_token TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS providers (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			base_url TEXT NOT NULL,
			protocol TEXT NOT NULL,
			api_key TEXT NOT NULL,
			enabled INTEGER NOT NULL,
			position INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS provider_models (
			provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
			id TEXT NOT NULL,
			upstream_model TEXT NOT NULL,
			alias TEXT NOT NULL DEFAULT '',
			position INTEGER NOT NULL,
			PRIMARY KEY (provider_id, id)
		);
		CREATE TABLE IF NOT EXISTS access_keys (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			secret TEXT NOT NULL,
			prefix TEXT NOT NULL,
			enabled INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			position INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			started_at TEXT NOT NULL,
			inbound TEXT NOT NULL,
			exposed_model TEXT NOT NULL,
			upstream_model TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			provider_name TEXT NOT NULL,
			client_address TEXT NOT NULL,
			stream INTEGER NOT NULL,
			status INTEGER NOT NULL,
			attempts INTEGER NOT NULL,
			error TEXT NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			cached_tokens INTEGER NOT NULL,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			first_token_ms INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS request_logs_started_at ON request_logs(started_at DESC);
		DROP TABLE IF EXISTS model_bindings;
		DROP TABLE IF EXISTS logical_models;`)
	if err != nil {
		return err
	}
	if err := addAliasColumn(database); err != nil {
		return err
	}
	return addRequestLogUsageColumns(database)
}

// addAliasColumn upgrades databases created before provider model aliases.
func addAliasColumn(database *sql.DB) error {
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('provider_models') WHERE name = 'alias'`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := database.Exec(`ALTER TABLE provider_models ADD COLUMN alias TEXT NOT NULL DEFAULT ''`)
	return err
}

// addRequestLogUsageColumns upgrades monitoring databases created before
// cache creation and reasoning tokens were tracked.
func addRequestLogUsageColumns(database *sql.DB) error {
	for _, column := range []struct {
		name string
		ddl  string
	}{
		{name: "cache_creation_tokens", ddl: "ALTER TABLE request_logs ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0"},
		{name: "reasoning_tokens", ddl: "ALTER TABLE request_logs ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0"},
	} {
		var count int
		if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_logs') WHERE name = ?`, column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := database.Exec(column.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasConfiguration(database *sql.DB) (bool, error) {
	var count int
	if err := database.QueryRow(`SELECT (SELECT COUNT(*) FROM settings) + (SELECT COUNT(*) FROM providers)`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) migrateJSON(database *sql.DB) error {
	legacyPath := filepath.Join(filepath.Dir(s.path), "config.json")
	raw, err := os.ReadFile(legacyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy model.Config
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return err
	}
	if legacy.Settings.AdminToken, err = s.codec.Decrypt(legacy.Settings.AdminToken); err != nil {
		return err
	}
	if legacy.Settings.ProxyToken, err = s.codec.Decrypt(legacy.Settings.ProxyToken); err != nil {
		return err
	}
	for index := range legacy.Providers {
		if legacy.Providers[index].APIKey, err = s.codec.Decrypt(legacy.Providers[index].APIKey); err != nil {
			return err
		}
	}
	if err := replaceConfig(database, s.codec, legacy); err != nil {
		return err
	}
	return os.Rename(legacyPath, legacyPath+".migrated")
}

func replaceConfig(database *sql.DB, codec secret.Codec, config model.Config) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	adminToken, err := codec.Encrypt(config.Settings.AdminToken)
	if err != nil {
		return err
	}
	proxyToken, err := codec.Encrypt(config.Settings.ProxyToken)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO settings (id, admin_token, proxy_token) VALUES (1, ?, ?) ON CONFLICT(id) DO UPDATE SET admin_token = excluded.admin_token, proxy_token = excluded.proxy_token`, adminToken, proxyToken); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM provider_models; DELETE FROM providers`); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM access_keys`); err != nil {
		return err
	}
	for position, accessKey := range config.AccessKeys {
		secretValue, encryptErr := codec.Encrypt(accessKey.Secret)
		if encryptErr != nil {
			return encryptErr
		}
		if _, err = tx.Exec(`INSERT INTO access_keys (id, name, secret, prefix, enabled, created_at, position) VALUES (?, ?, ?, ?, ?, ?, ?)`, accessKey.ID, accessKey.Name, secretValue, accessKey.Prefix, accessKey.Enabled, accessKey.CreatedAt.UTC().Format(time.RFC3339Nano), position); err != nil {
			return err
		}
	}
	for providerPosition, provider := range config.Providers {
		apiKey, encryptErr := codec.Encrypt(provider.APIKey)
		if encryptErr != nil {
			return encryptErr
		}
		if _, err = tx.Exec(`INSERT INTO providers (id, name, base_url, protocol, api_key, enabled, position) VALUES (?, ?, ?, ?, ?, ?, ?)`, provider.ID, provider.Name, provider.BaseURL, provider.Protocol, apiKey, provider.Enabled, providerPosition); err != nil {
			return err
		}
		for modelPosition, providerModel := range provider.Models {
			if _, err = tx.Exec(`INSERT INTO provider_models (provider_id, id, upstream_model, alias, position) VALUES (?, ?, ?, ?, ?)`, provider.ID, providerModel.ID, providerModel.UpstreamModel, providerModel.Alias, modelPosition); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func loadConfig(database *sql.DB, codec secret.Codec) (model.Config, error) {
	config := model.Config{}
	var adminToken, proxyToken string
	err := database.QueryRow(`SELECT admin_token, proxy_token FROM settings WHERE id = 1`).Scan(&adminToken, &proxyToken)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return config, err
	}
	if err == nil {
		var decryptErr error
		if config.Settings.AdminToken, decryptErr = codec.Decrypt(adminToken); decryptErr != nil {
			return config, decryptErr
		}
		if config.Settings.ProxyToken, decryptErr = codec.Decrypt(proxyToken); decryptErr != nil {
			return config, decryptErr
		}
	}
	providers, err := database.Query(`SELECT id, name, base_url, protocol, api_key, enabled FROM providers ORDER BY position`)
	if err != nil {
		return config, err
	}
	defer providers.Close()
	for providers.Next() {
		var provider model.Provider
		if err := providers.Scan(&provider.ID, &provider.Name, &provider.BaseURL, &provider.Protocol, &provider.APIKey, &provider.Enabled); err != nil {
			return config, err
		}
		if provider.APIKey, err = codec.Decrypt(provider.APIKey); err != nil {
			return config, err
		}
		models, queryErr := database.Query(`SELECT id, upstream_model, alias FROM provider_models WHERE provider_id = ? ORDER BY position`, provider.ID)
		if queryErr != nil {
			return config, queryErr
		}
		for models.Next() {
			var providerModel model.ProviderModel
			if scanErr := models.Scan(&providerModel.ID, &providerModel.UpstreamModel, &providerModel.Alias); scanErr != nil {
				_ = models.Close()
				return config, scanErr
			}
			provider.Models = append(provider.Models, providerModel)
		}
		if queryErr = models.Close(); queryErr != nil {
			return config, queryErr
		}
		config.Providers = append(config.Providers, provider)
	}
	if err := providers.Err(); err != nil {
		return config, err
	}
	accessKeys, err := database.Query(`SELECT id, name, secret, prefix, enabled, created_at FROM access_keys ORDER BY position`)
	if err != nil {
		return config, err
	}
	defer accessKeys.Close()
	for accessKeys.Next() {
		var accessKey model.AccessKey
		var createdAt string
		if err := accessKeys.Scan(&accessKey.ID, &accessKey.Name, &accessKey.Secret, &accessKey.Prefix, &accessKey.Enabled, &createdAt); err != nil {
			return config, err
		}
		if accessKey.Secret, err = codec.Decrypt(accessKey.Secret); err != nil {
			return config, err
		}
		if accessKey.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
			return config, err
		}
		config.AccessKeys = append(config.AccessKeys, accessKey)
	}
	if err := accessKeys.Err(); err != nil {
		return config, err
	}
	return config, nil
}
