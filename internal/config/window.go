package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type DeployWindow struct {
	start, end int
}

func ParseDeployWindow(s string) (DeployWindow, error) {
	from, to, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return DeployWindow{}, fmt.Errorf("deploy_window must look like 02:00-05:00, got %q", s)
	}
	start, err := parseClock(from)
	if err != nil {
		return DeployWindow{}, fmt.Errorf("deploy_window start: %w", err)
	}
	end, err := parseClock(to)
	if err != nil {
		return DeployWindow{}, fmt.Errorf("deploy_window end: %w", err)
	}
	if start == end {
		return DeployWindow{}, fmt.Errorf("deploy_window %q starts and ends at the same time; leave it empty to allow deploys at any time", s)
	}
	return DeployWindow{start: start, end: end}, nil
}

func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || len(h) != 2 || len(m) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	hour, errH := strconv.Atoi(h)
	minute, errM := strconv.Atoi(m)
	if errH != nil || errM != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("%q is not a time of day between 00:00 and 23:59", s)
	}
	return hour*60 + minute, nil
}

func (w DeployWindow) Open(t time.Time) bool {
	now := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return now >= w.start && now < w.end
	}
	return now >= w.start || now < w.end
}

func (w DeployWindow) NextOpening(t time.Time) time.Time {
	opening := time.Date(t.Year(), t.Month(), t.Day(), w.start/60, w.start%60, 0, 0, t.Location())
	if !opening.After(t) {
		opening = opening.AddDate(0, 0, 1)
	}
	return opening
}

func (w DeployWindow) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.start/60, w.start%60, w.end/60, w.end%60)
}

func (c *Config) Window() (DeployWindow, bool) {
	if c.DeployWindow == "" {
		return DeployWindow{}, false
	}
	w, err := ParseDeployWindow(c.DeployWindow)
	return w, err == nil
}
