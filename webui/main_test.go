package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	data := t.TempDir()
	db, e := openDB(filepath.Join(data, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return &App{db: db, data: data, repo: t.TempDir(), ctx: context.Background(), origin: "https://build.test", secret: strings.Repeat("s", 32), users: map[string]bool{"alice": true}, wake: make(chan struct{}, 1), docker: "docker"}
}
func request(a *App, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:2345"
	r.Header.Set("X-Build-Proxy-Secret", a.secret)
	r.Header.Set("X-Forwarded-User", "alice")
	r.Header.Set("Origin", a.origin)
	r.Header.Set("X-Requested-With", "ydfs-web")
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	return w
}
func putFile(t *testing.T, p, s string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(s), 0644); e != nil {
		t.Fatal(e)
	}
}
func saveFixtureJob(t *testing.T, a *App, state string) *Job {
	t.Helper()
	j := &Job{ID: "test-job", State: state, User: "alice", Settings: Settings{Target: "fast-iso", Verbose: true}, Created: now(), Artifacts: []Artifact{}}
	putFile(t, filepath.Join(a.dir(j), "build.log"), "")
	if e := a.saveJob(j); e != nil {
		t.Fatal(e)
	}
	return j
}
func TestSettingsValidation(t *testing.T) {
	for _, s := range []Settings{{Target: "fast-iso"}, {Target: "kernel", Kernel: "6.18.29"}, {Target: "full-iso", Kernel: "5.4.283"}, {Target: "mate"}} {
		if e := s.validate(); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range []Settings{{Target: "sh"}, {Target: "kernel", Kernel: "$(touch /tmp/injected)"}, {Target: "kernel", Kernel: "6.1\nSEND_OPKG=YES"}, {Target: "fast-iso", Kernel: "6.18.29"}, {Target: "kernel", Kernel: "latest"}, {Target: "kernel", Kernel: "../../etc/passwd"}} {
		if s.validate() == nil {
			t.Fatalf("accepted unsafe settings: %#v", s)
		}
	}
}
func TestConfigOverrideValidation(t *testing.T) {
	for _, s := range []Settings{
		{Target: "fast-iso", ConfigOverrides: "kernel3=6.18.29"},
		{Target: "fast-iso", ConfigOverrides: "KERNEL3=$(touch /tmp/injected)"},
		{Target: "fast-iso", ConfigOverrides: "KERNEL3=`touch /tmp/injected`"},
		{Target: "fast-iso", ConfigOverrides: "SEND_OPKG=YES"},
		{Target: "fast-iso", ConfigOverrides: "ISOTMP=/tmp/elsewhere"},
		{Target: "fast-iso", ConfigOverrides: "ARCH=arm"},
		{Target: "fast-iso", ConfigOverrides: "# just a comment"},
		{Target: "fast-iso", ConfigOverrides: "KERNEL3=6.18.29; rm -rf /"},
		{Target: "fast-iso", ConfigOverrides: strings.Repeat("A=1\n", 3000)},
	} {
		if s.validate() == nil {
			t.Fatalf("accepted unsafe config override: %#v", s)
		}
	}
	for _, s := range []Settings{
		{Target: "fast-iso", ConfigOverrides: "KERNEL3=6.18.29\nBUILDMODULES=YES\n"},
		{Target: "fast-iso", ConfigOverrides: `YDFS_MODULES="kde mate games"`},
	} {
		if e := s.validate(); e != nil {
			t.Fatalf("rejected safe config override %#v: %v", s, e)
		}
	}
}
func TestPackageListValidation(t *testing.T) {
	for _, s := range []Settings{
		{Target: "mate", PackageList: "mate"},
		{Target: "mate", PackageListText: "https://example.org/pkg.tar.gz\n"},
		{Target: "mate", PackageList: "../../etc/passwd", PackageListText: "x"},
		{Target: "mate", PackageList: "sub/dir", PackageListText: "x"},
		{Target: "mate", PackageList: "x86_64", PackageListText: strings.Repeat("x", maxPackageListTextLen+1)},
	} {
		if s.validate() == nil {
			t.Fatalf("accepted unsafe package list settings: %#v", s)
		}
	}
	if e := (Settings{Target: "mate", PackageList: "mate", PackageListText: "https://example.org/pkg.tar.gz\n"}).validate(); e != nil {
		t.Fatal(e)
	}
}
func TestPackageListSubmission(t *testing.T) {
	a := testApp(t)
	source := filepath.Join(a.repo, "2.12")
	putFile(t, filepath.Join(source, "Makefile"), "")
	putFile(t, filepath.Join(source, "packages", "list-mate"), "original content\n")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.org", "commit", "-m", "fixture"}} {
		cmd := exec.Command("git", append([]string{"-C", a.repo}, args...)...)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatal(e, string(b))
		}
	}
	w := request(a, "POST", "/api/jobs", `{"target":"mate","verbose":true,"kernel":"","packageList":"does-not-exist","packageListText":"x"}`)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(a, "POST", "/api/jobs", `{"target":"mate","verbose":true,"kernel":"","packageList":"mate","packageListText":"replacement content\n"}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var j Job
	if e := json.Unmarshal(w.Body.Bytes(), &j); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(a.dir(&j), "source", "packages", "list-mate"))
	if e != nil || string(b) != "replacement content\n" {
		t.Fatal(e, string(b))
	}
	untouched, e := os.ReadFile(filepath.Join(source, "packages", "list-mate"))
	if e != nil || string(untouched) != "original content\n" {
		t.Fatal("mutated shared checkout", e, string(untouched))
	}
	wl := request(a, "GET", "/api/packages", "")
	if wl.Code != 200 || !strings.Contains(wl.Body.String(), "mate") {
		t.Fatal(wl.Code, wl.Body.String())
	}
	wc := request(a, "GET", "/api/packages/mate", "")
	if wc.Code != 200 || !strings.Contains(wc.Body.String(), "original content") {
		t.Fatal(wc.Code, wc.Body.String())
	}
	for _, name := range []string{"..%2F..%2Fetc%2Fpasswd", "missing"} {
		wc = request(a, "GET", "/api/packages/"+name, "")
		if wc.Code == 200 {
			t.Fatalf("served unexpected package list: %s", name)
		}
	}
}
func TestFlathubValidation(t *testing.T) {
	for _, s := range []Settings{
		{Target: "fast-iso", Flatpaks: []string{"org.videolan.VLC", "com.github.tchx84.Flatseal"}},
		{Target: "full-iso", Flatpaks: []string{"org.DolphinEmu.dolphin-emu", "org.localsend.localsend_app"}},
		{Target: "mate"},
		{Target: "fast-iso", Flatpaks: []string{}},
	} {
		if e := s.validate(); e != nil {
			t.Fatal(e, s)
		}
	}
	tooMany := []string{}
	for i := 0; i <= maxFlatpakApps; i++ {
		tooMany = append(tooMany, fmt.Sprintf("org.example.App%d", i))
	}
	for _, s := range []Settings{
		// Only an ISO carries applications; a component build cannot.
		{Target: "mate", Flatpaks: []string{"org.videolan.VLC"}},
		{Target: "kernel", Flatpaks: []string{"org.videolan.VLC"}},
		{Target: "fast-iso", Flatpaks: tooMany},
		{Target: "fast-iso", Flatpaks: []string{"org.videolan.VLC", "org.videolan.VLC"}},
		// Every one of these would otherwise reach flatpak as an argument, or
		// data/flathub-apps as a line.
		{Target: "fast-iso", Flatpaks: []string{"org.videolan.VLC;rm -rf /"}},
		{Target: "fast-iso", Flatpaks: []string{"org.videolan.VLC\nSEND_OPKG=YES"}},
		{Target: "fast-iso", Flatpaks: []string{"$(touch /tmp/injected)"}},
		{Target: "fast-iso", Flatpaks: []string{"`touch /tmp/injected`"}},
		{Target: "fast-iso", Flatpaks: []string{"../../etc/passwd"}},
		{Target: "fast-iso", Flatpaks: []string{"org.videolan.VLC --system"}},
		{Target: "fast-iso", Flatpaks: []string{"org..VLC"}},
		{Target: "fast-iso", Flatpaks: []string{"VLC"}},
		{Target: "fast-iso", Flatpaks: []string{""}},
		{Target: "fast-iso", Flatpaks: []string{".org.videolan.VLC"}},
	} {
		if s.validate() == nil {
			t.Fatalf("accepted unsafe Flathub settings: %#v", s)
		}
	}
}
func TestFlathubCatalogue(t *testing.T) {
	a := testApp(t)
	// Nothing cached and no network: the built-in list still fills the form.
	w := request(a, "GET", "/api/flathub", "")
	var c FlathubCatalogue
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &c) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if c.Source != "built-in" || len(c.Apps) != len(flathubFallback) {
		t.Fatalf("unexpected cold-start catalogue: %+v", c)
	}
	if !a.allowedFlatpak("org.videolan.VLC") || a.allowedFlatpak("org.example.NotReal") {
		t.Fatal("built-in list is not acting as the allowlist")
	}
	// A refresh replaces it, and drops anything that is not an application ID.
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"hits":[{"app_id":"org.example.Live","name":"Live","summary":"s"},{"app_id":"not an id;rm -rf /","name":"Bad"}]}`)
	}))
	defer live.Close()
	a.flathubFrom = live.URL
	w = request(a, "POST", "/api/flathub/refresh", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &c) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if c.Source != "flathub" || c.Error != "" || len(c.Apps) != 1 || c.Apps[0].ID != "org.example.Live" {
		t.Fatalf("unexpected refreshed catalogue: %+v", c)
	}
	if !a.allowedFlatpak("org.example.Live") || !a.allowedFlatpak("org.videolan.VLC") {
		t.Fatal("refresh must add to the allowlist, never shrink it below the built-in list")
	}
	// A failed refresh reports why and keeps what the form already had.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer down.Close()
	a.flathubFrom = down.URL
	w = request(a, "POST", "/api/flathub/refresh", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &c) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if c.Error == "" || len(c.Apps) != 1 || c.Apps[0].ID != "org.example.Live" {
		t.Fatalf("failed refresh lost the catalogue: %+v", c)
	}
	// And it survives a restart, because it was written to the data directory.
	b := &App{db: a.db, data: a.data}
	if got := b.catalogue(); got.Source != "flathub" || len(got.Apps) != 1 {
		t.Fatalf("cached catalogue not reloaded: %+v", got)
	}
}
func TestFlathubSubmission(t *testing.T) {
	a := testApp(t)
	source := filepath.Join(a.repo, "2.12")
	putFile(t, filepath.Join(source, "Makefile"), "")
	putFile(t, filepath.Join(source, "data", "flathub-apps"), "# nothing selected\n")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.org", "commit", "-m", "fixture"}, {"tag", "v2.12.1"}} {
		cmd := exec.Command("git", append([]string{"-C", a.repo}, args...)...)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatal(e, string(b))
		}
	}
	// Syntactically valid, but not something the server offered.
	w := request(a, "POST", "/api/jobs", `{"target":"fast-iso","verbose":true,"kernel":"","configOverrides":"","packageList":"","packageListText":"","flatpaks":["org.example.NotReal"]}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "unknown Flathub application") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(a, "POST", "/api/jobs", `{"target":"fast-iso","verbose":true,"kernel":"","configOverrides":"","packageList":"","packageListText":"","flatpaks":["org.videolan.VLC","com.github.tchx84.Flatseal"]}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var j Job
	if e := json.Unmarshal(w.Body.Bytes(), &j); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(a.dir(&j), "source", "data", "flathub-apps"))
	if e != nil || string(b) != "org.videolan.VLC\ncom.github.tchx84.Flatseal\n" {
		t.Fatal(e, string(b))
	}
	untouched, e := os.ReadFile(filepath.Join(source, "data", "flathub-apps"))
	if e != nil || string(untouched) != "# nothing selected\n" {
		t.Fatal("mutated shared checkout", e, string(untouched))
	}
	// A build with no applications must leave the repository's own file alone.
	w = request(a, "POST", "/api/jobs", `{"target":"fast-iso","verbose":true,"kernel":"","configOverrides":"","packageList":"","packageListText":"","flatpaks":[]}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var empty Job
	if e := json.Unmarshal(w.Body.Bytes(), &empty); e != nil {
		t.Fatal(e)
	}
	b, e = os.ReadFile(filepath.Join(a.dir(&empty), "source", "data", "flathub-apps"))
	if e != nil || string(b) != "# nothing selected\n" {
		t.Fatal("empty selection rewrote the list", e, string(b))
	}
}

// saveArtifactJob creates a succeeded build whose artifact really exists on
// disk, alongside the config.ini and build.log retention must not touch.
func saveArtifactJob(t *testing.T, a *App, id, target, artifact string) *Job {
	t.Helper()
	j := &Job{ID: id, State: "succeeded", User: "alice", Settings: Settings{Target: target, Flatpaks: []string{}}, Created: now(), Artifacts: []Artifact{{artifact, 4}}}
	putFile(t, filepath.Join(a.dir(j), "output", artifact), "iso!")
	putFile(t, filepath.Join(a.dir(j), "output", "config.ini"), "ARCH=x86_64\n")
	putFile(t, filepath.Join(a.dir(j), "build.log"), "compiling\n")
	if e := a.saveJob(j); e != nil {
		t.Fatal(e)
	}
	return j
}
func artifactExists(t *testing.T, a *App, id, name string) bool {
	t.Helper()
	_, e := os.Stat(filepath.Join(a.data, "jobs", id, "output", name))
	return e == nil
}
func TestRetentionKeepsLastThreeOfEachClass(t *testing.T) {
	a := testApp(t)
	// Ids sort chronologically, which is the order jobs() returns them in.
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		saveArtifactJob(t, a, "2026010"+n, "fast-iso", "linuxconsole.iso")
		saveArtifactJob(t, a, "2026020"+n, "mate", "mate-x86_64.squashfs")
	}
	a.mu.Lock()
	a.pruneLocked()
	a.mu.Unlock()
	for _, tt := range []struct {
		id, name string
		kept     bool
	}{
		{"20260105", "linuxconsole.iso", true}, {"20260104", "linuxconsole.iso", true},
		{"20260103", "linuxconsole.iso", true}, {"20260102", "linuxconsole.iso", false},
		{"20260101", "linuxconsole.iso", false},
		// A separate window, so five ISO builds never evict the components.
		{"20260205", "mate-x86_64.squashfs", true}, {"20260204", "mate-x86_64.squashfs", true},
		{"20260203", "mate-x86_64.squashfs", true}, {"20260202", "mate-x86_64.squashfs", false},
		{"20260201", "mate-x86_64.squashfs", false},
	} {
		if got := artifactExists(t, a, tt.id, tt.name); got != tt.kept {
			t.Fatalf("%s: artifact present=%v, want %v", tt.id, got, tt.kept)
		}
		j, e := a.job(tt.id)
		if e != nil {
			t.Fatalf("%s: retention deleted the build record: %v", tt.id, e)
		}
		if tt.kept && (j.PrunedAt != "" || len(j.Artifacts) != 1) {
			t.Fatalf("%s: wrongly marked pruned: %+v", tt.id, j)
		}
		if !tt.kept && (j.PrunedAt == "" || len(j.Artifacts) != 0) {
			t.Fatalf("%s: not marked pruned: %+v", tt.id, j)
		}
		// The cheap, useful parts of a build always survive.
		for _, keep := range []string{filepath.Join("output", "config.ini"), "build.log"} {
			if _, e := os.Stat(filepath.Join(a.data, "jobs", tt.id, keep)); e != nil {
				t.Fatalf("%s: retention removed %s", tt.id, keep)
			}
		}
	}
}
func TestRetentionPinsFavorites(t *testing.T) {
	a := testApp(t)
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		saveArtifactJob(t, a, "2026010"+n, "fast-iso", "linuxconsole.iso")
	}
	// Star the oldest, which is well outside the window.
	if w := request(a, "POST", "/api/jobs/20260101/favorite", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	a.mu.Lock()
	a.pruneLocked()
	a.mu.Unlock()
	if !artifactExists(t, a, "20260101", "linuxconsole.iso") {
		t.Fatal("a favourite was reclaimed")
	}
	// An aged-out favourite must not consume a slot from the rolling window.
	for _, id := range []string{"20260105", "20260104", "20260103"} {
		if !artifactExists(t, a, id, "linuxconsole.iso") {
			t.Fatalf("%s: favourite shrank the window", id)
		}
	}
	if artifactExists(t, a, "20260102", "linuxconsole.iso") {
		t.Fatal("20260102 should have been reclaimed")
	}
	// Releasing it makes it eligible immediately.
	if w := request(a, "DELETE", "/api/jobs/20260101/favorite", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if artifactExists(t, a, "20260101", "linuxconsole.iso") {
		t.Fatal("releasing a favourite did not reclaim it")
	}
	// And a build whose artifacts are gone cannot be pinned after the fact.
	if w := request(a, "POST", "/api/jobs/20260101/favorite", ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestRetentionIgnoresUnfinishedAndFailedBuilds(t *testing.T) {
	a := testApp(t)
	for _, n := range []string{"1", "2", "3", "4"} {
		saveArtifactJob(t, a, "2026010"+n, "fast-iso", "linuxconsole.iso")
	}
	// A running build must never be touched, whatever its age.
	running := &Job{ID: "20250101", State: "running", User: "alice", Settings: Settings{Target: "fast-iso"}, Created: now(), Artifacts: []Artifact{{"linuxconsole.iso", 4}}}
	putFile(t, filepath.Join(a.dir(running), "output", "linuxconsole.iso"), "iso!")
	if e := a.saveJob(running); e != nil {
		t.Fatal(e)
	}
	a.mu.Lock()
	a.pruneLocked()
	a.mu.Unlock()
	if !artifactExists(t, a, "20250101", "linuxconsole.iso") {
		t.Fatal("retention touched a build that is still running")
	}
	if j, _ := a.job("20250101"); j.PrunedAt != "" {
		t.Fatal("running build marked pruned")
	}
	// Running builds do not consume a window slot either.
	if !artifactExists(t, a, "20260102", "linuxconsole.iso") {
		t.Fatal("a non-succeeded build consumed a retention slot")
	}
}
func TestArchivedConfigIsReadable(t *testing.T) {
	a := testApp(t)
	saveArtifactJob(t, a, "20260101", "fast-iso", "linuxconsole.iso")
	w := request(a, "GET", "/api/jobs/20260101/config", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ARCH=x86_64") {
		t.Fatal(w.Code, w.Body.String())
	}
	// Still readable once the ISO itself has been reclaimed.
	j, _ := a.job("20260101")
	a.mu.Lock()
	if e := a.pruneArtifacts(j); e != nil {
		t.Fatal(e)
	}
	a.mu.Unlock()
	w = request(a, "GET", "/api/jobs/20260101/config", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ARCH=x86_64") {
		t.Fatal("configuration lost with the artifact:", w.Code, w.Body.String())
	}
	if w := request(a, "GET", "/api/jobs/missing/config", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestAuthenticationAndCSRF(t *testing.T) {
	a := testApp(t)
	tests := []struct {
		name   string
		change func(*http.Request)
		code   int
	}{
		{"allowed", func(r *http.Request) {}, 200},
		{"missing secret", func(r *http.Request) { r.Header.Del("X-Build-Proxy-Secret") }, 401},
		{"unknown user", func(r *http.Request) { r.Header.Set("X-Forwarded-User", "mallory") }, 401},
		{"nonlocal proxy", func(r *http.Request) { r.RemoteAddr = "192.0.2.1:10" }, 401},
		{"bad origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"missing csrf header", func(r *http.Request) { r.Header.Del("X-Requested-With") }, 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("PUT", "/api/profiles", strings.NewReader(`{"name":"Daily","settings":{"target":"fast-iso","verbose":true,"kernel":""}}`))
			r.RemoteAddr = "127.0.0.1:22"
			r.Header.Set("X-Build-Proxy-Secret", a.secret)
			r.Header.Set("X-Forwarded-User", "alice")
			r.Header.Set("Origin", a.origin)
			r.Header.Set("X-Requested-With", "ydfs-web")
			tt.change(r)
			w := httptest.NewRecorder()
			a.handler().ServeHTTP(w, r)
			if w.Code != tt.code {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
	for _, path := range []string{"/", "/api/jobs", "/api/jobs/a/events", "/api/jobs/a/artifacts/file"} {
		w := httptest.NewRecorder()
		a.handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("unprotected %s", path)
		}
	}
}
func TestProfilesAndStrictJSON(t *testing.T) {
	a := testApp(t)
	body := `{"name":"Daily","settings":{"target":"full-iso","verbose":true,"kernel":"6.18.29"}}`
	if w := request(a, "PUT", "/api/profiles", body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := request(a, "GET", "/api/profiles", ""); !strings.Contains(w.Body.String(), "Daily") {
		t.Fatal(w.Body.String())
	}
	for _, b := range []string{body + `{}`, `{"name":"Daily","settings":{"target":"sh"}}`, `{"name":"Daily","command":"whoami","settings":{"target":"fast-iso"}}`} {
		if w := request(a, "PUT", "/api/profiles", b); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request(a, "DELETE", "/api/profiles/Daily", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestCancellationAndPersistence(t *testing.T) {
	a := testApp(t)
	j := saveFixtureJob(t, a, "queued")
	if w := request(a, "POST", "/api/jobs/"+j.ID+"/cancel", `{}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	read, e := a.job(j.ID)
	if e != nil || read.State != "cancelled" || read.Finished == "" {
		t.Fatal(read, e)
	}
	other, e := openDB(filepath.Join(a.data, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	var count int
	if e = other.QueryRow("SELECT count(*) FROM jobs").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	j.State = "running"
	a.saveJob(j)
	request(a, "POST", "/api/jobs/"+j.ID+"/cancel", `{}`)
	read, _ = a.job(j.ID)
	if read.State != "cancelling" {
		t.Fatal(read.State)
	}
	if w := request(a, "DELETE", "/api/jobs/"+j.ID, ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
}
func TestLogReplayAndArtifactBoundary(t *testing.T) {
	a := testApp(t)
	j := saveFixtureJob(t, a, "succeeded")
	text := strings.Repeat("compiler output\n", 50000)
	putFile(t, filepath.Join(a.dir(j), "build.log"), text)
	w := request(a, "GET", "/api/jobs/"+j.ID+"/events?offset="+fmt.Sprint(len(text)-16), "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: done") || !strings.Contains(w.Body.String(), fmt.Sprintf("id: %d", len(text))) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(a, "GET", "/api/jobs/"+j.ID+"/events?offset=999999999", "")
	if w.Code != 416 {
		t.Fatal(w.Code)
	}
	output := filepath.Join(a.dir(j), "output")
	putFile(t, filepath.Join(output, "image.iso"), "ISO")
	j.Artifacts = []Artifact{{"image.iso", 3}, {"escape.iso", 10}}
	a.saveJob(j)
	secret := filepath.Join(a.data, "secret")
	putFile(t, secret, "private")
	os.Symlink(secret, filepath.Join(output, "escape.iso"))
	w = request(a, "GET", "/api/jobs/"+j.ID+"/artifacts/image.iso", "")
	if w.Code != 200 || w.Body.String() != "ISO" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, name := range []string{"escape.iso", "secret", "..%2F..%2Fsecret"} {
		w = request(a, "GET", "/api/jobs/"+j.ID+"/artifacts/"+name, "")
		if w.Code == 200 {
			t.Fatalf("escaped boundary: %s", name)
		}
	}
}
func TestSnapshotAndExpectedArtifacts(t *testing.T) {
	a := testApp(t)
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "snapshot")
	putFile(t, filepath.Join(src, "config.ini"), "unsafe old config")
	putFile(t, filepath.Join(src, "Makefile"), "first")
	if e := snapshot(src, dst); e != nil {
		t.Fatal(e)
	}
	putFile(t, filepath.Join(src, "Makefile"), "second")
	b, _ := os.ReadFile(filepath.Join(dst, "Makefile"))
	if string(b) != "first" {
		t.Fatal(string(b))
	}
	if _, e := os.Stat(filepath.Join(dst, "config.ini")); !os.IsNotExist(e) {
		t.Fatal("copied config.ini")
	}
	j := saveFixtureJob(t, a, "running")
	os.MkdirAll(filepath.Join(a.dir(j), "output"), 0755)
	if e := a.collect(j); e == nil {
		t.Fatal("missing ISO accepted")
	}
	putFile(t, filepath.Join(a.dir(j), "output", "result.iso"), "valid fixture")
	if e := a.collect(j); e != nil || len(j.Artifacts) != 1 {
		t.Fatal(e, j.Artifacts)
	}
}
func TestGeneratedConfigurationWorksInMakeAndShell(t *testing.T) {
	script, e := filepath.Abs("../2.12/scripts/make_config_ini")
	if e != nil {
		t.Fatal(e)
	}
	for _, kernel := range []string{"", "6.12.42"} {
		t.Run("kernel_"+kernel, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command("bash", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "YDFS_ARCH=x86_64", "YDFS_CUSTOM_KERNEL="+kernel, "DIBAB_VERBOSE_BUILD=YES", "SEND_BUILD_LOG=NO", "DISTRONAME=linuxconsole", "BUILDYDFS=fast")
			if b, e := cmd.CombinedOutput(); e != nil {
				t.Fatal(e, string(b))
			}
			putFile(t, filepath.Join(dir, "check.mk"), "include config.ini\n.PHONY: check\ncheck:\n\t@echo $(ARCH):$(KERNEL3):$(DIBAB_VERBOSE_BUILD):$(SEND_BUILD_LOG)\n")
			makeCmd := exec.Command("make", "-s", "-f", "check.mk", "check")
			makeCmd.Dir = dir
			makeOut, e := makeCmd.CombinedOutput()
			if e != nil {
				t.Fatal(e, string(makeOut))
			}
			shell := exec.Command("bash", "-c", `. ./config.ini; printf '%s:%s:%s:%s\n' "$ARCH" "$KERNEL3" "$DIBAB_VERBOSE_BUILD" "$SEND_BUILD_LOG"`)
			shell.Dir = dir
			shellOut, e := shell.CombinedOutput()
			if e != nil || string(makeOut) != string(shellOut) || !strings.HasSuffix(string(makeOut), ":YES:NO\n") {
				t.Fatal(e, string(makeOut), string(shellOut))
			}
			if kernel != "" && !strings.Contains(string(makeOut), kernel) {
				t.Fatal(string(makeOut))
			}
		})
	}
}

// data/flathub-apps is hand-editable for command-line builds, so the script
// that turns it into flatpak arguments re-validates it rather than trusting
// whatever the web UI already checked.
func TestMakeFlatpakRejectsUnsafeApplicationIDs(t *testing.T) {
	script, e := filepath.Abs("../2.12/scripts/make_flatpak")
	if e != nil {
		t.Fatal(e)
	}
	run := func(t *testing.T, list string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		putFile(t, filepath.Join(dir, "config.ini"), "ARCH=x86_64\nHOME_DIBAB="+dir+"\n")
		putFile(t, filepath.Join(dir, "data", "flathub-apps"), list)
		cmd := exec.Command("bash", script)
		cmd.Dir = dir
		// An empty HOME keeps a hostile list from ever reaching a real flatpak.
		cmd.Env = append(os.Environ(), "HOME="+filepath.Join(dir, "empty"))
		b, e := cmd.CombinedOutput()
		return string(b), e
	}
	for _, list := range []string{
		"org.videolan.VLC; rm -rf /\n",
		"$(touch /tmp/injected)\n",
		"`touch /tmp/injected`\n",
		"../../etc/passwd\n",
		"org.videolan.VLC --installation=system\n",
	} {
		out, e := run(t, list)
		if e == nil {
			t.Fatalf("accepted unsafe list %q: %s", list, out)
		}
		if !strings.Contains(out, "invalid Flathub application ID") {
			t.Fatalf("wrong failure for %q: %s", list, out)
		}
	}
	// Nothing selected is not an error, and must not need flatpak at all.
	for _, list := range []string{"", "# nothing\n"} {
		if out, e := run(t, list); e != nil {
			t.Fatalf("empty list failed: %v %s", e, out)
		}
	}
	// A valid list gets past validation and only then misses the binary.
	out, e := run(t, "org.videolan.VLC\ncom.github.tchx84.Flatseal\n")
	if e == nil || !strings.Contains(out, "flatpak not found") {
		t.Fatalf("expected a missing-binary failure, got %v: %s", e, out)
	}
}

// The flatpak module must reach MODULES only when data/flathub-apps actually
// lists an application: scripts/make_iso exits 1 on a module named there but
// never built. ${ARCH} has to survive as a variable into config.ini, so this
// checks the expansion under both `make include` and `bash .` sourcing.
func TestFlathubModuleEntersConfiguration(t *testing.T) {
	script, e := filepath.Abs("../2.12/scripts/make_config_ini")
	if e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct {
		name, list string
		want       bool
	}{
		{"no list at all", "", false},
		{"comments only", "# nothing selected\n\n", false},
		{"one application", "org.videolan.VLC\n", true},
		{"comment and application", "# pick\norg.videolan.VLC\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.list != "" {
				putFile(t, filepath.Join(dir, "data", "flathub-apps"), tt.list)
			}
			cmd := exec.Command("bash", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "YDFS_ARCH=x86_64", "DISTRONAME=linuxconsole", "SEND_BUILD_LOG=NO", "BUILDYDFS=fast")
			if b, e := cmd.CombinedOutput(); e != nil {
				t.Fatal(e, string(b))
			}
			putFile(t, filepath.Join(dir, "check.mk"), "include config.ini\n.PHONY: check\ncheck:\n\t@echo $(MODULES)\n")
			makeCmd := exec.Command("make", "-s", "-f", "check.mk", "check")
			makeCmd.Dir = dir
			makeOut, e := makeCmd.CombinedOutput()
			if e != nil {
				t.Fatal(e, string(makeOut))
			}
			// MODULES is written before ARCH in config.ini, so a bare source
			// leaves ${ARCH} empty. The build scripts never see that: 2.12/Makefile
			// exports ARCH before running them, so reproduce that here.
			shell := exec.Command("bash", "-c", `export ARCH=x86_64; . ./config.ini; echo $MODULES`)
			shell.Dir = dir
			shellOut, e := shell.CombinedOutput()
			if e != nil || string(makeOut) != string(shellOut) {
				t.Fatal(e, string(makeOut), string(shellOut))
			}
			if got := strings.Contains(string(makeOut), "flatpak-x86_64"); got != tt.want {
				t.Fatalf("flatpak module present=%v, want %v: %s", got, tt.want, makeOut)
			}
			// The rest of the ISO must be unaffected either way.
			if !strings.Contains(string(makeOut), "linuxconsole-x86_64") || !strings.Contains(string(makeOut), "mate-x86_64") {
				t.Fatal(string(makeOut))
			}
		})
	}
}

