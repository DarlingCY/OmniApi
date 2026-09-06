package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/secret"
)

func testStore(t *testing.T) (*Store, string, secret.Codec) {
	t.Helper()
	directory := t.TempDir()
	codec, err := secret.NewAESCodec(directory, "test-passphrase")
	if err != nil {
		t.Fatalf("codec setup failed: %v", err)
	}
	store := New(filepath.Join(directory, "omni-api.db"), codec)
	t.Cleanup(func() { _ = store.Close() })
	return store, directory, codec
}

func testConfig() model.Config {
	return model.Config{
		Settings:   model.Settings{AdminToken: "admin-secret"},
		AccessKeys: []model.AccessKey{{ID: "key-one", Name: "CI", Secret: "oa_access-secret", Prefix: "oa_access", Enabled: true, CreatedAt: time.Date(2026, time.September, 5, 8, 0, 0, 0, time.UTC)}},
		Providers: []model.Provider{
			{ID: "first", Name: "First", BaseURL: "https://first.test", Protocol: model.ProtocolOpenAIChat, APIKey: "first-key", Enabled: true, Models: []model.ProviderModel{{ID: "one", UpstreamModel: "first-one", Alias: "chat"}, {ID: "three", UpstreamModel: "first-three"}}},
			{ID: "second", Name: "Second", BaseURL: "https://second.test", Protocol: model.ProtocolAnthropic, APIKey: "second-key", Enabled: false, Models: []model.ProviderModel{{ID: "two", UpstreamModel: "second-two"}}},
		},
	}
}

