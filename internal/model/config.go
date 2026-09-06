package model

import (
	"strings"
	"time"
)

// Protocol identifies an upstream or inbound API dialect.
type Protocol string

const (
	ProtocolOpenAIChat      Protocol = "openai-chat"
	ProtocolOpenAIResponses Protocol = "openai-responses"
	ProtocolAnthropic       Protocol = "anthropic-messages"
)

// Valid reports whether the protocol is one of the supported dialects.
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropic:
		return true
	}
	return false
}

// ProviderModel is a single upstream model exposed by a provider. ID is an
// internal identifier, UpstreamModel is the name sent upstream, and Alias
// optionally replaces it as the externally visible name.
type ProviderModel struct {
	ID            string `json:"id"`
	UpstreamModel string `json:"upstreamModel"`
	Alias         string `json:"alias,omitempty"`
}

// ExposedName returns the only model identifier external callers may use.
func (m ProviderModel) ExposedName() string {
	if alias := strings.TrimSpace(m.Alias); alias != "" {
		return alias
	}
	return strings.TrimSpace(m.UpstreamModel)
}

// Provider is an upstream API endpoint with credentials and routable models.
type Provider struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	BaseURL  string          `json:"baseUrl"`
	Protocol Protocol        `json:"protocol"`
	APIKey   string          `json:"apiKey,omitempty"`
	Models   []ProviderModel `json:"models"`
	Enabled  bool            `json:"enabled"`
}

// Settings holds gateway-wide configuration.
type Settings struct {
	AdminToken string `json:"adminToken,omitempty"`
	ProxyToken string `json:"proxyToken,omitempty"`
}

// AccessKey is one named credential allowed to call the external model APIs.
type AccessKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Secret    string    `json:"secret,omitempty"`
	Prefix    string    `json:"prefix"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// Config is the full persisted gateway configuration.
type Config struct {
	Settings   Settings    `json:"settings"`
	Providers  []Provider  `json:"providers"`
	AccessKeys []AccessKey `json:"accessKeys,omitempty"`
}

// PublicSettings reports whether tokens exist without revealing them.
type PublicSettings struct {
	HasAdminToken bool `json:"hasAdminToken"`
}

// Clone returns a deep copy so callers cannot mutate shared state.
func (c Config) Clone() Config {
	clone := Config{Settings: c.Settings}
	clone.AccessKeys = append([]AccessKey(nil), c.AccessKeys...)
	clone.Providers = make([]Provider, len(c.Providers))
	for i, provider := range c.Providers {
		clone.Providers[i] = provider
		clone.Providers[i].Models = make([]ProviderModel, len(provider.Models))
		copy(clone.Providers[i].Models, provider.Models)
	}
	return clone
}

// Public strips secrets from a config for administrative responses.
func (c Config) Public() Config {
	public := c.Clone()
	public.Settings = Settings{}
	for i := range public.AccessKeys {
		public.AccessKeys[i].Secret = ""
	}
	for i := range public.Providers {
		public.Providers[i].APIKey = ""
	}
	return public
}

// ExposedModel is one externally visible model backed by ordered targets that
// share the same exposed name.
type ExposedModel struct {
	Name    string
	Targets []Target
}

// Target is one provider model reachable under an exposed name.
type Target struct {
	Provider Provider
	Model    ProviderModel
}

// ExposedModels groups the models of enabled providers by exposed name,
// preserving provider order and per-provider model order.
func (c Config) ExposedModels() []ExposedModel {
	exposed := make([]ExposedModel, 0, len(c.Providers))
	index := make(map[string]int, len(c.Providers))
	for _, provider := range c.Providers {
		if !provider.Enabled {
			continue
		}
		for _, providerModel := range provider.Models {
			name := providerModel.ExposedName()
			if name == "" {
				continue
			}
			position, seen := index[name]
			if !seen {
				index[name] = len(exposed)
				exposed = append(exposed, ExposedModel{Name: name})
				position = len(exposed) - 1
			}
			exposed[position].Targets = append(exposed[position].Targets, Target{Provider: provider, Model: providerModel})
		}
	}
	return exposed
}

// ExposedModel returns the group published under name.
func (c Config) ExposedModel(name string) (ExposedModel, bool) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ExposedModel{}, false
	}
	for _, candidate := range c.ExposedModels() {
		if candidate.Name == trimmed {
			return candidate, true
		}
	}
	return ExposedModel{}, false
}