// Mirrors the config.ini assembly performed by (*App).launch: an override
// applies, but a protected key stays pinned to the value appended afterward,
// consistently under both `make include` and `bash .` sourcing.
func TestConfigOverridesApplyAndProtectedKeysWin(t *testing.T) {
	script, e := filepath.Abs("../2.12/scripts/make_config_ini")
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	assemble := exec.Command("bash", "-c", `bash "$1" </dev/null
if [ -n "$YDFS_CONFIG_OVERRIDES" ]; then printf '%s\n' "$YDFS_CONFIG_OVERRIDES" >> config.ini; fi
printf '\nISOTMP=/web-output\nSEND_BUILD_LOG=NO\nSEND_OPKG=NO\nMENUCONFIG=NO\n' >> config.ini
`, "_", script)
	assemble.Dir = dir
	assemble.Env = append(os.Environ(), "YDFS_ARCH=x86_64", "DIBAB_VERBOSE_BUILD=YES", "SEND_BUILD_LOG=NO", "DISTRONAME=linuxconsole", "BUILDYDFS=fast",
		"YDFS_CONFIG_OVERRIDES=BUILDMODULES=YES\nSEND_OPKG=YES\nMENUCONFIG=YES")
	if b, e := assemble.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	putFile(t, filepath.Join(dir, "check.mk"), "include config.ini\n.PHONY: check\ncheck:\n\t@echo $(BUILDMODULES):$(SEND_OPKG):$(MENUCONFIG)\n")
	makeCmd := exec.Command("make", "-s", "-f", "check.mk", "check")
	makeCmd.Dir = dir
	makeOut, e := makeCmd.CombinedOutput()
	if e != nil {
		t.Fatal(e, string(makeOut))
	}
	shell := exec.Command("bash", "-c", `. ./config.ini; printf '%s:%s:%s\n' "$BUILDMODULES" "$SEND_OPKG" "$MENUCONFIG"`)
	shell.Dir = dir
	shellOut, e := shell.CombinedOutput()
	if e != nil {
		t.Fatal(e, string(shellOut))
	}
	if string(makeOut) != string(shellOut) {
		t.Fatal(string(makeOut), string(shellOut))
	}
	if strings.TrimSpace(string(makeOut)) != "YES:NO:NO" {
		t.Fatalf("override did not apply, or a protected key was overridden: %s", makeOut)
	}
}
func TestDockerOutageDoesNotReleaseQueue(t *testing.T) {
	a := testApp(t)
	p := filepath.Join(t.TempDir(), "docker")
	putFile(t, p, "#!/bin/sh\necho 'Cannot connect to Docker daemon' >&2\nexit 1\n")
	os.Chmod(p, 0755)
	a.docker = p
	j := saveFixtureJob(t, a, "running")
	a.run(j)
	got, _ := a.job(j.ID)
	if got.State != "running" {
		t.Fatalf("released build during Docker outage: %s", got.State)
	}
}
func awaitJob(t *testing.T, a *App, id string, predicate func(*Job) bool) *Job {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		j, e := a.job(id)
		if e == nil && predicate(j) {
			return j
		}
		time.Sleep(100 * time.Millisecond)
	}
	j, _ := a.job(id)
	t.Fatalf("timed out: %#v", j)
	return nil
}

