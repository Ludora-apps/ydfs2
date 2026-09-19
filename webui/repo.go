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
// checkout at another repository through the browser. The fork the checkout was
// cloned from is read from the checkout's own git config instead, never from a
// request, so it is described and offered without widening that rule.
const upstreamURL = "https://github.com/linuxconsole-org/ydfs2"
const defaultBranch = "2.12"
const originRemote = "origin"
const upstreamRemoteName = "upstream"

// upstreamRefs is where a check parks the fixed project's branches. It is the
// tracking namespace of the remote that conventionally carries them, so a
// checkout that already has an "upstream" remote keeps one consistent set.
const upstreamRefs = "refs/remotes/" + upstreamRemoteName

// sourceDir is the only tree the build manager knows how to build (main.go
// requires it, runner.go snapshots it and mounts it as /2.12), so a revision
// without it cannot be checked out.
const sourceDir = "2.12"

// commitCount is how many commits each repository box lists.
const commitCount = 10

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
type Branch struct {
	Name string `json:"name"` // "2.12"
	Ref  string `json:"ref"`  // "origin/2.12" — what /checkout is asked for
	// Current is the branch the working tree is on; Buildable says the tree
	// carries sourceDir, without which a build cannot run.
	Current   bool `json:"current"`
	Buildable bool `json:"buildable"`
}

// Source is one repository box: the checkout itself, the fork, or upstream.
type Source struct {
	Name     string   `json:"name"`  // "local" | "origin" | "upstream"
	Label    string   `json:"label"` // "Ludora-apps/ydfs2"
	URL      string   `json:"url"`
	Selected string   `json:"selected"` // the branch whose commits are listed
	Branches []Branch `json:"branches"`
	Commits  []Commit `json:"commits"`
	Error    string   `json:"error,omitempty"`
}
type Repository struct {
	URL         string   `json:"url"`
	Branch      string   `json:"branch"`
	Detached    bool     `json:"detached"`
	Tag         string   `json:"tag"`
	Dirty       bool     `json:"dirty"`
	Local       Commit   `json:"local"`
	Upstream    *Commit  `json:"upstream,omitempty"`
	Ahead       int      `json:"ahead"`
	Behind      int      `json:"behind"`
	FastForward bool     `json:"fastForward"`
	CheckedAt   string   `json:"checkedAt,omitempty"`
	Sources     []Source `json:"sources"`
	// GitHub says whether a pull request can be opened from this checkout, and
	// Graft carries the merge or cherry-pick waiting to be resolved. Both ride
	// along with every repository response so a browser that reloads mid-merge
	// finds the session again without a second request.
	GitHub *GitHub `json:"github,omitempty"`
	Graft  *Graft  `json:"graft,omitempty"`
	// PullRequest is the URL of a pull request this very request opened. It is
	// never stored: it is how the browser learns where the PR landed.
	PullRequest string `json:"pullRequest,omitempty"`
}

// gitRaw runs a repository command with a deadline and without any interactive
// credential or SSH prompt, so a network call can never wedge a request. dir
// selects the tree it runs in: the checkout itself, or the linked worktree a
// conflict is being resolved in (see graft.go). env carries extra variables —
// a credential for a push, never anything that would show up in argv.
func (a *App) gitRaw(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	cmd.Env = append(cmd.Env, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, e := cmd.Output()
	if e != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return nil, errors.New(s)
		}
		return nil, e
	}
	return out, nil
}

// gitAt is gitRaw with the output trimmed, which is what every caller reading a
// revision, a ref name or a status line wants.
func (a *App) gitAt(ctx context.Context, dir string, args ...string) (string, error) {
	out, e := a.gitRaw(ctx, dir, nil, args...)
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(out)), nil
}
func (a *App) git(ctx context.Context, args ...string) (string, error) {
	return a.gitAt(ctx, a.repo, args...)
}

