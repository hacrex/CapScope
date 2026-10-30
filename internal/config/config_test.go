package config

import (
	"strings"
	"testing"
	"time"
)

func TestAlertDefaults(t *testing.T) {
	// Minimal valid config.
	t.Setenv("AUTH_PASS", "secret")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AlertsFile != "/data/alerts.json" {
		t.Errorf("ALERTS_FILE default = %q", c.AlertsFile)
	}
	if c.AlertsInterval != time.Minute {
		t.Errorf("ALERTS_INTERVAL default = %v, want 1m", c.AlertsInterval)
	}
	if c.AlertWebhookURL != "" || c.AlertTelegramTok != "" || c.AlertSlackURL != "" || c.AlertDiscordURL != "" {
		t.Error("channel settings should default to empty")
	}
}

func TestAlertsIntervalClamped(t *testing.T) {
	t.Setenv("AUTH_PASS", "secret")
	t.Setenv("ALERTS_INTERVAL", "1s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AlertsInterval != 10*time.Second {
		t.Errorf("interval = %v, want clamp to 10s", c.AlertsInterval)
	}
}

func TestBadChannelURLRejected(t *testing.T) {
	t.Setenv("AUTH_PASS", "secret")
	t.Setenv("ALERT_WEBHOOK_URL", "not-a-url")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALERT_WEBHOOK_URL") {
		t.Errorf("want ALERT_WEBHOOK_URL error, got %v", err)
	}
}

func TestTelegramPairRequired(t *testing.T) {
	t.Setenv("AUTH_PASS", "secret")
	t.Setenv("ALERT_TELEGRAM_TOKEN", "tok") // missing chat id
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALERT_TELEGRAM") {
		t.Errorf("want telegram pairing error, got %v", err)
	}
	t.Setenv("ALERT_TELEGRAM_CHAT_ID", "123")
	if _, err := Load(); err != nil {
		t.Errorf("paired telegram should be valid, got %v", err)
	}
}

func TestPerAppBudgetsParsed(t *testing.T) {
	t.Setenv("AUTH_PASS", "secret")
	t.Setenv("APP_RETENTION", "web=3d, api=1d")
	t.Setenv("APP_MAX_MB", "web=500,api=200")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AppRetention["web"] != 72*time.Hour {
		t.Errorf("web retention = %v, want 72h", c.AppRetention["web"])
	}
	if c.AppRetention["api"] != 24*time.Hour {
		t.Errorf("api retention = %v, want 24h", c.AppRetention["api"])
	}
	if c.AppMaxMB["web"] != 500 || c.AppMaxMB["api"] != 200 {
		t.Errorf("app max MB = %v", c.AppMaxMB)
	}
}

func TestPerAppBudgetsEmptyAndInvalid(t *testing.T) {
	t.Setenv("AUTH_PASS", "secret")
	t.Setenv("APP_RETENTION", "")
	t.Setenv("APP_MAX_MB", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.AppRetention) != 0 || len(c.AppMaxMB) != 0 {
		t.Errorf("want empty budgets, got %v / %v", c.AppRetention, c.AppMaxMB)
	}

	// Invalid values are collected as errors, valid ones still parse.
	t.Setenv("APP_RETENTION", "web=nonsense,api=2d")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "APP_RETENTION") {
		t.Errorf("want APP_RETENTION error, got %v", err)
	}
	t.Setenv("APP_MAX_MB", "web=abc")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "APP_MAX_MB") {
		t.Errorf("want APP_MAX_MB error, got %v", err)
	}
}
