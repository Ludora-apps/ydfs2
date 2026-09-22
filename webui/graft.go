package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A graft is one merge or cherry-pick in progress, held open across several
// requests so its conflicts can be resolved a file at a time from the browser.
//
// None of it happens in the checkout. git replays the commit inside a linked
// worktree under <data>/graft, detached on the commit the checkout was sitting
// on; the checkout only ever moves at the very end, by fast-forwarding onto the
// commit the worktree produced. That is what lets the Repository screen keep
// its promise — a resolution that is abandoned, or a server that is restarted
// mid-merge, leaves the working tree exactly as it was — and it is why a
// conflict can never wedge a build: the build worker snapshots the checkout,
// which no part of this touches.
//
// One at a time, guarded by App.graftMu. Where both locks are needed the order
// is App.mu then App.graftMu, the same rule as App.vmMu.

const graftDirName = "graft"

// Conflict is one file git could not merge. Ours/Theirs say which sides still
// exist: a file deleted on one side has no version to keep there, so the UI
// offers "keep the deletion" rather than a version.
type Conflict struct {
	Path string `json:"path"`
	// "content" | "add/add" | "modify/delete" | "delete/modify"
	Kind     string `json:"kind"`
	Ours     bool   `json:"ours"`
	Theirs   bool   `json:"theirs"`
	Binary   bool   `json:"binary"`
	Resolved string `json:"resolved"` // "" | "ours" | "theirs" | "both" | "edited"
}

type Graft struct {
	Kind     string `json:"kind"`     // "merge" | "pick"
	Base     string `json:"base"`     // the commit the replay starts from
	Branch   string `json:"branch"`   // the checkout's branch when it opened
	Revision string `json:"revision"` // what is being merged or picked
	Subject  string `json:"subject"`
	// Clean means git applied it with no conflict at all: the worktree already
	// carries the commit, and applying it is a fast-forward away.
	Clean bool       `json:"clean"`
	Files []Conflict `json:"files"`
	// OursLabel and TheirsLabel name the two sides for a person. They swap
	// between the two kinds: a merge replays upstream onto the checkout, so
	// "ours" is the checkout; a cherry-pick replays one of your commits onto
	// upstream, so "ours" is upstream. Git's stage numbers never move, and the
	// browser must not be left guessing which is which.
	OursLabel   string `json:"oursLabel"`
	TheirsLabel string `json:"theirsLabel"`
	OpenedAt    string `json:"openedAt"`
	// PR is set on a "pick": the pull request this resolution is on its way to.
	PR *PRPlan `json:"pr,omitempty"`
}

func (g *Graft) pending() int {
	n := 0
	for _, c := range g.Files {
		if c.Resolved == "" {
			n++
		}
	}
	return n
}
func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
func (a *App) graftDir() string { return filepath.Join(a.data, graftDirName) }

// worktree prepares <data>/graft detached on at. It is reused from one
// resolution to the next: "git worktree add" copies the whole tree (~200 MB
// for this repository), while resetting the one already there costs nothing.
func (a *App) worktree(ctx context.Context, at string) (string, error) {
	dir := a.graftDir()
	if _, e := os.Stat(filepath.Join(dir, ".git")); e == nil {
		// Whatever the previous session left: an unfinished merge or
		// cherry-pick, resolved files, untracked leftovers. Each of these
		// fails harmlessly when there is nothing of that kind to undo.
		a.gitAt(ctx, dir, "merge", "--abort")
		a.gitAt(ctx, dir, "cherry-pick", "--abort")
		if _, e := a.gitAt(ctx, dir, "checkout", "--detach", "--force", at); e == nil {
			if _, e := a.gitAt(ctx, dir, "reset", "--hard", at); e == nil {
				a.gitAt(ctx, dir, "clean", "-ffdx")
				return dir, nil
			}
		}
	}
	a.discardWorktree(ctx)
	if _, e := a.git(ctx, "worktree", "add", "--detach", dir, at); e != nil {
		return "", errors.New("cannot prepare a merge worktree: " + e.Error())
	}
	return dir, nil
}
func (a *App) discardWorktree(ctx context.Context) {
	dir := a.graftDir()
	a.git(ctx, "worktree", "remove", "--force", dir)
	os.RemoveAll(dir)
	a.git(ctx, "worktree", "prune")
}

