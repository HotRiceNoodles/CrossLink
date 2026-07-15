package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// TemplateHandler manages prompt_templates CRUD + preview. Writes are
// super-admin only (RequireAction template:* in route registration). On any
// mutation it invalidates the TemplateRegistry cache, broadcasts to other
// instances via TemplateRegistrySync, and flushes the response cache (B3).
type TemplateHandler struct {
	db       *gorm.DB
	registry *service.TemplateRegistry
	sync     *service.TemplateRegistrySync // nil in tests / single-instance
	cacheSvc *service.CacheService          // nil in tests; flushed on update/delete (B3)
	auditSvc *service.AuditService          // nil in Community
}

func NewTemplateHandler(db *gorm.DB, registry *service.TemplateRegistry, sync *service.TemplateRegistrySync, cacheSvc *service.CacheService, auditSvc *service.AuditService) *TemplateHandler {
	return &TemplateHandler{db: db, registry: registry, sync: sync, cacheSvc: cacheSvc, auditSvc: auditSvc}
}

func (h *TemplateHandler) List(c *gin.Context) {
	var templates []model.PromptTemplate
	q := h.db.WithContext(c.Request.Context()).Where("deleted_at IS NULL")
	if orgID := GetOrgID(c); orgID != 0 {
		q = q.Where("org_id IS NULL OR org_id = ?", orgID)
	}
	q = q.Order("updated_at DESC")
	if err := q.Find(&templates).Error; err != nil {
		internalErr(c, err, "list templates failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": templates})
}

func (h *TemplateHandler) Create(c *gin.Context) {
	var input struct {
		Name            string          `json:"name" binding:"required,max=64"`
		Description     string          `json:"description"`
		SystemPrompt    string          `json:"system_prompt"`
		VariablesSchema json.RawMessage `json:"variables_schema"`
		FewShot         json.RawMessage `json:"few_shot"`
		ToolDefs        json.RawMessage `json:"tool_defs"`
		TargetFormat    string          `json:"target_format"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if !validTargetFormat(input.TargetFormat) {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "target_format must be auto|anthropic|openai")
		return
	}

	tpl := &model.PromptTemplate{
		Name:            input.Name,
		Description:     input.Description,
		SystemPrompt:    input.SystemPrompt,
		VariablesSchema: datatypes.JSON(input.VariablesSchema),
		FewShot:         datatypes.JSON(input.FewShot),
		ToolDefs:        datatypes.JSON(input.ToolDefs),
		TargetFormat:    defTargetFormat(input.TargetFormat),
		Status:          1,
	}
	if orgID := GetOrgID(c); orgID != 0 {
		tpl.OrgID = &orgID
	}
	if err := h.db.WithContext(c.Request.Context()).Create(tpl).Error; err != nil {
		if isDuplicateNameErr(err) {
			errorResp(c, http.StatusConflict, ErrConflict, "template name already exists")
			return
		}
		internalErr(c, err, "create template failed")
		return
	}
	h.notifyReload(tpl.Name)
	h.audit(c, "template:create", tpl.ID, tpl.Name)
	c.JSON(http.StatusCreated, gin.H{"data": tpl})
}

func (h *TemplateHandler) Get(c *gin.Context) {
	tpl, ok := h.findByID(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": tpl})
}

func (h *TemplateHandler) Update(c *gin.Context) {
	tpl, ok := h.findByID(c)
	if !ok {
		return
	}
	var input struct {
		Name            *string         `json:"name"`
		Description     *string         `json:"description"`
		SystemPrompt    *string         `json:"system_prompt"`
		VariablesSchema json.RawMessage `json:"variables_schema"`
		FewShot         json.RawMessage `json:"few_shot"`
		ToolDefs        json.RawMessage `json:"tool_defs"`
		TargetFormat    *string         `json:"target_format"`
		Status          *int16          `json:"status"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.TargetFormat != nil && !validTargetFormat(*input.TargetFormat) {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "target_format must be auto|anthropic|openai")
		return
	}
	if input.Name != nil {
		tpl.Name = *input.Name
	}
	if input.Description != nil {
		tpl.Description = *input.Description
	}
	if input.SystemPrompt != nil {
		tpl.SystemPrompt = *input.SystemPrompt
	}
	if input.VariablesSchema != nil {
		tpl.VariablesSchema = datatypes.JSON(input.VariablesSchema)
	}
	if input.FewShot != nil {
		tpl.FewShot = datatypes.JSON(input.FewShot)
	}
	if input.ToolDefs != nil {
		tpl.ToolDefs = datatypes.JSON(input.ToolDefs)
	}
	if input.TargetFormat != nil {
		tpl.TargetFormat = defTargetFormat(*input.TargetFormat)
	}
	if input.Status != nil {
		tpl.Status = *input.Status
	}

	if err := h.db.WithContext(c.Request.Context()).Save(tpl).Error; err != nil {
		if isDuplicateNameErr(err) {
			errorResp(c, http.StatusConflict, ErrConflict, "template name already exists")
			return
		}
		internalErr(c, err, "update template failed")
		return
	}
	// B3: invalidate registry + broadcast + flush response cache (assembled
	// requests cached under the old template must not be served).
	h.notifyReload(tpl.Name)
	h.flushCache(c)
	h.audit(c, "template:update", tpl.ID, tpl.Name)
	c.JSON(http.StatusOK, gin.H{"data": tpl})
}

