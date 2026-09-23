package reconcile

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/teyhouse/ComposeLock/internal/notify"
	"github.com/teyhouse/ComposeLock/internal/state"
)

func TestReconcileNotificationCarriesCommitContext(t *testing.T) {
	g := gitChange("aaa111", "bbb222")
	g.subjects = map[string]string{"bbb222": "bump vaultwarden to 1.34.1"}
	g.remoteURL = "git@github.com:teyhouse/ComposeLock.git"
	deps, _ := testDeps(t, g, &fakeCompose{}, snapshots(healthySnapshot()), state.New())
	deps.Notifier = notify.New("http://127.0.0.1:1/", slog.New(slog.DiscardHandler))

	result := Reconcile(t.Context(), Options{Trigger: "cli"}, deps)
	if result.Notification == nil {
		t.Fatalf("no notification, result = %+v", result)
	}

	want := "bump vaultwarden to 1.34.1\nteyhouse · 3 commits since aaa111 · [compare](https://github.com/teyhouse/ComposeLock/compare/aaa111...bbb222)"
	if got := result.Notification.Description; got != want {
		t.Errorf("Description =\n%s\nwant\n%s", got, want)
	}
	for _, f := range result.Notification.Fields {
		if f.Name == "Commit" && !strings.HasPrefix(f.Value, "[bbb222](https://github.com/teyhouse/ComposeLock/commit/bbb222)") {
			t.Errorf("Commit field = %q, want a link", f.Value)
		}
	}
}