// reapGrafts drops a resolution left over from an earlier run: its worktree is
// hundreds of megabytes and it belongs to a browser session nobody can reach
// any more. Sessions are ephemeral, never adopted — the same rule as reapVMs.
func (a *App) reapGrafts(ctx context.Context) {
	a.graftMu.Lock()
	defer a.graftMu.Unlock()
	a.graft = nil
	a.discardWorktree(ctx)
}

// conflicts reads the unmerged index: one entry per side git could not
// reconcile, stage 1 being the common ancestor, 2 ours and 3 theirs.
func (a *App) conflicts(ctx context.Context, dir string) ([]Conflict, error) {
	out, e := a.gitRaw(ctx, dir, nil, "ls-files", "-u", "-z")
	if e != nil {
		return nil, e
	}
	stages := map[string]map[string]bool{}
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		meta, path, ok := strings.Cut(entry, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, errors.New("cannot read the conflicting files")
		}
		if stages[path] == nil {
			stages[path] = map[string]bool{}
		}
		stages[path][f[2]] = true
	}
	paths := make([]string, 0, len(stages))
	for p := range stages {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	list := make([]Conflict, 0, len(paths))
	for _, p := range paths {
		st := stages[p]
		c := Conflict{Path: p, Ours: st["2"], Theirs: st["3"]}
		switch {
		case st["1"] && st["2"] && st["3"]:
			c.Kind = "content"
		case st["2"] && st["3"]:
			c.Kind = "add/add"
		case st["2"]:
			c.Kind = "modify/delete" // kept here, deleted on the other side
		default:
			c.Kind = "delete/modify" // deleted here, changed on the other side
		}
		c.Binary = a.binaryConflict(ctx, dir, p, st)
		list = append(list, c)
	}
	return list, nil
}

// binaryConflict reports a file no conflict marker can describe, so the UI
// offers one whole side or the other and never "keep both" or an editor.
func (a *App) binaryConflict(ctx context.Context, dir, path string, st map[string]bool) bool {
	for _, stage := range []string{"2", "3"} {
		if !st[stage] {
			continue
		}
		b, e := a.gitRaw(ctx, dir, nil, "show", ":"+stage+":"+path)
		if e != nil {
			continue
		}
		if len(b) > 8000 {
			b = b[:8000]
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return true
		}
	}
	return false
}

// openGraft replays Revision on top of Base inside the worktree. A clean result
// is committed there and nothing else happens: applying it to the checkout is a
// separate, confirmed step. A conflicting one is left exactly as git left it,
// so its files can be resolved one at a time across several requests.
func (a *App) openGraft(ctx context.Context, g *Graft) error {
	dir, e := a.worktree(ctx, g.Base)
	if e != nil {
		return e
	}
	args := append([]string{}, a.mergeIdentity(ctx)...)
	if g.Kind == "pick" {
		g.OursLabel, g.TheirsLabel = "Upstream "+g.Branch, "This commit"
		args = append(args, "cherry-pick", g.Revision)
	} else {
		g.OursLabel, g.TheirsLabel = "This checkout", "Upstream"
		args = append(args, "merge", "--no-edit", "-m", "Merge upstream "+short(g.Revision)+" into "+g.Branch, g.Revision)
	}
	_, replayErr := a.gitAt(ctx, dir, args...)
	files, e := a.conflicts(ctx, dir)
	if e != nil {
		a.discardWorktree(ctx)
		return e
	}
	if replayErr != nil && len(files) == 0 {
		// Not a conflict but a plain failure — an empty cherry-pick, a revision
		// git cannot read. Report git's own words and leave nothing behind.
		a.discardWorktree(ctx)
		return replayErr
	}
	g.Files, g.Clean = files, len(files) == 0
	g.OpenedAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}

// writeInto writes one resolved file inside the worktree. The path comes from
// git's own conflict list, never from the browser, but it is still resolved
// through os.OpenRoot so no path can reach outside the worktree.
func writeInto(dir, path string, data []byte) error {
	root, e := os.OpenRoot(dir)
	if e != nil {
		return e
	}
	defer root.Close()
	f, e := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = f.Write(data)
	return e
}

