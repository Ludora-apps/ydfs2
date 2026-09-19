package main

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// The working tree of the checkout: what differs from HEAD, staged one file at
// a time, committed with a message, and pushed by the Push button that was
// already there.
//
// Everything here happens in the checkout itself, which is why every one of
// these handlers holds App.mu for the whole operation: a build submission
// snapshots the working tree, and it must never catch one halfway through a
// staging or a commit. A graft (see graft.go) is refused a commit rather than
// merged with one — it was prepared against the commit the checkout sits on,
// and moving HEAD out from under it would strand the resolution.

// maxChanges caps the listing. A checkout with a build tree left inside it can
// carry tens of thousands of untracked files, which no one is going to stage
// from a browser; the count of the rest is reported instead so the screen never
// claims the tree is smaller than it is.
const maxChanges = 500

// Change is one path git reports as different from HEAD. Index and Work are
// git's own status letters for the two sides — staged and unstaged — because a
// file can be both at once (staged edit, then edited again), and the UI offers
// the two diffs separately when it is.
type Change struct {
	Path string `json:"path"`
	From string `json:"from,omitempty"` // where a rename or copy came from
	// " " when that side has no change: M modified, A added, D deleted,
	// R renamed, C copied, T type changed, ? untracked, U unmerged.
	Index      string `json:"index"`
	Work       string `json:"work"`
	Staged     bool   `json:"staged"`
	Unstaged   bool   `json:"unstaged"`
	Untracked  bool   `json:"untracked"`
	Conflicted bool   `json:"conflicted"`
}

// changes reads the working tree. Untracked files are listed individually
// (--untracked-files=all) rather than collapsed into their directory, since a
// directory is not something that can be staged file by file or diffed.
func (a *App) changes(ctx context.Context) ([]Change, int, error) {
	out, e := a.gitRaw(ctx, a.repo, nil, "status", "--porcelain", "-z", "--untracked-files=all")
	if e != nil {
		return nil, 0, e
	}
	// -z gives "XY path\0", with a rename's original path as its own field
	// straight after the entry that names it.
	fields := strings.Split(string(out), "\x00")
	var list []Change
	more := 0
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		x, y := entry[0], entry[1]
		c := Change{Path: entry[3:], Index: string(x), Work: string(y)}
		if x == 'R' || x == 'C' {
			if i+1 < len(fields) {
				i++
				c.From = fields[i]
			}
		}
		switch {
		case x == '?':
			c.Untracked, c.Unstaged = true, true
		case x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D'):
			// An unmerged path, from a merge run at the command line: it can
			// still be staged once its markers are settled there.
			c.Conflicted, c.Unstaged = true, true
		default:
			c.Staged, c.Unstaged = x != ' ', y != ' '
		}
		if len(list) >= maxChanges {
			more++
			continue
		}
		list = append(list, c)
	}
	return list, more, nil
}

// changed finds one path in the listing. The browser's list is never
// authoritative: a path is only ever acted on if git itself just reported it,
// the same rule offered() applies to revisions.
func changed(list []Change, path string) *Change {
	for i := range list {
		if list[i].Path == path {
			return &list[i]
		}
	}
	return nil
}

// stagedAnything reports whether the index carries anything a commit would
// record. It asks git rather than counting the listing, which is capped: with
// a very dirty tree the staged file may be one of the ones not listed.
func (a *App) stagedAnything(ctx context.Context) (bool, error) {
	e := a.gitCmd(ctx, a.repo, nil, "diff", "--cached", "--quiet").Run()
	if e == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if errors.As(e, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}
	return false, e
}

// pathspec is what a git command is given for one change: the path, plus the
// original name of a rename, so a diff shows the move and an unstage undoes
// both halves of it.
func (c *Change) pathspec() []string {
	if c.From != "" {
		return []string{c.Path, c.From}
	}
	return []string{c.Path}
}

// diffText runs a diff, tolerating the exit status git uses to say the two
// sides differ — which is the whole point of asking. Anything else is a real
// failure and is reported as one.
func (a *App) diffText(ctx context.Context, args ...string) ([]byte, error) {
	cmd := a.gitCmd(ctx, a.repo, nil, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, e := cmd.Output()
	if e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) && exit.ExitCode() == 1 {
			return out, nil
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return nil, errors.New(s)
		}
		return nil, e
	}
	return out, nil
}

