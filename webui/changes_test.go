package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func changeOf(t *testing.T, s *Repository, path string) Change {
	t.Helper()
	if c := changed(s.Changes, path); c != nil {
		return *c
	}
	t.Fatalf("no change for %q in %+v", path, s.Changes)
	return Change{}
}

// TestWorkingTreeStagesAndCommits walks the whole screen: a tracked edit and an
// untracked file are listed, staged one at a time, and committed — after which
// the checkout is no longer reported dirty for the part that was committed.
func TestWorkingTreeStagesAndCommits(t *testing.T) {
	a := forkFixture(t, false)
	putFile(t, filepath.Join(a.repo, "shared.txt"), "edited\n")
	putFile(t, filepath.Join(a.repo, "new.txt"), "brand new\n")

	s := repoOf(t, request(a, "GET", "/api/repository", ""))
	if !s.Dirty || len(s.Changes) != 2 || s.MoreChanges != 0 {
		t.Fatalf("unexpected working tree %+v", s.Changes)
	}
	if c := changeOf(t, s, "shared.txt"); c.Staged || !c.Unstaged || c.Untracked || c.Work != "M" {
		t.Fatalf("unexpected tracked change %+v", c)
	}
	if c := changeOf(t, s, "new.txt"); !c.Untracked || c.Staged {
		t.Fatalf("unexpected untracked change %+v", c)
	}
	// Both diffs are readable before anything is staged: an edit against HEAD,
	// an untracked file against nothing.
	for _, f := range []struct{ path, want string }{
		{"shared.txt", "+edited"},
		{"new.txt", "+brand new"},
	} {
		w := request(a, "GET", "/api/repository/diff?path="+f.path, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), f.want) {
			t.Fatalf("diff of %s: %d %s", f.path, w.Code, w.Body)
		}
	}
	// Nothing staged yet, so there is nothing to commit.
	if w := request(a, "POST", "/api/repository/commit", `{"message":"nothing"}`); w.Code != 409 {
		t.Fatalf("empty commit: %d %s", w.Code, w.Body)
	}
	s = repoOf(t, request(a, "POST", "/api/repository/stage", `{"paths":["shared.txt"],"stage":true}`))
	if c := changeOf(t, s, "shared.txt"); !c.Staged || c.Unstaged || c.Index != "M" {
		t.Fatalf("not staged: %+v", c)
	}
	// The staged half is now its own diff, and the unstaged half is empty.
	if w := request(a, "GET", "/api/repository/diff?path=shared.txt&side=staged", ""); !strings.Contains(w.Body.String(), "+edited") {
		t.Fatalf("staged diff: %d %s", w.Code, w.Body)
	}
	if w := request(a, "GET", "/api/repository/diff?path=shared.txt&side=worktree", ""); !strings.Contains(w.Body.String(), "No differences") {
		t.Fatalf("worktree diff: %d %s", w.Code, w.Body)
	}
	// Unstaging only ever touches the index: the edit itself survives it.
	s = repoOf(t, request(a, "POST", "/api/repository/stage", `{"paths":["shared.txt"],"stage":false}`))
	if c := changeOf(t, s, "shared.txt"); c.Staged || !c.Unstaged {
		t.Fatalf("not unstaged: %+v", c)
	}
	if got := read(t, filepath.Join(a.repo, "shared.txt")); got != "edited\n" {
		t.Fatalf("unstaging changed the file: %q", got)
	}
	request(a, "POST", "/api/repository/stage", `{"paths":["shared.txt"],"stage":true}`)
	if w := request(a, "POST", "/api/repository/commit", `{"message":"  "}`); w.Code != 400 {
		t.Fatalf("blank message: %d %s", w.Code, w.Body)
	}
	s = repoOf(t, request(a, "POST", "/api/repository/commit", `{"message":"edit shared"}`))
	if s.Local.Subject != "edit shared" {
		t.Fatalf("commit not recorded: %+v", s.Local)
	}
	// Only what was staged went in: the untracked file is still waiting.
	if len(s.Changes) != 1 || !s.Changes[0].Untracked || s.Changes[0].Path != "new.txt" {
		t.Fatalf("unexpected tree after commit %+v", s.Changes)
	}
	if !s.Dirty {
		t.Fatal("an untracked file must still count as dirty")
	}
	// And committing the rest leaves a clean checkout.
	request(a, "POST", "/api/repository/stage", `{"all":true,"stage":true}`)
	s = repoOf(t, request(a, "POST", "/api/repository/commit", `{"message":"add new"}`))
	if s.Dirty || len(s.Changes) != 0 {
		t.Fatalf("checkout still dirty: %+v", s.Changes)
	}
	if got := gitOut(t, a.repo, "log", "-1", "--format=%s"); got != "add new" {
		t.Fatalf("unexpected HEAD %q", got)
	}
}

