package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (a *App) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, a.docker, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd
}
func (a *App) freeBytes() (uint64, error) {
	var s syscall.Statfs_t
	e := syscall.Statfs(a.data, &s)
	return s.Bavail * uint64(s.Bsize), e
}

// capabilities is polled every few seconds by every open browser, so it stays
// small: the Flathub catalogue is thousands of applications and has its own
// endpoint (GET /api/flathub), fetched once when the page loads.
func (a *App) capabilities(w http.ResponseWriter, r *http.Request) {
	ctx, c := context.WithTimeout(r.Context(), 5*time.Second)
	defer c()
	out, e := a.command(ctx, "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	dockerOK := e == nil
	ce := a.command(ctx, "compose", "version").Run()
	free, fe := a.freeBytes()
	msg := ""
	if !dockerOK {
		msg = "Docker is unavailable: " + strings.TrimSpace(string(out))
	} else if ce != nil {
		msg = "Docker Compose is unavailable"
	} else if fe != nil {
		msg = fe.Error()
	} else if free < a.minFree {
		msg = "Insufficient free storage to start a build"
	}
	user := r.Header.Get("X-Forwarded-User")
	if a.dev {
		user = "local-developer"
	}
	vmMessage := ""
	if dockerOK {
		vmMessage = a.vmSupport(ctx)
	} else {
		vmMessage = "Docker is unavailable, so a build cannot be booted here"
	}
	packageLists, _ := a.packageListNames()
	respond(w, map[string]any{"vm": vmMessage == "", "vmMessage": vmMessage, "targets": targets, "packageLists": packageLists, "architecture": "x86_64", "distribution": "linuxconsole", "user": user, "docker": dockerOK && ce == nil, "freeBytes": free, "minFreeBytes": a.minFree, "ready": msg == "", "message": msg, "defaults": Settings{Target: "fast-iso", Verbose: true, Flatpaks: []string{}}, "development": a.dev})
}
func gitOutput(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	b, e := cmd.Output()
	return strings.TrimSpace(string(b)), e
}
func (a *App) packageListNames() ([]string, error) {
	entries, e := os.ReadDir(filepath.Join(a.repo, "2.12", "packages"))
	if e != nil {
		return nil, e
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "list-") {
			names = append(names, strings.TrimPrefix(entry.Name(), "list-"))
		}
	}
	return names, nil
}
func (a *App) packageListPath(name string) (string, error) {
	if !packageListNamePattern.MatchString(name) || filepath.Base(name) != name {
		return "", errors.New("invalid package list name")
	}
	path := filepath.Join(a.repo, "2.12", "packages", "list-"+name)
	info, e := os.Stat(path)
	if e != nil || !info.Mode().IsRegular() {
		return "", errors.New("unknown package list")
	}
	return path, nil
}
func (a *App) packageLists(w http.ResponseWriter, r *http.Request) {
	names, e := a.packageListNames()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, map[string][]string{"lists": names})
}
func (a *App) packageListContent(w http.ResponseWriter, r *http.Request) {
	path, e := a.packageListPath(r.PathValue("name"))
	if e != nil {
		fail(w, 404, e.Error())
		return
	}
	b, e := os.ReadFile(path)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, map[string]string{"name": r.PathValue("name"), "content": string(b)})
}
func (a *App) submit(w http.ResponseWriter, r *http.Request) {
	var s Settings
	if !decode(w, r, &s) {
		return
	}
	if e := s.validate(); e != nil {
		fail(w, 400, e.Error())
		return
	}
	if s.PackageList != "" {
		if _, e := a.packageListPath(s.PackageList); e != nil {
			fail(w, 400, e.Error())
			return
		}
	}
	// The browser's list is never authoritative about what may be built in.
	for _, id := range s.Flatpaks {
		if !a.allowedFlatpak(id) {
			fail(w, 400, "unknown Flathub application "+id)
			return
		}
	}
	if s.Flatpaks == nil {
		s.Flatpaks = []string{}
	}
	free, e := a.freeBytes()
	if e != nil || free < a.minFree {
		fail(w, 409, "insufficient free storage")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	revision, e := gitOutput(a.repo, "rev-parse", "HEAD")
	if e != nil {
		fail(w, 500, "cannot read repository revision")
		return
	}
	tag, e := gitOutput(a.repo, "describe", "--tags", "--abbrev=0")
	if e != nil && s.Target == "fast-iso" {
		fail(w, 409, "fast ISO requires a repository release tag")
		return
	}
	b := make([]byte, 6)
	if _, e = rand.Read(b); e != nil {
		fail(w, 500, e.Error())
		return
	}
	id := time.Now().UTC().Format("20060102T150405.000000000") + "-" + hex.EncodeToString(b)
	user := r.Header.Get("X-Forwarded-User")
	if a.dev {
		user = "local-developer"
	}
	j := &Job{ID: id, State: "queued", User: user, Settings: s, Created: now(), Revision: revision, Tag: tag, Container: "ydfs-web-" + hex.EncodeToString(b), Artifacts: []Artifact{}}
	dir := a.dir(j)
	if e = os.MkdirAll(dir, 0700); e != nil {
		fail(w, 500, e.Error())
		return
	}
	if e = snapshot(filepath.Join(a.repo, "2.12"), filepath.Join(dir, "source")); e != nil {
		os.RemoveAll(dir)
		fail(w, 500, "snapshot failed: "+e.Error())
		return
	}
	if s.PackageList != "" {
		// The submitted text replaces this file inside the job's own snapshot
		// only; the shared checkout and other queued/running jobs are untouched.
		target := filepath.Join(dir, "source", "packages", "list-"+s.PackageList)
		if e = os.Chmod(target, 0644); e == nil {
			e = os.WriteFile(target, []byte(s.PackageListText), 0644)
		}
		if e != nil {
			os.RemoveAll(dir)
			fail(w, 500, "cannot apply package list override")
			return
		}
	}
	if len(s.Flatpaks) > 0 {
		// Same rule as the package list: the selection replaces this file
		// inside the job's own snapshot only, so the shared checkout and other
		// queued jobs keep theirs. scripts/make_config_ini and the flatpak make
		// target read it from /tmp/ydfs once the container copies the snapshot.
		target := filepath.Join(dir, "source", "data", "flathub-apps")
		if e = os.MkdirAll(filepath.Dir(target), 0755); e == nil {
			if _, se := os.Stat(target); se == nil {
				e = os.Chmod(target, 0644)
			}
		}
		if e == nil {
			e = os.WriteFile(target, []byte(strings.Join(s.Flatpaks, "\n")+"\n"), 0644)
		}
		if e != nil {
			os.RemoveAll(dir)
			fail(w, 500, "cannot apply Flathub selection")
			return
		}
	}
	// Record the exact submitted working tree, including local edits, for review.
	diff := exec.Command("git", "-C", a.repo, "diff", "HEAD", "--", "2.12")
	if data, e := diff.Output(); e == nil {
		os.WriteFile(filepath.Join(dir, "working-tree.patch"), data, 0600)
	}
	if e = os.MkdirAll(a.logDir(), 0700); e != nil {
		os.RemoveAll(dir)
		fail(w, 500, e.Error())
		return
	}
	if e = os.WriteFile(a.logPath(j.ID), nil, 0600); e != nil {
		os.RemoveAll(dir)
		fail(w, 500, e.Error())
		return
	}
	if e = a.saveJob(j); e != nil {
		os.RemoveAll(dir)
		fail(w, 500, e.Error())
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusAccepted)
	respond(w, j)
}
func snapshot(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(src, path)
		if e != nil {
			return e
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == "config.ini" {
			return nil
		}
		to := filepath.Join(dst, rel)
		info, e := d.Info()
		if e != nil {
			return e
		}
		if d.IsDir() {
			return os.MkdirAll(to, 0755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, e := os.Readlink(path)
			if e != nil {
				return e
			}
			return os.Symlink(link, to)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported source file: %s", rel)
		}
		in, e := os.Open(path)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()|0444)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		ce := out.Close()
		if e != nil {
			return e
		}
		return ce
	})
}
func (a *App) cancel(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, e := a.job(r.PathValue("id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	if terminal(j.State) {
		respond(w, j)
		return
	}
	if j.State == "queued" {
		j.State = "cancelled"
		j.Finished = now()
	} else {
		j.State = "cancelling"
	}
	if e = a.saveJob(j); e != nil {
		fail(w, 500, e.Error())
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
	respond(w, j)
}
func (a *App) deleteJob(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, e := a.job(r.PathValue("id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	if !terminal(j.State) {
		fail(w, 409, "cancel the build before deleting it")
		return
	}
	ctx, c := context.WithTimeout(r.Context(), 15*time.Second)
	defer c()
	if st, e := a.inspect(ctx, j); (e == nil && st.Running) || (e != nil && !errors.Is(e, errContainerMissing)) {
		fail(w, 409, "cannot confirm container is stopped")
		return
	}
	a.command(ctx, "rm", j.Container).Run()
	if e = os.RemoveAll(a.dir(j)); e != nil {
		fail(w, 500, e.Error())
		return
	}
	// The log no longer lives under the job directory, so it needs removing
	// explicitly: a build's log must not outlive the build.
	if e = a.removeLog(j.ID); e != nil {
		fail(w, 500, e.Error())
		return
	}
	if _, e = a.db.Exec("DELETE FROM jobs WHERE id=?", j.ID); e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (a *App) worker() {
	for a.ctx.Err() == nil {
		a.mu.Lock()
		js, e := a.jobs()
		var next *Job
		if e == nil {
			for i := len(js) - 1; i >= 0; i-- {
				if !terminal(js[i].State) && js[i].State != "queued" {
					next = &js[i]
					break
				}
			}
			if next == nil {
				for i := len(js) - 1; i >= 0; i-- {
					if js[i].State == "queued" {
						next = &js[i]
						break
					}
				}
			}
		}
		if next != nil && next.State == "queued" {
			free, fe := a.freeBytes()
			if fe != nil || free < a.minFree {
				next = nil
			} else {
				next.State = "starting"
				next.Started = now()
				if e = a.saveJob(next); e != nil {
					next = nil
				}
			}
		}
		a.mu.Unlock()
		if next != nil {
			a.run(next)
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		select {
		case <-a.ctx.Done():
			return
		case <-a.wake:
		case <-time.After(3 * time.Second):
		}
	}
}

type containerState struct {
	Status   string
	Running  bool
	ExitCode int
	Error    string
}

var errContainerMissing = errors.New("container missing")

func (a *App) inspect(ctx context.Context, j *Job) (containerState, error) {
	var s containerState
	b, e := a.command(ctx, "inspect", "--format", "{{json .State}}", j.Container).CombinedOutput()
	if e != nil {
		if strings.Contains(strings.ToLower(string(b)), "no such object") || strings.Contains(strings.ToLower(string(b)), "no such container") {
			return s, errContainerMissing
		}
		return s, e
	}
	e = json.Unmarshal(b, &s)
	return s, e
}
func (a *App) finish(j *Job, state string, code *int, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	latest, e := a.job(j.ID)
	if e != nil {
		return
	}
	if latest.State == "cancelling" {
		state = "cancelled"
	}
	latest.State = state
	latest.Finished = now()
	latest.ExitCode = code
	latest.Error = msg
	latest.Artifacts = j.Artifacts
	if e = a.saveJob(latest); e != nil {
		fmt.Fprintln(os.Stderr, "saving build result:", e)
	}
	// A new artifact just landed: reclaim whatever fell out of the window.
	// a.mu is held above, which is what pruneLocked requires.
	a.pruneLocked()
}
func (a *App) run(j *Job) {
	ctx, c := context.WithTimeout(a.ctx, 10*time.Second)
	st, e := a.inspect(ctx, j)
	c()
	if e != nil {
		if !errors.Is(e, errContainerMissing) {
			return
		}
		if j.State == "cancelling" {
			a.finish(j, "cancelled", nil, "")
			return
		}
		if j.State != "starting" {
			a.finish(j, "failed", nil, "Build container is missing; cannot resume")
			return
		}
		if e = a.launch(j); e != nil {
			if a.ctx.Err() != nil {
				return
			}
			checkCtx, cc := context.WithTimeout(a.ctx, 10*time.Second)
			_, ie := a.inspect(checkCtx, j)
			cc()
			if ie != nil {
				if !errors.Is(ie, errContainerMissing) {
					return
				}
				a.finish(j, "failed", nil, e.Error())
				return
			}
		}
	} else if st.Status == "created" {
		if e = a.command(a.ctx, "start", j.Container).Run(); e != nil {
			if a.ctx.Err() == nil {
				a.finish(j, "failed", nil, e.Error())
			}
			return
		}
	}
	a.mu.Lock()
	latest, e := a.job(j.ID)
	if e == nil && latest.State != "cancelling" {
		latest.State = "running"
		e = a.saveJob(latest)
	}
	a.mu.Unlock()
	if e != nil {
		return
	}
	logsCtx, stopLogs := context.WithCancel(a.ctx)
	logsDone := make(chan error, 1)
	go func() { logsDone <- a.captureLogs(logsCtx, j) }()
	defer stopLogs()

	for a.ctx.Err() == nil {
		current, e := a.job(j.ID)
		if e != nil {
			return
		}
		ctx, c := context.WithTimeout(a.ctx, 15*time.Second)
		if current.State == "cancelling" {
			a.command(ctx, "stop", "--time", "5", j.Container).Run()
		}
		st, e = a.inspect(ctx, j)
		c()
		if errors.Is(e, errContainerMissing) {
			stopLogs()
			<-logsDone
			a.finish(j, "failed", nil, "Build container was removed outside the manager")
			return
		}
		if e == nil {
			if !st.Running && st.Status != "created" {
				var logErr error
				select {
				case logErr = <-logsDone:
				case <-time.After(5 * time.Second):
					stopLogs()
					logErr = <-logsDone
				}
				if a.ctx.Err() != nil {
					return
				}
				if current.State == "cancelling" {
					a.finish(j, "cancelled", &st.ExitCode, "")
					return
				}
				if st.ExitCode != 0 {
					a.finish(j, "failed", &st.ExitCode, fmt.Sprintf("Build exited with code %d. %s", st.ExitCode, st.Error))
					return
				}
				if logErr != nil {
					a.finish(j, "failed", &st.ExitCode, "Could not persist complete build logs: "+logErr.Error())
					return
				}
				if e = a.collect(j); e != nil {
					a.finish(j, "failed", &st.ExitCode, e.Error())
					return
				}
				a.finish(j, "succeeded", &st.ExitCode, "")
				return
			}
		}
		select {
		case <-a.ctx.Done():
		case <-a.wake:
		case <-time.After(time.Second):
		}
	}
	stopLogs()
	<-logsDone
}
func (a *App) launch(j *Job) error {
	cache := filepath.Join(a.data, "build-home")
	for _, name := range []string{"ydfs", "opkg", "mate", "cinnamon", "linuxconsole", "x86_64", "multilib", "llvm-multilib", "kde"} {
		p := filepath.Join(cache, "2.12", name)
		if e := os.MkdirAll(p, 0777); e != nil {
			return e
		}
		if e := os.Chmod(p, 0777); e != nil {
			return e
		}
	}
	for _, p := range []string{filepath.Join(cache, "iso"), filepath.Join(a.dir(j), "output")} {
		if e := os.MkdirAll(p, 0777); e != nil {
			return e
		}
		if e := os.Chmod(p, 0777); e != nil {
			return e
		}
	}
	// Fixed script and argv: user-controlled text never becomes shell source.
	script := `set -e
mkdir -p /tmp/ydfs
cp -a /web-source/. /tmp/ydfs/
cd /tmp/ydfs
cp init-x86/etc/profile "$HOME/.bashrc"
bash scripts/make_config_ini </dev/null
if [ -n "$YDFS_CONFIG_OVERRIDES" ]; then printf '%s\n' "$YDFS_CONFIG_OVERRIDES" >> config.ini; fi
printf '\nISOTMP=/web-output\nSEND_BUILD_LOG=NO\nSEND_OPKG=NO\nMENUCONFIG=NO\n' >> config.ini
cp config.ini /web-output/config.ini
exec make "$1"
`
	target := j.Settings.Target
	switch target {
	case "fast-iso", "full-iso":
		target = "iso"
	case "kernel":
		target = "linux"
	}
	if target == "updates" {
		script = strings.Replace(script, `exec make "$1"`, `make updates
exec bash scripts/make_module linuxconsole`, 1)
	}
	if target == "initramfs" {
		script = strings.Replace(script, `exec make "$1"`, `make busybox
exec make "$1"`, 1)
	}
	args := []string{"compose", "-f", filepath.Join(a.repo, "2.12", "docker-compose.yml"), "run", "--detach", "--no-deps", "-T", "--name", j.Container, "--volume", filepath.Join(a.dir(j), "source") + ":/web-source:ro", "--volume", filepath.Join(a.dir(j), "source") + ":/2.12:ro", "--volume", filepath.Join(a.dir(j), "output") + ":/web-output", "-e", "YDFS_ARCH=x86_64", "-e", "DISTRONAME=linuxconsole", "-e", "GIT_BRANCH=2.12", "-e", "GIT_TAG=" + j.Tag, "-e", "SEND_BUILD_LOG=NO", "-e", "SEND_OPKG=NO", "-e", "MENUCONFIG=NO", "-e", "SLEEPTIME=1"}
	verbose := "NO"
	if j.Settings.Verbose {
		verbose = "YES"
	}
	fast := ""
	if j.Settings.Target == "fast-iso" {
		fast = "fast"
	}
	args = append(args, "-e", "DIBAB_VERBOSE_BUILD="+verbose, "-e", "BUILDYDFS="+fast, "-e", "YDFS_CUSTOM_KERNEL="+j.Settings.Kernel, "-e", "YDFS_CONFIG_OVERRIDES="+j.Settings.ConfigOverrides, "ydfs2.12", "bash", "-c", script, "ydfs-web", target)
	launchCtx, cancelLaunch := context.WithCancel(a.ctx)
	defer cancelLaunch()
	cmd := a.command(launchCtx, args...)
	cmd.Dir = filepath.Join(a.repo, "2.12")
	cmd.Env = append(os.Environ(), "HOME="+cache, "PWD="+cmd.Dir, "DISTRONAME=linuxconsole")
	// Allow cancellation while Compose is pulling an image or creating the container.
	result := make(chan error, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("cannot launch Docker build: %w: %s", err, out)
		}
		result <- err
	}()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			return err
		case <-a.ctx.Done():
			cancelLaunch()
			return <-result
		case <-ticker.C:
			current, err := a.job(j.ID)
			if err == nil && current.State == "cancelling" {
				cancelLaunch()
				return <-result
			}
		}
	}
}
func (a *App) captureLogs(ctx context.Context, j *Job) error {
	path := a.logPath(j.ID)
	f, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	// Replay Docker's persisted log after a restart, skipping bytes already written.
	rd, wr, e := os.Pipe()
	if e != nil {
		return e
	}
	defer rd.Close()
	cmd := a.command(ctx, "logs", "--follow", j.Container)
	cmd.Stdout = wr
	cmd.Stderr = wr
	if e = cmd.Start(); e != nil {
		wr.Close()
		return e
	}
	wr.Close()
	if info.Size() > 0 {
		_, e = io.CopyN(io.Discard, rd, info.Size())
	}
	if e == nil {
		_, e = io.Copy(f, rd)
	}
	ce := cmd.Wait()
	if e != nil {
		return e
	}
	return ce
}
func (a *App) collect(j *Job) error {
	output := filepath.Join(a.dir(j), "output")
	entries, e := os.ReadDir(output)
	if e != nil {
		return e
	}
	j.Artifacts = []Artifact{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".iso") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		j.Artifacts = append(j.Artifacts, Artifact{entry.Name(), info.Size()})
	}
	if (j.Settings.Target == "fast-iso" || j.Settings.Target == "full-iso") && len(j.Artifacts) == 0 {
		return errors.New("build exited successfully but produced no nonempty ISO")
	}
	if j.Settings.Target != "fast-iso" && j.Settings.Target != "full-iso" {
		// Copy component outputs out of mutable caches while the queue is locked to this job.
		cache := filepath.Join(a.data, "build-home", "2.12", "ydfs")
		pattern := ""
		switch j.Settings.Target {
		case "kernel":
			k := j.Settings.Kernel
			if k == "" {
				k = generatedValue(output, "KERNEL3")
			}
			pattern = filepath.Join(cache, "build", "linux-x86_64-"+k, "arch", "x86_64", "boot", "bzImage")
		case "busybox":
			pattern = filepath.Join(cache, "build", "busybox-x86_64-"+generatedValue(output, "BUSYBOX"), "_install", "bin", "busybox")
		case "initramfs":
			pattern = filepath.Join(cache, "build-x86_64", "initramfs")
		case "updates":
			pattern = filepath.Join(cache, "build", "modules", "linuxconsole-x86_64.squashfs")
		default:
			pattern = filepath.Join(cache, "build", "modules", j.Settings.Target+"-x86_64.squashfs")
		}
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			return errors.New("build exited successfully but expected component output is missing")
		}
		for _, src := range matches {
			in, e := os.Open(src)
			if e != nil {
				return e
			}
			info, e := in.Stat()
			if e != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				in.Close()
				return errors.New("component output is not a nonempty regular file")
			}
			name := filepath.Base(src)
			dst, e := os.OpenFile(filepath.Join(output, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if e != nil {
				in.Close()
				return e
			}
			_, e = io.Copy(dst, in)
			in.Close()
			ce := dst.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
			j.Artifacts = append(j.Artifacts, Artifact{name, info.Size()})
		}
	}
	return nil
}
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	j, e := a.job(r.PathValue("id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	offset := int64(0)
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("offset")
	}
	if raw != "" {
		offset, e = strconv.ParseInt(raw, 10, 64)
		if e != nil || offset < 0 {
			fail(w, 400, "invalid log offset")
			return
		}
	}
	f, e := os.Open(a.logPath(j.ID))
	if e != nil {
		fail(w, 404, "log unavailable")
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	if offset > st.Size() {
		fail(w, 416, "log offset exceeds file size")
		return
	}
	f.Seek(offset, io.SeekStart)
	flush, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flush.Flush()
	buf := make([]byte, 16384)
	for {
		n, re := f.Read(buf)
		if n > 0 {
			offset += int64(n)
			b, _ := json.Marshal(string(buf[:n]))
			if _, e = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", offset, b); e != nil {
				return
			}
			flush.Flush()
			continue
		}
		if re != nil && re != io.EOF {
			return
		}
		latest, e := a.job(j.ID)
		if e != nil {
			return
		}
		if terminal(latest.State) {
			// The worker can finish between our EOF read and this state query.
			// Drain its final bytes before emitting the terminal event.
			if info, err := f.Stat(); err == nil && info.Size() > offset {
				continue
			}
			b, _ := json.Marshal(latest)
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", b)
			flush.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-a.ctx.Done():
			return
		case <-time.After(time.Second):
		}
		fmt.Fprint(w, ": heartbeat\n\n")
		flush.Flush()
	}
}
func (a *App) downloadLog(w http.ResponseWriter, r *http.Request) {
	j, e := a.job(r.PathValue("id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="build.log"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeFile(w, r, a.logPath(j.ID))
}
func (a *App) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	j, e := a.job(r.PathValue("id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	name := r.PathValue("name")
	found := false
	for _, v := range j.Artifacts {
		if name == v.Name {
			found = true
		}
	}
	if !found || filepath.Base(name) != name {
		fail(w, 404, "artifact not found")
		return
	}
	root, e := os.OpenRoot(filepath.Join(a.dir(j), "output"))
	if e != nil {
		fail(w, 404, "artifact unavailable")
		return
	}
	defer root.Close()
	f, e := root.Open(name)
	if e != nil {
		fail(w, 404, "artifact unavailable")
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		fail(w, 404, "artifact unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func generatedValue(output, key string) string {
	b, _ := os.ReadFile(filepath.Join(output, "config.ini"))
	value := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, key+"=") {
			value = strings.TrimPrefix(line, key+"=")
		}
	}
	if !kernelPattern.MatchString(value) {
		return "invalid"
	}
	return value
}
