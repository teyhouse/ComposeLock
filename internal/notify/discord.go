package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
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

	outcome    Outcome
	title      string
	commit     string
	message    string
	commitURL  string
	compareURL string
}

func (e *Embed) Commit() string {
	return e.commit
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
		Title:   truncate(title, maxTitleLen),
		Color:   int(r.Outcome.color()),
		Fields:  fields,
		outcome: r.Outcome,
		title:   r.Title,
		commit:  r.Commit,
	}
}

type Alert struct {
	Stack        string
	Commit       string
	Branch       string
	Problem      string
	UnhealthyFor time.Duration
}

func BuildAlertEmbed(a Alert) Embed {
	commit := a.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	fields := []Field{
		{Name: "Stack", Value: orDash(truncate(a.Stack, maxFieldValueLen)), Inline: true},
		{Name: "Commit", Value: orDash(commit), Inline: true},
		{Name: "Branch", Value: orDash(truncate(a.Branch, maxFieldValueLen)), Inline: true},
		{Name: "Unhealthy for", Value: formatDuration(a.UnhealthyFor), Inline: true},
		{Name: "Problem", Value: orDash(truncate(a.Problem, maxErrorFieldLen))},
	}
	return Embed{
		Title:   truncate(OutcomeFailure.icon()+" Stack unhealthy: "+a.Stack, maxTitleLen),
		Color:   int(ColorFailure),
		Fields:  fields,
		outcome: OutcomeFailure,
		title:   "Stack unhealthy: " + a.Stack,
		commit:  a.Commit,
	}
}

type CommitInfo struct {
	ID      string
	Subject string
	Author  string
}

type CommitContext struct {
	RepoURL  string
	Commit   CommitInfo
	Base     string
	Count    int
	Rollback *CommitInfo
}

const (
	maxSubjectLen = 100
	maxAuthorLen  = 64
)

func (e *Embed) AddCommitContext(c CommitContext) {
	if c.Commit.ID == "" {
		return
	}
	var lines []string
	if c.Rollback != nil {
		lines = append(lines,
			"**Failed:** "+c.describe(c.Commit),
			"**Rollback target:** "+c.describe(*c.Rollback),
		)
	} else if c.Commit.Subject != "" {
		lines = append(lines, truncate(c.Commit.Subject, maxSubjectLen))
	}

	var meta []string
	if c.Rollback == nil && c.Commit.Author != "" {
		meta = append(meta, truncate(c.Commit.Author, maxAuthorLen))
	}
	if c.Rollback == nil && c.Count > 1 {
		meta = append(meta, fmt.Sprintf("%d commits since %s", c.Count, shortHash(c.Base)))
	}
	plain := slices.Clone(lines)
	if len(meta) > 0 {
		plain = append(plain, strings.Join(meta, " · "))
	}
	e.message = plainText(strings.Join(plain, "\n"))

	if c.RepoURL != "" && c.Base != "" && c.Base != c.Commit.ID {
		e.compareURL = fmt.Sprintf("%s/compare/%s...%s", c.RepoURL, c.Base, c.Commit.ID)
		meta = append(meta, "[compare]("+e.compareURL+")")
	}
	if len(meta) > 0 {
		lines = append(lines, strings.Join(meta, " · "))
	}
	e.Description = strings.Join(lines, "\n")

	if c.RepoURL == "" {
		return
	}
	e.commitURL = c.RepoURL + "/commit/" + c.Commit.ID
	for i := range e.Fields {
		if e.Fields[i].Name == "Commit" && e.Fields[i].Value == shortHash(c.Commit.ID) {
			e.Fields[i].Value = c.link(c.Commit.ID)
		}
	}
}

func (c CommitContext) describe(info CommitInfo) string {
	out := "`" + shortHash(info.ID) + "`"
	if c.RepoURL != "" {
		out = c.link(info.ID)
	}
	if info.Subject != "" {
		out += " " + truncate(info.Subject, maxSubjectLen)
	}
	if info.Author != "" {
		out += " (" + truncate(info.Author, maxAuthorLen) + ")"
	}
	return out
}

func (c CommitContext) link(id string) string {
	return fmt.Sprintf("[%s](%s/commit/%s)", shortHash(id), c.RepoURL, id)
}

func shortHash(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
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
