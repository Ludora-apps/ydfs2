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
