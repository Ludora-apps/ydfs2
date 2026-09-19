package main

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Pull requests towards the fixed upstream project. Two shapes, both asked for
// from a commit listed in one of the repository boxes:
//
//   - "through" — this commit and everything before it. The branch is the
//     commit itself, so nothing is replayed and nothing can conflict.
//   - "single"  — this commit alone, cherry-picked onto the upstream branch.
//     A conflicting cherry-pick opens the same resolution screen a conflicting
//     merge does (see graft.go), and the pull request is opened once it is
//     resolved.
//
// Authentication is the host's gh, already logged in for the account running
// the service: `gh auth token` reads its configuration without touching the
// network, so it is cheap enough to report on every repository read. The token
// is passed to git and to gh through the environment, never in argv.

// prBranchPrefix is the namespace this manager owns on the fork. Branches under
// it are force-pushed: replaying a resolution produces a new commit for the
// same name, and nothing else is allowed to live there.
const prBranchPrefix = "ydfs-web/"

// GitHub says whether the Pull request buttons can do anything, and why not
// when they cannot.
type GitHub struct {
	Available bool   `json:"available"`
	Origin    string `json:"origin,omitempty"`   // "Ludora-apps/ydfs2", the fork pushed to
	Upstream  string `json:"upstream,omitempty"` // "linuxconsole-org/ydfs2", the PR target
	Base      string `json:"base,omitempty"`     // the upstream branch a PR targets
	Error     string `json:"error,omitempty"`
}

// PRPlan is the pull request a graft is on its way to, carried through a
// resolution so the browser can say where the work is heading.
type PRPlan struct {
	Mode     string `json:"mode"` // "single" | "through"
	Revision string `json:"revision"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url,omitempty"`
}

// prBase is the upstream branch a pull request targets: the one named like the
// checkout's branch when upstream carries it, the release branch otherwise —
// the same rule check() uses to decide what to compare against.
func (a *App) prBase(ctx context.Context) string {
	b := a.branch(ctx)
	if _, e := a.git(ctx, "rev-parse", "--verify", "--quiet", upstreamRefs+"/"+b+"^{commit}"); e == nil {
		return b
	}
	return defaultBranch
}

// forkURL is where a pull request branch is pushed: the fork the checkout was
// cloned from, over HTTPS so the host's gh credential can answer for it. Only
// tests substitute another one, so that no test ever reaches the network.
func (a *App) forkURL(ctx context.Context) string {
	if a.forkFrom != "" {
		return a.forkFrom
	}
	origin := a.remoteURL(ctx, originRemote)
	if origin == "" {
		return ""
	}
	return "https://github.com/" + repoLabel(origin) + ".git"
}
func (a *App) github(ctx context.Context) *GitHub {
	origin := a.remoteURL(ctx, originRemote)
	if origin == "" {
		return nil
	}
	g := &GitHub{Origin: repoLabel(origin), Upstream: repoLabel(a.upstreamRemote()), Base: a.prBase(ctx)}
	hosted := strings.Contains(origin, "github.com") && strings.Contains(a.upstreamRemote(), "github.com")
	switch {
	case !hosted && a.forkFrom == "":
		g.Error = "pull requests need both the fork and upstream on github.com"
	default:
		if _, e := a.ghToken(ctx); e != nil {
			g.Error = "the gh command is not authenticated on this server — run: gh auth login"
		} else {
			g.Available = true
		}
	}
	return g
}

// ghToken reads the host's stored credential. It is a local read of gh's own
// configuration, with no network call, so state() can afford it.
func (a *App) ghToken(ctx context.Context) (string, error) {
	if a.gh == "" {
		return "", errors.New("the gh command is not configured on this server")
	}
	out, e := exec.CommandContext(ctx, a.gh, "auth", "token").Output()
	if t := strings.TrimSpace(string(out)); e == nil && t != "" {
		return t, nil
	}
	return "", errors.New("the gh command is not authenticated on this server")
}