// commits reads the newest n commits of a revision, one per line.
func (a *App) commits(ctx context.Context, rev string, n int) ([]Commit, error) {
	out, e := a.git(ctx, "log", "-n", strconv.Itoa(n), "--format=%H%x1f%s%x1f%an%x1f%cI", rev, "--")
	if e != nil {
		return nil, e
	}
	if out == "" {
		return nil, nil
	}
	var list []Commit
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) != 4 {
			return nil, errors.New("cannot read commit metadata")
		}
		list = append(list, Commit{Revision: f[0], Subject: f[1], Author: f[2], Date: f[3]})
	}
	return list, nil
}
func (a *App) commit(ctx context.Context, rev string) (Commit, error) {
	list, e := a.commits(ctx, rev, 1)
	if e != nil {
		return Commit{}, e
	}
	if len(list) == 0 {
		return Commit{}, errors.New("cannot read commit metadata")
	}
	return list[0], nil
}
func (a *App) branch(ctx context.Context) string {
	b, e := a.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	// A detached HEAD (or an unreadable name) compares against the release branch.
	if e != nil || b == "HEAD" || !branchPattern.MatchString(b) {
		return defaultBranch
	}
	return b
}

// detached reports a checkout sitting on a commit rather than a branch. The
// command-line build needs a branch (Makefile-docker derives GIT_BRANCH, and
// therefore the source directory, from it), so the UI has to say so plainly.
func (a *App) detached(ctx context.Context) bool {
	_, e := a.git(ctx, "symbolic-ref", "-q", "HEAD")
	return e != nil
}
func (a *App) remoteURL(ctx context.Context, name string) string {
	u, _ := a.git(ctx, "remote", "get-url", name)
	return u
}

// buildable reports whether a revision carries the tree the build manager knows.
func (a *App) buildable(ctx context.Context, rev string) bool {
	_, e := a.git(ctx, "cat-file", "-e", rev+":"+sourceDir+"/Makefile")
	return e == nil
}

// refNames lists the branches under one namespace, newest first, without the
// symbolic HEAD alias a remote carries.
func (a *App) refNames(ctx context.Context, prefix string) []string {
	out, e := a.git(ctx, "for-each-ref", "--format=%(refname:short)", "--sort=-committerdate", prefix)
	if e != nil || out == "" {
		return nil
	}
	var names []string
	for _, n := range strings.Split(out, "\n") {
		if n == "" || n == "HEAD" || strings.HasSuffix(n, "/HEAD") {
			continue
		}
		names = append(names, n)
	}
	return names
}

// repoLabel turns a clone URL into the owner/name a person recognises.
func repoLabel(url string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "/"); j >= 0 {
			s = s[j+1:]
		}
	} else if i := strings.Index(s, ":"); i >= 0 {
		// scp-style git@host:owner/name
		s = s[i+1:]
	}
	if s == "" {
		return url
	}
	return s
}

// source describes one repository box. It reads refs only: a box is as fresh as
// the last check, and rendering it never touches the network.
func (a *App) source(ctx context.Context, name, label, url, prefix, head, failure string) Source {
	s := Source{Name: name, Label: label, URL: url, Error: failure}
	if name == "local" && head == "HEAD" {
		// A detached checkout still has somewhere to show commits from.
		s.Branches = append(s.Branches, Branch{Name: "detached HEAD", Ref: "HEAD", Current: true, Buildable: true})
	}
	for _, ref := range a.refNames(ctx, prefix) {
		short := ref
		if i := strings.Index(short, "/"); name != "local" && i >= 0 {
			short = short[i+1:]
		}
		s.Branches = append(s.Branches, Branch{Name: short, Ref: ref, Current: name == "local" && ref == head, Buildable: a.buildable(ctx, ref)})
	}
	for _, want := range []string{head, defaultBranch} {
		for _, b := range s.Branches {
			if b.Name == want || b.Ref == want {
				s.Selected = b.Ref
				break
			}
		}
		if s.Selected != "" {
			break
		}
	}
	if s.Selected == "" && len(s.Branches) > 0 {
		s.Selected = s.Branches[0].Ref
	}
	if s.Selected != "" {
		s.Commits, _ = a.commits(ctx, s.Selected, commitCount)
	}
	return s
}

// sources lists every repository the checkout can be moved to. The fork box is
// dropped when the checkout has no origin remote at all.
func (a *App) sources(ctx context.Context, r *Repository, originErr string) []Source {
	head := r.Branch
	if r.Detached {
		head = "HEAD"
	}
	list := []Source{a.source(ctx, "local", "This checkout", "", "refs/heads", head, "")}
	if url := a.remoteURL(ctx, originRemote); url != "" {
		list = append(list, a.source(ctx, originRemote, repoLabel(url), url, "refs/remotes/"+originRemote, head, originErr))
	}
	list = append(list, a.source(ctx, upstreamRemoteName, repoLabel(a.upstreamRemote()), a.upstreamRemote(), upstreamRefs, head, ""))
	return list
}

