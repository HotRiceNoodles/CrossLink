package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P3 modality: enum-validated on create/update; "" (unclassified) is the
// default and always legal; settable and clearable via update.

func TestModelCreate_Modality(t *testing.T) {
	t.Parallel()
	h, repo, providerID, _ := newModelGapEnv(t)

	t.Run("valid modality persisted", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
			"provider_id": providerID, "model_name": "img-model", "provider_model": "up-img",
			"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
			"modality": "image",
		})
		h.Create(c)
		require.Equal(t, http.StatusCreated, w.Code)
		got, err := repo.FindByProviderAndName(context.Background(), providerID, "img-model")
		require.NoError(t, err)
		assert.Equal(t, "image", got.Modality)
	})

	t.Run("absent defaults to unclassified", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
			"provider_id": providerID, "model_name": "plain-model", "provider_model": "up-plain",
			"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
		})
		h.Create(c)
		require.Equal(t, http.StatusCreated, w.Code)
		got, err := repo.FindByProviderAndName(context.Background(), providerID, "plain-model")
		require.NoError(t, err)
		assert.Equal(t, "", got.Modality, "absent = unclassified (legacy semantics)")
	})

	t.Run("invalid modality rejected", func(t *testing.T) {
		c, w := newTestContext(t, http.MethodPost, "/admin/api/models", gin.H{
			"provider_id": providerID, "model_name": "bad-mod", "provider_model": "up-bad",
			"weight": 1, "priority": 1, "routing_strategy": "weighted_random",
			"modality": "picture",
		})
		h.Create(c)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "invalid modality")
	})
}

func TestModelUpdate_Modality(t *testing.T) {
	t.Parallel()
	h, _, _, modelID := newModelGapEnv(t)

	set := func(v string) int {
		c, w := newTestContext(t, http.MethodPut, "/admin/api/models/"+idOf(modelID), gin.H{"modality": v})
		c.Params = gin.Params{{Key: "id", Value: idOf(modelID)}}
		h.Update(c)
		return w.Code
	}

	require.Equal(t, http.StatusOK, set("image"), "classify")
	assert.Equal(t, "image", refetchModel(t, h, modelID).Modality)

	require.Equal(t, http.StatusOK, set("text"), "reclassify")
	assert.Equal(t, "text", refetchModel(t, h, modelID).Modality)

	require.Equal(t, http.StatusOK, set(""), "explicit unclassify")
	assert.Equal(t, "", refetchModel(t, h, modelID).Modality)

	require.Equal(t, http.StatusBadRequest, set("voice"), "invalid enum rejected")
	assert.Equal(t, "", refetchModel(t, h, modelID).Modality, "rejected write must not mutate")

	c, w := newTestContext(t, http.MethodPut, "/admin/api/models/"+idOf(modelID), gin.H{"weight": 2})
	c.Params = gin.Params{{Key: "id", Value: idOf(modelID)}}
	h.Update(c)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "", refetchModel(t, h, modelID).Modality, "nil = untouched")
}
