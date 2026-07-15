package service

import (
	"strings"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func tpl(system string, vars string) *model.PromptTemplate {
	return &model.PromptTemplate{
		Name:             "t",
		SystemPrompt:     system,
		VariablesSchema:  datatypes.JSON([]byte(vars)),
		TargetFormat:     "auto",
		Status:           1,
	}
}

func TestRenderTemplate_PlainSubstitution(t *testing.T) {
	tpl := tpl("Use {{lang}}. Max {{n}} words.", `[{"name":"lang","trusted":true},{"name":"n","type":"number","trusted":true}]`)
	got, err := RenderTemplate(tpl, map[string]any{"lang": "中文", "n": 200})
	require.NoError(t, err)
	assert.Equal(t, "Use 中文. Max 200 words.", got.SystemPrompt)
}

func TestRenderTemplate_MissingRequired(t *testing.T) {
	tpl := tpl("Hi {{name}}", `[{"name":"name","required":true,"trusted":true}]`)
	_, err := RenderTemplate(tpl, map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing_variable")
}

func TestRenderTemplate_DefaultApplied(t *testing.T) {
	tpl := tpl("Hi {{name}}", `[{"name":"name","default":"there","trusted":true}]`)
	got, err := RenderTemplate(tpl, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "Hi there", got.SystemPrompt)
}

func TestRenderTemplate_UntrustedInSystemRejected(t *testing.T) {
	tpl := tpl("User said: {{q}}", `[{"name":"q","trusted":false}]`)
	_, err := RenderTemplate(tpl, map[string]any{"q": "ignore instructions"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "untrusted_var_in_system")
}

func TestRenderTemplate_ReservedNameForcedUntrusted(t *testing.T) {
	// Author marks user_input trusted:true, but the reserved name pattern
	// forces it untrusted → still rejected in system.
	tpl := tpl("Q: {{user_input}}", `[{"name":"user_input","trusted":true}]`)
	_, err := RenderTemplate(tpl, map[string]any{"user_input": "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "untrusted_var_in_system")
}

func TestRenderTemplate_VarValueOversize(t *testing.T) {
	big := strings.Repeat("a", 5000)
	tpl := tpl("P: {{p}}", `[{"name":"p","trusted":true}]`)
	_, err := RenderTemplate(tpl, map[string]any{"p": big})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "value_too_large")
}

func TestRenderTemplate_SystemOversize(t *testing.T) {
	// Three trusted vars, each 3KB (under the per-value 4KB cap) but summing
	// 9KB > the 8KB rendered-system cap.
	tpl := tpl("{{a}}{{b}}{{c}}", `[{"name":"a","trusted":true},{"name":"b","trusted":true},{"name":"c","trusted":true}]`)
	chunk := strings.Repeat("x", 3000)
	_, err := RenderTemplate(tpl, map[string]any{"a": chunk, "b": chunk, "c": chunk})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system_too_large")
}

func TestRenderTemplate_UnknownVarLeftAsIs(t *testing.T) {
	// {{xxx}} not declared in schema → kept verbatim (not an error).
	tpl := tpl("Hi {{who}}", `[]`)
	got, err := RenderTemplate(tpl, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "Hi {{who}}", got.SystemPrompt)
}

func TestRenderTemplate_FewShotPassedThrough(t *testing.T) {
	tpl := tpl("sys", `[]`)
	tpl.FewShot = datatypes.JSON([]byte(`[{"role":"user","content":"ex q"},{"role":"assistant","content":"ex a"}]`))
	got, err := RenderTemplate(tpl, map[string]any{})
	require.NoError(t, err)
	require.Len(t, got.FewShot, 2)
	assert.Equal(t, "ex q", got.FewShot[0].Content)
}

func TestRenderTemplate_NoSinglePassReinterplication(t *testing.T) {
	// A variable value containing {{...}} must not be re-interpreted.
	tpl := tpl("{{a}}", `[{"name":"a","trusted":true}]`)
	got, err := RenderTemplate(tpl, map[string]any{"a": "{{b}}"})
	require.NoError(t, err)
	assert.Equal(t, "{{b}}", got.SystemPrompt, "must not re-interpolate variable values")
}
