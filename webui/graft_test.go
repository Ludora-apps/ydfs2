package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forkFixture builds an upstream repository and a checkout cloned from it, one
// commit ahead on each side. With conflict set, both sides changed the same
// file; without it, they changed different ones.
func forkFixture(t *testing.T, conflict bool) *App {
	t.Helper()
	a := testApp(t)
	origin := t.TempDir()
	gitFixture(t, origin, "init", "-b", defaultBranch)
	putFile(t, filepath.Join(origin, "2.12", "Makefile"), "all:\n")
	putFile(t, filepath.Join(origin, "shared.txt"), "base\n")
	gitFixture(t, origin, "add", ".")
	gitFixture(t, origin, "commit", "-m", "first")
	gitFixture(t, t.TempDir(), "clone", origin, a.repo)
	a.upstreamFrom = origin
	putFile(t, filepath.Join(origin, "shared.txt"), "upstream\n")
	gitFixture(t, origin, "commit", "-am", "upstream change")
	if conflict {
		putFile(t, filepath.Join(a.repo, "shared.txt"), "local\n")
		gitFixture(t, a.repo, "commit", "-am", "local change")
	} else {
		putFile(t, filepath.Join(a.repo, "local.txt"), "local\n")
		gitFixture(t, a.repo, "add", ".")
		gitFixture(t, a.repo, "commit", "-m", "local change")
	}
	return a
}
func repoOf(t *testing.T, w *httptest.ResponseRecorder) *Repository {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var s Repository
	if e := json.Unmarshal(w.Body.Bytes(), &s); e != nil {
		t.Fatalf("%v: %s", e, w.Body.String())
	}
	return &s
}
func read(t *testing.T, p string) string {
	t.Helper()
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

// gitOut runs a read-only git command against a fixture and returns its output.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	a := &App{}
	out, e := a.gitAt(t.Context(), dir, args...)
	if e != nil {
		t.Fatalf("git %v: %v", args, e)
	}
	return out
}

func TestGraftCleanMergeApplies(t *testing.T) {
	a := forkFixture(t, false)
	s := repoOf(t, request(a, "POST", "/api/repository/merge/preview", "{}"))
	if s.Graft == nil || !s.Graft.Clean || len(s.Graft.Files) != 0 {
		t.Fatalf("expected a clean graft, got %+v", s.Graft)
	}
	if s.Graft.Kind != "merge" || s.Graft.Base != s.Local.Revision {
		t.Fatalf("graft does not start from the checkout: %+v", s.Graft)
	}
	// Nothing has moved yet: a preview only reads.
	if head := gitOut(t, a.repo, "rev-parse", "HEAD"); head != s.Local.Revision {
		t.Fatal("the preview moved the checkout")
	}
	s = repoOf(t, request(a, "POST", "/api/repository/graft/apply", "{}"))
	if s.Graft != nil || s.Behind != 0 {
		t.Fatalf("apply left %+v behind=%d", s.Graft, s.Behind)
	}
	if got := read(t, filepath.Join(a.repo, "shared.txt")); got != "upstream\n" {
		t.Fatalf("upstream change not merged: %q", got)
	}
	if got := read(t, filepath.Join(a.repo, "local.txt")); got != "local\n" {
		t.Fatalf("local work lost: %q", got)
	}
	if status := gitOut(t, a.repo, "status", "--porcelain"); status != "" {
		t.Fatalf("checkout left dirty: %q", status)
	}
	if _, e := os.Stat(a.graftDir()); !os.IsNotExist(e) {
		t.Fatal("the worktree outlived the merge")
	}
}

