package provider

// U-35 regression: the Azure chat path decodes through a bounded reader —
// an oversized upstream body (over maxResponseRead) must fail the decode
// instead of being read unbounded into memory. Embeddings already had the
// cap; the chat path was bare json.NewDecoder(resp.Body).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestAzureOpenAI_Chat_OversizedBodyFailsDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON prefix, then bytes far beyond the cap: with the bounded
		// reader the decode sees truncated JSON and MUST error; unbounded it
		// would allocate and "succeed".
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"`))
		io.Copy(w, io.LimitReader(zeroReader{}, maxResponseRead+(1<<20)))
	}))
	defer server.Close()

	extraConfig, _ := json.Marshal(map[string]string{
		"deployment_name": "my-gpt4",
		"api_version":     "2024-02-01",
	})
	p, err := NewAzureOpenAIProvider("azure", server.URL, "azure-key", extraConfig, 30*time.Second)
	require.NoError(t, err)

	_, err = p.Chat(context.Background(), &domain.OpenAIRequest{
		Model:    "gpt-4",
		Messages: []domain.OpenAIMessage{{Role: "user", Content: "Hi"}},
	}, "azure-key")
	if err == nil {
		t.Fatal("oversized upstream body must fail the bounded decode, not parse as success")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
