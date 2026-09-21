package main

// The workspace half of the AI Code Assistant: what the model is allowed to
// see, what it is allowed to propose, and how a proposal becomes a change.
//
// The model never touches the filesystem. It receives text this file
// assembled, and it answers with text this file parses. The only write is
// applyPatch, after the operator has read the diff and pressed Apply.
//
// Three boundaries do the security work:
//
//   - The workspace is the checkout and nothing else. Every path is
//     repository-relative and is opened through os.OpenRoot, the same guard
//     downloadArtifact and the VM console use, so neither "../../etc/passwd"
//     nor a symlink pointing out of the tree can resolve.
//   - A path is only ever acted on if git itself just listed it — the rule
//     offered() applies to revisions and changed() applies to working-tree
//     paths. The browser's list, and the model's, are never authoritative.
//   - git ls-files --exclude-standard is what enumerates the tree, so
//     .gitignore, .git/ and every build artifact the distro build leaves
//     behind are out of scope for free, and stay out as the ignore rules change.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits. The point of the context layer is that a repository is never sent
// whole: these are what "relevant context" means in bytes.
const (
	maxContextFiles = 24
	maxFileBytes    = 192 * 1024
	maxContextBytes = 480 * 1024
	maxListedFiles  = 4000
	maxSearchHits   = 120
	maxPatchFiles   = 20
	maxPatchBytes   = 512 * 1024
	maxErrorLines   = 60
	maxLogScan      = 4 << 20
)

// Extensions worth sending to a model, by the content this repository holds:
// build scripts, makefiles, C and C++ sources, init scripts, package lists and
// configuration. Anything else — images, archives, firmware blobs, compiled
// objects — is skipped whatever .gitignore says about it.
var textExtensions = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".h": true, ".hh": true, ".hpp": true,
	".s": true, ".asm": true, ".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".sh": true, ".bash": true, ".mk": true, ".make": true, ".am": true, ".ac": true, ".m4": true,
	".py": true, ".pl": true, ".rb": true, ".lua": true, ".css": true, ".html": true, ".htm": true,
	".json": true, ".yml": true, ".yaml": true, ".toml": true, ".ini": true, ".cfg": true,
	".conf": true, ".txt": true, ".md": true, ".patch": true, ".diff": true, ".desc": true,
	".rules": true, ".service": true, ".sql": true, ".xml": true,
}

// Files with no extension that are still source: the build system is full of
// them, and a package list is exactly the kind of file a fix has to change.
var textNames = map[string]bool{
	"Makefile": true, "makefile": true, "GNUmakefile": true, "Makefile-docker": true,
	"Dockerfile": true, "Dockerfile32": true, "configure": true, "config.ini": true,
	"build-envars": true, "rcS": true, "COPYING": true, "README": true, "BUGS": true, "TIPS": true,
}

func looksTextual(p string) bool {
	if textExtensions[strings.ToLower(filepath.Ext(p))] {
		return true
	}
	base := path.Base(p)
	if textNames[base] {
		return true
	}
	// packages/list-x86_64, init-x86/ydfs/enable/flatpak-preinstalled and the
	// other extensionless build inputs this distro is mostly made of.
	return filepath.Ext(base) == "" && (strings.HasPrefix(base, "list-") || strings.HasPrefix(base, "make_") || strings.Contains(p, "/scripts/") || strings.Contains(p, "/init-"))
}

// workspacePath turns a client- or model-supplied string into a path that is
// safe to join, or rejects it. It refuses absolute paths, traversal, and
// anything that does not stay inside the workspace once cleaned; os.OpenRoot
// enforces the same thing again at open time, symlinks included.
func workspacePath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "", errors.New("no path given")
	}
	if len(p) > 1024 {
		return "", errors.New("that path is too long")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("invalid path")
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || (len(p) > 1 && p[1] == ':') {
		return "", fmt.Errorf("%q is outside the workspace", p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q is outside the workspace", p)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return "", errors.New("the git directory is not part of the workspace")
	}
	return clean, nil
}

// workspaceFiles lists what the assistant may see. --exclude-standard applies
// .gitignore, .git/info/exclude and the global excludes, so build output under
// the checkout is never listed; --cached --others adds untracked working files
// without adding ignored ones.
func (a *App) workspaceFiles(ctx context.Context) ([]string, error) {
	out, e := a.gitRaw(ctx, a.repo, nil, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if e != nil {
		return nil, errors.New("cannot list the workspace: " + e.Error())
	}
	var list []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" || !looksTextual(p) {
			continue
		}
		list = append(list, p)
		if len(list) >= maxListedFiles {
			break
		}
	}
	sort.Strings(list)
	return list, nil
}

