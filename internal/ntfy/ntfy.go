// Package ntfy publishes phone notifications to an ntfy server
// (https://docs.ntfy.sh/publish/). It knows nothing about sessions: callers
// compose the title and body.
//
// Credentials stay out of every error this package returns: they travel only
// in the Authorization header, and the server URL is validated by config to
// carry no userinfo.
package ntfy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/config"
)

// publishTimeout bounds one publication, so an unreachable server fails the
// agent's call promptly instead of hanging it.
const publishTimeout = 15 * time.Second

// Priority is ntfy's 1 (min) to 5 (max) scale.
type Priority int

const (
	PriorityDefault Priority = 3
	PriorityHigh    Priority = 4
)

// Message is one notification.
type Message struct {
	Title    string
	Body     string
	Priority Priority
	Tags     []string
}

// Publisher sends messages to one configured server and topic.
type Publisher struct {
	cfg    config.NtfyConfig
	client *http.Client
}

// New returns a publisher for cfg, which config has already validated. A nil
// client uses one with publishTimeout.
func New(cfg config.NtfyConfig, client *http.Client) *Publisher {
	if client == nil {
		client = &http.Client{Timeout: publishTimeout}
	}
	return &Publisher{cfg: cfg, client: client}
}

// Server is the configured server URL, safe to show: it carries no
// credentials.
func (p *Publisher) Server() string { return p.cfg.Server }

// publishBody is ntfy's JSON publishing format, posted to the server root.
type publishBody struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Priority Priority `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// Publish returns nil once the server accepted the message (HTTP 2xx). That
// is publication, not proof a phone received it.
func (p *Publisher) Publish(ctx context.Context, msg Message) error {
	payload, err := json.Marshal(publishBody{
		Topic:    p.cfg.Topic,
		Title:    msg.Title,
		Message:  msg.Body,
		Priority: msg.Priority,
		Tags:     msg.Tags,
	})
	if err != nil {
		return fmt.Errorf("encode ntfy message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Server+"/", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build ntfy request to %s: %w", p.cfg.Server, err)
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case p.cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	case p.cfg.Username != "":
		req.SetBasicAuth(p.cfg.Username, p.cfg.Password)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy server %s unreachable: %w", p.cfg.Server, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &StatusError{Server: p.cfg.Server, StatusCode: resp.StatusCode, Detail: serverDetail(body)}
}

// StatusError is a response the server refused.
type StatusError struct {
	Server     string
	StatusCode int
	Detail     string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("ntfy server %s refused the notification: HTTP %d", e.Server, e.StatusCode)
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " (check notifications.ntfy token or username/password and the topic's access rights)"
	case http.StatusTooManyRequests:
		msg += " (rate limited by the server)"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// serverDetail extracts ntfy's JSON error text, or the trimmed raw body.
func serverDetail(body []byte) string {
	var parsed struct {
		Error string `json:"error"`
		Link  string `json:"link"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		if parsed.Link != "" {
			return parsed.Error + " (" + parsed.Link + ")"
		}
		return parsed.Error
	}
	return strings.TrimSpace(string(body))
}
