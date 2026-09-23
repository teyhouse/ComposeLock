package notify

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type sink struct {
	name    string
	url     string
	headers map[string]string
	encode  func(Embed) ([]byte, error)
}

type Notifier struct {
	sinks  []sink
	client *http.Client
	log    *slog.Logger

	mu   sync.Mutex
	sent map[string]time.Time
}

type Option func(*Notifier)

func WithWebhook(webhookURL string, headers map[string]string) Option {
	return func(n *Notifier) {
		if webhookURL == "" {
			return
		}
		n.sinks = append(n.sinks, sink{name: "webhook", url: webhookURL, headers: maps.Clone(headers), encode: encodeWebhook})
	}
}

func New(discordURL string, log *slog.Logger, opts ...Option) *Notifier {
	n := &Notifier{
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log,
		sent:   make(map[string]time.Time),
	}
	if discordURL != "" {
		n.sinks = append(n.sinks, sink{name: "discord", url: discordURL, encode: encodeDiscord})
	}
	for _, opt := range opts {
		opt(n)
	}
	if len(n.sinks) == 0 {
		log.Info("notifications disabled: no discord_webhook or notify_webhook configured")
	}
	return n
}

func (n *Notifier) Enabled() bool {
	return len(n.sinks) > 0
}

func (n *Notifier) SendThrottled(ctx context.Context, key string, embed Embed) bool {
	if key == "" {
		n.reset()
		return n.Send(ctx, embed)
	}
	delivered := true
	for _, s := range n.sinks {
		sinkKey := s.name + ":" + key
		if !n.claim(sinkKey) {
			n.log.Info("notify: suppressing repeated notification", "sink", s.name, "key", key, "repeat_interval", RepeatInterval.String())
			continue
		}
		if !n.deliver(ctx, s, embed) {
			n.release(sinkKey)
			delivered = false
		}
	}
	return delivered
}

func (n *Notifier) Send(ctx context.Context, embed Embed) bool {
	if !n.Enabled() {
		return false
	}
	delivered := true
	for _, s := range n.sinks {
		if !n.deliver(ctx, s, embed) {
			delivered = false
		}
	}
	return delivered
}

func (n *Notifier) reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	clear(n.sent)
}

func (n *Notifier) claim(key string) bool {
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, at := range n.sent {
		if now.Sub(at) >= RepeatInterval {
			delete(n.sent, k)
		}
	}
	if _, blocked := n.sent[key]; blocked {
		return false
	}
	n.sent[key] = now
	return true
}

func (n *Notifier) release(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.sent, key)
}

func (n *Notifier) deliver(ctx context.Context, s sink, embed Embed) bool {
	data, err := s.encode(embed)
	if err != nil {
		n.log.Warn("notify: marshaling payload failed", "sink", s.name, "err", err)
		return false
	}
	for attempt := range 2 {
		retryAfter, err := n.post(ctx, s, data)
		if err != nil {
			n.log.Warn("notify: request failed", "sink", s.name, "err", err)
			return false
		}
		if retryAfter <= 0 {
			return true
		}
		if attempt > 0 {
			n.log.Warn("notify: still rate limited, dropping notification", "sink", s.name)
			return false
		}
		n.log.Warn("notify: rate limited, retrying", "sink", s.name, "retry_after", retryAfter.String())
		if !sleepCtx(ctx, retryAfter) {
			return false
		}
	}
	return false
}

func (n *Notifier) post(ctx context.Context, s sink, data []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(data))
	if err != nil {
		return 0, redactURL(err)
	}
	for name, value := range s.headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "composelock")

	resp, err := n.client.Do(req)
	if err != nil {
		return 0, redactURL(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusTooManyRequests {
		return rateLimitDelay(resp.Header.Get("Retry-After")), nil
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("non-2xx response: %d", resp.StatusCode)
	}
	return 0, nil
}

func encodeDiscord(embed Embed) ([]byte, error) {
	return json.Marshal(Payload{Embeds: []Embed{embed}})
}

const webhookSchemaVersion = 1

type WebhookPayload struct {
	SchemaVersion int               `json:"schema_version"`
	Source        string            `json:"source"`
	Status        Outcome           `json:"status"`
	Title         string            `json:"title"`
	Message       string            `json:"message,omitempty"`
	Commit        string            `json:"commit,omitempty"`
	CommitURL     string            `json:"commit_url,omitempty"`
	CompareURL    string            `json:"compare_url,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"`
	Timestamp     time.Time         `json:"timestamp"`
}

func encodeWebhook(embed Embed) ([]byte, error) {
	return json.Marshal(webhookPayload(embed, time.Now().UTC()))
}

func webhookPayload(embed Embed, now time.Time) WebhookPayload {
	fields := make(map[string]string, len(embed.Fields))
	for _, f := range embed.Fields {
		fields[f.Name] = plainText(f.Value)
	}
	return WebhookPayload{
		SchemaVersion: webhookSchemaVersion,
		Source:        "composelock",
		Status:        embed.outcome,
		Title:         embed.title,
		Message:       cmp.Or(embed.message, plainText(embed.Description)),
		Commit:        embed.commit,
		CommitURL:     embed.commitURL,
		CompareURL:    embed.compareURL,
		Fields:        fields,
		Timestamp:     now,
	}
}

var markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

func plainText(s string) string {
	s = markdownLink.ReplaceAllString(s, "$1")
	return strings.NewReplacer("**", "", "`", "").Replace(s)
}
