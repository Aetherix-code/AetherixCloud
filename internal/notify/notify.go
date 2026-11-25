// Package notify delivers cluster events to webhooks, Slack and Discord.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/events"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
)

const (
	channelPrefix = "notify/channel/"
	Path          = "/api/notifications"
)

// Kind of notification channel.
const (
	KindWebhook = "webhook"
	KindSlack   = "slack"
	KindDiscord = "discord"
)

// Channel is a notification destination.
type Channel struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"` // webhook|slack|discord
	URL          string   `json:"url"`
	Enabled      bool     `json:"enabled"`
	EventPrefix  string   `json:"event_prefix"` // "" = all; e.g. "deployment." "backup."
	EventFilter  []string `json:"event_filter,omitempty"` // exact types; overrides prefix when set
}

// Dispatcher watches the event stream and fans out to channels.
type Dispatcher struct {
	Store  store.Store
	Logger *slog.Logger
	Client *http.Client
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(st store.Store, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{Store: st, Logger: log, Client: &http.Client{Timeout: 10 * time.Second}}
}

// Run watches events until ctx is done.
func (d *Dispatcher) Run(ctx context.Context) {
	ch, err := events.Watch(ctx, d.Store, "", 0)
	if err != nil {
		d.Logger.Warn("notify watch failed", "err", err)
		return
	}
	for e := range ch {
		channels, err := ListChannels(ctx, d.Store)
		if err != nil {
			continue
		}
		for _, c := range channels {
			if !c.Enabled || !match(c, e.Type) {
				continue
			}
			if err := d.deliver(ctx, c, e.Type, e.Message, e.SubjectId, e.Attributes); err != nil {
				d.Logger.Warn("notify deliver failed", "channel", c.ID, "err", err)
			}
		}
	}
}

func match(c Channel, typ string) bool {
	if len(c.EventFilter) > 0 {
		for _, f := range c.EventFilter {
			if f == typ {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(typ, c.EventPrefix)
}

func (d *Dispatcher) deliver(ctx context.Context, c Channel, typ, msg, subject string, attrs map[string]string) error {
	switch c.Kind {
	case KindSlack:
		body := map[string]any{
			"text": fmt.Sprintf("*%s*: %s", typ, msg),
			"blocks": []map[string]any{{
				"type": "section",
				"text": map[string]string{"type": "mrkdwn", "text": fmt.Sprintf("*%s*\n%s\n`%s`", typ, msg, subject)},
			}},
		}
		return d.postJSON(ctx, c.URL, body)
	case KindDiscord:
		body := map[string]any{
			"content": fmt.Sprintf("**%s**: %s (`%s`)", typ, msg, subject),
		}
		return d.postJSON(ctx, c.URL, body)
	default: // webhook
		body := map[string]any{
			"type": typ, "message": msg, "subject_id": subject, "attributes": attrs,
			"channel": c.Name, "time": time.Now().UTC().Format(time.RFC3339),
		}
		return d.postJSON(ctx, c.URL, body)
	}
}

func (d *Dispatcher) postJSON(ctx context.Context, url string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AetherixCloud-Notify/1")
	res, err := d.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1024))
	if res.StatusCode >= 300 {
		return fmt.Errorf("webhook status %s", res.Status)
	}
	return nil
}

// ListChannels returns notification channels.
func ListChannels(ctx context.Context, st store.Store) ([]Channel, error) {
	recs, err := st.List(ctx, store.TableSettings, store.Query{})
	if err != nil {
		return nil, err
	}
	var out []Channel
	for _, r := range recs {
		if !strings.HasPrefix(r.ID, channelPrefix) {
			continue
		}
		var c Channel
		if json.Unmarshal(r.Data, &c) != nil {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// PutChannel upserts a channel.
func PutChannel(ctx context.Context, st store.Store, c Channel) error {
	if c.ID == "" {
		c.ID = ids.New()
	}
	if c.Kind == "" {
		c.Kind = KindWebhook
	}
	return store.PutJSON(ctx, st, store.TableSettings, channelPrefix+c.ID, nil, c)
}

// DeleteChannel removes a channel.
func DeleteChannel(ctx context.Context, st store.Store, id string) error {
	return st.Delete(ctx, store.TableSettings, channelPrefix+id)
}

// API serves /api/notifications.
type API struct {
	Store        store.Store
	Authenticate func(*http.Request) error
}

func (a *API) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Authenticate != nil {
			if err := a.Authenticate(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		path := strings.TrimPrefix(r.URL.Path, Path)
		path = strings.Trim(path, "/")
		switch {
		case path == "" && r.Method == http.MethodGet:
			chs, err := ListChannels(r.Context(), a.Store)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			// redact URLs partially
			for i := range chs {
				chs[i].URL = redactURL(chs[i].URL)
			}
			writeJSON(w, map[string]any{"channels": chs})
		case path == "" && r.Method == http.MethodPost:
			var c Channel
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if c.URL == "" {
				http.Error(w, "url required", 400)
				return
			}
			c.Enabled = true
			if err := PutChannel(r.Context(), a.Store, c); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			c.URL = redactURL(c.URL)
			writeJSON(w, c)
		case path != "" && r.Method == http.MethodDelete:
			if err := DeleteChannel(r.Context(), a.Store, path); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
}

func redactURL(u string) string {
	if len(u) < 12 {
		return "••••"
	}
	return u[:8] + "…" + u[len(u)-4:]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