// offered is every revision the browser may ask for: the branches each box
// lists and the commits shown under them. The browser's list is never
// authoritative, exactly as with the Flatpak catalogue.
func offered(r *Repository) map[string]bool {
	set := map[string]bool{}
	for _, s := range r.Sources {
		for _, b := range s.Branches {
			set[b.Ref] = true
		}
		for _, c := range s.Commits {
			set[c.Revision] = true
		}
	}
	return set
}

// state reports the checkout as it is right now, merged with whatever the last
// upstream check found. It never touches the network itself.
func (a *App) state(ctx context.Context) (*Repository, error) {
	local, e := a.commit(ctx, "HEAD")
	if e != nil {
		return nil, errors.New("cannot read the repository: " + e.Error())
	}
	r := &Repository{URL: a.upstreamRemote(), Branch: a.branch(ctx), Detached: a.detached(ctx), Local: local}
	r.Tag, _ = a.git(ctx, "describe", "--tags", "--abbrev=0")
	if status, e := a.git(ctx, "status", "--porcelain"); e == nil {
		r.Dirty = status != ""
	}
	a.repoMu.Lock()
	up, checkedAt, originErr := a.upstream, a.upstreamAt, a.originError
	a.repoMu.Unlock()
	if up != nil {
		c := *up
		r.Upstream, r.CheckedAt = &c, checkedAt.UTC().Format(time.RFC3339)
		// The cached counts describe the commit the check ran against; a local
		// commit or a pull since then makes them stale, so recompute cheaply.
		if ahead, behind, e := a.counts(ctx, c.Revision); e == nil {
			r.Ahead, r.Behind = ahead, behind
			_, e := a.git(ctx, "merge-base", "--is-ancestor", "HEAD", c.Revision)
			r.FastForward = e == nil
		}
	}
	r.Sources = a.sources(ctx, r, originErr)
	r.GitHub = a.github(ctx)
	a.graftMu.Lock()
	r.Graft = a.graft
	a.graftMu.Unlock()
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

// check downloads both repositories: the fixed upstream by URL into its own
// tracking namespace (no local branch or working tree is touched), and the fork
// the checkout was cloned from. A fork that cannot be reached — commonly a
// missing SSH key for the service account — is reported in its own box rather
// than failing the upstream check.
func (a *App) check(ctx context.Context) error {
	branch := a.branch(ctx)
	if _, e := a.git(ctx, "fetch", "--tags", "--force", "--prune", a.upstreamRemote(), "+refs/heads/*:"+upstreamRefs+"/*"); e != nil {
		return errors.New("cannot reach " + a.upstreamRemote() + ": " + e.Error())
	}
	rev := upstreamRefs + "/" + branch
	if _, e := a.git(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}"); e != nil {
		// The checkout is on a branch upstream does not carry; compare against
		// the release branch rather than failing the whole check.
		rev = upstreamRefs + "/" + defaultBranch
	}
	c, e := a.commit(ctx, rev)
	if e != nil {
		return e
	}
	originErr := ""
	if a.remoteURL(ctx, originRemote) != "" {
		if _, e := a.git(ctx, "fetch", "--tags", "--prune", originRemote); e != nil {
			originErr = "cannot reach " + originRemote + ": " + e.Error()
		}
	}
	a.repoMu.Lock()
	a.upstream, a.upstreamAt, a.originError = &c, time.Now(), originErr
	a.repoMu.Unlock()
	return nil
}
func (a *App) repository(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
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

// repositoryCommits lists the newest commits of a branch the browser picked in
// one of the repository boxes. It reads refs only, so it costs no network call.
func (a *App) repositoryCommits(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ref := r.URL.Query().Get("ref")
	s, e := a.state(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	if !branchPattern.MatchString(ref) || !offered(s)[ref] {
		fail(w, 400, "unknown revision")
		return
	}
	list, e := a.commits(ctx, ref, commitCount)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	respond(w, map[string]any{"selected": ref, "commits": list})
}

// switchTo moves the working tree. A remote branch becomes — or reuses — a
// local branch of the same name, so the checkout keeps a name the command-line
// Makefile can turn into GIT_BRANCH; a bare commit can only be detached.
func (a *App) switchTo(ctx context.Context, ref string) error {
	local := ref
	for _, remote := range []string{originRemote, upstreamRemoteName} {
		if strings.HasPrefix(ref, remote+"/") {
			local = strings.TrimPrefix(ref, remote+"/")
			if _, e := a.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+local); e != nil {
				_, e := a.git(ctx, "switch", "--track", ref)
				return e
			}
			// A local branch of that name already exists: switching to it never
			// moves it. Bringing new commits in stays the Update button's job.
			_, e := a.git(ctx, "switch", local)
			return e
		}
	}
	if _, e := a.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+local); e == nil {
		_, e := a.git(ctx, "switch", local)
		return e
	}
	// "switch" rather than "checkout": the release branches are named after the
	// source directory (2.12), which "checkout" would read as ambiguous.
	_, e := a.git(ctx, "switch", "--detach", ref)
	return e
}