// ghAPI calls the GitHub API through gh, with the credential in the
// environment. gh's own stderr is reported: it carries GitHub's message.
func (a *App) ghAPI(ctx context.Context, args ...string) (string, error) {
	token, e := a.ghToken(ctx)
	if e != nil {
		return "", e
	}
	cmd := exec.CommandContext(ctx, a.gh, append([]string{"api"}, args...)...)
	cmd.Env = append(cmd.Environ(), "GH_TOKEN="+token, "GH_PAGER=cat", "NO_COLOR=1")
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

// pushTo publishes one commit on the fork. The credential is fed through the
// process environment, never through argv or the URL, so it cannot surface in
// a process listing or in the git error text the UI displays.
//
// force is only ever set for a branch under prBranchPrefix; the checkout's own
// branch is pushed fast-forward-only, so this can never discard work on the
// fork.
func (a *App) pushTo(ctx context.Context, dir, sha, branch string, force bool) error {
	token, e := a.ghToken(ctx)
	if e != nil {
		return e
	}
	url := a.forkURL(ctx)
	if url == "" {
		return errors.New("this checkout has no origin remote to push to")
	}
	const helper = `!f() { test "$1" = get && printf 'username=x-access-token\npassword=%s\n' "$GH_TOKEN"; }; f`
	args := []string{
		// The empty value first clears any helper inherited from the host's
		// git configuration, so only this one can answer.
		"-c", "credential.helper=",
		"-c", "credential.helper=" + helper,
		"push",
	}
	if force {
		args = append(args, "--force")
	}
	args = append(args, url, sha+":refs/heads/"+branch)
	_, e = a.gitRaw(ctx, dir, []string{"GH_TOKEN=" + token}, args...)
	return e
}

// publish pushes the branch and opens the pull request for it — or finds the
// one already open, so retrying a resolution never fails at the last step.
func (a *App) publish(ctx context.Context, dir, sha string, p *PRPlan) (string, error) {
	gh := a.github(ctx)
	if gh == nil || !gh.Available {
		if gh != nil && gh.Error != "" {
			return "", errors.New(gh.Error)
		}
		return "", errors.New("this checkout has no origin remote to open a pull request from")
	}
	owner, _, _ := strings.Cut(gh.Origin, "/")
	if e := a.pushTo(ctx, dir, sha, p.Branch, true); e != nil {
		return "", errors.New("cannot push " + p.Branch + " to " + gh.Origin + ": " + e.Error())
	}
	head := owner + ":" + p.Branch
	url, e := a.ghAPI(ctx, "repos/"+gh.Upstream+"/pulls",
		"-f", "head="+head, "-f", "base="+p.Base, "-f", "title="+p.Title, "-f", "body="+p.Body, "--jq", ".html_url")
	if e != nil {
		if open := a.openPR(ctx, gh.Upstream, head); open != "" {
			p.URL = open
			return open, nil
		}
		return "", errors.New("cannot open the pull request: " + e.Error())
	}
	p.URL = url
	return url, nil
}
func (a *App) openPR(ctx context.Context, upstream, head string) string {
	out, e := a.ghAPI(ctx, "repos/"+upstream+"/pulls?state=open&head="+head, "--jq", ".[0].html_url")
	if e != nil || out == "null" {
		return ""
	}
	return out
}

// finishPick commits a resolved cherry-pick and opens its pull request.
// The caller holds App.graftMu.
func (a *App) finishPick(ctx context.Context, g *Graft) (string, error) {
	sha, e := a.commitGraft(ctx, g)
	if e != nil {
		return "", e
	}
	return a.publish(ctx, a.graftDir(), sha, g.PR)
}

func (a *App) prPlan(ctx context.Context, mode, rev string, c Commit) *PRPlan {
	base := a.prBase(ctx)
	body := "Opened from the LinuxConsole build manager.\n\n"
	if mode == "through" {
		body += "`" + short(rev) + "` and every commit before it on `" + a.branch(ctx) + "`."
	} else {
		body += "`" + short(rev) + "` alone, cherry-picked onto `" + base + "`."
	}
	return &PRPlan{Mode: mode, Revision: rev, Base: base, Title: c.Subject, Body: body,
		Branch: prBranchPrefix + mode + "-" + short(rev)}
}

// repositoryPR opens a pull request against upstream from one listed commit.
func (a *App) repositoryPR(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	var body struct {
		Revision string `json:"revision"`
		Mode     string `json:"mode"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Mode != "single" && body.Mode != "through" {
		fail(w, 400, "unknown pull request mode")
		return
	}
	s, e := a.state(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	// The browser's list is never authoritative: only a revision one of the
	// repository boxes actually listed can be proposed upstream.
	if !branchPattern.MatchString(body.Revision) || !offered(s)[body.Revision] {
		fail(w, 400, "unknown revision")
		return
	}
	rev, e := a.git(ctx, "rev-parse", "--verify", "--quiet", body.Revision+"^{commit}")
	if e != nil || rev == "" {
		fail(w, 400, "unknown revision")
		return
	}
	c, e := a.commit(ctx, rev)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	plan := a.prPlan(ctx, body.Mode, rev, c)
	if body.Mode == "through" {
		// Nothing is replayed: the branch is the commit, so its whole history
		// goes up as it stands and no conflict is possible. It is pushed
		// straight out of the checkout's object store.
		url, e := a.publish(ctx, a.repo, rev, plan)
		if e != nil {
			fail(w, 409, e.Error())
			return
		}
		a.respondRepository(w, ctx, url)
		return
	}
	var url string
	code, e := func() (int, error) {
		a.graftMu.Lock()
		defer a.graftMu.Unlock()
		onto, e := a.git(ctx, "rev-parse", "--verify", "--quiet", upstreamRefs+"/"+plan.Base+"^{commit}")
		if e != nil || onto == "" {
			return 409, errors.New("check upstream first: its " + plan.Base + " branch has not been read yet")
		}
		g := &Graft{Kind: "pick", Base: onto, Branch: plan.Base, Revision: rev, Subject: c.Subject, PR: plan}
		if e := a.openGraft(ctx, g); e != nil {
			a.graft = nil
			return 409, errors.New("cannot replay that commit onto " + plan.Base + ": " + e.Error())
		}
		if !g.Clean {
			// Left open: the same screen that resolves a conflicting merge
			// resolves this, and applying it opens the pull request.
			a.graft = g
			return 0, nil
		}
		u, e := a.finishPick(ctx, g)
		if e != nil {
			a.graft = nil
			a.discardWorktree(ctx)
			return 409, e
		}
		url = u
		a.discardWorktree(ctx)
		return 0, nil
	}()
	if e != nil {
		fail(w, code, e.Error())
		return
	}
	a.respondRepository(w, ctx, url)
}

// repositoryPush publishes the checkout's branch on the fork — the separate,
// explicit step after a merge, never part of one.
func (a *App) repositoryPush(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	// Held for the whole push so the revision pushed is the one the checkout
	// carries, not one a submission is halfway through changing.
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.detached(ctx) {
		fail(w, 409, "this checkout is on a detached HEAD; switch to a branch before pushing")
		return
	}
	if status, e := a.git(ctx, "status", "--porcelain"); e == nil && status != "" {
		fail(w, 409, "the checkout has uncommitted changes; commit or discard them before pushing")
		return
	}
	branch := a.branch(ctx)
	head, e := a.git(ctx, "rev-parse", "HEAD")
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	// Never forced: this is a branch people share, not one this manager owns.
	if e := a.pushTo(ctx, a.repo, head, branch, false); e != nil {
		fail(w, 409, "cannot push "+branch+" to "+originRemote+": "+e.Error())
		return
	}
	if _, e := a.git(ctx, "fetch", "--tags", "--prune", originRemote); e != nil {
		// The push worked; a stale tracking ref is not worth failing on.
		_ = e
	}
	a.respondRepository(w, ctx, "")
}
