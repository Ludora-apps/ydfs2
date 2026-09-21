package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// One finished ISO, booted in QEMU/KVM inside a container and shown in the
// browser over noVNC. One VM at a time, like the build worker: a test session
// costs 4 GiB and the point is to look at a screen, not to run a fleet.
//
// The container publishes only QEMU's VNC *websocket* port, and only on the
// loopback interface; the VNC server itself listens on a UNIX socket inside the
// container, so no raw VNC port exists anywhere. Browsers reach it exclusively
// through vmConsole, behind the same authentication as every other endpoint.
const (
	vmImageTag  = "ydfs-vm:1"
	vmGuestPort = "5700"
	vmMemoryMB  = 4096
	vmCPUs      = 4
	// Idle rules, in order of how often they actually fire: a console nobody
	// is watching, an app nobody has open (a browser that died without closing
	// its socket still counts as a viewer), and a hard ceiling.
	vmIdleLimit = 30 * time.Minute
	vmPollLimit = 15 * time.Minute
	vmMaxLife   = 4 * time.Hour
	// A guest needs 4 GiB and the host also runs builds; refuse rather than
	// push the machine into swap.
	vmMinAvailable = 6 << 30
)

type VM struct {
	Session   string `json:"session"`
	JobID     string `json:"jobId"`
	Target    string `json:"target"`
	ISO       string `json:"iso"`
	Container string `json:"container"`
	State     string `json:"state"` // starting | running | stopping | stopped
	Started   string `json:"started"`
	User      string `json:"user"`
	Viewers   int    `json:"viewers"`
	Password  string `json:"password,omitempty"`
	Error     string `json:"error,omitempty"`

	endpoint  string
	lastEmpty time.Time
	lastPoll  time.Time
	startedAt time.Time
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// vmSupport reports whether a VM can be started at all, and why not. Both the
// capabilities probe and vmStart use it, so the button's tooltip and the error
// the button produces can never disagree.
func (a *App) vmSupport(ctx context.Context) string {
	if _, e := os.Stat(a.kvm); e != nil {
		return "KVM is unavailable on this host, so a build cannot be booted here"
	}
	if e := a.command(ctx, "image", "inspect", "--format", "{{.Id}}", a.vmImage).Run(); e != nil {
		return "the test-VM image is missing: run `make vm-image` in webui/"
	}
	return ""
}

// availableMemory reports MemAvailable, which unlike MemFree accounts for
// reclaimable page cache — most of this host's memory is build-artifact cache.
func availableMemory() uint64 {
	b, e := os.ReadFile("/proc/meminfo")
	if e != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		kb, _ := strconv.ParseUint(f[1], 10, 64)
		return kb * 1024
	}
	return 0
}