// repositoryCheckout moves the checkout onto another branch or commit. Running
// and queued builds are unaffected: each one already built its own snapshot of
// the working tree when it was submitted.
func (a *App) repositoryCheckout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var body struct {
		Ref string `json:"ref"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Held for the whole switch so a submission cannot snapshot a half-changed tree.
	a.mu.Lock()
	defer a.mu.Unlock()
	s, e := a.state(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	if !branchPattern.MatchString(body.Ref) || !offered(s)[body.Ref] {
		fail(w, 400, "unknown revision")
		return
	}
	if s.Dirty {
		fail(w, 409, "the checkout has uncommitted changes; commit or discard them before switching")
		return
	}
	rev, e := a.git(ctx, "rev-parse", "--verify", "--quiet", body.Ref+"^{commit}")
	if e != nil || rev == "" {
		fail(w, 400, "unknown revision")
		return
	}
	if !a.buildable(ctx, rev) {
		fail(w, 409, "that revision carries no "+sourceDir+"/Makefile; the build manager only knows how to build that tree")
		return
	}
	if e := a.switchTo(ctx, body.Ref); e != nil {
		fail(w, 409, "checkout failed: "+e.Error())
		return
	}
	if s, e = a.state(ctx); e != nil {
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

// repositoryUpdate merges upstream into the checkout in one step, for a client
// that does not want the resolution screen. It is the same machinery: the
// replay happens in the graft worktree, so a conflict changes nothing here —
// it leaves the resolution open for the Repository screen to finish.
//
// Running and queued builds are unaffected either way: each one already built
// its own snapshot of the working tree when it was submitted.
func (a *App) repositoryUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	// Held for the whole update so a submission cannot snapshot a half-merged
	// tree. Lock order: mu then graftMu.
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.check(ctx); e != nil {
		fail(w, 502, e.Error())
		return
	}
	code, e := func() (int, error) {
		a.graftMu.Lock()
		defer a.graftMu.Unlock()
		a.repoMu.Lock()
		up := a.upstream
		a.repoMu.Unlock()
		if up == nil {
			return 502, errors.New("upstream has not been read yet")
		}
		if status, e := a.git(ctx, "status", "--porcelain"); e == nil && status != "" {
			return 409, errors.New("the checkout has uncommitted changes; commit or discard them before updating")
		}
		head, e := a.git(ctx, "rev-parse", "HEAD")
		if e != nil {
			return 500, e
		}
		if _, e := a.git(ctx, "merge-base", "--is-ancestor", up.Revision, head); e == nil {
			return 0, nil // already in
		}
		g := &Graft{Kind: "merge", Base: head, Branch: a.branch(ctx), Revision: up.Revision, Subject: up.Subject}
		if e := a.openGraft(ctx, g); e != nil {
			a.graft = nil
			return 409, errors.New("update failed: " + e.Error())
		}
		if !g.Clean {
			a.graft = g
			paths := make([]string, 0, len(g.Files))
			for _, c := range g.Files {
				paths = append(paths, c.Path)
			}
			return 409, errors.New("conflicting changes in: " + strings.Join(paths, ", ") +
				" — nothing was changed; resolve them on the Repository screen")
		}
		if e := a.applyMerge(ctx, g); e != nil {
			a.graft = nil
			a.discardWorktree(ctx)
			return 409, errors.New("update failed: " + e.Error())
		}
		a.graft = nil
		a.discardWorktree(ctx)
		return 0, nil
	}()
	if e != nil {
		fail(w, code, e.Error())
		return
	}
	a.respondRepository(w, ctx, "")
}
