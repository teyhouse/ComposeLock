package reconcile

import (
	"context"
	"strings"
	"time"

	"github.com/teyhouse/ComposeLock/internal/git"
	"github.com/teyhouse/ComposeLock/internal/notify"
)

const commitContextTimeout = 10 * time.Second

func addCommitContext(ctx context.Context, deps Deps, embed *notify.Embed, base, rollback string) {
	commit := embed.Commit()
	if commit == "" || deps.Git == nil || !deps.Notifier.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitContextTimeout)
	defer cancel()
	info, err := describeCommit(ctx, deps, commit)
	if err != nil {
		deps.Log.Debug("notification: commit context unavailable", "commit", commit, "err", err)
		return
	}
	c := notify.CommitContext{RepoURL: repoWebURL(ctx, deps), Commit: info}
	if rollback != "" && rollback != commit {
		if r, err := describeCommit(ctx, deps, rollback); err == nil {
			c.Rollback = &r
			c.Base = rollback
		}
	} else if base != "" && base != commit {
		c.Base = base
		if n, err := deps.Git.CountCommits(ctx, base, commit); err == nil {
			c.Count = n
		}
	}
	embed.AddCommitContext(c)
}

func describeCommit(ctx context.Context, deps Deps, commit string) (notify.CommitInfo, error) {
	subject, author, err := deps.Git.Describe(ctx, commit)
	if err != nil {
		return notify.CommitInfo{}, err
	}
	return notify.CommitInfo{ID: commit, Subject: subject, Author: author}, nil
}

func repoWebURL(ctx context.Context, deps Deps) string {
	if deps.Config.RepoURL != "" {
		return strings.TrimSuffix(deps.Config.RepoURL, "/")
	}
	remote, err := deps.Git.RemoteURL(ctx)
	if err != nil {
		return ""
	}
	return git.WebURL(remote)
}