// isoPath resolves a build's ISO the way downloadArtifact does: the name has to
// be one this build actually recorded, and the file is opened through an
// fileRoot so a symlink cannot escape the job directory.
func (a *App) isoPath(j *Job) (string, error) {
	name := ""
	for _, v := range j.Artifacts {
		if strings.HasSuffix(v.Name, ".iso") && filepath.Base(v.Name) == v.Name {
			name = v.Name
			break
		}
	}
	if name == "" {
		return "", errors.New("this build has no ISO on disk")
	}
	root, e := openRoot(filepath.Join(a.dir(j), "output"))
	if e != nil {
		return "", errors.New("this build's ISO is unavailable")
	}
	defer root.Close()
	f, e := root.Open(name)
	if e != nil {
		return "", errors.New("this build's ISO is unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return "", errors.New("this build's ISO is unavailable")
	}
	return filepath.Join(a.dir(j), "output", name), nil
}

// vmHolds reports whether a VM is currently booted from this build's ISO, so
// retention leaves the files alone: deleting them under a live console would
// flip the UI to "reclaimed" without even freeing the space, since QEMU holds
// the descriptor until it exits.
func (a *App) vmHolds(id string) bool {
	a.vmMu.Lock()
	defer a.vmMu.Unlock()
	return a.vm != nil && a.vm.JobID == id
}

func (a *App) vmStart(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	a.mu.Lock()
	j, e := a.job(id)
	if e != nil {
		a.mu.Unlock()
		fail(w, 404, "build not found")
		return
	}
	switch {
	case j.State != "succeeded":
		a.mu.Unlock()
		fail(w, 409, "only a succeeded build can be booted")
		return
	case !isoTarget(j.Settings.Target):
		a.mu.Unlock()
		fail(w, 409, "only an ISO build can be booted in a virtual machine")
		return
	case j.PrunedAt != "":
		a.mu.Unlock()
		fail(w, 409, "this build's ISO was reclaimed; keep a build to pin it before testing")
		return
	}
	iso, e := a.isoPath(j)
	a.mu.Unlock()
	if e != nil {
		fail(w, 409, e.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if msg := a.vmSupport(ctx); msg != "" {
		fail(w, 503, msg)
		return
	}
	if free := availableMemory(); free > 0 && free < vmMinAvailable {
		fail(w, 503, "not enough free memory to start a virtual machine right now")
		return
	}

	user := r.Header.Get("X-Forwarded-User")
	if a.dev {
		user = "local-developer"
	}
	vm := &VM{
		Session:   randomHex(8),
		JobID:     j.ID,
		Target:    j.Settings.Target,
		ISO:       filepath.Base(iso),
		Container: "ydfs-vm-" + randomHex(6),
		State:     "starting",
		Started:   now(),
		User:      user,
		Password:  randomHex(4), // VNC passwords are 8 characters
		startedAt: time.Now(),
		lastEmpty: time.Now(),
		lastPoll:  time.Now(),
	}
	a.vmMu.Lock()
	if a.vm != nil {
		running := a.vm.JobID
		a.vmMu.Unlock()
		fail(w, 409, "a test machine is already running for build "+running)
		return
	}
	a.vm = vm
	a.vmMu.Unlock()

	if e := a.vmLaunch(ctx, vm, iso); e != nil {
		a.vmFinish(vm, e.Error())
		fail(w, 500, e.Error())
		return
	}
	a.vmMu.Lock()
	vm.State = "running"
	a.vmMu.Unlock()
	go a.vmWatch(vm)
	respond(w, a.vmView())
}

// vmLaunch runs the container and waits until QEMU's websocket port answers.
// Nothing here holds a lock: docker run pulls no images but still takes a
// second, and a build must never wait on a test machine.
func (a *App) vmLaunch(ctx context.Context, vm *VM, iso string) error {
	st, e := os.Stat(a.kvm)
	if e != nil {
		return errors.New("KVM is unavailable on this host")
	}
	kvmGID := "0"
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		kvmGID = strconv.FormatUint(uint64(sys.Gid), 10)
	}
	args := []string{
		"run", "--rm", "--detach", "--name", vm.Container,
		"--label", "ydfs-vm=1", "--label", "ydfs-job=" + vm.JobID,
		"--device", a.kvm,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--group-add", kvmGID,
		"--memory", "5g", "--memory-swap", "5g", "--cpus", strconv.Itoa(vmCPUs),
		"--pids-limit", "256", "--oom-score-adj", "500",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=16m",
		"--publish", "127.0.0.1:0:" + vmGuestPort,
		"--mount", "type=bind,src=" + iso + ",dst=/iso/live.iso,readonly",
		a.vmImage,
		"qemu-system-x86_64",
		"-machine", "q35,accel=kvm", "-cpu", "host",
		"-smp", strconv.Itoa(vmCPUs), "-m", strconv.Itoa(vmMemoryMB),
		"-device", "qemu-xhci,id=xhci", "-device", "usb-tablet,bus=xhci.0",
		"-vga", "std", "-display", "none",
		"-object", "secret,id=vncsec,data=" + vm.Password,
		// A UNIX-socket primary is what keeps the websocket listener on
		// 0.0.0.0 *inside* the container (QEMU otherwise copies the primary's
		// address onto it, and the published port would never connect), and it
		// means no raw VNC port exists to be found.
		"-vnc", "unix:/tmp/vnc.sock,websocket=" + vmGuestPort + ",password-secret=vncsec",
		"-drive", "if=none,id=cd0,media=cdrom,file=/iso/live.iso,readonly=on",
		"-device", "ide-cd,drive=cd0,bus=ide.0,bootindex=1",
		"-boot", "order=d,menu=off",
		"-nic", "user,model=e1000",
		"-rtc", "base=utc", "-no-reboot", "-monitor", "none",
	}
	if out, e := a.command(ctx, args...).CombinedOutput(); e != nil {
		return fmt.Errorf("could not start the virtual machine: %s", firstLine(string(out)))
	}

	for i := 0; ; i++ {
		out, e := a.command(ctx, "port", vm.Container, vmGuestPort+"/tcp").Output()
		if line := firstLine(string(out)); e == nil && line != "" {
			a.vmMu.Lock()
			vm.endpoint = line
			a.vmMu.Unlock()
			break
		}
		if i == 10 {
			return errors.New("the virtual machine did not publish a console port")
		}
		time.Sleep(300 * time.Millisecond)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if vmConsoleAnswers(vm.endpoint) {
			return nil
		}
		if !a.vmAlive(ctx, vm.Container) {
			return fmt.Errorf("the virtual machine exited while starting: %s", a.vmTail(ctx, vm.Container))
		}
		if time.Now().After(deadline) {
			return errors.New("the virtual machine console did not come up")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// vmConsoleAnswers reports whether QEMU's websocket console is actually serving
// yet. A bare TCP dial is not enough: Docker's userland proxy accepts the
// connection as soon as the port is published and only then connects to the
// container, so a dial succeeds — and the first real request is reset — for the
// second or so before QEMU opens its listener. QEMU answers any non-websocket
// request with "400 Bad Request", so one byte of HTTP response is the proof.
func vmConsoleAnswers(endpoint string) bool {
	c, e := net.DialTimeout("tcp", endpoint, time.Second)
	if e != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, e = c.Write([]byte("GET / HTTP/1.0\r\n\r\n")); e != nil {
		return false
	}
	b := make([]byte, 5)
	n, e := c.Read(b)
	return e == nil && n > 0 && strings.HasPrefix(string(b[:n]), "HTTP")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func (a *App) vmAlive(ctx context.Context, container string) bool {
	out, e := a.command(ctx, "inspect", "--format", "{{.State.Running}}", container).Output()
	return e == nil && strings.TrimSpace(string(out)) == "true"
}

// vmTail is only ever used to explain a failure. The ISO's kernel command line
// has no console=ttyS0, so this is usually QEMU's own complaint rather than
// guest output.
func (a *App) vmTail(ctx context.Context, container string) string {
	out, _ := a.command(ctx, "logs", "--tail", "3", container).CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		return firstLine(s)
	}
	return "no output"
}

// vmFinish stops the container and releases the single VM slot. It runs on
// every exit path, including the ones where docker itself failed, because a
// slot that is never released wedges the feature until a restart.
func (a *App) vmFinish(vm *VM, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.vmMu.Lock()
	if a.vm == vm {
		a.vm.State = "stopping"
	}
	a.vmMu.Unlock()
	a.command(ctx, "stop", "--time", "5", vm.Container).Run()
	a.command(ctx, "rm", "--force", vm.Container).Run()
	a.vmMu.Lock()
	if a.vm == vm {
		a.vm = nil
	}
	vm.State = "stopped"
	vm.Error = reason
	vm.Password = ""
	a.vmLast = vm
	a.vmMu.Unlock()
}

// vmWatch owns one session's lifetime. Every rule here exists because the
// obvious one is not enough: noVNC sends nothing while the screen is still, so
// a viewer count alone cannot tell a watched VM from an abandoned one.
func (a *App) vmWatch(vm *VM) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			a.vmFinish(vm, "the server shut down")
			return
		case <-t.C:
		}
		a.vmMu.Lock()
		if a.vm != vm {
			a.vmMu.Unlock()
			return
		}
		viewers, lastEmpty, lastPoll := vm.Viewers, vm.lastEmpty, vm.lastPoll
		a.vmMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		alive := a.vmAlive(ctx, vm.Container)
		cancel()
		switch {
		case !alive:
			a.vmFinish(vm, "the virtual machine stopped")
			return
		case viewers == 0 && !lastEmpty.IsZero() && time.Since(lastEmpty) > vmIdleLimit:
			a.vmFinish(vm, "stopped after 30 minutes with nobody watching")
			return
		case time.Since(lastPoll) > vmPollLimit:
			a.vmFinish(vm, "stopped because the build manager was no longer open")
			return
		case time.Since(vm.startedAt) > vmMaxLife:
			a.vmFinish(vm, "stopped after the four-hour limit")
			return
		}
	}
}

func (a *App) vmStop(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	a.vmMu.Lock()
	vm := a.vm
	a.vmMu.Unlock()
	if vm == nil {
		respond(w, a.vmView())
		return
	}
	if vm.JobID != id {
		fail(w, 409, "the running machine belongs to build "+vm.JobID)
		return
	}
	by := r.Header.Get("X-Forwarded-User")
	if a.dev || by == "" {
		by = "local-developer"
	}
	a.vmFinish(vm, "stopped by "+by)
	respond(w, a.vmView())
}

func (a *App) vmStatus(w http.ResponseWriter, r *http.Request) {
	a.vmMu.Lock()
	if a.vm != nil {
		a.vm.lastPoll = time.Now()
	}
	a.vmMu.Unlock()
	respond(w, a.vmView())
}

// vmView is the whole VM state the browser gets: the live session if there is
// one, otherwise the last one that ended, so the UI can say why it ended.
func (a *App) vmView() map[string]any {
	a.vmMu.Lock()
	defer a.vmMu.Unlock()
	vm := a.vm
	if vm == nil {
		vm = a.vmLast
	}
	if vm == nil {
		return map[string]any{"state": "stopped"}
	}
	idle := 0
	if vm.Viewers == 0 && !vm.lastEmpty.IsZero() && a.vm == vm {
		idle = int(time.Since(vm.lastEmpty).Seconds())
	}
	return map[string]any{
		"session": vm.Session, "jobId": vm.JobID, "target": vm.Target, "iso": vm.ISO,
		"state": vm.State, "started": vm.Started, "user": vm.User,
		"viewers": vm.Viewers, "idleFor": idle, "password": vm.Password,
		"error": vm.Error, "idleLimit": int(vmIdleLimit.Seconds()),
	}
}

// vmConsole proxies the browser's WebSocket to QEMU's VNC websocket listener.
//
// A WebSocket handshake is a GET, so handler()'s CSRF check does not cover it,
// and a browser cannot be made to send X-Requested-With on a WebSocket. Origin
// is the one header a page cannot forge, so it is checked explicitly here --
// without it, any site the operator visits could open a console.
func (a *App) vmConsole(w http.ResponseWriter, r *http.Request) {
	if !a.dev && r.Header.Get("Origin") != a.origin {
		fail(w, 403, "request origin rejected")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		fail(w, 400, "websocket upgrade required")
		return
	}
	a.vmMu.Lock()
	vm := a.vm
	if vm == nil || vm.State != "running" || vm.endpoint == "" {
		a.vmMu.Unlock()
		fail(w, 409, "no test machine is running")
		return
	}
	endpoint := vm.endpoint
	vm.Viewers++
	vm.lastEmpty = time.Time{}
	a.vmMu.Unlock()
	defer func() {
		a.vmMu.Lock()
		if vm.Viewers > 0 {
			vm.Viewers--
		}
		if vm.Viewers == 0 {
			vm.lastEmpty = time.Now()
		}
		a.vmMu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// A hijacked request's context is not cancelled by srv.Shutdown, so wire
	// the process context in explicitly or shutdown would block on a viewer.
	go func() {
		select {
		case <-a.ctx.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	proxy := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL.Scheme = "http"
			out.URL.Host = endpoint
			out.URL.Path = "/"
			out.URL.RawQuery = ""
			out.URL.RawPath = ""
			out.Header.Del("Forwarded")
			out.Header.Del("X-Forwarded-Host")
			out.Header.Del("X-Forwarded-Proto")
			out.Header["X-Forwarded-For"] = nil
			// QEMU rejects a websocket handshake that carries no Host header.
			out.Host = endpoint
			// QEMU matches these header names case-sensitively, while Go
			// canonicalises them on the way in ("Sec-WebSocket-Key" becomes
			// "Sec-Websocket-Key"). Handing QEMU the canonical spelling gets
			// the connection reset with no reply, so put the RFC spelling
			// back by writing the map directly -- Header.Set would just
			// canonicalise it again, and the Transport writes keys verbatim.
			for _, k := range []string{"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol", "Sec-Websocket-Extensions"} {
				if v, ok := out.Header[k]; ok {
					delete(out.Header, k)
					out.Header[strings.Replace(k, "Websocket", "WebSocket", 1)] = v
				}
			}
		},
		FlushInterval: -1,
		Transport:     &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext},
		// After a hijack the status line is already on the wire, so writing a
		// second one panics: log, and let the browser see a closed socket.
		ErrorHandler: func(_ http.ResponseWriter, _ *http.Request, e error) {
			fmt.Fprintln(os.Stderr, "vm console:", e)
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// reapVMs removes every container this feature has ever started. Sessions are
// ephemeral by definition, so a VM that outlived its server is waste, not state
// worth adopting: nothing on disk records its viewers, its idle clock or its
// published port.
func (a *App) reapVMs(ctx context.Context) {
	out, e := a.command(ctx, "ps", "--all", "--quiet", "--filter", "label=ydfs-vm=1").Output()
	if e != nil {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		a.command(ctx, "rm", "--force", id).Run()
	}
}
