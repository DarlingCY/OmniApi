package app

import (
	"path/filepath"
	"testing"

	"github.com/omniapi/omni-api/internal/secret"
	"github.com/omniapi/omni-api/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dataDir := t.TempDir()
	codec, err := secret.NewAESCodec(dataDir, "test-master-key")
	if err != nil {
		t.Fatal(err)
	}
	configStore := store.New(filepath.Join(dataDir, "omni-api.db"), codec)
	if err := configStore.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = configStore.Close() })
	return configStore
}

func TestInitializeAdminTokenSetsEmptyStore(t *testing.T) {
	configStore := newTestStore(t)
	if err := initializeAdminToken(configStore, "  admin-secret  "); err != nil {
		t.Fatal(err)
	}
	if actual := configStore.Get().Settings.AdminToken; actual != "admin-secret" {
		t.Fatalf("expected initialized admin token, got %q", actual)
	}
}

func TestInitializeAdminTokenDoesNotOverwriteExistingToken(t *testing.T) {
	configStore := newTestStore(t)
	existing := "existing-secret"
	if err := configStore.UpdateSettings(&existing, nil); err != nil {
		t.Fatal(err)
	}
	if err := initializeAdminToken(configStore, "replacement-secret"); err != nil {
		t.Fatal(err)
	}
	if actual := configStore.Get().Settings.AdminToken; actual != existing {
		t.Fatalf("expected existing admin token to remain, got %q", actual)
	}
}
