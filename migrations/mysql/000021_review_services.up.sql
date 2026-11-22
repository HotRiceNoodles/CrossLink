CREATE TABLE review_services (
    id BIGINT PRIMARY KEY AUTO_INCREMENT, org_id BIGINT NOT NULL, name VARCHAR(128) NOT NULL, kind VARCHAR(16) NOT NULL,
    provider_model_id BIGINT NOT NULL, endpoint VARCHAR(1024) NOT NULL, credential TEXT NOT NULL,
    model_version VARCHAR(128) NOT NULL, threshold DOUBLE PRECISION NOT NULL, timeout_ms INTEGER NOT NULL
);
CREATE INDEX idx_review_services_org ON review_services(org_id);
