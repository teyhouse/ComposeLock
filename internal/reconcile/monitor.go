package reconcile

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/teyhouse/ComposeLock/internal/health"
	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

type stackMemory struct {
	services map[string]struct{}
	restarts map[string]int
	streak   int
	since    time.Time
	alerted  bool
}

type Monitor struct {
	deps   Deps
	key    string
	stacks map[string]*stackMemory
}

func NewMonitor(deps Deps) *Monitor {
	return &Monitor{deps: deps, stacks: make(map[string]*stackMemory)}
}

func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	m.deps.Log.Info("health monitor started", "interval", interval.String(), "alert_after", m.deps.Config.HealthUnhealthyStreak)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Check(ctx)
		}
	}
}

type stackSnapshot struct {
	name string
	snap health.Snapshot
}

func (m *Monitor) Check(ctx context.Context) {
	if m.check(ctx) {
		m.deps.Heartbeat.Ping(ctx)
	}
}

func (m *Monitor) check(ctx context.Context) (completed bool) {
	activity := m.deps.activity()
	before := activity.Load()
	if before%2 == 1 {
		return false
	}
	st, err := m.deps.State.Load()
	if err != nil {
		m.deps.Log.Warn("health monitor: loading state", "err", err)
		return true
	}
	if st.PendingCommit != "" || st.PendingRevert || st.LastHealthyCommit == "" || st.Paused(m.deps.Clock.Now()) {
		return true
	}
	names, err := m.watched(st)
	if err != nil {
		m.deps.Log.Warn("health monitor: discovering compose stacks", "err", err)
		return true
	}
	if key := st.LastHealthyCommit + "|" + st.LastCheckoutCommit + "|" + strings.Join(names, ","); key != m.key {
		m.key = key
		clear(m.stacks)
	}

	snaps := make([]stackSnapshot, 0, len(names))
	for _, name := range names {
		snap, err := m.deps.Health.Snapshot(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			m.deps.Log.Warn("health monitor: snapshot failed", "project", name, "err", err)
			continue
		}
		snaps = append(snaps, stackSnapshot{name: name, snap: snap})
	}
	if activity.Load() != before {
		return false
	}

	now := m.deps.Clock.Now()
	for _, s := range snaps {
		m.observe(ctx, st, s.name, s.snap, now)
	}
	return true
}

func (m *Monitor) watched(st *state.State) ([]string, error) {
	if len(st.LastHealthyStacks) > 0 {
		return st.LastHealthyStacks, nil
	}
	stacks, err := StacksFor(m.deps.Config, m.deps.Log)
	if err != nil {
		return nil, err
	}
	return stackNames(stacks), nil
}

func (m *Monitor) observe(ctx context.Context, st *state.State, name string, snap health.Snapshot, now time.Time) {
	mem := m.stacks[name]
	if mem == nil {
		mem = &stackMemory{services: make(map[string]struct{}), restarts: make(map[string]int)}
		m.stacks[name] = mem
	}
	problem := mem.problem(snap)
	mem.remember(snap)

	if problem == "" {
		if mem.streak > 0 {
			m.deps.Log.Info("health monitor: stack healthy again", "project", name)
		}
		mem.streak, mem.alerted = 0, false
		return
	}
	if mem.streak == 0 {
		mem.since = now
		m.deps.Log.Warn("health monitor: stack unhealthy", "project", name, "problem", problem)
	}
	mem.streak++
	if mem.alerted || mem.streak < m.deps.Config.HealthUnhealthyStreak {
		return
	}

	embed := notify.BuildAlertEmbed(notify.Alert{
		Stack:        name,
		Commit:       st.LastHealthyCommit,
		Branch:       m.deps.Config.Branch,
		Problem:      problem,
		UnhealthyFor: now.Sub(mem.since),
	})
	addCommitContext(ctx, m.deps, &embed, "", "")
	if !m.deps.Notifier.Enabled() {
		mem.alerted = true
		return
	}
	if mem.alerted = m.deps.Notifier.Send(ctx, embed); mem.alerted {
		m.deps.Log.Info("health monitor: alert sent", "project", name, "problem", problem)
	}
}

func (mem *stackMemory) problem(snap health.Snapshot) string {
	if len(snap.Containers) == 0 {
		return "no containers"
	}
	present := make(map[string]struct{}, len(snap.Containers))
	var restarted []string
	for _, c := range snap.Containers {
		present[c.Service] = struct{}{}
		if prev, ok := mem.restarts[c.ID]; ok && c.RestartCount > prev {
			restarted = append(restarted, c.Service)
		}
	}
	var missing []string
	for s := range mem.services {
		if _, ok := present[s]; !ok {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return "no containers for: " + strings.Join(missing, ", ")
	}
	if healthy, reason := health.EvaluatePreflight(snap); !healthy {
		return reason
	}
	if len(restarted) > 0 {
		slices.Sort(restarted)
		return "restarted since last check: " + strings.Join(slices.Compact(restarted), ", ")
	}
	return ""
}

func (mem *stackMemory) remember(snap health.Snapshot) {
	clear(mem.restarts)
	for _, c := range snap.Containers {
		mem.services[c.Service] = struct{}{}
		mem.restarts[c.ID] = c.RestartCount
	}
}
