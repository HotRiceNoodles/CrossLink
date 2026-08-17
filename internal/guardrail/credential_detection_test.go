package guardrail

import (
	"context"
	"encoding/json"
	"testing"

)

func TestCredentialDetection_OpenAIKey(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "my key is sk-abcdefghijklmnopqrstuvwxyz123456", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block OpenAI API key")
	}
	if result.RuleName != "credential_detection" {
		t.Errorf("expected rule_name=credential_detection, got %q", result.RuleName)
	}
}

func TestCredentialDetection_AWSKey(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "AWS_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block AWS access key")
	}
}

func TestCredentialDetection_PrivateKey(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "-----BEGIN RSA PRIVATE KEY-----\nMIIE...", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block private key")
	}
}

func TestCredentialDetection_DatabaseString(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "DB_URL=mysql://admin:password123@db.example.com:3306/mydb", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block database connection string")
	}
}

func TestCredentialDetection_GitHubPAT(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "token=ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block GitHub PAT")
	}
}

func TestCredentialDetection_SlackToken(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	// Assembled at runtime so secret scanners don't flag the fixture.
	slackToken := "xoxb-" + "1234567890" + "-" + "1234567890" + "-" + "abcdefghijklmnopqrstuvwx"
	result, err := eng.Check(context.Background(), "SLACK_TOKEN="+slackToken, DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block Slack bot token")
	}
}

func TestCredentialDetection_NoMatch(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "Hello, how are you today?", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Blocked {
		t.Error("should not block normal content")
	}
}

func TestCredentialDetection_CategoryFilter(t *testing.T) {
	raw := json.RawMessage(`{"categories": ["api_key"]}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	// Database string should NOT be detected when only api_key category is active
	result, err := eng.Check(context.Background(), "DB_URL=mysql://admin:pass@db.example.com:3306/mydb", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Blocked {
		t.Error("should not block database string when only api_key category active")
	}

	// But API key should still be detected
	result, err = eng.Check(context.Background(), "key=sk-abcdefghijklmnopqrstuvwxyz123456", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block API key in api_key category")
	}
}

func TestCredentialDetection_CustomPattern(t *testing.T) {
	raw := json.RawMessage(`{
		"custom_patterns": [
			{
				"name": "my_key",
				"regex": "MYKEY-[a-zA-Z0-9]{32}",
				"severity": "critical"
			}
		]
	}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "auth=MYKEY-abcdefghijklmnopqrstuvwxyz123456", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block custom pattern")
	}
}

func TestCredentialDetection_Masking(t *testing.T) {
	raw := json.RawMessage(`{
		"mask_char": "*",
		"mask_keep_prefix": 4,
		"mask_keep_suffix": 4
	}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "key=sk-abcdefghijklmnopqrstuvwxyz123456", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Fatal("should block")
	}
	if result.MaskedContent == "" {
		t.Error("expected masked content")
	}
	// Masked content should retain prefix and suffix
	if len(result.MaskedContent) > 0 && result.MaskedContent[0] == '*' {
		t.Error("masked content prefix should be preserved")
	}
}

func TestCredentialDetection_LowConfidenceNoBlock(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	// .env pattern is low confidence — should not block, just log
	result, err := eng.Check(context.Background(), "PASSWORD=supersecretpassword123", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Blocked {
		t.Error("low confidence env pattern should not block")
	}
}

func TestCredentialDetection_MediumConfidencePlaceholder(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	// Bearer token with placeholder value should be skipped
	result, err := eng.Check(context.Background(), "Authorization: Bearer your_api_key_here", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Blocked {
		t.Error("placeholder bearer token should not block")
	}
}

func TestCredentialDetection_JWT(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	// Valid-looking JWT
	result, err := eng.Check(context.Background(), "token=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block JWT token")
	}
}

func TestCredentialDetection_AliyunAK(t *testing.T) {
	raw := json.RawMessage(`{}`)
	eng, err := NewCredentialDetectionEngineFromConfig(raw)
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	result, err := eng.Check(context.Background(), "ACCESS_KEY=LTAI5tFakeExampleKey12", DirectionRequest, "gpt-4")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.Blocked {
		t.Error("should block Aliyun access key")
	}
}
