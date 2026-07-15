package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// TemplateCatalogHandler exposes a read-only catalog of active prompt templates
// to API key holders (consumer discovery / DX): developers list available templates
// and their variable schemas, and get a ready-to-use curl example, so they can call
// x_context without asking the admin or guessing variable names.
//
// Only metadata is exposed — system_prompt content and few_shot (the prompt IP)
// stay server-side and are NEVER returned. See
// docs/plans/2026-07-14-context-engineering-gateway-design.md §B.
type TemplateCatalogHandler struct {
	db *gorm.DB
}

func NewTemplateCatalogHandler(db *gorm.DB) *TemplateCatalogHandler {
	return &TemplateCatalogHandler{db: db}
}

type catalogVariable struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required"`
}

type catalogEntry struct {
	Name         string            `json:"name"`
	Description  string            `json:"description,omitempty"`
	TargetFormat string            `json:"target_format"`
	Variables    []catalogVariable `json:"variables,omitempty"`
	Example      string            `json:"example"`
}

// Only REQUIRED variables are surfaced: optional ones have defaults the caller
// need not fill, so the catalog stays concise.
func (h *TemplateCatalogHandler) List(c *gin.Context) {
	var tpls []model.PromptTemplate
	q := h.db.WithContext(c.Request.Context()).
		Where("status = 1 AND deleted_at IS NULL").
		Order("name ASC")
	if orgID := c.GetInt64("org_id"); orgID != 0 {
		q = q.Where("org_id IS NULL OR org_id = ?", orgID)
	}
	if err := q.Find(&tpls).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list templates"})
		return
	}

	out := make([]catalogEntry, 0, len(tpls))
	for i := range tpls {
		tpl := &tpls[i]
		vars := requiredVariables(tpl.VariablesSchema)
		out = append(out, catalogEntry{
			Name:         tpl.Name,
			Description:  tpl.Description,
			TargetFormat: tpl.TargetFormat,
			Variables:    vars,
			Example:      buildExample(tpl.Name, vars),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func requiredVariables(raw []byte) []catalogVariable {
	if len(raw) == 0 {
		return nil
	}
	var defs []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Required bool   `json:"required"`
	}
	if json.Unmarshal(raw, &defs) != nil {
		return nil
	}
	out := make([]catalogVariable, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" || !d.Required {
			continue
		}
		out = append(out, catalogVariable{Name: d.Name, Type: d.Type, Required: d.Required})
	}
	return out
}

// buildExample renders a curl snippet calling the template with its required vars
// filled with placeholder values.
func buildExample(name string, vars []catalogVariable) string {
	var pairs []string
	for _, v := range vars {
		pairs = append(pairs, fmt.Sprintf("%q:%q", v.Name, "<"+v.Name+">"))
	}
	varJSON := "{" + strings.Join(pairs, ",") + "}"
	body := fmt.Sprintf(`{"x_context":{"template":%q,"variables":%s},"model":"<model>","messages":[{"role":"user","content":"<your message>"}]}`, name, varJSON)
	return fmt.Sprintf("curl -X POST $GATEWAY/v1/chat/completions \\\n  -H \"Authorization: Bearer $KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '%s'", body)
}