func TestGraftResolvesConflictKeepingUpstream(t *testing.T) {
	a := forkFixture(t, true)
	s := repoOf(t, request(a, "POST", "/api/repository/merge/preview", "{}"))
	if s.Graft == nil || s.Graft.Clean || len(s.Graft.Files) != 1 {
		t.Fatalf("expected one conflicting file, got %+v", s.Graft)
	}
	c := s.Graft.Files[0]
	if c.Path != "shared.txt" || c.Kind != "content" || !c.Ours || !c.Theirs || c.Binary {
		t.Fatalf("conflict misread: %+v", c)
	}
	if s.Graft.OursLabel != "This checkout" || s.Graft.TheirsLabel != "Upstream" {
		t.Fatalf("sides mislabelled: %q / %q", s.Graft.OursLabel, s.Graft.TheirsLabel)
	}
	// The checkout is untouched while the conflict stands.
	if got := read(t, filepath.Join(a.repo, "shared.txt")); got != "local\n" {
		t.Fatalf("the checkout was modified: %q", got)
	}
	// Applying before resolving must be refused.
	if w := request(a, "POST", "/api/repository/graft/apply", "{}"); w.Code != 409 {
		t.Fatalf("unresolved apply returned %d", w.Code)
	}
	s = repoOf(t, request(a, "POST", "/api/repository/graft/resolve", `{"path":"shared.txt","choice":"theirs","content":""}`))
	if s.Graft.Files[0].Resolved != "theirs" {
		t.Fatalf("resolution not recorded: %+v", s.Graft.Files[0])
	}
	s = repoOf(t, request(a, "POST", "/api/repository/graft/apply", "{}"))
	if s.Graft != nil {
		t.Fatal("the graft outlived its apply")
	}
	if got := read(t, filepath.Join(a.repo, "shared.txt")); got != "upstream\n" {
		t.Fatalf("resolution not applied: %q", got)
	}
	if status := gitOut(t, a.repo, "status", "--porcelain"); status != "" {
		t.Fatalf("checkout left dirty: %q", status)
	}
	// A merge commit, so both histories are kept.
	if parents := strings.Fields(gitOut(t, a.repo, "rev-list", "--parents", "-1", "HEAD")); len(parents) != 3 {
		t.Fatalf("expected a merge commit, got %v", parents)
	}
}

func TestGraftKeepsBothSides(t *testing.T) {
	a := forkFixture(t, true)
	repoOf(t, request(a, "POST", "/api/repository/merge/preview", "{}"))
	repoOf(t, request(a, "POST", "/api/repository/graft/resolve", `{"path":"shared.txt","choice":"both","content":""}`))
	repoOf(t, request(a, "POST", "/api/repository/graft/apply", "{}"))
	got := read(t, filepath.Join(a.repo, "shared.txt"))
	if !strings.Contains(got, "local") || !strings.Contains(got, "upstream") {
		t.Fatalf("both sides not kept: %q", got)
	}
	if strings.Contains(got, "<<<<<<<") || strings.Contains(got, ">>>>>>>") {
		t.Fatalf("conflict markers survived: %q", got)
	}
}

func TestGraftAcceptsAnEditedFile(t *testing.T) {
	a := forkFixture(t, true)
	repoOf(t, request(a, "POST", "/api/repository/merge/preview", "{}"))
	repoOf(t, request(a, "POST", "/api/repository/graft/resolve", `{"path":"shared.txt","choice":"edited","content":"settled by hand\n"}`))
	repoOf(t, request(a, "POST", "/api/repository/graft/apply", "{}"))
	if got := read(t, filepath.Join(a.repo, "shared.txt")); got != "settled by hand\n" {
		t.Fatalf("edited content not committed: %q", got)
	}
}

func TestGraftServesEveryVersionOfAConflict(t *testing.T) {
	a := forkFixture(t, true)
	repoOf(t, request(a, "POST", "/api/repository/merge/preview", "{}"))
	for side, want := range map[string]string{"ours": "local\n", "theirs": "upstream\n"} {
		w := request(a, "GET", "/api/repository/graft/file?path=shared.txt&side="+side, "")
		if w.Code != 200 || w.Body.String() != want {
			t.Fatalf("%s: %d %q", side, w.Code, w.Body.String())
		}
	}
	if w := request(a, "GET", "/api/repository/graft/file?path=shared.txt&side=merged", ""); !strings.Contains(w.Body.String(), "<<<<<<<") {
		t.Fatalf("merged version carries no markers: %q", w.Body.String())
	}
	if w := request(a, "GET", "/api/repository/graft/file?path=nope.txt&side=ours", ""); w.Code != 400 {
		t.Fatalf("unknown path returned %d", w.Code)
	}
	if w := request(a, "POST", "/api/repository/graft/resolve", `{"path":"nope.txt","choice":"ours","content":""}`); w.Code != 400 {
		t.Fatalf("resolving an unlisted path returned %d", w.Code)
	}
}

