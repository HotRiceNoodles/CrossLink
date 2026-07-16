-- Mock fixtures: recorded (request → response) pairs for VCR-style playback.
-- The RecordingProvider stores real upstream responses here; MockProvider looks
-- them up by request hash to replay recorded responses at zero cost.
-- See docs/plans/2026-07-15-mock-record-playback-design.md.
CREATE TABLE mock_fixtures (
    id            BIGSERIAL PRIMARY KEY,
    provider_name VARCHAR(64) NOT NULL,
    model         VARCHAR(128) NOT NULL,
    request_hash  VARCHAR(64) NOT NULL,
    response_body JSONB,
    stream_chunks JSONB,
    is_stream     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX mock_fixtures_lookup ON mock_fixtures (request_hash, model);
CREATE INDEX mock_fixtures_provider ON mock_fixtures (provider_name);