func (h *TemplateHandler) Delete(c *gin.Context) {
	tpl, ok := h.findByID(c)
	if !ok {
		return
	}
	if err := h.db.WithContext(c.Request.Context()).Delete(tpl).Error; err != nil {
		internalErr(c, err, "delete template failed")
		return
	}
	if h.registry != nil {
		h.registry.Invalidate(tpl.Name)
	}
	if h.sync != nil {
		h.sync.NotifyRemove(tpl.Name)
	}
	h.flushCache(c)
	h.audit(c, "template:delete", tpl.ID, tpl.Name)
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// Preview renders the template with caller-supplied sample variables and
// returns the assembled system prompt + few-shot, WITHOUT calling any upstream.
// Surface render errors (validation, untrusted_var_in_system) as 400.
func (h *TemplateHandler) Preview(c *gin.Context) {
	tpl, ok := h.findByID(c)
	if !ok {
		return
	}
	var input struct {
		Variables map[string]any `json:"variables"`
	}
	_ = c.ShouldBindJSON(&input) // variables optional

	rendered, err := service.RenderTemplate(tpl, input.Variables)
	if err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rendered})
}

// --- helpers ---

func (h *TemplateHandler) findByID(c *gin.Context) (*model.PromptTemplate, bool) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return nil, false
	}
	var tpl model.PromptTemplate
	q := h.db.WithContext(c.Request.Context()).Where("id = ? AND deleted_at IS NULL", id)
	if orgID := GetOrgID(c); orgID != 0 {
		q = q.Where("org_id IS NULL OR org_id = ?", orgID)
	}
	if err := q.First(&tpl).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			errorResp(c, http.StatusNotFound, ErrNotFound, "template not found")
			return nil, false
		}
		internalErr(c, err, "load template failed")
		return nil, false
	}
	return &tpl, true
}

func (h *TemplateHandler) notifyReload(name string) {
	if h.registry != nil {
		h.registry.Invalidate(name)
	}
	if h.sync != nil {
		h.sync.NotifyReload(name)
	}
}

func (h *TemplateHandler) flushCache(c *gin.Context) {
	if h.cacheSvc != nil {
		h.cacheSvc.FlushAll(c.Request.Context())
	}
}

func (h *TemplateHandler) audit(c *gin.Context, action string, id int64, name string) {
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, action, "template", itoaInt64(id), name, service.AuditDetail(map[string]any{"name": name}))
	}
}

func itoaInt64(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func validTargetFormat(f string) bool {
	return f == "" || f == "auto" || f == "anthropic" || f == "openai"
}

func defTargetFormat(f string) string {
	if f == "" {
		return "auto"
	}
	return f
}
