package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/crosslink/internal/model"
	"gopkg.in/yaml.v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// TemplateSeed is the YAML structure for seeding prompt templates.
type TemplateSeed struct {
	Templates []TemplateConfig `yaml:"templates"`
}

type TemplateConfig struct {
	Name            string          `yaml:"name"`
	Description     string          `yaml:"description"`
	SystemPrompt    string          `yaml:"system_prompt"`
	VariablesSchema []VariableConfig `yaml:"variables_schema"`
	FewShot         []FewShotConfig `yaml:"few_shot"`
	TargetFormat    string          `yaml:"target_format"`
	Status          int             `yaml:"status"`
}

type VariableConfig struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Required bool   `yaml:"required"`
	Default  any    `yaml:"default"`
	Trusted  bool   `yaml:"trusted"`
	Desc     string `yaml:"desc"`
}

type FewShotConfig struct {
	Role    string `yaml:"role"`
	Content string `yaml:"content"`
}

// SeedPromptTemplates seeds example prompt templates from a YAML file on first
// boot (when the table is empty), giving users reference examples to learn from
// and duplicate. Mirrors the SeedProviders pattern.
func SeedPromptTemplates(db *gorm.DB, path string) error {
	var count int64
	db.Table("prompt_templates").Count(&count)
	if count > 0 {
		slog.Info("prompt_templates already exist, skipping seed")
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read prompt_templates config: %w", err)
	}

	var seed TemplateSeed
	if err := yaml.Unmarshal(data, &seed); err != nil {
		return fmt.Errorf("parse prompt_templates config: %w", err)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, t := range seed.Templates {
			tpl := model.PromptTemplate{
				Name:         t.Name,
				Description:  t.Description,
				SystemPrompt: t.SystemPrompt,
				TargetFormat: t.TargetFormat,
				Status:       1,
			}
			if tpl.TargetFormat == "" {
				tpl.TargetFormat = "auto"
			}
			if t.VariablesSchema != nil {
				b, _ := json.Marshal(t.VariablesSchema)
				tpl.VariablesSchema = datatypes.JSON(b)
			}
			if t.FewShot != nil {
				b, _ := json.Marshal(t.FewShot)
				tpl.FewShot = datatypes.JSON(b)
			}
			if err := tx.Create(&tpl).Error; err != nil {
				return fmt.Errorf("create prompt_template %s: %w", t.Name, err)
			}
		}
		slog.Info("seeded prompt templates from config", "count", len(seed.Templates))
		return nil
	})
}