func TestCancelDuringContainerPreparation(t *testing.T) {
	a := testApp(t)
	p := filepath.Join(t.TempDir(), "docker")
	putFile(t, p, "#!/bin/sh\nsleep 30 &\nwait\n")
	if e := os.Chmod(p, 0755); e != nil {
		t.Fatal(e)
	}
	a.docker = p
	j := saveFixtureJob(t, a, "cancelling")
	if e := os.MkdirAll(filepath.Join(a.repo, "2.12"), 0755); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	if e := a.launch(j); e == nil {
		t.Fatal("cancelled launch succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("compose process group did not stop promptly")
	}
}
func gitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("git %v: %v\n%s", args, e, out)
	}
}
func TestRepositoryUpdate(t *testing.T) {
	a := testApp(t)
	origin := t.TempDir()
	gitFixture(t, origin, "init", "-b", defaultBranch)
	putFile(t, filepath.Join(origin, "2.12", "Makefile"), "all:\n")
	gitFixture(t, origin, "add", ".")
	gitFixture(t, origin, "commit", "-m", "first")
	gitFixture(t, t.TempDir(), "clone", origin, a.repo)
	putFile(t, filepath.Join(origin, "2.12", "Makefile"), "all:\n\t@true\n")
	gitFixture(t, origin, "commit", "-am", "second commit")
	a.upstreamFrom = origin
	first, e := a.commit(context.Background(), "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	// Before any check the box shows the checkout alone, with no network hit.
	var s Repository
	if w := request(a, "GET", "/api/repository", ""); w.Code != 200 {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	} else if json.Unmarshal(w.Body.Bytes(), &s); s.Local.Revision != first.Revision || s.Upstream != nil {
		t.Fatalf("unexpected state %+v", s)
	}
	if w := request(a, "POST", "/api/repository/check", ""); w.Code != 200 {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	} else if json.Unmarshal(w.Body.Bytes(), &s); s.Behind != 1 || s.Ahead != 0 || !s.FastForward || s.Upstream.Subject != "second commit" {
		t.Fatalf("unexpected comparison %+v", s)
	}
	// A dirty checkout must not be merged over.
	putFile(t, filepath.Join(a.repo, "2.12", "Makefile"), "dirty\n")
	if w := request(a, "POST", "/api/repository/update", ""); w.Code != 409 {
		t.Fatalf("dirty update: %d %s", w.Code, w.Body)
	}
	gitFixture(t, a.repo, "checkout", "--", ".")
	if w := request(a, "POST", "/api/repository/update", ""); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	} else if json.Unmarshal(w.Body.Bytes(), &s); s.Behind != 0 || s.Local.Subject != "second commit" || s.Dirty {
		t.Fatalf("unexpected state after update %+v", s)
	}
	// A fork carrying its own commits is permanently diverged: the update must
	// merge instead of refusing, and must keep the local work.
	putFile(t, filepath.Join(a.repo, "webui", "local.txt"), "fork\n")
	gitFixture(t, a.repo, "add", ".")
	gitFixture(t, a.repo, "commit", "-m", "fork commit")
	putFile(t, filepath.Join(origin, "2.12", "packages", "list-x86_64"), "pkg\n")
	gitFixture(t, origin, "add", ".")
	gitFixture(t, origin, "commit", "-m", "third commit")
	if w := request(a, "POST", "/api/repository/update", ""); w.Code != 200 {
		t.Fatalf("merge update: %d %s", w.Code, w.Body)
	} else if json.Unmarshal(w.Body.Bytes(), &s); s.Behind != 0 || s.Ahead != 2 {
		t.Fatalf("unexpected state after merge %+v", s)
	}
	for _, f := range []string{"webui/local.txt", "2.12/packages/list-x86_64"} {
		if _, e := os.Stat(filepath.Join(a.repo, f)); e != nil {
			t.Fatalf("%s missing after merge: %v", f, e)
		}
	}
	// A conflicting upstream change is rolled back, leaving a usable checkout.
	putFile(t, filepath.Join(a.repo, "shared.txt"), "ours\n")
	gitFixture(t, a.repo, "add", ".")
	gitFixture(t, a.repo, "commit", "-m", "ours")
	putFile(t, filepath.Join(origin, "shared.txt"), "theirs\n")
	gitFixture(t, origin, "add", ".")
	gitFixture(t, origin, "commit", "-m", "theirs")
	if w := request(a, "POST", "/api/repository/update", ""); w.Code != 409 || !strings.Contains(w.Body.String(), "shared.txt") {
		t.Fatalf("conflicting update: %d %s", w.Code, w.Body)
	}
	if out, e := a.git(context.Background(), "status", "--porcelain"); e != nil || out != "" {
		t.Fatalf("checkout left dirty after conflict: %q %v", out, e)
	}
}
