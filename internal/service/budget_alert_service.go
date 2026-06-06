package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
)

type BudgetAlertService struct {
	alertRepo *repository.BudgetAlertRepo
	client    *http.Client
	sem       chan struct{}
}

func NewBudgetAlertService(alertRepo *repository.BudgetAlertRepo) *BudgetAlertService {
	return &BudgetAlertService{
		alertRepo: alertRepo,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: guardrail.NewSSRFSafeDialer(10 * time.Second),
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				if isInternalIP(req.URL.Hostname()) {
					return fmt.Errorf("redirect to internal host blocked")
				}
				return nil
			},
		},
		sem: make(chan struct{}, 16),
	}
}

func (s *BudgetAlertService) CheckAndAlert(ctx context.Context, scope, targetID, periodType string, spent, budget float64) {
	if budget <= 0 {
		return
	}
	pct := spent / budget * 100
	id, _ := strconv.ParseInt(targetID, 10, 64)
	if id == 0 {
		return
	}

	alerts, err := s.alertRepo.ListByTarget(ctx, scope, id)
	if err != nil {
		return
	}

	currentPeriod := PeriodKey(periodType)
	for _, alert := range alerts {
		if pct < float64(alert.ThresholdPct) {
			continue
		}
		if alert.LastTriggeredAt != nil {
			triggeredPeriod := periodKeyAt(periodType, *alert.LastTriggeredAt)
			if triggeredPeriod == currentPeriod {
				continue
			}
		}
		// Acquire semaphore slot (bounded concurrency)
		select {
		case s.sem <- struct{}{}:
		default:
			slog.Warn("budget alert webhook skipped: concurrency limit reached", "alert_id", alert.ID)
			continue
		}
		go func(a model.BudgetAlert) {
			defer func() {
				<-s.sem
				if r := recover(); r != nil {
					slog.Warn("budget alert goroutine panic", "error", r)
				}
			}()
			if s.sendWebhook(a, scope, targetID, spent, budget, pct) {
				alertCtx, alertCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer alertCancel()
				s.alertRepo.MarkTriggered(alertCtx, a.ID)
			}
		}(alert)
	}
}

func periodKeyAt(period string, t time.Time) string {
	utc := t.UTC()
	switch period {
	case "daily":
		return utc.Format("2006-01-02")
	case "weekly":
		y, w := utc.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	case "monthly":
		return utc.Format("2006-01")
	default:
		return utc.Format("2006-01")
	}
}

func (s *BudgetAlertService) sendWebhook(alert model.BudgetAlert, scope, targetID string, spent, budget float64, pct float64) bool {
	// Generate smart action suggestion based on usage level
	suggestedAction := ""
	actionType := ""
	if pct >= 100 {
		suggestedAction = "Budget exhausted. New requests will be rejected. Consider requesting additional budget or investigating unusual usage."
		actionType = "budget_exhausted"
	} else if pct >= 90 {
		suggestedAction = fmt.Sprintf("Budget usage at %.1f%%, nearing exhaustion. Consider requesting additional budget immediately.", pct)
		actionType = "budget_critical"
	} else if pct >= 80 {
		suggestedAction = fmt.Sprintf("Budget usage at %.1f%%. Monitor spending trends and consider adjusting the budget.", pct)
		actionType = "budget_warning"
	}

	payload := map[string]any{
		"scope":           scope,
		"target_id":       targetID,
		"spent":           spent,
		"budget":          budget,
		"usage_pct":       pct,
		"threshold":       alert.ThresholdPct,
		"triggered_at":    time.Now().Format(time.RFC3339),
		"suggested_action": suggestedAction,
		"action_type":     actionType,
	}
	body, _ := json.Marshal(payload)

	resp, err := s.client.Post(alert.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("budget alert webhook failed", "error", err, "url", alert.WebhookURL)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		slog.Warn("budget alert webhook returned non-success", "status", resp.StatusCode, "url", alert.WebhookURL)
		return false
	}
	return true
}

func isInternalIP(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Hostname that isn't a bare IP — resolve via DNS
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return true // treat unresolvable as internal to be safe
		}
		for _, resolved := range ips {
			if isInternalIPAddr(resolved) {
				return true
			}
		}
		return false
	}
	return isInternalIPAddr(ip)
}

func isInternalIPAddr(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