func TestGraftAbortLeavesTheCheckoutAlone(t *testing.T) {
	a := forkFixture(t, true)
	before := gitOut(t, a.repo, "rev-parse", "HEAD")
	repoOf(t, request(a, "POST", "/api/repository/merge/preview", "{}"))
	s := repoOf(t, request(a, "DELETE", "/api/repository/graft", ""))
	if s.Graft != nil {
		t.Fatal("the graft survived its abort")
	}
	if head := gitOut(t, a.repo, "rev-parse", "HEAD"); head != before {
		t.Fatal("abort moved the checkout")
	}
	if got := read(t, filepath.Join(a.repo, "shared.txt")); got != "local\n" {
		t.Fatalf("abort changed the working tree: %q", got)
	}
	if _, e := os.Stat(a.graftDir()); !os.IsNotExist(e) {
		t.Fatal("the worktree survived its abort")
	}
}

func TestGraftRefusesToApplyOntoAMovedCheckout(t *testing.T) {
	a := forkFixture(t, true)
	repoOf(t, request(a, "POST", "/api/repository/merge/preview", "{}"))
	putFile(t, filepath.Join(a.repo, "later.txt"), "later\n")
	gitFixture(t, a.repo, "add", ".")
	gitFixture(t, a.repo, "commit", "-m", "later")
	repoOf(t, request(a, "POST", "/api/repository/graft/resolve", `{"path":"shared.txt","choice":"ours","content":""}`))
	w := request(a, "POST", "/api/repository/graft/apply", "{}")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "moved") {
		t.Fatalf("stale apply returned %d: %s", w.Code, w.Body.String())
	}
	if got := read(t, filepath.Join(a.repo, "shared.txt")); got != "local\n" {
		t.Fatalf("a refused apply still changed the tree: %q", got)
	}
}

func TestGraftRefusesADirtyCheckout(t *testing.T) {
	a := forkFixture(t, true)
	putFile(t, filepath.Join(a.repo, "shared.txt"), "uncommitted\n")
	w := request(a, "POST", "/api/repository/merge/preview", "{}")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "uncommitted") {
		t.Fatalf("dirty preview returned %d: %s", w.Code, w.Body.String())
	}
}

// fakeGh stands in for the host's gh: it answers "auth token" locally and
// records every API call so a test can check what would have been sent.
func fakeGh(t *testing.T, log string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = auth ]; then echo faketoken; exit 0; fi\n" +
		"for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> " + log + "; done\n" +
		"echo https://github.com/upstream/ydfs2/pull/7\n"
	if e := os.WriteFile(p, []byte(script), 0o755); e != nil {
		t.Fatal(e)
	}
	return p
}

// prFixture points a fork fixture at a local bare repository and a fake gh, so
// the pull request path is exercised without ever reaching the network.
func prFixture(t *testing.T, conflict bool) (*App, string, string) {
	t.Helper()
	a := forkFixture(t, conflict)
	fork := t.TempDir()
	gitFixture(t, fork, "init", "--bare", "-b", defaultBranch)
	log := filepath.Join(t.TempDir(), "gh.log")
	a.forkFrom, a.gh = fork, fakeGh(t, log)
	return a, fork, log
}

func TestPullRequestThroughPushesTheCommitAsItStands(t *testing.T) {
	a, fork, log := prFixture(t, true)
	s := repoOf(t, request(a, "GET", "/api/repository", ""))
	if s.GitHub == nil || !s.GitHub.Available {
		t.Fatalf("pull requests reported unavailable: %+v", s.GitHub)
	}
	head := s.Local.Revision
	s = repoOf(t, request(a, "POST", "/api/repository/pr", `{"revision":"`+head+`","mode":"through"}`))
	if s.PullRequest != "https://github.com/upstream/ydfs2/pull/7" {
		t.Fatalf("no pull request URL: %q", s.PullRequest)
	}
	branch := prBranchPrefix + "through-" + short(head)
	if got := gitOut(t, fork, "rev-parse", "refs/heads/"+branch); got != head {
		t.Fatalf("branch %s points at %s, want %s", branch, got, head)
	}
	sent := read(t, log)
	if !strings.Contains(sent, "head=fork:"+branch) && !strings.Contains(sent, ":"+branch) {
		t.Fatalf("pull request head not sent: %s", sent)
	}
	if !strings.Contains(sent, "base="+defaultBranch) {
		t.Fatalf("pull request base not sent: %s", sent)
	}
	// The credential never travels in argv, where a process listing would show it.
	if strings.Contains(sent, "faketoken") {
		t.Fatal("the credential leaked into the gh command line")
	}
	// Nothing was replayed, so nothing is left open.
	if s.Graft != nil {
		t.Fatalf("a graft was opened for a through pull request: %+v", s.Graft)
	}
}

