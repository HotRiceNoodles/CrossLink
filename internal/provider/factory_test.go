package provider

import (
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFromModel_OpenAICompatible(t *testing.T) {
	p := &model.Provider{
		Name:        "deepseek",
		AdapterType: "openai_compatible",
		BaseURL:     "https://api.deepseek.com",
	}
	prov, err := NewFromModel(p, 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "deepseek", prov.Name())
}

func TestNewFromModel_Ollama(t *testing.T) {
	p := &model.Provider{
		Name:        "local-ollama",
		AdapterType: "ollama",
		BaseURL:     "http://localhost:11434",
	}
	prov, err := NewFromModel(p, 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "local-ollama", prov.Name())
}

func TestNewFromModel_Anthropic(t *testing.T) {
	p := &model.Provider{
		Name:        "anthropic",
		AdapterType: "anthropic",
		BaseURL:     "https://api.anthropic.com",
		APIKey:      "sk-ant-test",
	}
	prov, err := NewFromModel(p, 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "anthropic", prov.Name())
}

func TestNewFromModel_AzureOpenAI(t *testing.T) {
	p := &model.Provider{
		Name:        "azure",
		AdapterType: "azure_openai",
		BaseURL:     "https://myresource.openai.azure.com",
		APIKey:      "azure-key",
		ExtraConfig: []byte(`{"deployment_name":"my-gpt4"}`),
	}
	prov, err := NewFromModel(p, 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "azure", prov.Name())
}

func TestNewFromModel_UnsupportedType(t *testing.T) {
	p := &model.Provider{
		Name:        "unknown",
		AdapterType: "unsupported_type",
	}
	_, err := NewFromModel(p, 10*time.Second)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported adapter_type")
}
