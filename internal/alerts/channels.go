package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// refusedEnvPrefixes are environment names a ${VAR} placeholder may never
// reference, matching Dozzle's guard: a commit-me-able rules/channels config
// must not be able to pull the host's cloud or container credentials.
var refusedEnvPrefixes = []string{"AWS_", "DOCKER_", "KUBERNETES_", "DOZZLE_"}

// expandEnv replaces ${VAR} placeholders (and bare $VAR) with environment
// values. It is evaluated at send time so secrets can rotate without a
// restart, and refuses protected prefixes instead of silently expanding them.
func expandEnv(s string) (string, error) {
	var refused string
	out := os.Expand(s, func(k string) string {
		for _, p := range refusedEnvPrefixes {
			if strings.HasPrefix(strings.ToUpper(k), p) {
				refused = k
				return ""
			}
		}
		return os.Getenv(k)
	})
	if refused != "" {
		return "", fmt.Errorf("alerts: ${%s} uses a refused environment prefix (aws_/docker_/kubernetes_/dozzle_)", refused)
	}
	return out, nil
}

// Channel is one notification destination, configured by environment
// variables (see config). All send one plain-text message.
type Channel struct {
	Name   string // display name, e.g. "telegram"
	Send   func(ctx context.Context, msg string) error
	Client *http.Client
}

const notifyTimeout = 10 * time.Second

func httpClient() *http.Client { return &http.Client{Timeout: notifyTimeout} }

// postJSON posts body as JSON and tolerates a non-2xx by reading a snippet.
func postJSON(ctx context.Context, c *http.Client, endpoint string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("%s: HTTP %d: %s", endpoint, resp.StatusCode, snippet)
	}
	return nil
}

// NewWebhook sends {"text": msg} to any JSON endpoint (generic webhook).
// The endpoint may contain ${ENV} placeholders, expanded per send.
func NewWebhook(endpoint string) Channel {
	c := httpClient()
	return Channel{
		Name:   "webhook",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			ep, err := expandEnv(endpoint)
			if err != nil {
				return err
			}
			return postJSON(ctx, c, ep, map[string]string{"text": msg})
		},
	}
}

// NewNtfy posts the message as plain text to an ntfy topic URL, e.g.
// https://ntfy.sh/my-topic. Alert title and priority ride in headers.
// An access token, when set, becomes a Bearer authorization.
func NewNtfy(endpoint, token string) Channel {
	c := httpClient()
	return Channel{
		Name:   "ntfy",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			ep, err := expandEnv(endpoint)
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, strings.NewReader(msg))
			if err != nil {
				return err
			}
			req.Header.Set("X-Title", "CapScope alert")
			req.Header.Set("X-Priority", "high")
			if tk, err := expandEnv(token); err == nil && tk != "" {
				req.Header.Set("Authorization", "Bearer "+tk)
			} else if err != nil {
				return err
			}
			resp, err := c.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
				return fmt.Errorf("%s: HTTP %d: %s", ep, resp.StatusCode, snippet)
			}
			return nil
		},
	}
}

// NewTelegram calls the Bot API sendMessage. The token may use ${ENV}.
func NewTelegram(token, chatID string) Channel {
	c := httpClient()
	return Channel{
		Name:   "telegram",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			tk, err := expandEnv(token)
			if err != nil {
				return err
			}
			bot := "https://api.telegram.org/bot" + tk + "/sendMessage"
			chat, err := expandEnv(chatID)
			if err != nil {
				return err
			}
			return postJSON(ctx, c, bot, map[string]string{"chat_id": chat, "text": msg})
		},
	}
}

// NewSlack posts an incoming-webhook payload. The URL may use ${ENV}.
func NewSlack(webhookURL string) Channel {
	c := httpClient()
	return Channel{
		Name:   "slack",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			ep, err := expandEnv(webhookURL)
			if err != nil {
				return err
			}
			return postJSON(ctx, c, ep, map[string]string{"text": msg})
		},
	}
}

// NewDiscord posts a webhook payload (content limit is 2000 chars, trimmed).
// The URL may use ${ENV}.
func NewDiscord(webhookURL string) Channel {
	c := httpClient()
	return Channel{
		Name:   "discord",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			ep, err := expandEnv(webhookURL)
			if err != nil {
				return err
			}
			if len(msg) > 1900 {
				msg = msg[:1900] + "…"
			}
			return postJSON(ctx, c, ep, map[string]string{"content": msg})
		},
	}
}

// BuildChannels constructs the configured channels from raw settings; empty
// entries are skipped. Used by main and by tests (payload shape check).
func BuildChannels(webhookURL, telegramToken, telegramChat, slackURL, discordURL, ntfyURL, ntfyToken string) []Channel {
	var out []Channel
	if webhookURL != "" {
		out = append(out, NewWebhook(webhookURL))
	}
	if telegramToken != "" && telegramChat != "" {
		out = append(out, NewTelegram(telegramToken, telegramChat))
	}
	if slackURL != "" {
		out = append(out, NewSlack(slackURL))
	}
	if discordURL != "" {
		out = append(out, NewDiscord(discordURL))
	}
	if ntfyURL != "" {
		out = append(out, NewNtfy(ntfyURL, ntfyToken))
	}
	return out
}
