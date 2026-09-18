package main

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The upstream project is fixed: an operator must never be able to point the
// checkout at another repository through the browser.
const upstreamURL = "https://github.com/linuxconsole-org/ydfs2"
const defaultBranch = "2.12"

var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,100}$`)

// upstreamRemote is the fixed project URL; only tests substitute another one.
func (a *App) upstreamRemote() string {
	if a.upstreamFrom != "" {
		return a.upstreamFrom
	}
	return upstreamURL
}

type Commit struct {
	Revision string `json:"revision"`
	Subject  string `json:"subject"`
	Author   string `json:"author"`
	Date     string `json:"date"`
}
type Repository struct {
	URL         string  `json:"url"`
	Branch      string  `json:"branch"`
	Tag         string  `json:"tag"`
	Dirty       bool    `json:"dirty"`
	Local       Commit  `json:"local"`
	Upstream    *Commit `json:"upstream,omitempty"`
	Ahead       int     `json:"ahead"`
	Behind      int     `json:"behind"`
	FastForward bool    `json:"fastForward"`
	CheckedAt   string  `json:"checkedAt,omitempty"`
}

// git runs a repository command with a deadline and without any interactive
// credential or SSH prompt, so a network call can never wedge a request.
func (a *App) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", a.repo}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, e := cmd.Output()
	if e != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return "", errors.New(s)
		}
		return "", e
	}
	return strings.TrimSpace(string(out)), nil
}
func (a *App) commit(ctx context.Context, rev string) (Commit, error) {
	out, e := a.git(ctx, "log", "-1", "--format=%H%x1f%s%x1f%an%x1f%cI", rev)
	if e != nil {
		return Commit{}, e
	}
	f := strings.Split(out, "\x1f")
	if len(f) != 4 {
		return Commit{}, errors.New("cannot read commit metadata")
	}
	return Commit{Revision: f[0], Subject: f[1], Author: f[2], Date: f[3]}, nil
}
func (a *App) branch(ctx context.Context) string {
	b, e := a.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	// A detached HEAD (or an unreadable name) compares against the release branch.
	if e != nil || b == "HEAD" || !branchPattern.MatchString(b) {
		return defaultBranch
	}
	return b
}

// state reports the checkout as it is right now, merged with whatever the last
// upstream check found. It never touches the network itself.
func (a *App) state(ctx context.Context) (*Repository, error) {
	local, e := a.commit(ctx, "HEAD")
	if e != nil {
		return nil, errors.New("cannot read the repository: " + e.Error())
	}
	r := &Repository{URL: a.upstreamRemote(), Branch: a.branch(ctx), Local: local}
	r.Tag, _ = a.git(ctx, "describe", "--tags", "--abbrev=0")
	if status, e := a.git(ctx, "status", "--porcelain"); e == nil {
		r.Dirty = status != ""
	}
	a.repoMu.Lock()
	defer a.repoMu.Unlock()
	if a.upstream == nil {
		return r, nil
	}
	up := *a.upstream
	r.Upstream, r.CheckedAt = &up, a.upstreamAt.UTC().Format(time.RFC3339)
	// The cached counts describe the commit the check ran against; a local
	// commit or a pull since then makes them stale, so recompute cheaply.
	if ahead, behind, e := a.counts(ctx, up.Revision); e == nil {
		r.Ahead, r.Behind = ahead, behind
		_, e := a.git(ctx, "merge-base", "--is-ancestor", "HEAD", up.Revision)
		r.FastForward = e == nil
	}
	return r, nil
}
func (a *App) counts(ctx context.Context, rev string) (int, int, error) {
	out, e := a.git(ctx, "rev-list", "--left-right", "--count", "HEAD..."+rev)
	if e != nil {
		return 0, 0, e
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, errors.New("cannot compare revisions")
	}
	ahead, e1 := strconv.Atoi(f[0])
	behind, e2 := strconv.Atoi(f[1])
	if e1 != nil || e2 != nil {
		return 0, 0, errors.New("cannot compare revisions")
	}
	return ahead, behind, nil
}

// check downloads the upstream branch into FETCH_HEAD (no local ref or working
// tree is touched) and caches what it found.
func (a *App) check(ctx context.Context) error {
	branch := a.branch(ctx)
	if _, e := a.git(ctx, "fetch", "--tags", "--force", a.upstreamRemote(), branch); e != nil {
		return errors.New("cannot reach " + a.upstreamRemote() + ": " + e.Error())
	}
	c, e := a.commit(ctx, "FETCH_HEAD")
	if e != nil {
		return e
	}
	a.repoMu.Lock()
	a.upstream, a.upstreamAt = &c, time.Now()
	a.repoMu.Unlock()
	return nil
}
func (a *App) repository(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	s, e := a.state(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, s)
}
func (a *App) repositoryCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if e := a.check(ctx); e != nil {
		fail(w, 502, e.Error())
		return
	}
	s, e := a.state(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, s)
}

// mergeIdentity supplies an author only when the checkout has none, so a merge
// commit can never fail for want of a git identity, and never hides a real one.
func (a *App) mergeIdentity(ctx context.Context) []string {
	if mail, e := a.git(ctx, "config", "user.email"); e == nil && mail != "" {
		return nil
	}
	return []string{"-c", "user.name=LinuxConsole build manager", "-c", "user.email=ydfs-web@localhost"}
}

// merge brings the upstream commit into the checkout: a fast-forward when the
// checkout has no commits of its own, a merge commit otherwise (a fork carrying
// local work is permanently diverged, which is normal, not an error). A merge
// that conflicts is rolled back and reported with the conflicting paths.
func (a *App) merge(ctx context.Context, rev string, fastForward bool) error {
	if fastForward {
		_, e := a.git(ctx, "merge", "--ff-only", rev)
		return e
	}
	args := append(a.mergeIdentity(ctx), "merge", "--no-edit", "-m", "Merge upstream "+rev[:12]+" into "+a.branch(ctx), rev)
	_, e := a.git(ctx, args...)
	if e == nil {
		return nil
	}
	conflicts, _ := a.git(ctx, "diff", "--name-only", "--diff-filter=U")
	a.git(ctx, "merge", "--abort")
	if conflicts != "" {
		return errors.New("conflicting changes in: " + strings.Join(strings.Fields(conflicts), ", ") + " — merge it with git, nothing was changed")
	}
	return e
}

// repositoryUpdate merges the upstream branch into the checkout. Running and
// queued builds are unaffected: each one already built its own snapshot of the
// working tree when it was submitted.
func (a *App) repositoryUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	// Held for the whole update so a submission cannot snapshot a half-merged tree.
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.check(ctx); e != nil {
		fail(w, 502, e.Error())
		return
	}
	s, e := a.state(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	switch {
	case s.Dirty:
		fail(w, 409, "the checkout has uncommitted changes; commit or discard them before updating")
		return
	case s.Behind == 0:
		respond(w, s)
		return
	}
	if e := a.merge(ctx, s.Upstream.Revision, s.FastForward); e != nil {
		fail(w, 409, "update failed: "+e.Error())
		return
	}
	if s, e = a.state(ctx); e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, s)
}
