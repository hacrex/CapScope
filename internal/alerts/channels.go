package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

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
func NewWebhook(endpoint string) Channel {
	c := httpClient()
	return Channel{
		Name:   "webhook",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			return postJSON(ctx, c, endpoint, map[string]string{"text": msg})
		},
	}
}

// NewTelegram calls the Bot API sendMessage.
func NewTelegram(token, chatID string) Channel {
	c := httpClient()
	endpoint := "https://api.telegram.org/bot" + token + "/sendMessage"
	return Channel{
		Name:   "telegram",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			return postJSON(ctx, c, endpoint, map[string]string{"chat_id": chatID, "text": msg})
		},
	}
}

// NewSlack posts an incoming-webhook payload.
func NewSlack(webhookURL string) Channel {
	c := httpClient()
	return Channel{
		Name:   "slack",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			return postJSON(ctx, c, webhookURL, map[string]string{"text": msg})
		},
	}
}

// NewDiscord posts a webhook payload (content limit is 2000 chars, trimmed).
func NewDiscord(webhookURL string) Channel {
	c := httpClient()
	return Channel{
		Name:   "discord",
		Client: c,
		Send: func(ctx context.Context, msg string) error {
			if len(msg) > 1900 {
				msg = msg[:1900] + "…"
			}
			return postJSON(ctx, c, webhookURL, map[string]string{"content": msg})
		},
	}
}

// BuildChannels constructs the configured channels from raw settings; empty
// entries are skipped. Used by main and by tests (payload shape check).
func BuildChannels(webhookURL, telegramToken, telegramChat, slackURL, discordURL string) []Channel {
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
	return out
}
