package admin

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/repository"
)

var validModules = map[string]bool{
	"totalCost": true, "totalTokens": true, "costTrend": true,
	"modelDistribution": true, "teamRanking": true, "kpiCards": true,
	"requestTrend": true, "usageHeatmap": true, "anomalyBar": true,
	"providerStatus": true, "securityOverview": true, "securityTrend": true,
	"severityChart": true, "alertTicker": true, "requestTicker": true,
}

var validTemplates = map[string]bool{
	"executive": true, "operations": true, "security": true,
}

type PreferencesHandler struct {
	userRepo *repository.UserRepo
}

func NewPreferencesHandler(userRepo *repository.UserRepo) *PreferencesHandler {
	return &PreferencesHandler{userRepo: userRepo}
}

func (h *PreferencesHandler) Get(c *gin.Context) {
	userID := GetUserID(c)
	if userID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	user, err := h.userRepo.GetByID(c.Request.Context(), userID)
	if err != nil {
		internalErr(c, err, "get user preferences failed")
		return
	}

	var prefs map[string]interface{}
	if user.Preferences != nil {
		if err := json.Unmarshal(user.Preferences, &prefs); err != nil {
			prefs = map[string]interface{}{}
		}
	}
	if prefs == nil {
		prefs = map[string]interface{}{}
	}
	c.JSON(http.StatusOK, gin.H{"data": prefs})
}

func (h *PreferencesHandler) Update(c *gin.Context) {
	userID := GetUserID(c)
	if userID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Limit request body size
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10*1024)

	var input map[string]interface{}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid JSON")
		return
	}

	// Validate overview_screen if present
	if os, ok := input["overview_screen"].(map[string]interface{}); ok {
		if tmpl, ok := os["template"].(string); ok {
			if !validTemplates[tmpl] {
				errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid template")
				return
			}
		}
		if modules, ok := os["modules"].([]interface{}); ok {
			for _, m := range modules {
				name, ok := m.(string)
				if !ok || !validModules[name] {
					errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid module: "+name)
					return
				}
			}
		}
	}

	raw, err := json.Marshal(input)
	if err != nil {
		internalErr(c, err, "marshal preferences failed")
		return
	}

	user, err := h.userRepo.GetByID(c.Request.Context(), userID)
	if err != nil {
		internalErr(c, err, "get user failed")
		return
	}

	user.Preferences = raw
	if err := h.userRepo.Update(c.Request.Context(), user); err != nil {
		internalErr(c, err, "save preferences failed")
		return
	}

	var result map[string]interface{}
	json.Unmarshal(raw, &result)
	c.JSON(http.StatusOK, gin.H{"data": result})
}
