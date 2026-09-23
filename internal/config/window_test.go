package config

import (
	"log/slog"
	"testing"
	"time"
)

func at(hour, minute int) time.Time {
	return time.Date(2026, 9, 23, hour, minute, 0, 0, time.UTC)
}

func TestParseDeployWindow(t *testing.T) {
	for _, bad := range []string{"", "02:00", "2:00-5:00", "25:00-01:00", "01:60-02:00", "01:00-01:00", "ab:cd-ef:gh"} {
		if _, err := ParseDeployWindow(bad); err == nil {
			t.Errorf("ParseDeployWindow(%q) accepted, want an error", bad)
		}
	}
	w, err := ParseDeployWindow(" 02:00 - 05:30 ")
	if err != nil || w.String() != "02:00-05:30" {
		t.Errorf("ParseDeployWindow = %v, %v, want 02:00-05:30", w, err)
	}
}

func TestDeployWindowOpenAndNextOpening(t *testing.T) {
	night, _ := ParseDeployWindow("22:00-06:00")
	day, _ := ParseDeployWindow("02:00-05:00")
	cases := []struct {
		w    DeployWindow
		now  time.Time
		open bool
		next time.Time
	}{
		{day, at(1, 59), false, at(2, 0)},
		{day, at(2, 0), true, at(2, 0).AddDate(0, 0, 1)},
		{day, at(5, 0), false, at(2, 0).AddDate(0, 0, 1)},
		{night, at(23, 30), true, at(22, 0).AddDate(0, 0, 1)},
		{night, at(5, 59), true, at(22, 0)},
		{night, at(12, 0), false, at(22, 0)},
	}
	for _, c := range cases {
		if got := c.w.Open(c.now); got != c.open {
			t.Errorf("%s at %s: open = %v, want %v", c.w, c.now.Format("15:04"), got, c.open)
		}
		if got := c.w.NextOpening(c.now); !got.Equal(c.next) {
			t.Errorf("%s at %s: next opening = %s, want %s", c.w, c.now.Format("15:04"), got, c.next)
		}
	}
}

func TestConfigWithoutDeployWindowAllowsAnyTime(t *testing.T) {
	path := writeConfig(t, t.TempDir(), []byte(`{"repo_path": ".", "compose_file": "x", "project_name": "y"}`))
	cfg, err := Load(path, Overrides{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("loading a config without deploy_window: %v", err)
	}
	if _, ok := cfg.Window(); ok {
		t.Errorf("deploy window enabled for a config that never set one: %q", cfg.DeployWindow)
	}
}
