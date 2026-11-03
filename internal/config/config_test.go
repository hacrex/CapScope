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

// Z5-prep: AUTH_PROVIDER=proxy — authentication is owned by the reverse proxy
// and a password is not required; the identity header must be a valid name.
func TestProxyProviderConfig(t *testing.T) {
	t.Setenv("AUTH_PASS", "") // proxy mode must not require a password
	t.Setenv("AUTH_PROVIDER", "proxy")
	c, err := Load()
	if err != nil {
		t.Fatalf("proxy mode should not require AUTH_PASS: %v", err)
	}
	if c.ProxyUserHeader != "Remote-User" {
		t.Errorf("PROXY_USER_HEADER default = %q, want Remote-User", c.ProxyUserHeader)
	}
	t.Setenv("PROXY_USER_HEADER", "X-WebAuth-User")
	if c, err = Load(); err != nil || c.ProxyUserHeader != "X-WebAuth-User" {
		t.Errorf("custom header = %q err %v", c.ProxyUserHeader, err)
	}

	t.Setenv("PROXY_USER_HEADER", "bad header with a space") // not a valid MIME header key
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PROXY_USER_HEADER") {
		t.Errorf("want PROXY_USER_HEADER error, got %v", err)
	}

	t.Setenv("PROXY_USER_HEADER", "Remote-User")
	t.Setenv("AUTH_PROVIDER", "sso")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AUTH_PROVIDER") {
		t.Errorf("want unknown provider error, got %v", err)
	}

	// Back to local: an empty password is refused again (regression guard on
	// the provider switch).
	t.Setenv("AUTH_PROVIDER", "local")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AUTH_PASS") {
		t.Errorf("local mode must still require AUTH_PASS, got %v", err)
	}
}

// A7: ntfy channel threading and the update-check knobs.
func TestNtfyAndUpdateCheckConfig(t *testing.T) {
	t.Setenv("AUTH_PASS", "secret")
	// Update check is on by default with a 24h cadence; ntfy defaults empty.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.UpdateCheck {
		t.Error("UPDATE_CHECK should default to true")
	}
	if c.UpdateCheckInterval != 24*time.Hour {
		t.Errorf("UPDATE_CHECK_INTERVAL default = %v, want 24h", c.UpdateCheckInterval)
	}
	if c.AlertNtfyURL != "" || c.AlertNtfyToken != "" {
		t.Error("ntfy settings should default to empty")
	}
	// Both set: parsed.
	t.Setenv("ALERT_NTFY_URL", "https://ntfy.sh/me/capscope")
	t.Setenv("ALERT_NTFY_TOKEN", "tk_abc")
	t.Setenv("UPDATE_CHECK", "false")
	t.Setenv("UPDATE_CHECK_INTERVAL", "2h")
	if c, err = Load(); err != nil || c.AlertNtfyURL != "https://ntfy.sh/me/capscope" || c.AlertNtfyToken != "tk_abc" {
		t.Fatalf("ntfy cfg = %q err %v", c.AlertNtfyURL, err)
	}
	if c.UpdateCheck || c.UpdateCheckInterval != 2*time.Hour {
		t.Errorf("update cfg = %v/%v", c.UpdateCheck, c.UpdateCheckInterval)
	}
	// Token without a URL is refused; a bad URL is refused; short interval clamps.
	t.Setenv("ALERT_NTFY_URL", "")
	t.Setenv("UPDATE_CHECK_INTERVAL", "5m")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALERT_NTFY_TOKEN") {
		t.Errorf("want token-without-url error, got %v", err)
	}
	t.Setenv("ALERT_NTFY_URL", "sneaky://x")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ALERT_NTFY_URL") {
		t.Errorf("want bad ntfy url error, got %v", err)
	}
	t.Setenv("ALERT_NTFY_URL", "https://ntfy.sh/me")
	if c, err = Load(); err != nil || c.UpdateCheckInterval != time.Hour {
		t.Fatalf("want interval clamp to 1h, got %v err %v", c.UpdateCheckInterval, err)
	}
}

// Phase 4: agent config knobs.
func TestAgentConfig(t *testing.T) {
	t.Setenv("AUTH_PASS", "secret")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AgentHub != "" || c.AgentHostID != "" || c.DockerFilter != "" {
		t.Errorf("agent settings should default to empty: %+v", c)
	}
	t.Setenv("AGENT_HUB", "hub.example.com:9092")
	t.Setenv("AGENT_HOST_ID", "node-1")
	t.Setenv("AGENT_CA_CERT", "/certs/ca.crt")
	t.Setenv("AGENT_CERT", "/certs/agent.crt")
	t.Setenv("AGENT_KEY", "/certs/agent.key")
	t.Setenv("DOCKER_FILTER", "com.docker.swarm.node.id=abc,env=prod")
	if c, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AgentHub != "hub.example.com:9092" || c.AgentHostID != "node-1" {
		t.Errorf("agent hub/host = %q/%q", c.AgentHub, c.AgentHostID)
	}
	if c.AgentCACert != "/certs/ca.crt" || c.AgentCert != "/certs/agent.crt" || c.AgentKey != "/certs/agent.key" {
		t.Errorf("agent cert paths = %q/%q/%q", c.AgentCACert, c.AgentCert, c.AgentKey)
	}
	if c.DockerFilter != "com.docker.swarm.node.id=abc,env=prod" {
		t.Errorf("DockerFilter = %q", c.DockerFilter)
	}
}