// union keeps both sides of a text conflict: git's own union merge, which
// replays every hunk from both versions instead of choosing between them.
func (a *App) union(ctx context.Context, dir, path string) ([]byte, error) {
	tmp, e := os.MkdirTemp("", "ydfs-union")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(tmp)
	names := map[string]string{"2": "ours", "1": "base", "3": "theirs"}
	for stage, name := range names {
		// A file added on both sides has no common ancestor; an empty base is
		// exactly what a union of the two additions needs.
		blob, e := a.gitRaw(ctx, dir, nil, "show", ":"+stage+":"+path)
		if e != nil && stage != "1" {
			return nil, e
		}
		if e := os.WriteFile(filepath.Join(tmp, name), blob, 0o600); e != nil {
			return nil, e
		}
	}
	// --union leaves no conflict, so git exits 0; a non-zero status here is a
	// real failure (an unreadable or binary input).
	return a.gitRaw(ctx, dir, nil, "merge-file", "-p", "--union",
		filepath.Join(tmp, "ours"), filepath.Join(tmp, "base"), filepath.Join(tmp, "theirs"))
}

// resolveFile settles one conflicting file and stages the result. It never
// touches a path the open graft did not list.
func (a *App) resolveFile(ctx context.Context, g *Graft, path, choice, content string) error {
	var c *Conflict
	for i := range g.Files {
		if g.Files[i].Path == path {
			c = &g.Files[i]
		}
	}
	if c == nil {
		return errors.New("that file is not in conflict")
	}
	dir := a.graftDir()
	stage := map[string]string{"ours": "2", "theirs": "3"}[choice]
	switch choice {
	case "ours", "theirs":
		if (choice == "ours" && !c.Ours) || (choice == "theirs" && !c.Theirs) {
			// That side deleted the file: keeping it means keeping the deletion.
			if _, e := a.gitAt(ctx, dir, "rm", "-q", "-f", "--", path); e != nil {
				return e
			}
		} else {
			blob, e := a.gitRaw(ctx, dir, nil, "show", ":"+stage+":"+path)
			if e != nil {
				return e
			}
			if e := writeInto(dir, path, blob); e != nil {
				return e
			}
			if _, e := a.gitAt(ctx, dir, "add", "--", path); e != nil {
				return e
			}
		}
	case "both":
		if !c.Ours || !c.Theirs || c.Binary {
			return errors.New("keeping both sides needs a text conflict with a version on each side")
		}
		merged, e := a.union(ctx, dir, path)
		if e != nil {
			return e
		}
		if e := writeInto(dir, path, merged); e != nil {
			return e
		}
		if _, e := a.gitAt(ctx, dir, "add", "--", path); e != nil {
			return e
		}
	case "edited":
		if c.Binary {
			return errors.New("a binary file cannot be edited here; keep one side or the other")
		}
		if e := writeInto(dir, path, []byte(content)); e != nil {
			return e
		}
		if _, e := a.gitAt(ctx, dir, "add", "--", path); e != nil {
			return e
		}
	default:
		return errors.New("unknown resolution")
	}
	c.Resolved = choice
	return nil
}

// commitGraft turns the resolved worktree into a commit. A clean graft already
// has one: git committed it when the merge or the cherry-pick succeeded.
func (a *App) commitGraft(ctx context.Context, g *Graft) (string, error) {
	dir := a.graftDir()
	if !g.Clean {
		if n := g.pending(); n > 0 {
			return "", errors.New("resolve the remaining conflicting file(s) first")
		}
		args := append([]string{}, a.mergeIdentity(ctx)...)
		args = append(args, "commit", "--no-edit")
		if g.Kind == "pick" {
			// Resolving every hunk in favour of upstream can leave nothing to
			// commit; the pull request should still carry the (empty) commit
			// rather than fail at the last step.
			args = append(args, "--allow-empty")
		}
		if _, e := a.gitAt(ctx, dir, args...); e != nil {
			return "", e
		}
	}
	return a.gitAt(ctx, dir, "rev-parse", "HEAD")
}

// applyMerge fast-forwards the checkout onto the commit the worktree produced.
// Base is its first parent, so this is always a fast-forward — unless the
// checkout moved since the preview, which is exactly what the guard catches.
// The caller holds App.mu.
func (a *App) applyMerge(ctx context.Context, g *Graft) error {
	head, e := a.git(ctx, "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	if head != g.Base {
		return errors.New("the checkout moved to " + short(head) + " since this merge was prepared; check it again")
	}
	if status, e := a.git(ctx, "status", "--porcelain"); e == nil && status != "" {
		return errors.New("the checkout has uncommitted changes; commit or discard them before applying")
	}
	sha, e := a.commitGraft(ctx, g)
	if e != nil {
		return e
	}
	_, e = a.git(ctx, "merge", "--ff-only", sha)
	return e
}

