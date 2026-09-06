package routing

import (
	"context"
	"testing"

	"github.com/omniapi/omni-api/internal/model"
	"github.com/omniapi/omni-api/internal/upstream"
)

func fixtures() model.ExposedModel {
	config := model.Config{}
	for _, id := range []string{"one", "two", "three"} {
		config.Providers = append(config.Providers, model.Provider{
			ID:       id,
			Name:     id,
			BaseURL:  "https://example.test",
			Protocol: model.ProtocolOpenAIChat,
			Enabled:  true,
			Models:   []model.ProviderModel{{ID: id + "-model", UpstreamModel: "model", Alias: "chat"}},
		})
	}
	exposed, found := config.ExposedModel("chat")
	if !found {
		panic("fixture did not expose chat")
	}
	return exposed
}

func TestRoundRobinAdvancesStartingProvider(t *testing.T) {
	exposed := fixtures()
	router := NewRouter()
	calls := []string{}
	call := func(_ context.Context, target model.Target) (*upstream.Result, error) {
		calls = append(calls, target.Provider.ID)
		return &upstream.Result{Status: 200, Body: map[string]any{}}, nil
	}
	if _, err := router.Execute(context.Background(), exposed, call); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if _, err := router.Execute(context.Background(), exposed, call); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if len(calls) != 2 || calls[0] != "one" || calls[1] != "two" {
		t.Fatalf("expected [one two], got %v", calls)
	}
}

func TestSwitchesPastTwoFailures(t *testing.T) {
	exposed := fixtures()
	router := NewRouter()
	calls := []string{}
	result, err := router.Execute(context.Background(), exposed,
		func(_ context.Context, target model.Target) (*upstream.Result, error) {
			calls = append(calls, target.Provider.ID)
			if target.Provider.ID == "three" {
				return &upstream.Result{Status: 200, Body: map[string]any{}}, nil
			}
			return &upstream.Result{Status: 503, Body: map[string]any{}}, nil
		})
	if err != nil {
		t.Fatalf("expected success after failover, got %v", err)
	}
	if result.Status != 200 {
		t.Fatalf("expected status 200, got %d", result.Status)
	}
	if len(calls) != 3 || calls[2] != "three" {
		t.Fatalf("expected all three providers attempted in order, got %v", calls)
	}
}

func TestKeepsLastFailureWhenEveryProviderFails(t *testing.T) {
	exposed := fixtures()
	router := NewRouter()
	_, err := router.Execute(context.Background(), exposed,
		func(_ context.Context, target model.Target) (*upstream.Result, error) {
			if target.Provider.ID == "three" {
				return &upstream.Result{Status: 429, Body: map[string]any{}}, nil
			}
			return &upstream.Result{Status: 503, Body: map[string]any{}}, nil
		})
	failed, ok := err.(*AllProvidersFailedError)
	if !ok {
		t.Fatalf("expected AllProvidersFailedError, got %v", err)
	}
	if failed.Last.Status != 429 {
		t.Fatalf("expected the last failure status 429, got %d", failed.Last.Status)
	}
}

func TestDoesNotSwitchOnNonSwitchableStatus(t *testing.T) {
	exposed := fixtures()
	router := NewRouter()
	calls := []string{}
	_, err := router.Execute(context.Background(), exposed,
		func(_ context.Context, target model.Target) (*upstream.Result, error) {
			calls = append(calls, target.Provider.ID)
			return &upstream.Result{Status: 400, Body: map[string]any{}}, nil
		})
	if _, ok := err.(*AllProvidersFailedError); !ok {
		t.Fatalf("expected AllProvidersFailedError, got %v", err)
	}
	if len(calls) != 1 || calls[0] != "one" {
		t.Fatalf("expected a single attempt, got %v", calls)
	}
}

func TestRejectsWhenNoTargetExists(t *testing.T) {
	_, err := NewRouter().Execute(context.Background(), model.ExposedModel{Name: "chat"},
		func(context.Context, model.Target) (*upstream.Result, error) {
			t.Fatal("no provider should be called")
			return nil, nil
		})
	failed, ok := err.(*AllProvidersFailedError)
	if !ok {
		t.Fatalf("expected AllProvidersFailedError, got %v", err)
	}
	if failed.Last.Status != 503 {
		t.Fatalf("expected status 503, got %d", failed.Last.Status)
	}
}

func TestSkipsDisabledProviders(t *testing.T) {
	config := model.Config{Providers: []model.Provider{
		{ID: "off", Enabled: false, Models: []model.ProviderModel{{ID: "a", UpstreamModel: "model", Alias: "chat"}}},
		{ID: "on", Enabled: true, Models: []model.ProviderModel{{ID: "b", UpstreamModel: "model", Alias: "chat"}}},
	}}
	exposed, found := config.ExposedModel("chat")
	if !found {
		t.Fatal("expected chat to be exposed")
	}
	if len(exposed.Targets) != 1 || exposed.Targets[0].Provider.ID != "on" {
		t.Fatalf("expected only the enabled provider, got %+v", exposed.Targets)
	}
}

func TestAliasHidesUpstreamModelName(t *testing.T) {
	config := model.Config{Providers: []model.Provider{{
		ID:      "provider",
		Enabled: true,
		Models:  []model.ProviderModel{{ID: "a", UpstreamModel: "gpt-5.6-luna", Alias: "chat"}},
	}}}
	if _, found := config.ExposedModel("gpt-5.6-luna"); found {
		t.Fatal("aliased upstream model must not stay callable")
	}
	if _, found := config.ExposedModel("chat"); !found {
		t.Fatal("alias must be callable")
	}
}