// offeredFile is the workspace equivalent of offered() and changed(): a path is
// only usable if this listing, taken now, actually contains it.
func offeredFile(list []string, p string) bool {
	for _, f := range list {
		if f == p {
			return true
		}
	}
	return false
}

// readWorkspaceFile reads one file through the root guard. Binary content and
// anything oversized is refused rather than truncated into nonsense.
func (a *App) readWorkspaceFile(rel string) (string, error) {
	clean, e := workspacePath(rel)
	if e != nil {
		return "", e
	}
	root, e := os.OpenRoot(a.repo)
	if e != nil {
		return "", errors.New("the workspace is unavailable")
	}
	defer root.Close()
	f, e := root.Open(clean)
	if e != nil {
		return "", fmt.Errorf("cannot read %s", clean)
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", clean)
	}
	if st.Size() > maxFileBytes {
		return "", fmt.Errorf("%s is larger than the %d KiB context limit", clean, maxFileBytes/1024)
	}
	b := make([]byte, st.Size())
	n, _ := f.Read(b)
	b = b[:n]
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return "", fmt.Errorf("%s is not a text file", clean)
	}
	return string(b), nil
}

func (a *App) fileExists(rel string) (bool, os.FileInfo) {
	root, e := os.OpenRoot(a.repo)
	if e != nil {
		return false, nil
	}
	defer root.Close()
	st, e := root.Stat(rel)
	if e != nil {
		return false, nil
	}
	return st.Mode().IsRegular(), st
}

// ------------------------------------------------------------- the file tools

// aiFiles is list_files and search_files in one read: the tree the assistant
// may see, narrowed by a substring, plus how much was left out.
func (a *App) aiFiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, e := a.workspaceFiles(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	shown := []string{}
	more := 0
	for _, p := range list {
		if q != "" && !strings.Contains(strings.ToLower(p), q) {
			continue
		}
		if len(shown) >= 400 {
			more++
			continue
		}
		shown = append(shown, p)
	}
	respond(w, map[string]any{"files": shown, "more": more, "total": len(list)})
}

// aiSearch is search_files: git grep, which is already restricted to tracked
// and non-ignored content and skips binaries with -I.
func (a *App) aiSearch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		fail(w, 400, "type at least two characters to search")
		return
	}
	if len(q) > 200 {
		fail(w, 400, "that search is too long")
		return
	}
	hits, e := a.searchWorkspace(ctx, q)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	respond(w, map[string]any{"hits": hits})
}

type SearchHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func (a *App) searchWorkspace(ctx context.Context, q string) ([]SearchHit, error) {
	// --fixed-strings: the query is a literal, never a pattern the operator
	// has to escape, and never one that can be made to backtrack.
	cmd := a.gitCmd(ctx, a.repo, nil, "grep", "--no-color", "-I", "--fixed-strings", "--line-number", "--untracked", "--max-count", "5", "-e", q)
	out, _ := cmd.Output() // exit status 1 simply means no match
	hits := []SearchHit{}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		n, e := strconv.Atoi(parts[1])
		if e != nil || !looksTextual(parts[0]) {
			continue
		}
		text := parts[2]
		if len(text) > 300 {
			text = text[:300] + "…"
		}
		hits = append(hits, SearchHit{Path: parts[0], Line: n, Text: text})
		if len(hits) >= maxSearchHits {
			break
		}
	}
	return hits, nil
}

