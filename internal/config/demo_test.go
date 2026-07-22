package config

import (
	"strings"
	"testing"
)

func TestValidate_Demo(t *testing.T) {
	t.Run("enabled without api_key fails", func(t *testing.T) {
		cfg := validConfig()
		cfg.Demo.Enabled = true
		cfg.Demo.APIKey = ""
		if err := cfg.Validate(); err == nil {
			t.Error("demo.enabled=true with empty api_key should fail")
		}
	})
	t.Run("enabled with api_key passes demo check", func(t *testing.T) {
		cfg := validConfig()
		cfg.Demo.Enabled = true
		cfg.Demo.APIKey = "cl-demo-0000-0000-0000-0000"
		if err := cfg.Validate(); err != nil {
			if strings.Contains(err.Error(), "demo") {
				t.Errorf("should not have demo error with api_key set, got: %v", err)
			}
		}
	})
	t.Run("disabled with empty api_key is fine", func(t *testing.T) {
		cfg := validConfig()
		cfg.Demo.Enabled = false
		cfg.Demo.APIKey = ""
		_ = cfg.Validate() // demo check must not trigger
	})
}
