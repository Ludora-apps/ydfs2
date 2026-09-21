package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Every build log lives in one directory, named by job id, rather than inside
// the per-job directory: they outlive the artifacts they describe, and keeping
// them together makes them listable, searchable and removable on their own.
const logsDir = "logs-build"

// jobIDPattern guards the only user-controlled part of a log path. Job ids are
// generated in submit() as a UTC timestamp plus hex, never supplied by a client,
// but the id arrives back through the URL so it is validated before use.
func validJobID(id string) bool {
	if id == "" || len(id) > 128 || filepath.Base(id) != id {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			continue
		}
		return false
	}
	return !strings.Contains(id, "..")
}

func (a *App) logDir() string { return filepath.Join(a.data, logsDir) }

// logPath is the single source of truth for where a build's log is written,
// read, streamed and served from. An invalid id yields "" so callers fail
// closed rather than touching a path outside the directory.
func (a *App) logPath(id string) string {
	if !validJobID(id) {
		return ""
	}
	return filepath.Join(a.logDir(), id+".log")
}

// migrateLogs moves logs written by earlier versions, which kept build.log
// inside the per-job directory, into the shared directory. Runs once at
// startup; a job whose log is already moved is left alone.
func (a *App) migrateLogs() {
	if e := os.MkdirAll(a.logDir(), 0700); e != nil {
		fmt.Fprintln(os.Stderr, "logs:", e)
		return
	}
	entries, e := os.ReadDir(filepath.Join(a.data, "jobs"))
	if e != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		old := filepath.Join(a.data, "jobs", entry.Name(), "build.log")
		if _, e := os.Stat(old); e != nil {
			continue
		}
		to := a.logPath(entry.Name())
		if to == "" {
			continue
		}
		if _, e := os.Stat(to); e == nil {
			continue // already migrated
		}
		if e := os.Rename(old, to); e != nil {
			fmt.Fprintln(os.Stderr, "logs: migrating", entry.Name(), e)
		}
	}
}

// removeLog deletes a build's log. Missing is not an error: the log may have
// been deleted on its own, or the build may never have produced one.
func (a *App) removeLog(id string) error {
	path := a.logPath(id)
	if path == "" {
		return errors.New("invalid build id")
	}
	if e := os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return nil
}

type LogEntry struct {
	ID      string `json:"id"`
	Target  string `json:"target"`
	State   string `json:"state"`
	Created string `json:"created"`
	User    string `json:"user"`
	Size    int64  `json:"size"`
	// False when the build still exists but its log was deleted on its own.
	Present bool `json:"present"`
}

// logs lists every build log, newest first, so the UI can offer them without
// walking the jobs list itself.
func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	js, e := a.jobs()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	out := []LogEntry{}
	for i := range js {
		j := &js[i]
		entry := LogEntry{ID: j.ID, Target: j.Settings.Target, State: j.State, Created: j.Created, User: j.User}
		if path := a.logPath(j.ID); path != "" {
			if info, e := os.Stat(path); e == nil && info.Mode().IsRegular() {
				entry.Size, entry.Present = info.Size(), true
			}
		}
		out = append(out, entry)
	}
	respond(w, out)
}

// deleteLog removes one build's log without touching the build itself.
func (a *App) deleteLog(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, e := a.job(pathValue(r, "id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	// A running build is still being written to; removing the file underneath
	// captureLogs would lose the rest of the build's output.
	if !terminal(j.State) {
		fail(w, 409, "wait for the build to finish before deleting its log")
		return
	}
	if e := a.removeLog(j.ID); e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, map[string]bool{"ok": true})
}
