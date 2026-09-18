package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Settings struct {
	Target          string `json:"target"`
	Verbose         bool   `json:"verbose"`
	Kernel          string `json:"kernel"`
	ConfigOverrides string `json:"configOverrides"`
	PackageList     string `json:"packageList"`
	PackageListText string `json:"packageListText"`
}
type Profile struct {
	Name     string   `json:"name"`
	Settings Settings `json:"settings"`
}
type Artifact struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}
type Job struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	User      string     `json:"user"`
	Settings  Settings   `json:"settings"`
	Created   string     `json:"created"`
	Started   string     `json:"started,omitempty"`
	Finished  string     `json:"finished,omitempty"`
	Revision  string     `json:"revision"`
	Tag       string     `json:"tag"`
	Container string     `json:"container"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	Error     string     `json:"error,omitempty"`
	Artifacts []Artifact `json:"artifacts"`
}

var targets = []string{"fast-iso", "full-iso", "kernel", "busybox", "initramfs", "updates", "mate", "kde", "cinnamon"}
var kernelPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// A config.ini line as either NAME=value or NAME="value with safe spaces".
// No $, backticks, quotes-within-quotes, semicolons or other shell
// metacharacters: config.ini is later "." (dot-)sourced by build scripts, so
// anything accepted here becomes literal shell input.
var configOverrideLine = regexp.MustCompile(`^([A-Z][A-Z0-9_]{0,63})=("[A-Za-z0-9 _./:+-]{0,256}"|[A-Za-z0-9_./:+-]{0,256})$`)

// Generated after every user override, so these always win; listed here only
// to reject the override outright with a clear error instead of silently
// discarding it.
var protectedConfigKeys = map[string]bool{"ARCH": true, "DISTRONAME": true, "BUILDYDFS": true, "ISOTMP": true, "SEND_BUILD_LOG": true, "SEND_OPKG": true, "MENUCONFIG": true}

const maxConfigOverridesLen = 8192
const maxPackageListTextLen = 400000

// packages/list-<name>: only plain filename characters, checked again with
// filepath.Base before any filesystem access.
var packageListNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func validateConfigOverrides(text string) error {
	if len(text) > maxConfigOverridesLen {
		return errors.New("config overrides are too long")
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := configOverrideLine.FindStringSubmatch(line)
		if m == nil {
			return fmt.Errorf("invalid config override line %q: expected NAME=value with safe characters", line)
		}
		if protectedConfigKeys[m[1]] {
			return fmt.Errorf("config override key %s is managed by the build system and cannot be overridden", m[1])
		}
	}
	return nil
}
func (s Settings) validate() error {
	found := false
	for _, t := range targets {
		if s.Target == t {
			found = true
		}
	}
	if !found {
		return errors.New("unknown build target")
	}
	if s.Kernel != "" && (!kernelPattern.MatchString(s.Kernel) || len(s.Kernel) > 32 || (s.Target != "full-iso" && s.Target != "kernel")) {
		return errors.New("custom kernel must be a numeric version such as 6.18.29, for full ISO or kernel builds")
	}
	if e := validateConfigOverrides(s.ConfigOverrides); e != nil {
		return e
	}
	if (s.PackageList == "") != (s.PackageListText == "") {
		return errors.New("packageList and packageListText must be supplied together")
	}
	if s.PackageList != "" {
		if !packageListNamePattern.MatchString(s.PackageList) || filepath.Base(s.PackageList) != s.PackageList {
			return errors.New("invalid package list name")
		}
		if len(s.PackageListText) > maxPackageListTextLen {
			return errors.New("package list is too long")
		}
		if !utf8.ValidString(s.PackageListText) || strings.ContainsRune(s.PackageListText, 0) {
			return errors.New("package list must be valid UTF-8 text without NUL bytes")
		}
	}
	return nil
}
func now() string { return time.Now().UTC().Format(time.RFC3339) }
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY, data TEXT NOT NULL); CREATE TABLE IF NOT EXISTS profiles(name TEXT PRIMARY KEY, data TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func (a *App) saveJob(j *Job) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	_, e = a.db.Exec("INSERT INTO jobs VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", j.ID, string(b))
	return e
}
func (a *App) job(id string) (*Job, error) {
	var b string
	if e := a.db.QueryRow("SELECT data FROM jobs WHERE id=?", id).Scan(&b); e != nil {
		return nil, e
	}
	var j Job
	e := json.Unmarshal([]byte(b), &j)
	return &j, e
}
func (a *App) jobs() ([]Job, error) {
	rows, e := a.db.Query("SELECT data FROM jobs ORDER BY id DESC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var b string
		var j Job
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &j); e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (a *App) dir(j *Job) string { return filepath.Join(a.data, "jobs", j.ID) }
func terminal(s string) bool     { return s == "succeeded" || s == "failed" || s == "cancelled" }
