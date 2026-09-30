package settings

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	"gorm.io/gorm"
)

// Provider is the runtime operational-config source. Consumers read the
// immutable snapshot per request / per send (pull model); the only push
// consumer is the captcha gate (Subscribe → ApplyConfig).
//
// Refresh happens (a) at startup, (b) every interval via RunRefreshLoop
// (multi-instance eventual consistency, same contract as the resilience
// keys), (c) immediately after an admin PUT via RefreshNow.
type Provider struct {
	db       *gorm.DB
	baseline *config.Config
	encStore *secret.EncryptedDBStore // may be nil (keyless boot)

	snap atomic.Pointer[Snapshot]

	mu          sync.Mutex
	subscribers []func(*Snapshot)
}

// NewProvider constructs the provider and loads the first snapshot.
// encStore may be nil — smtp_password rows stay encrypted-undecryptable in
// that mode (source "db-error") until a key activates.
func NewProvider(db *gorm.DB, cfg *config.Config, encStore *secret.EncryptedDBStore) *Provider {
	p := &Provider{db: db, baseline: cfg, encStore: encStore}
	p.snap.Store(buildSnapshot(cfg, map[string]string{}, nil))
	// Initial load so the snapshot reflects DB rows even before RunRefreshLoop
	// starts. A failed read (e.g. fresh install pre-migration) keeps the
	// baseline snapshot — never fatal.
	if err := p.load(context.Background()); err != nil {
		slog.Warn("settings initial load failed, using yaml/env baseline", "error", err)
	}
	return p
}

// Get returns the current snapshot. Never nil after NewProvider.
func (p *Provider) Get() *Snapshot {
	return p.snap.Load()
}

// Section getters — value copies, safe for concurrent use.

func (p *Provider) SMTP() config.SMTPConfig               { return p.Get().SMTP }
func (p *Provider) GatewayBaseURL() string                { return p.Get().GatewayBaseURL }
func (p *Provider) CORSAllowed() []string                 { return p.Get().CORSAllowed }
func (p *Provider) RateLimit() config.RateLimitConfig     { return p.Get().RateLimit }
func (p *Provider) GuardrailAlert() config.GuardrailAlertConfig { return p.Get().GuardrailAlert }
func (p *Provider) IPBinding() config.IPBindingConfig     { return p.Get().IPBinding }
func (p *Provider) DataLens() config.DataLensConfig       { return p.Get().DataLens }
func (p *Provider) MCP() config.MCPConfig                 { return p.Get().MCP }
func (p *Provider) Captcha() config.CaptchaConfig         { return p.Get().Captcha }

// Subscribe registers a callback invoked with every new snapshot (initial
// load, poll tick, RefreshNow). Callbacks must be cheap and non-blocking —
// they run inline on refresh. Used by the captcha gate applier.
func (p *Provider) Subscribe(fn func(*Snapshot)) {
	p.mu.Lock()
	p.subscribers = append(p.subscribers, fn)
	p.mu.Unlock()
}

// RefreshNow reloads the snapshot from the DB and swaps it in. Called by the
// admin PUT handler after committing rows (hot-apply) and once at startup.
func (p *Provider) RefreshNow(ctx context.Context) error {
	return p.load(ctx)
}

// RunRefreshLoop applies once immediately, then re-reads every interval
// until ctx is cancelled — the RunResilienceRefreshLoop shape. A failed
// read keeps the previous snapshot (logged).
func (p *Provider) RunRefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	apply := func() {
		if err := p.load(ctx); err != nil {
			slog.Warn("settings refresh failed, keeping previous snapshot", "error", err)
		}
	}
	apply()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			apply()
		}
	}
}

// EncryptSecret encrypts a sensitive value for at-rest storage when an
// encryption key is active. Keyless mode returns the plaintext with
// encrypted=false — the same fallback contract as provider secrets (usable,
// re-encrypted opportunistically once a key activates).
func (p *Provider) EncryptSecret(plain string) (stored string, encrypted bool, err error) {
	if p.encStore == nil {
		return plain, false, nil
	}
	enc, err := p.encStore.Encrypt(plain)
	if err != nil {
		return "", false, err
	}
	return enc, true, nil
}

func (p *Provider) load(ctx context.Context) error {
	var rows []model.SystemSetting
	if err := p.db.WithContext(ctx).
		Where("key IN ?", KeyNames()).
		Find(&rows).Error; err != nil {
		return err
	}
	values := make(map[string]string, len(rows))
	for _, r := range rows {
		values[r.Key] = r.Value
	}

	var decrypt decryptFunc
	if p.encStore != nil {
		decrypt = p.encStore.Decrypt
	}
	snap := buildSnapshot(p.baseline, values, decrypt)

	p.mu.Lock()
	p.snap.Store(snap)
	subs := make([]func(*Snapshot), len(p.subscribers))
	copy(subs, p.subscribers)
	p.mu.Unlock()
	for _, fn := range subs {
		fn(snap)
	}
	return nil
}