// A path git has not just reported cannot be staged, exactly as a revision the
// boxes did not list cannot be checked out.
func TestStagingRefusesAnUnlistedPath(t *testing.T) {
	a := forkFixture(t, false)
	for _, p := range []string{"2.12/Makefile", "../outside", "shared.txt"} {
		if w := request(a, "POST", "/api/repository/stage", `{"paths":["`+p+`"],"stage":true}`); w.Code != 400 {
			t.Fatalf("staging %s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := request(a, "GET", "/api/repository/diff?path=2.12/Makefile", ""); w.Code != 404 {
		t.Fatalf("diff of an unchanged file: %d %s", w.Code, w.Body)
	}
}

// Committing while a merge is being resolved would move HEAD out from under it
// and strand the resolution, so it is refused rather than allowed to.
func TestCommitRefusedWhileAMergeIsOpen(t *testing.T) {
	a := forkFixture(t, true)
	if w := request(a, "POST", "/api/repository/merge/preview", ""); w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	putFile(t, filepath.Join(a.repo, "new.txt"), "later\n")
	request(a, "POST", "/api/repository/stage", `{"paths":["new.txt"],"stage":true}`)
	w := request(a, "POST", "/api/repository/commit", `{"message":"during a merge"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "merge") {
		t.Fatalf("commit during a graft: %d %s", w.Code, w.Body)
	}
}

// A rename is one change with two paths, and both of them have to move
// together — staging the new name alone would leave the old one deleted.
func TestRenamesCarryBothPaths(t *testing.T) {
	a := forkFixture(t, false)
	if e := os.Rename(filepath.Join(a.repo, "shared.txt"), filepath.Join(a.repo, "moved.txt")); e != nil {
		t.Fatal(e)
	}
	s := repoOf(t, request(a, "POST", "/api/repository/stage", `{"all":true,"stage":true}`))
	c := changeOf(t, s, "moved.txt")
	if c.From != "shared.txt" || c.Index != "R" {
		t.Fatalf("rename not reported as one change: %+v", s.Changes)
	}
	s = repoOf(t, request(a, "POST", "/api/repository/stage", `{"paths":["moved.txt"],"stage":false}`))
	if len(s.Changes) != 2 {
		t.Fatalf("unstaging a rename left half of it staged: %+v", s.Changes)
	}
}

// fakeGhPulls answers "gh api .../pulls" with a fixed listing: two open pull
// requests from the fork and one from somewhere else.
func fakeGhPulls(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	body := `[{"number":12,"title":"fix mate build","html_url":"https://github.com/upstream/ydfs2/pull/12","draft":false,` +
		`"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-02T10:00:00Z","user":{"login":"boyquotes"},` +
		`"head":{"ref":"ydfs-web/single-abc","repo":{"full_name":"Ludora-apps/ydfs2"}},"base":{"ref":"2.12"}},` +
		`{"number":11,"title":"someone else","html_url":"https://github.com/upstream/ydfs2/pull/11","draft":false,` +
		`"created_at":"2026-08-01T10:00:00Z","updated_at":"2026-08-02T10:00:00Z","user":{"login":"other"},` +
		`"head":{"ref":"topic","repo":{"full_name":"other/ydfs2"}},"base":{"ref":"2.12"}},` +
		`{"number":10,"title":"deleted fork","html_url":"https://github.com/upstream/ydfs2/pull/10","draft":true,` +
		`"created_at":"2026-07-01T10:00:00Z","updated_at":"2026-07-02T10:00:00Z","user":{"login":"ghost"},` +
		`"head":{"ref":"gone","repo":null},"base":{"ref":"2.12"}}]`
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then echo faketoken; exit 0; fi\ncat <<'JSON'\n" + body + "\nJSON\n"
	if e := os.WriteFile(p, []byte(script), 0o755); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestOpenPullRequestsListOnlyTheFork(t *testing.T) {
	a, _, _ := prFixture(t, false)
	a.gh = fakeGhPulls(t)
	// repoLabel of the fork fixture is a temporary directory; name it the way
	// the listing does so the filter has something to match.
	gitFixture(t, a.repo, "remote", "set-url", originRemote, "https://github.com/Ludora-apps/ydfs2.git")
	w := request(a, "GET", "/api/repository/pulls", "")
	if w.Code != 200 {
		t.Fatalf("pulls: %d %s", w.Code, w.Body)
	}
	var p PullRequests
	if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil {
		t.Fatal(e)
	}
	if len(p.Pulls) != 1 || p.Pulls[0].Number != 12 || p.Pulls[0].Head != "ydfs-web/single-abc" {
		t.Fatalf("unexpected listing %+v", p.Pulls)
	}
	if p.Others != 2 {
		t.Fatalf("pull requests from elsewhere not counted: %+v", p)
	}
	if p.CheckedAt == "" || p.Error != "" {
		t.Fatalf("unexpected metadata %+v", p)
	}
}

// Without an authenticated gh there is nothing to list, and the box has to say
// why rather than look empty.
func TestOpenPullRequestsReportWhyTheyCannotBeListed(t *testing.T) {
	a := forkFixture(t, false)
	w := request(a, "GET", "/api/repository/pulls", "")
	var p PullRequests
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Error == "" {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body)
	}
}
