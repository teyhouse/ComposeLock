package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	Outcome     Outcome
	Title       string
	Commit      string
	RepoURL     string
	Branch      string
	Services    []string
	HealthWatch time.Duration
	Duration    time.Duration
	Err         error
	StderrTail  string
}

const maxErrorFieldLen = 500

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
	if r.HealthWatch > 0 {
		fields = append(fields, Field{
			Name:   "Health Watch",
			Value:  fmt.Sprintf("%s (%s)", r.HealthWatch, r.Outcome),
			Inline: true,
		})
	}
	fields = append(fields, Field{Name: "Duration", Value: r.Duration.String(), Inline: true})

	if r.Err != nil {
		fields = append(fields, Field{Name: "Error", Value: truncate(r.Err.Error(), maxErrorFieldLen)})
	}
	if r.StderrTail != "" {
		fields = append(fields, Field{Name: "Stderr", Value: truncate(r.StderrTail, maxErrorFieldLen)})
	}

	return Embed{
		Title:  r.Title,
		Color:  int(r.Outcome.color()),
		Fields: fields,
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type Notifier struct {
	webhookURL string
	client     *http.Client
	log        *slog.Logger
}

func New(webhookURL string, log *slog.Logger) *Notifier {
	if webhookURL == "" {
		log.Info("discord notifications disabled: no webhook configured")
	}
	return &Notifier{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
		log:        log,
	}
}

func (n *Notifier) Enabled() bool {
	return n.webhookURL != ""
}

func (n *Notifier) Send(ctx context.Context, embed Embed) {
	if !n.Enabled() {
		return
	}

	data, err := json.Marshal(Payload{Embeds: []Embed{embed}})
	if err != nil {
		n.log.Warn("discord notify: marshaling payload failed", "err", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(data))
	if err != nil {
		n.log.Warn("discord notify: building request failed", "err", redactURL(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		n.log.Warn("discord notify: request failed", "err", redactURL(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		n.log.Warn("discord notify: non-2xx response", "status", resp.StatusCode)
	}
}

func redactURL(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}