// aiFile is read_file, served as plain text for the same viewer the build log,
// config.ini and a conflicting file already use.
func (a *App) aiFile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rel, e := workspacePath(r.URL.Query().Get("path"))
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	list, e := a.workspaceFiles(ctx)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	if !offeredFile(list, rel) {
		fail(w, 404, "that file is not part of the workspace")
		return
	}
	text, e := a.readWorkspaceFile(rel)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(text))
}

// -------------------------------------------------------------- the context

// buildContext assembles what the model is shown. Only what was asked for: the
// files ticked, a search if one was typed, the working-tree diff if it was
// wanted, and the errors of one named build. Never the repository.
func (a *App) buildContext(ctx context.Context, files []string, search, buildID string, includeDiff bool) (string, string, error) {
	var b strings.Builder
	var notes []string
	budget := maxContextBytes
	if len(files) > maxContextFiles {
		return "", "", fmt.Errorf("at most %d files can be sent as context at once", maxContextFiles)
	}
	if len(files) > 0 {
		list, e := a.workspaceFiles(ctx)
		if e != nil {
			return "", "", e
		}
		b.WriteString("# Selected files\n\n")
		for _, f := range files {
			rel, e := workspacePath(f)
			if e != nil {
				return "", "", e
			}
			if !offeredFile(list, rel) {
				return "", "", fmt.Errorf("%s is not part of the workspace", rel)
			}
			text, e := a.readWorkspaceFile(rel)
			if e != nil {
				return "", "", e
			}
			if len(text) > budget {
				notes = append(notes, rel+" was left out: the context budget is full")
				continue
			}
			budget -= len(text)
			fmt.Fprintf(&b, "## %s\n\n```\n%s\n```\n\n", rel, text)
		}
	}
	if search != "" {
		hits, e := a.searchWorkspace(ctx, search)
		if e == nil && len(hits) > 0 {
			fmt.Fprintf(&b, "# Search results for %q\n\n", search)
			for _, h := range hits {
				fmt.Fprintf(&b, "%s:%d: %s\n", h.Path, h.Line, h.Text)
			}
			b.WriteString("\n")
		}
	}
	if includeDiff {
		out, e := a.diffText(ctx, "diff", "--no-color", "HEAD")
		if e == nil && len(out) > 0 {
			d := string(out)
			if len(d) > budget {
				d = d[:budget] + "\n… diff truncated\n"
				notes = append(notes, "the working-tree diff was truncated to fit the context budget")
			}
			budget -= len(d)
			fmt.Fprintf(&b, "# Uncommitted changes (git diff HEAD)\n\n```diff\n%s\n```\n\n", d)
		}
	}
	if buildID != "" {
		errs, e := a.compilerErrors(buildID)
		if e != nil {
			return "", "", e
		}
		if errs.Text != "" {
			fmt.Fprintf(&b, "# Build %s failed\n\nTarget: %s. Exit status: %s.\n\n```\n%s\n```\n\n", errs.JobID, errs.Target, errs.Exit, errs.Text)
			// The files the compiler itself named, which is the cheapest
			// possible way to pick the right context for a failure.
			for _, f := range errs.Files {
				if containsString(files, f) {
					continue
				}
				text, e := a.readWorkspaceFile(f)
				if e != nil || len(text) > budget {
					continue
				}
				budget -= len(text)
				fmt.Fprintf(&b, "## %s (named by the compiler)\n\n```\n%s\n```\n\n", f, text)
			}
		}
	}
	note := strings.Join(notes, "; ")
	return strings.TrimSpace(b.String()), note, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ------------------------------------------------------- compiler error loop

// CompilerErrors is what a failed build tells the assistant: the diagnostic
// lines themselves and the workspace files they named.
type CompilerErrors struct {
	JobID  string   `json:"jobId"`
	Target string   `json:"target"`
	Exit   string   `json:"exit"`
	State  string   `json:"state"`
	Text   string   `json:"text"`
	Files  []string `json:"files"`
	Count  int      `json:"count"`
}

// gcc, clang and the kernel build all write "path:line:col: error: message";
// make writes its own summary line. Both are worth carrying back to the model.
var diagnosticLine = regexp.MustCompile(`^([^\s:][^:]*):(\d+)(?::(\d+))?:\s*(fatal error|error|warning|Error):`)
var makeFailure = regexp.MustCompile(`^(make(\[\d+\])?|ninja|cc1|collect2|ld):.*(\*\*\*|error|Error)`)

// compilerErrors reads the tail of a build's log and pulls the diagnostics out
// of it. The log is the existing build log — this is a reader of the pipeline
// that is already there, not a second one.
func (a *App) compilerErrors(jobID string) (*CompilerErrors, error) {
	j, e := a.job(jobID)
	if e != nil {
		return nil, errors.New("that build is not in the history")
	}
	out := &CompilerErrors{JobID: j.ID, Target: j.Settings.Target, State: j.State, Exit: "still running"}
	if j.ExitCode != nil {
		out.Exit = strconv.Itoa(*j.ExitCode)
	}
	p := a.logPath(j.ID)
	if p == "" {
		return nil, errors.New("that build has no readable log")
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, errors.New("that build's log is unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	// Only the tail: a full ISO build writes hundreds of megabytes, and the
	// failure is always at the end of it.
	if st.Size() > maxLogScan {
		f.Seek(st.Size()-maxLogScan, 0)
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return nil, e
	}
	if len(b) > maxLogScan {
		b = b[len(b)-maxLogScan:]
	}
	seen := map[string]bool{}
	var lines []string
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(stripANSI(raw), "\r")
		if len(line) > 500 {
			line = line[:500] + "…"
		}
		m := diagnosticLine.FindStringSubmatch(line)
		if m == nil && !makeFailure.MatchString(line) {
			continue
		}
		out.Count++
		if len(lines) < maxErrorLines {
			lines = append(lines, line)
		}
		if m == nil {
			continue
		}
		// The compiler's path is relative to wherever it ran inside the
		// container; match it back to the workspace by suffix, and only ever
		// accept a file the workspace actually lists.
		if f := a.matchWorkspaceFile(m[1]); f != "" && !seen[f] {
			seen[f] = true
			if len(out.Files) < 6 {
				out.Files = append(out.Files, f)
			}
		}
	}
	out.Text = strings.Join(lines, "\n")
	return out, nil
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func stripANSI(s string) string { return ansiPattern.ReplaceAllString(s, "") }

// matchWorkspaceFile resolves a path a compiler printed to a workspace file.
// It only ever returns something git listed, so a diagnostic mentioning
// /usr/include/stdio.h or a generated file inside the container contributes
// nothing to the context.
func (a *App) matchWorkspaceFile(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.Contains(p, "..") {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	list, e := a.workspaceFiles(ctx)
	if e != nil {
		return ""
	}
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	for _, f := range list {
		if f == p || strings.HasSuffix(p, "/"+f) || strings.HasSuffix(f, "/"+p) || path.Base(f) == path.Base(p) && strings.Contains(p, path.Dir(f)) {
			return f
		}
	}
	return ""
}

func (a *App) aiErrors(w http.ResponseWriter, r *http.Request) {
	errs, e := a.compilerErrors(r.URL.Query().Get("job"))
	if e != nil {
		fail(w, 404, e.Error())
		return
	}
	respond(w, errs)
}

// ------------------------------------------------------------------ patches

// PatchFile is one file a proposal changes. Content and Original stay on the
// server: the browser reviews the diff, which is what a review is.
type PatchFile struct {
	Path    string `json:"path"`
	Diff    string `json:"diff"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Created bool   `json:"created"`
	Bytes   int    `json:"bytes"`
	// sha256 of the file as it was when the patch was built. Apply refuses if
	// it no longer matches: the diff the operator read would not describe what
	// would happen.
	BaseHash string `json:"-"`
	Content  string `json:"-"`
	Original string `json:"-"`
}

// Patch is one proposal, awaiting review.
type Patch struct {
	ID      string      `json:"id"`
	At      string      `json:"at"`
	Files   []PatchFile `json:"files"`
	Applied bool        `json:"applied"`
	// Set once applied, so the page can offer to undo exactly this.
	AppliedAt string `json:"appliedAt,omitempty"`
}

// The fence the model is asked to answer with. A whole file is far more
// reliable from a language model than a unified diff with correct line
// numbers, so the model writes the file and this server computes the diff —
// which means the diff shown to the operator is always the real one.
var patchFence = regexp.MustCompile("(?s)```ydfs-patch[ \t]+path=([^\n`]+)\n(.*?)\n?```")

const aiSystemPrompt = `You are a careful engineering assistant working inside the LinuxConsole (ydfs2) source-based Linux distribution build system. The user gives you selected files, search results, git diffs and compiler errors from their checkout; you never have filesystem or shell access.

Answer concisely and explain your reasoning before any change.

When you want to change a file, output the COMPLETE new content of that file in a fenced block of this exact form:

` + "```" + `ydfs-patch path=relative/path/from/the/repository/root
<the entire new file content>
` + "```" + `

Rules for those blocks:
- One block per file. Repeat the block for each file you change.
- The path must be relative to the repository root and must be a file that exists in the context you were given, or a new file inside the same project.
- Emit the whole file, not a fragment and not a diff: the server computes the diff and shows it to the user for approval.
- Never emit a patch block for a file you have not been shown, unless you are creating it.
- If you are only explaining something, emit no patch block at all.
- Do not ask the user to run shell commands; nothing you write is executed.`

// stagePatch parses an assistant reply and, if it proposed files, prepares the
// proposal: the real diff against the working tree, ready for review. Nothing
// is written.
func (a *App) stagePatch(ctx context.Context, reply string) (*Patch, error) {
	matches := patchFence.FindAllStringSubmatch(reply, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > maxPatchFiles {
		return nil, fmt.Errorf("the reply proposed %d files, more than the %d this manager will stage at once", len(matches), maxPatchFiles)
	}
	list, e := a.workspaceFiles(ctx)
	if e != nil {
		return nil, e
	}
	p := &Patch{ID: fmt.Sprintf("%d", time.Now().UnixNano()), At: now()}
	total := 0
	for _, m := range matches {
		rel, e := workspacePath(m[1])
		if e != nil {
			return nil, fmt.Errorf("the reply proposed an unusable path: %v", e)
		}
		content := m[2]
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		total += len(content)
		if total > maxPatchBytes {
			return nil, errors.New("the proposed changes are larger than this manager will stage at once")
		}
		if !looksTextual(rel) {
			return nil, fmt.Errorf("%s is not a kind of file this manager will write", rel)
		}
		pf := PatchFile{Path: rel, Content: content, Bytes: len(content)}
		if offeredFile(list, rel) {
			original, e := a.readWorkspaceFile(rel)
			if e != nil {
				return nil, e
			}
			pf.Original, pf.BaseHash = original, hashString(original)
		} else if exists, _ := a.fileExists(rel); exists {
			// Present but ignored or binary: refuse rather than overwrite
			// something that was deliberately kept out of the workspace.
			return nil, fmt.Errorf("%s is not part of the workspace", rel)
		} else {
			pf.Created = true
		}
		if pf.Original == content {
			continue // proposed no change to this file
		}
		diff, added, removed, e := a.unifiedDiff(ctx, pf)
		if e != nil {
			return nil, e
		}
		pf.Diff, pf.Added, pf.Removed = diff, added, removed
		p.Files = append(p.Files, pf)
	}
	if len(p.Files) == 0 {
		return nil, nil
	}
	a.dropPatchRecord()
	a.aiMu.Lock()
	a.patch = p
	a.aiMu.Unlock()
	return p, nil
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// unifiedDiff produces the real diff with git rather than a hand-rolled one,
// then gives it the headers a reader expects: git's --no-index output names
// the temporary files it was handed, which would be meaningless on screen.
func (a *App) unifiedDiff(ctx context.Context, pf PatchFile) (string, int, int, error) {
	dir, e := os.MkdirTemp(a.data, "ai-diff-")
	if e != nil {
		return "", 0, 0, e
	}
	defer os.RemoveAll(dir)
	oldPath := filepath.Join(dir, "old")
	newPath := filepath.Join(dir, "new")
	if e := os.WriteFile(oldPath, []byte(pf.Original), 0600); e != nil {
		return "", 0, 0, e
	}
	if e := os.WriteFile(newPath, []byte(pf.Content), 0600); e != nil {
		return "", 0, 0, e
	}
	out, e := a.diffText(ctx, "diff", "--no-color", "--no-index", "--unified=3", "--", oldPath, newPath)
	if e != nil {
		return "", 0, 0, errors.New("cannot diff " + pf.Path + ": " + e.Error())
	}
	body := string(out)
	if i := strings.Index(body, "@@"); i >= 0 {
		body = body[i:]
	} else {
		body = ""
	}
	from := "a/" + pf.Path
	if pf.Created {
		from = "/dev/null"
	}
	added, removed := 0, 0
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return fmt.Sprintf("--- %s\n+++ b/%s\n%s", from, pf.Path, body), added, removed, nil
}

// aiPatch serves the pending proposal, and one file's diff as plain text for
// the shared viewer.
func (a *App) aiPatch(w http.ResponseWriter, r *http.Request) {
	a.aiMu.Lock()
	p := a.patch
	a.aiMu.Unlock()
	if p == nil {
		fail(w, 404, "no change is waiting for review")
		return
	}
	if want := r.URL.Query().Get("path"); want != "" {
		for _, f := range p.Files {
			if f.Path == want {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Write([]byte(f.Diff))
				return
			}
		}
		fail(w, 404, "that file is not in the proposal")
		return
	}
	respond(w, p)
}

// aiApplyPatch writes the proposal into the working tree.
//
// It holds App.mu for the whole write, exactly as staging and committing do: a
// build submission snapshots the working tree, and must never catch it halfway
// through a patch. What lands is ordinary uncommitted work, so the Repository
// screen reviews, stages, commits and reverts it with the machinery that was
// already there.
func (a *App) aiApplyPatch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var body struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &body) {
		return
	}
	a.aiMu.Lock()
	p := a.patch
	a.aiMu.Unlock()
	if p == nil || (body.ID != "" && p.ID != body.ID) {
		fail(w, 409, "that proposal is no longer the one waiting for review")
		return
	}
	if p.Applied {
		fail(w, 409, "that proposal has already been applied")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Nothing is written until every file has been checked, so a proposal can
	// never land half-applied.
	for i := range p.Files {
		f := &p.Files[i]
		if f.Created {
			if exists, _ := a.fileExists(f.Path); exists {
				fail(w, 409, f.Path+" now exists; ask the assistant to revise the change")
				return
			}
			continue
		}
		current, e := a.readWorkspaceFile(f.Path)
		if e != nil {
			fail(w, 409, e.Error())
			return
		}
		if hashString(current) != f.BaseHash {
			fail(w, 409, f.Path+" changed after the proposal was made; ask the assistant to revise it against the current file")
			return
		}
	}
	root, e := os.OpenRoot(a.repo)
	if e != nil {
		fail(w, 500, "the workspace is unavailable")
		return
	}
	defer root.Close()
	written := []string{}
	for i := range p.Files {
		f := &p.Files[i]
		if e := writeThroughRoot(root, f.Path, f.Content); e != nil {
			// Undo what this call already wrote, so the tree is never left
			// with half a proposal in it.
			for _, done := range written {
				for j := range p.Files {
					if p.Files[j].Path != done {
						continue
					}
					if p.Files[j].Created {
						root.Remove(done)
					} else {
						writeThroughRoot(root, done, p.Files[j].Original)
					}
				}
			}
			fail(w, 500, "cannot write "+f.Path+": "+e.Error())
			return
		}
		written = append(written, f.Path)
	}
	p.Applied, p.AppliedAt = true, now()
	a.savePatchRecord(p)
	a.aiMu.Lock()
	a.patch = p
	a.aiMu.Unlock()
	list, more, _ := a.changes(ctx)
	respond(w, map[string]any{"patch": p, "changes": list, "moreChanges": more})
}

// writeThroughRoot creates the parent directories a new file needs and writes
// it, never leaving the root. Directories are created one component at a time
// through the same guard, so no component can be a symlink out of the tree.
func writeThroughRoot(root *os.Root, rel, content string) error {
	dir := path.Dir(rel)
	if dir != "." {
		parts := strings.Split(dir, "/")
		for i := range parts {
			if e := root.Mkdir(strings.Join(parts[:i+1], "/"), 0755); e != nil && !errors.Is(e, os.ErrExist) {
				return e
			}
		}
	}
	f, e := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = f.WriteString(content)
	return e
}

// An applied proposal is the one piece of assistant state that must outlive a
// restart: the pending one can be asked for again, but once files have been
// written, the content they replaced is the only way back. It is kept beside
// the database rather than in it because it is bulk text, and there is only
// ever one — a new proposal supersedes it.
func (a *App) patchRecordPath() string {
	return filepath.Join(a.data, "ai-patches", "current.json")
}

// storedPatch is the Patch with the two fields the browser never sees, which
// are exactly the ones an undo needs.
type storedPatch struct {
	Patch
	Contents  map[string]string `json:"contents"`
	Originals map[string]string `json:"originals"`
}

func (a *App) savePatchRecord(p *Patch) {
	if e := os.MkdirAll(filepath.Dir(a.patchRecordPath()), 0700); e != nil {
		return
	}
	rec := storedPatch{Patch: *p, Contents: map[string]string{}, Originals: map[string]string{}}
	for _, f := range p.Files {
		rec.Contents[f.Path] = f.Content
		rec.Originals[f.Path] = f.Original
	}
	b, e := json.Marshal(rec)
	if e != nil {
		return
	}
	os.WriteFile(a.patchRecordPath(), b, 0600)
}

func (a *App) dropPatchRecord() { os.Remove(a.patchRecordPath()) }

// restorePatch is called once at startup, so the Undo button is still there
// after a restart. Only an applied proposal is kept: an unapplied one changed
// nothing and can simply be asked for again.
func (a *App) restorePatch() {
	b, e := os.ReadFile(a.patchRecordPath())
	if e != nil {
		return
	}
	var rec storedPatch
	if json.Unmarshal(b, &rec) != nil || !rec.Applied {
		return
	}
	p := rec.Patch
	for i := range p.Files {
		p.Files[i].Content = rec.Contents[p.Files[i].Path]
		p.Files[i].Original = rec.Originals[p.Files[i].Path]
	}
	a.aiMu.Lock()
	a.patch = &p
	a.aiMu.Unlock()
}

// aiRejectPatch drops a proposal that was never applied, or undoes one that
// was — putting back exactly the bytes that were there before.
func (a *App) aiRejectPatch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	a.aiMu.Lock()
	p := a.patch
	a.aiMu.Unlock()
	if p == nil {
		fail(w, 404, "no change is waiting for review")
		return
	}
	if p.Applied {
		a.mu.Lock()
		root, e := os.OpenRoot(a.repo)
		if e != nil {
			a.mu.Unlock()
			fail(w, 500, "the workspace is unavailable")
			return
		}
		for _, f := range p.Files {
			if f.Created {
				root.Remove(f.Path)
				continue
			}
			writeThroughRoot(root, f.Path, f.Original)
		}
		root.Close()
		a.mu.Unlock()
	}
	a.dropPatchRecord()
	a.aiMu.Lock()
	a.patch = nil
	a.aiMu.Unlock()
	list, more, _ := a.changes(ctx)
	respond(w, map[string]any{"ok": true, "changes": list, "moreChanges": more})
}

// aiAdoptBuild records which build the page started for the current proposal.
// The build itself is an ordinary job, queued through POST /api/jobs and
// streamed through the log endpoint that was already there: this only
// remembers which one, so the page finds it again after a reload.
func (a *App) aiAdoptBuild(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobID string `json:"jobId"`
	}
	if !decode(w, r, &body) {
		return
	}
	j, e := a.job(body.JobID)
	if e != nil {
		fail(w, 404, "that build is not in the history")
		return
	}
	sess, e := a.aiSession()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	sess.Build = &AIBuild{JobID: j.ID, Target: j.Settings.Target, At: now()}
	if e := a.saveAISession(sess); e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, sess.Build)
}

// AIBuild is which build the assistant's work is currently being tested by.
type AIBuild struct {
	JobID  string `json:"jobId"`
	Target string `json:"target"`
	At     string `json:"at"`
}