func TestPullRequestSingleCherryPicksCleanly(t *testing.T) {
	a, fork, _ := prFixture(t, false)
	repoOf(t, request(a, "POST", "/api/repository/check", "{}"))
	s := repoOf(t, request(a, "GET", "/api/repository", ""))
	head := s.Local.Revision
	s = repoOf(t, request(a, "POST", "/api/repository/pr", `{"revision":"`+head+`","mode":"single"}`))
	if s.PullRequest == "" || s.Graft != nil {
		t.Fatalf("expected an immediate pull request, got %q / %+v", s.PullRequest, s.Graft)
	}
	branch := prBranchPrefix + "single-" + short(head)
	// The pushed commit is a replay onto upstream, so it carries only that one
	// change: upstream's own file is untouched by it.
	if got := gitOut(t, fork, "show", branch+":local.txt"); got != "local" {
		t.Fatalf("cherry-picked branch content: %q", got)
	}
	if got := gitOut(t, fork, "show", branch+":shared.txt"); got != "upstream" {
		t.Fatalf("the cherry-pick dragged local work along: %q", got)
	}
}

func TestPullRequestSingleResolvesItsConflict(t *testing.T) {
	a, fork, _ := prFixture(t, true)
	repoOf(t, request(a, "POST", "/api/repository/check", "{}"))
	s := repoOf(t, request(a, "GET", "/api/repository", ""))
	head := s.Local.Revision
	s = repoOf(t, request(a, "POST", "/api/repository/pr", `{"revision":"`+head+`","mode":"single"}`))
	if s.PullRequest != "" {
		t.Fatal("a conflicting cherry-pick opened a pull request anyway")
	}
	if s.Graft == nil || s.Graft.Kind != "pick" || s.Graft.PR == nil || s.Graft.PR.Mode != "single" {
		t.Fatalf("no cherry-pick waiting to be resolved: %+v", s.Graft)
	}
	if len(s.Graft.Files) != 1 || s.Graft.Files[0].Path != "shared.txt" {
		t.Fatalf("conflict misread: %+v", s.Graft.Files)
	}
	// A cherry-pick replays onto upstream, so the sides are the other way round
	// from a merge: "theirs" is the commit being proposed.
	if s.Graft.OursLabel != "Upstream "+defaultBranch || s.Graft.TheirsLabel != "This commit" {
		t.Fatalf("sides mislabelled: %q / %q", s.Graft.OursLabel, s.Graft.TheirsLabel)
	}
	// Resolving a cherry-pick must not touch the checkout either.
	before := gitOut(t, a.repo, "rev-parse", "HEAD")
	repoOf(t, request(a, "POST", "/api/repository/graft/resolve", `{"path":"shared.txt","choice":"theirs","content":""}`))
	s = repoOf(t, request(a, "POST", "/api/repository/graft/apply", "{}"))
	if s.PullRequest == "" {
		t.Fatal("applying the resolved cherry-pick opened no pull request")
	}
	if s.Graft != nil {
		t.Fatalf("the cherry-pick outlived its apply: %+v", s.Graft)
	}
	if got := gitOut(t, a.repo, "rev-parse", "HEAD"); got != before {
		t.Fatal("a pull request moved the checkout")
	}
	branch := prBranchPrefix + "single-" + short(head)
	if got := gitOut(t, fork, "show", branch+":shared.txt"); got != "local" {
		t.Fatalf("resolution not published: %q", got)
	}
}