func TestSQLiteRoundTripEncryptsSecretsAndPreservesModelOrder(t *testing.T) {
	store, directory, codec := testStore(t)
	want := testConfig()
	if err := store.Replace(want); err != nil {
		t.Fatalf("replace failed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "omni-api.db"))
	if err != nil {
		t.Fatalf("read database failed: %v", err)
	}
	for _, secretValue := range []string{"admin-secret", "oa_access-secret", "first-key", "second-key"} {
		if strings.Contains(string(raw), secretValue) {
			t.Fatalf("database contains plaintext secret %q", secretValue)
		}
	}
	reopened := New(filepath.Join(directory, "omni-api.db"), codec)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if got := reopened.Get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLoadMigratesLegacyJSONAfterSuccessfulImport(t *testing.T) {
	store, directory, codec := testStore(t)
	want := testConfig()
	want.Settings.ProxyToken = "proxy-secret"
	legacy := want.Clone()
	var err error
	if legacy.Settings.AdminToken, err = codec.Encrypt(legacy.Settings.AdminToken); err != nil {
		t.Fatal(err)
	}
	if legacy.Settings.ProxyToken, err = codec.Encrypt(legacy.Settings.ProxyToken); err != nil {
		t.Fatal(err)
	}
	for index := range legacy.Providers {
		if legacy.Providers[index].APIKey, err = codec.Encrypt(legacy.Providers[index].APIKey); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy config failed: %v", err)
	}
	legacyPath := filepath.Join(directory, "config.json")
	if err := os.WriteFile(legacyPath, raw, 0o600); err != nil {
		t.Fatalf("write legacy config failed: %v", err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("load with migration failed: %v", err)
	}
	got := store.Get()
	if got.Settings.ProxyToken != "" || len(got.AccessKeys) != 2 {
		t.Fatalf("expected legacy proxy token to move into access keys, got %#v", got)
	}
	migrated := got.AccessKeys[1]
	if migrated.Name != "默认代理密钥" || migrated.Secret != "proxy-secret" || !migrated.Enabled {
		t.Fatalf("unexpected migrated access key: %#v", migrated)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy config was not renamed, stat error: %v", err)
	}
	if _, err := os.Stat(legacyPath + ".migrated"); err != nil {
		t.Fatalf("migrated backup missing: %v", err)
	}
}

func TestLoadAddsAliasColumnToLegacyProviderModels(t *testing.T) {
	store, directory, codec := testStore(t)
	path := filepath.Join(directory, "omni-api.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database failed: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK (id = 1), admin_token TEXT NOT NULL, proxy_token TEXT NOT NULL);
		CREATE TABLE providers (id TEXT PRIMARY KEY, name TEXT NOT NULL, base_url TEXT NOT NULL, protocol TEXT NOT NULL, api_key TEXT NOT NULL, enabled INTEGER NOT NULL, position INTEGER NOT NULL);
		CREATE TABLE provider_models (provider_id TEXT NOT NULL, id TEXT NOT NULL, upstream_model TEXT NOT NULL, position INTEGER NOT NULL, PRIMARY KEY (provider_id, id));
		INSERT INTO providers (id, name, base_url, protocol, api_key, enabled, position) VALUES ('first', 'First', 'https://first.test', 'openai-chat', '', 1, 0);
		INSERT INTO provider_models (provider_id, id, upstream_model, position) VALUES ('first', 'one', 'first-one', 0);`); err != nil {
		t.Fatalf("seed legacy schema failed: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database failed: %v", err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("load with alias migration failed: %v", err)
	}
	providers := store.Get().Providers
	if len(providers) != 1 || len(providers[0].Models) != 1 || providers[0].Models[0].Alias != "" {
		t.Fatalf("expected one provider model with an empty alias, got %#v", providers)
	}
	reopened := New(path, codec)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Replace(model.Config{Providers: []model.Provider{{
		ID: "first", Name: "First", BaseURL: "https://first.test", Protocol: model.ProtocolOpenAIChat, Enabled: true,
		Models: []model.ProviderModel{{ID: "one", UpstreamModel: "first-one", Alias: "chat"}},
	}}}); err != nil {
		t.Fatalf("replace after migration failed: %v", err)
	}
	if got := reopened.Get().Providers[0].Models[0].Alias; got != "chat" {
		t.Fatalf("expected alias chat, got %q", got)
	}
}

func TestRequestLogsRoundTripAndClear(t *testing.T) {
	configStore, _, _ := testStore(t)
	entry := model.RequestLog{
		ID: "request-one", StartedAt: time.Date(2026, 9, 4, 16, 17, 51, 0, time.UTC),
		Inbound: "chat", ExposedModel: "gpt-public", UpstreamModel: "gpt-upstream",
		ProviderID: "provider-one", ProviderName: "Provider One", ClientAddress: "127.0.0.1:5000",
		Stream: true, Status: 200, Attempts: 2, Usage: model.Usage{InputTokens: 100, OutputTokens: 20, CachedTokens: 80, CacheCreationTokens: 10, ReasoningTokens: 5},
		FirstTokenMs: 120, DurationMs: 900,
	}
	if err := configStore.AddRequestLog(entry); err != nil {
		t.Fatalf("add request log failed: %v", err)
	}
	entries, total, err := configStore.RequestLogs(50, 0)
	if err != nil {
		t.Fatalf("load request logs failed: %v", err)
	}
	if total != 1 || len(entries) != 1 || !reflect.DeepEqual(entries[0], entry) {
		t.Fatalf("request log mismatch: total=%d entries=%#v", total, entries)
	}
	if err := configStore.ClearRequestLogs(); err != nil {
		t.Fatalf("clear request logs failed: %v", err)
	}
	entries, total, err = configStore.RequestLogs(50, 0)
	if err != nil || total != 0 || len(entries) != 0 {
		t.Fatalf("expected empty logs after clear, total=%d entries=%#v err=%v", total, entries, err)
	}
}

func TestRequestLogsCleanupRunsInBatches(t *testing.T) {
	configStore, _, _ := testStore(t)
	database, err := configStore.ensureDatabase()
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 10001; index++ {
		if _, err := transaction.Exec(`INSERT INTO request_logs (
			id, started_at, inbound, exposed_model, upstream_model, provider_id, provider_name,
			client_address, stream, status, attempts, error, input_tokens, output_tokens,
			cached_tokens, cache_creation_tokens, reasoning_tokens, first_token_ms, duration_ms
		) VALUES (?, ?, '', '', '', '', '', '', 0, 0, 0, '', 0, 0, 0, 0, 0, 0, 0)`,
			fmt.Sprintf("seed-%05d", index), time.Unix(int64(index), 0).UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	configStore.requestWrites.Store(99)
	if err := configStore.AddRequestLog(model.RequestLog{ID: "trigger", StartedAt: time.Unix(20000, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := database.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 10000 {
		t.Fatalf("expected cleanup to retain 10000 logs, got %d", total)
	}
}

func TestConcurrentAccessKeyAddsPreserveEveryKey(t *testing.T) {
	configStore, _, _ := testStore(t)
	const count = 12
	var wait sync.WaitGroup
	errors := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errors <- configStore.AddAccessKey(model.AccessKey{
				ID: fmt.Sprintf("key-%d", index), Secret: fmt.Sprintf("secret-%d", index),
			})
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(configStore.Get().AccessKeys); got != count {
		t.Fatalf("expected %d access keys, got %d", count, got)
	}
}

func TestRequestLogsFiltersBeforePagination(t *testing.T) {
	configStore, _, _ := testStore(t)
	base := time.Date(2026, 9, 4, 16, 0, 0, 0, time.UTC)
	for _, entry := range []model.RequestLog{
		{ID: "one", StartedAt: base, ExposedModel: "public-a", UpstreamModel: "actual-a", ProviderID: "provider-a"},
		{ID: "two", StartedAt: base.Add(time.Hour), ExposedModel: "public-a", UpstreamModel: "actual-b", ProviderID: "provider-b"},
		{ID: "three", StartedAt: base.Add(2 * time.Hour), ExposedModel: "public-b", UpstreamModel: "actual-a", ProviderID: "provider-a"},
	} {
		if err := configStore.AddRequestLog(entry); err != nil {
			t.Fatalf("add request log failed: %v", err)
		}
	}
	startedAfter := base.Add(30 * time.Minute)
	startedBefore := base.Add(90 * time.Minute)
	entries, total, err := configStore.RequestLogs(1, 0, RequestLogFilter{
		StartedAfter: &startedAfter, StartedBefore: &startedBefore, ExposedModel: "public-a", ProviderID: "provider-b",
	})
	if err != nil || total != 1 || len(entries) != 1 || entries[0].ID != "two" {
		t.Fatalf("unexpected filtered logs: total=%d entries=%#v err=%v", total, entries, err)
	}
}

func TestLoadAddsUsageColumnsToLegacyRequestLogs(t *testing.T) {
	store, directory, _ := testStore(t)
	path := filepath.Join(directory, "omni-api.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database failed: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE request_logs (
		id TEXT PRIMARY KEY, started_at TEXT NOT NULL, inbound TEXT NOT NULL, exposed_model TEXT NOT NULL,
		upstream_model TEXT NOT NULL, provider_id TEXT NOT NULL, provider_name TEXT NOT NULL,
		client_address TEXT NOT NULL, stream INTEGER NOT NULL, status INTEGER NOT NULL, attempts INTEGER NOT NULL,
		error TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, cached_tokens INTEGER NOT NULL,
		first_token_ms INTEGER NOT NULL, duration_ms INTEGER NOT NULL
	);
		INSERT INTO request_logs VALUES ('old', '2026-09-04T16:17:51Z', 'chat', 'public', 'upstream', 'provider', 'Provider', '127.0.0.1', 0, 200, 1, '', 10, 2, 3, 4, 5)`); err != nil {
		t.Fatalf("seed legacy request logs failed: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database failed: %v", err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("load with request log migration failed: %v", err)
	}
	entries, total, err := store.RequestLogs(50, 0)
	if err != nil {
		t.Fatalf("read migrated request log failed: %v", err)
	}
	if total != 1 || len(entries) != 1 || entries[0].Usage.CacheCreationTokens != 0 || entries[0].Usage.ReasoningTokens != 0 {
		t.Fatalf("unexpected migrated logs: total=%d entries=%#v", total, entries)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("second idempotent migration failed: %v", err)
	}
}
