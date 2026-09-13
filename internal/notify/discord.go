package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Color int

const (
	ColorSuccess   Color = 0x00FF00
	ColorFailure   Color = 0xFF0000
	ColorRecovered Color = 0xFF8800
	ColorDryRun    Color = 0xFFFF00
)

type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeFailure   Outcome = "failure"
	OutcomeRecovered Outcome = "recovered"
	OutcomeDryRun    Outcome = "dry_run"
)

func (o Outcome) color() Color {
	switch o {
	case OutcomeSuccess:
		return ColorSuccess
	case OutcomeRecovered:
		return ColorRecovered
	case OutcomeDryRun:
		return ColorDryRun
	default:
		return ColorFailure
	}
}

func (o Outcome) icon() string {
	switch o {
	case OutcomeSuccess:
		return "✅"
	case OutcomeRecovered:
		return "⚠️"
	case OutcomeDryRun:
		return "🔍"
	default:
		return "❌"
	}
}

type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type Embed struct {
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Color       int     `json:"color"`
	Fields      []Field `json:"fields,omitempty"`
}

type Payload struct {
	Embeds []Embed `json:"embeds"`
}

type Report struct {
	Outcome      Outcome
	Title        string
	Commit       string
	RepoURL      string
	Branch       string
	Services     []string
	Updated      []string
	UpdatedKnown bool
	Restarted    []string
	Stacks       []string
	HealthWatch  time.Duration
	Duration     time.Duration
	Err          error
	StderrTail   string
}

const (
	maxErrorFieldLen = 500
	maxFieldValueLen = 1024
	maxTitleLen      = 256
	maxFields        = 25
)

func BuildEmbed(r Report) Embed {
	commit := r.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	commitValue := commit
	if r.RepoURL != "" && commit != "" {
		commitValue = fmt.Sprintf("[%s](%s/commit/%s)", commit, r.RepoURL, r.Commit)
	}

	services := "all"
	if len(r.Services) > 0 {
		services = strings.Join(r.Services, ", ")
	}

	fields := []Field{
		{Name: "Commit", Value: orDash(commitValue), Inline: true},
		{Name: "Branch", Value: orDash(r.Branch), Inline: true},
		{Name: "Services", Value: services, Inline: true},
	}
	if r.UpdatedKnown {
		updated := "none"
		if len(r.Updated) > 0 {
			updated = strings.Join(r.Updated, ", ")
		}
		fields = append(fields, Field{Name: "Updated", Value: updated, Inline: true})
	}
	if len(r.Restarted) > 0 {
		fields = append(fields, Field{Name: "Restarted", Value: strings.Join(r.Restarted, ", "), Inline: true})
	}
	if len(r.Stacks) > 0 {
		fields = append(fields, Field{Name: "Stacks", Value: strings.Join(r.Stacks, ", "), Inline: true})
	}
	if r.HealthWatch > 0 {
		fields = append(fields, Field{
			Name:   "Health Watch",
			Value:  fmt.Sprintf("%s (%s)", r.HealthWatch, r.Outcome),
			Inline: true,
		})
	}
	fields = append(fields, Field{Name: "Duration", Value: formatDuration(r.Duration), Inline: true})

	if r.Err != nil {
		fields = append(fields, Field{Name: "Error", Value: truncate(r.Err.Error(), maxErrorFieldLen)})
	}
	if r.StderrTail != "" {
		fields = append(fields, Field{Name: "Stderr", Value: truncate(r.StderrTail, maxErrorFieldLen)})
	}

	title := r.Outcome.icon()
	if r.Title != "" {
		title += " " + r.Title
	}

	if len(fields) > maxFields {
		fields = fields[:maxFields]
	}
	for i := range fields {
		fields[i].Name = truncate(fields[i].Name, maxFieldValueLen)
		fields[i].Value = orDash(truncate(fields[i].Value, maxFieldValueLen))
	}

	return Embed{
		Title:  truncate(title, maxTitleLen),
		Color:  int(r.Outcome.color()),
		Fields: fields,
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

const ellipsis = "…"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := max(n-len(ellipsis), 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

const (
	RepeatInterval   = time.Hour
	maxRateLimitWait = 15 * time.Second
	drainLimit       = 8 << 10
)

type Notifier struct {
	webhookURL string
	client     *http.Client
	log        *slog.Logger

	mu   sync.Mutex
	sent map[string]time.Time
}

func New(webhookURL string, log *slog.Logger) *Notifier {
	if webhookURL == "" {
		log.Info("discord notifications disabled: no webhook configured")
	}
	return &Notifier{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
		log:        log,
		sent:       make(map[string]time.Time),
	}
}

func (n *Notifier) Enabled() bool {
	return n.webhookURL != ""
}

func (n *Notifier) SendThrottled(ctx context.Context, key string, embed Embed) {
	if !n.Enabled() {
		return
	}
	if key == "" {
		n.reset()
	} else if !n.claim(key) {
		n.log.Info("discord notify: suppressing repeated notification", "key", key, "repeat_interval", RepeatInterval.String())
		return
	}
	if !n.send(ctx, embed) && key != "" {
		n.release(key)
	}
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

func (n *Notifier) Send(ctx context.Context, embed Embed) {
	n.send(ctx, embed)
}

func (n *Notifier) send(ctx context.Context, embed Embed) bool {
	if !n.Enabled() {
		return false
	}

	data, err := json.Marshal(Payload{Embeds: []Embed{embed}})
	if err != nil {
		n.log.Warn("discord notify: marshaling payload failed", "err", err)
		return false
	}

	for attempt := range 2 {
		retryAfter, err := n.post(ctx, data)
		if err != nil {
			n.log.Warn("discord notify: request failed", "err", err)
			return false
		}
		if retryAfter <= 0 {
			return true
		}
		if attempt > 0 {
			n.log.Warn("discord notify: still rate limited, dropping notification")
			return false
		}
		n.log.Warn("discord notify: rate limited, retrying", "retry_after", retryAfter.String())
		if !sleepCtx(ctx, retryAfter) {
			return false
		}
	}
	return false
}

func (n *Notifier) post(ctx context.Context, data []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(data))
	if err != nil {
		return 0, redactURL(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return 0, redactURL(err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusTooManyRequests {
		return rateLimitDelay(resp.Header.Get("Retry-After")), nil
	}
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("non-2xx response: %d", resp.StatusCode)
	}
	return 0, nil
}

func rateLimitDelay(retryAfter string) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSpace(retryAfter), 64)
	if err != nil || secs <= 0 {
		return time.Second
	}
	return min(time.Duration(secs*float64(time.Second)), maxRateLimitWait)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func redactURL(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}