func (a *App) repositoryMergePreview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
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
		head, e := a.git(ctx, "rev-parse", "HEAD")
		if e != nil {
			return 500, e
		}
		if status, e := a.git(ctx, "status", "--porcelain"); e == nil && status != "" {
			return 409, errors.New("the checkout has uncommitted changes; commit or discard them before merging")
		}
		if _, e := a.git(ctx, "merge-base", "--is-ancestor", up.Revision, head); e == nil {
			return 409, errors.New("upstream is already merged into this checkout")
		}
		g := &Graft{Kind: "merge", Base: head, Branch: a.branch(ctx), Revision: up.Revision, Subject: up.Subject}
		if e := a.openGraft(ctx, g); e != nil {
			a.graft = nil
			return 409, errors.New("cannot prepare the merge: " + e.Error())
		}
		a.graft = g
		return 0, nil
	}()
	if e != nil {
		fail(w, code, e.Error())
		return
	}
	a.respondRepository(w, ctx, "")
}

func (a *App) graftResolve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var body struct {
		Path    string `json:"path"`
		Choice  string `json:"choice"`
		Content string `json:"content"`
	}
	if !decode(w, r, &body) {
		return
	}
	code, e := func() (int, error) {
		a.graftMu.Lock()
		defer a.graftMu.Unlock()
		if a.graft == nil {
			return 409, errors.New("no merge is being resolved")
		}
		if e := a.resolveFile(ctx, a.graft, body.Path, body.Choice, body.Content); e != nil {
			return 400, e
		}
		return 0, nil
	}()
	if e != nil {
		fail(w, code, e.Error())
		return
	}
	a.respondRepository(w, ctx, "")
}

func (a *App) graftApply(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	// Held for the whole apply so a submission cannot snapshot a tree that is
	// halfway onto the merge commit. Lock order: mu then graftMu.
	a.mu.Lock()
	defer a.mu.Unlock()
	var url string
	code, e := func() (int, error) {
		a.graftMu.Lock()
		defer a.graftMu.Unlock()
		g := a.graft
		if g == nil {
			return 409, errors.New("no merge is being resolved")
		}
		if g.Kind == "pick" {
			u, e := a.finishPick(ctx, g)
			if e != nil {
				return 409, e
			}
			url = u
		} else if e := a.applyMerge(ctx, g); e != nil {
			return 409, e
		}
		a.graft = nil
		a.discardWorktree(ctx)
		return 0, nil
	}()
	if e != nil {
		fail(w, code, e.Error())
		return
	}
	a.respondRepository(w, ctx, url)
}

func (a *App) graftAbort(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	func() {
		a.graftMu.Lock()
		defer a.graftMu.Unlock()
		a.graft = nil
		a.discardWorktree(ctx)
	}()
	a.respondRepository(w, ctx, "")
}

// graftFile serves one version of one conflicting file as plain text, for the
// same viewer the build log and config.ini use.
func (a *App) graftFile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	path, side := r.URL.Query().Get("path"), r.URL.Query().Get("side")
	a.graftMu.Lock()
	defer a.graftMu.Unlock()
	g := a.graft
	if g == nil {
		fail(w, 409, "no merge is being resolved")
		return
	}
	var c *Conflict
	for i := range g.Files {
		if g.Files[i].Path == path {
			c = &g.Files[i]
		}
	}
	if c == nil {
		fail(w, 400, "that file is not in conflict")
		return
	}
	dir := a.graftDir()
	var out []byte
	var e error
	switch side {
	case "merged":
		// What git left in the worktree: the conflict markers a person reads.
		out, e = readInside(dir, path)
	case "ours":
		out, e = a.gitRaw(ctx, dir, nil, "show", ":2:"+path)
	case "theirs":
		out, e = a.gitRaw(ctx, dir, nil, "show", ":3:"+path)
	case "diff":
		out, e = a.gitRaw(ctx, dir, nil, "diff", "--no-color", "--", path)
	default:
		fail(w, 400, "unknown side")
		return
	}
	if e != nil {
		fail(w, 404, "that version does not exist")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(out)
}
func readInside(dir, path string) ([]byte, error) {
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	f, e := root.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return io.ReadAll(f)
}

// respondRepository answers every repository action with the whole screen, so
// the browser never has to stitch a partial update together. url, when a pull
// request was just opened, rides along on that same one shape.
func (a *App) respondRepository(w http.ResponseWriter, ctx context.Context, url string) {
	s, e := a.state(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	s.PullRequest = url
	respond(w, s)
}
