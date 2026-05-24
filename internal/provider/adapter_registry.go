package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/crosslink/internal/model"
)

type AdapterFactory func(p *model.Provider, timeout time.Duration) (Provider, error)

type AdapterMeta struct {
	DisplayName       string            `json:"display_name"`
	Description       string            `json:"description"`
	NeedsBaseURL      bool              `json:"needs_base_url"`
	NeedsAPIKey       bool              `json:"needs_api_key"`
	BaseURLDefault    string            `json:"base_url_default,omitempty"`
	ProtocolBaseURLs  map[string]string `json:"protocol_base_urls,omitempty"`
	Capabilities      []string          `json:"capabilities"`
	ExtraFields       []AdapterField    `json:"extra_fields"`
	MinimumTier       string            `json:"minimum_tier,omitempty"` // "community", "pro", "enterprise"
}

type AdapterField struct {
	Name         string        `json:"name"`
	Label        string        `json:"label"`
	Type         string        `json:"type"`         // text, password, select, number, textarea
	Required     bool          `json:"required"`
	Placeholder  string        `json:"placeholder"`
	DefaultValue string        `json:"default_value"`
	Options      []FieldOption `json:"options,omitempty"`
	Secret       bool          `json:"secret"`
}

type FieldOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

var (
	adapterFactories = map[string]AdapterFactory{}
	adapterMetas     = map[string]*AdapterMeta{}
)

func RegisterAdapter(adapterType string, factory AdapterFactory, meta *AdapterMeta) {
	adapterFactories[adapterType] = factory
	adapterMetas[adapterType] = meta
}

func CreateProvider(p *model.Provider, timeout time.Duration) (Provider, error) {
	factory, ok := adapterFactories[p.AdapterType]
	if !ok {
		return nil, fmt.Errorf("unsupported adapter_type: %s", p.AdapterType)
	}
	return factory(p, timeout)
}

type AdapterInfo struct {
	Type string       `json:"type"`
	Meta *AdapterMeta `json:"meta"`
}

func ListAdapters() []AdapterInfo {
	list := make([]AdapterInfo, 0, len(adapterMetas))
	for typ, meta := range adapterMetas {
		list = append(list, AdapterInfo{Type: typ, Meta: meta})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Type < list[j].Type })
	return list
}

// GetAdapterMeta returns the metadata for a registered adapter type, or nil if not found.
func GetAdapterMeta(adapterType string) *AdapterMeta {
	return adapterMetas[adapterType]
}

// extractProtocol reads api_protocol from ExtraConfig JSON, defaults to "openai".
func extractProtocol(extraConfig []byte) string {
	if len(extraConfig) == 0 {
		return "openai"
	}
	var cfg struct {
		APIProtocol string `json:"api_protocol"`
	}
	if json.Unmarshal(extraConfig, &cfg) != nil || cfg.APIProtocol == "" {
		return "openai"
	}
	return cfg.APIProtocol
}
