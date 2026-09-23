package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	timeout    = 10 * time.Second
	drainLimit = 8 << 10
)

type Pinger struct {
	url    string
	client *http.Client
	log    *slog.Logger
}

func New(pingURL string, log *slog.Logger) *Pinger {
	if pingURL == "" {
		return nil
	}
	return &Pinger{url: pingURL, client: &http.Client{Timeout: timeout}, log: log}
}

func (p *Pinger) Ping(ctx context.Context) {
	if p == nil {
		return
	}
	if err := p.ping(ctx); err != nil && ctx.Err() == nil {
		p.log.Warn("heartbeat ping failed", "err", err)
	}
}

func (p *Pinger) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return redactURL(err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return redactURL(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

func redactURL(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}
