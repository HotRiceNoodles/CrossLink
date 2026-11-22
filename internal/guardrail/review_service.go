package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	"gorm.io/gorm"
)

type ReviewService struct {
	ID              int64   `json:"id"`
	OrgID           int64   `json:"-"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	ProviderModelID int64   `json:"provider_model_id"`
	Endpoint        string  `json:"endpoint"`
	Credential      string  `json:"-"`
	ModelVersion    string  `json:"model_version"`
	Threshold       float64 `json:"threshold"`
	TimeoutMS       int     `json:"timeout_ms"`
}

type ReviewCatalogEntry struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func ReviewCatalog(db *gorm.DB, org int64) *gorm.DB {
	return db.Model(&model.ProviderModel{}).Joins("JOIN providers p ON p.id=provider_models.provider_id").
		Where("p.org_id=? AND p.status=1 AND p.deleted_at IS NULL AND p.adapter_type='openai_compatible' AND provider_models.status=1", org)
}

var reviewVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]{0,127}$`)

func (s *GuardrailService) SetReviewSecrets(resolver *secret.SecretResolver, store *secret.EncryptedDBStore) {
	s.reviewResolver, s.reviewStore = resolver, store
}

func (s *GuardrailService) ConnectReview(ctx context.Context, r *ReviewService, token string) error {
	if r.OrgID <= 0 || strings.TrimSpace(r.Name) == "" || len(r.Name) > 128 || r.TimeoutMS < 1 || r.TimeoutMS > 60000 {
		return errors.New("名称或超时无效")
	}
	switch r.Kind {
	case "provider":
		var entry struct{ ProviderModel string }
		if r.Endpoint != "" || token != "" || ReviewCatalog(s.db.WithContext(ctx), r.OrgID).Select("provider_models.provider_model").Where("provider_models.id=?", r.ProviderModelID).Take(&entry).Error != nil {
			return errors.New("请选择本组织已启用的 OpenAI 兼容供应商模型")
		}
		r.ModelVersion = entry.ProviderModel
	case "local":
		u, err := url.Parse(r.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !net.ParseIP(u.Hostname()).IsPrivate() || ValidateURLSafety(r.Endpoint) != nil {
			return errors.New("本地服务需要出站白名单内的内网 IP 地址")
		}
		if !reviewVersion.MatchString(r.ModelVersion) || r.Threshold <= 0 || r.Threshold > 1 {
			return errors.New("请填写部署版本及其校准阈值 (0, 1]")
		}
		r.ProviderModelID = 0
		if token != "" {
			if s.reviewStore == nil {
				return errors.New("请先配置凭据加密")
			}
			r.Credential, err = s.reviewStore.Encrypt(token)
			if err != nil {
				return errors.New("无法保存服务凭据")
			}
		}
	default:
		return errors.New("未知审核服务类型")
	}
	return s.db.WithContext(ctx).Create(r).Error
}

var errInvalidReview = errors.New("invalid_output")
var errReviewUnavailable = errors.New("unavailable")

// 每次连接重新检查出站白名单；禁止重定向转发凭据和正文。
var reviewHTTP = &http.Client{
	Transport:     &http.Transport{DisableKeepAlives: true, DialContext: NewSSRFSafeDialer(5 * time.Second)},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (s *GuardrailService) reviewHTTPJSON(ctx context.Context, endpoint, credential string, payload any, dest any) error {
	method := http.MethodGet
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return errInvalidReview
		}
		body = bytes.NewReader(raw)
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errReviewUnavailable
	}
	if credential != "" {
		if s.reviewResolver != nil {
			credential, err = s.reviewResolver.Resolve(ctx, credential)
			if err != nil {
				return errReviewUnavailable
			}
		} else if secret.IsReference(credential) {
			return errReviewUnavailable
		}
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CrossLink-Model-Review", "1")
	resp, err := reviewHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errReviewUnavailable
	}
	const limit = 128 << 10
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if len(raw) > limit || json.Unmarshal(raw, dest) != nil {
		return errInvalidReview
	}
	return nil
}

type ReviewHealth struct {
	Status       string `json:"status"`
	ModelVersion string `json:"model_version,omitempty"`
	MaxLength    int    `json:"max_length,omitempty"`
}

func (s *GuardrailService) ReviewHealth(ctx context.Context, r ReviewService) ReviewHealth {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.TimeoutMS)*time.Millisecond)
	defer cancel()
	var health ReviewHealth
	if r.Kind != "local" {
		return ReviewHealth{Status: "configured", ModelVersion: r.ModelVersion}
	}
	if s.reviewHTTPJSON(ctx, strings.TrimRight(r.Endpoint, "/")+"/healthz", r.Credential, nil, &health) != nil || health.Status != "ok" || !reviewVersion.MatchString(health.ModelVersion) || health.MaxLength <= 0 {
		return ReviewHealth{Status: "unavailable"}
	}
	if health.ModelVersion != r.ModelVersion {
		health.Status = "version_mismatch"
	}
	return health
}