// repositoryDiff serves one file's changes as plain text, for the same viewer
// the build log, config.ini and a conflicting file use.
//
// side picks which half: "staged" is what a commit would record, "worktree"
// what it would leave behind, and the default is everything since HEAD — which
// for an untracked file is the file itself, diffed against nothing.
func (a *App) repositoryDiff(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	path, side := r.URL.Query().Get("path"), r.URL.Query().Get("side")
	list, _, e := a.changes(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	c := changed(list, path)
	if c == nil {
		fail(w, 404, "that file has no changes")
		return
	}
	var args []string
	switch side {
	case "staged":
		args = append([]string{"diff", "--no-color", "--cached", "--"}, c.pathspec()...)
	case "worktree":
		args = append([]string{"diff", "--no-color", "--"}, c.pathspec()...)
	case "", "all":
		if c.Untracked {
			// Nothing to compare against: show the whole file as an addition.
			args = []string{"diff", "--no-color", "--no-index", "--", "/dev/null", c.Path}
		} else {
			args = append([]string{"diff", "--no-color", "HEAD", "--"}, c.pathspec()...)
		}
	default:
		fail(w, 400, "unknown side")
		return
	}
	out, e := a.diffText(ctx, args...)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	if len(out) == 0 {
		out = []byte("No differences to show.\n")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(out)
}

// repositoryStage moves files in and out of the index. all covers the ones the
// listing had to truncate, so "Stage everything" means everything even when the
// screen could not show it all.
func (a *App) repositoryStage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var body struct {
		Paths []string `json:"paths"`
		All   bool     `json:"all"`
		Stage bool     `json:"stage"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Held for the whole staging so a submission cannot snapshot a tree with
	// half of it staged — and so two browsers cannot stage over each other.
	a.mu.Lock()
	defer a.mu.Unlock()
	code, e := func() (int, error) {
		list, _, e := a.changes(ctx)
		if e != nil {
			return 500, e
		}
		var paths []string
		if body.All {
			paths = []string{"."}
		} else {
			for _, p := range body.Paths {
				c := changed(list, p)
				if c == nil {
					return 400, errors.New("no change to stage in " + p)
				}
				paths = append(paths, c.pathspec()...)
			}
		}
		if len(paths) == 0 {
			return 400, errors.New("no files given")
		}
		if body.Stage {
			// "add" records deletions as well as edits, so one command covers
			// every kind of change a file can be in.
			args := append([]string{"add", "--"}, paths...)
			if _, e := a.git(ctx, args...); e != nil {
				return 409, errors.New("cannot stage: " + e.Error())
			}
			return 0, nil
		}
		// "restore --staged" only ever touches the index: whatever is in the
		// working tree stays exactly as it is, so nothing can be lost here.
		args := append([]string{"restore", "--staged", "--"}, paths...)
		if _, e := a.git(ctx, args...); e != nil {
			return 409, errors.New("cannot unstage: " + e.Error())
		}
		return 0, nil
	}()
	if e != nil {
		fail(w, code, e.Error())
		return
	}
	a.respondRepository(w, ctx, "")
}

// maxMessage is a generous ceiling on a commit message: long enough for a real
// body, short enough that no one can post a novel through this form.
const maxMessage = 5000

// repositoryCommit records the staged files. Only the index is committed —
// never "commit -a" — so what a person ticked on the screen is exactly what
// lands in the commit.
func (a *App) repositoryCommit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var body struct {
		Message string `json:"message"`
	}
	if !decode(w, r, &body) {
		return
	}
	message := strings.TrimSpace(body.Message)
	if message == "" {
		fail(w, 400, "a commit needs a message")
		return
	}
	if len(message) > maxMessage {
		fail(w, 400, "that commit message is too long")
		return
	}
	// Held for the whole commit so a submission cannot snapshot the tree while
	// HEAD is moving. Lock order: mu then graftMu.
	a.mu.Lock()
	defer a.mu.Unlock()
	code, e := func() (int, error) {
		a.graftMu.Lock()
		open := a.graft != nil
		a.graftMu.Unlock()
		if open {
			// It was prepared against the commit the checkout sits on; moving
			// HEAD now would leave it unappliable (see applyMerge's guard).
			return 409, errors.New("a merge is being resolved; finish or abandon it before committing")
		}
		staged, e := a.stagedAnything(ctx)
		if e != nil {
			return 500, e
		}
		if !staged {
			return 409, errors.New("nothing is staged; tick the files to commit first")
		}
		args := append([]string{}, a.mergeIdentity(ctx)...)
		args = append(args, "commit", "-m", message)
		if _, e := a.git(ctx, args...); e != nil {
			return 409, errors.New("commit failed: " + e.Error())
		}
		return 0, nil
	}()
	if e != nil {
		fail(w, code, e.Error())
		return
	}
	a.respondRepository(w, ctx, "")
}