func TestPullRequestRefusesAnUnlistedRevision(t *testing.T) {
	a, _, _ := prFixture(t, false)
	for _, body := range []string{
		`{"revision":"0000000000000000000000000000000000000000","mode":"through"}`,
		`{"revision":"HEAD","mode":"through"}`,
	} {
		if w := request(a, "POST", "/api/repository/pr", body); w.Code != 400 {
			t.Fatalf("%s returned %d", body, w.Code)
		}
	}
	s := repoOf(t, request(a, "GET", "/api/repository", ""))
	if w := request(a, "POST", "/api/repository/pr", `{"revision":"`+s.Local.Revision+`","mode":"rebase"}`); w.Code != 400 {
		t.Fatalf("unknown mode returned %d", w.Code)
	}
}

// A checkout fully pushed to its fork is still every one of those commits
// ahead of upstream. The two counts are not interchangeable, and the Push
// button reads the fork one.
func TestForkCountsAreSeparateFromUpstreamCounts(t *testing.T) {
	a := testApp(t)
	upstream := t.TempDir()
	gitFixture(t, upstream, "init", "-b", defaultBranch)
	putFile(t, filepath.Join(upstream, "2.12", "Makefile"), "all:\n")
	gitFixture(t, upstream, "add", ".")
	gitFixture(t, upstream, "commit", "-m", "first")
	fork := t.TempDir()
	gitFixture(t, t.TempDir(), "clone", "--bare", upstream, fork)
	gitFixture(t, t.TempDir(), "clone", fork, a.repo)
	a.upstreamFrom = upstream

	s := repoOf(t, request(a, "POST", "/api/repository/check", "{}"))
	if s.Ahead != 0 || !s.ForkTracked || s.ForkAhead != 0 {
		t.Fatalf("a fresh clone is ahead of nothing: %+v", s)
	}
	putFile(t, filepath.Join(a.repo, "work.txt"), "work\n")
	gitFixture(t, a.repo, "add", ".")
	gitFixture(t, a.repo, "commit", "-m", "local work")
	s = repoOf(t, request(a, "GET", "/api/repository", ""))
	if s.Ahead != 1 || s.ForkAhead != 1 {
		t.Fatalf("an unpushed commit is ahead of both: ahead=%d forkAhead=%d", s.Ahead, s.ForkAhead)
	}
	gitFixture(t, a.repo, "push", originRemote, defaultBranch)
	s = repoOf(t, request(a, "GET", "/api/repository", ""))
	if s.Ahead != 1 {
		t.Fatalf("pushing to the fork does not reach upstream: ahead=%d", s.Ahead)
	}
	if s.ForkAhead != 0 || s.ForkBehind != 0 {
		t.Fatalf("a pushed commit is still reported as unpushed: forkAhead=%d forkBehind=%d", s.ForkAhead, s.ForkBehind)
	}
}

// Each box says how many commits a pull request opened from it would carry —
// its selected branch measured against upstream, not the checkout's own count.
func TestSourceAheadCountsWhatAPullRequestWouldCarry(t *testing.T) {
	a := forkFixture(t, false)
	s := repoOf(t, request(a, "GET", "/api/repository", ""))
	for _, src := range s.Sources {
		if src.Ahead != 0 {
			t.Fatalf("%s counts %d before upstream was ever read", src.Name, src.Ahead)
		}
	}
	s = repoOf(t, request(a, "POST", "/api/repository/check", "{}"))
	by := map[string]Source{}
	for _, src := range s.Sources {
		by[src.Name] = src
	}
	if by["local"].Ahead != 1 {
		t.Fatalf("the checkout carries one commit upstream lacks, got %d", by["local"].Ahead)
	}
	if by[upstreamRemoteName].Ahead != 0 {
		t.Fatalf("upstream cannot be ahead of itself: %d", by[upstreamRemoteName].Ahead)
	}
	// Switching the box to another branch moves the count with it.
	w := request(a, "GET", "/api/repository/commits?ref="+upstreamRefs[len("refs/remotes/"):]+"/"+defaultBranch, "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Ahead int `json:"ahead"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &got); e != nil || got.Ahead != 0 {
		t.Fatalf("upstream branch reported %d ahead (%v)", got.Ahead, e)
	}
}
