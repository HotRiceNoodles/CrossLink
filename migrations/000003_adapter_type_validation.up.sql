ALTER TABLE providers ADD CONSTRAINT chk_adapter_type
  CHECK (adapter_type IN ('openai_compatible', 'anthropic', 'azure_openai', 'aws_bedrock', 'google_vertex', 'ollama'));
