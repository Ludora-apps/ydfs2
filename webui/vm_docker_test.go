package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The whole console path with nothing stubbed: Docker, KVM, QEMU, the Go
// middleware and httputil.ReverseProxy's protocol upgrade. Opt-in, because it
// needs a working /dev/kvm and the ydfs-vm image.
//
//	YDFS_VM_TEST=1 go test -race -run TestVMLifecycle -v .
func TestVMLifecycle(t *testing.T) {
	if os.Getenv("YDFS_VM_TEST") != "1" {
		t.Skip("set YDFS_VM_TEST=1 to run the real virtual-machine test")
	}
	if _, e := os.Stat("/dev/kvm"); e != nil {
		t.Skip("no /dev/kvm on this host")
	}
	if b, e := exec.Command("docker", "image", "inspect", vmImageTag).CombinedOutput(); e != nil {
		t.Skipf("%s is missing, run `make vm-image`: %s", vmImageTag, b)
	}
	a := testApp(t)
	a.kvm = "/dev/kvm"
	a.vmImage = vmImageTag
	// A megabyte of zeros is enough: SeaBIOS finds nothing bootable and stays
	// at its prompt, which is exactly the state this test is about -- QEMU is
	// up and its websocket console answers. A real 3.5 GB ISO would only make
	// the test slower.
	j := saveArtifactJob(t, a, "20260101", "fast-iso", "linuxconsole.iso")
	if e := os.WriteFile(filepath.Join(a.dir(j), "output", "linuxconsole.iso"), make([]byte, 1<<20), 0644); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		exec.Command("docker", "rm", "--force", "--volumes", a.vmContainerName(t)).Run()
	})

	srv := httptest.NewServer(a.handler())
	defer srv.Close()

	w := request(a, "POST", "/api/jobs/20260101/vm", "{}")
	if w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var status struct {
		State, Session, Password string
	}
	if e := json.Unmarshal(w.Body.Bytes(), &status); e != nil {
		t.Fatal(e)
	}
	if status.State != "running" || status.Session == "" {
		t.Fatalf("unexpected status: %+v", status)
	}
	defer func() {
		if w := request(a, "DELETE", "/api/jobs/20260101/vm", ""); w.Code != 200 {
			t.Errorf("stop: %d %s", w.Code, w.Body.String())
		}
		if out, _ := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label=ydfs-vm=1").Output(); len(strings.TrimSpace(string(out))) != 0 {
			t.Errorf("container survived the stop: %s", out)
		}
	}()

	// One machine at a time.
	if w := request(a, "POST", "/api/jobs/20260101/vm", "{}"); w.Code != 409 {
		t.Fatalf("second machine allowed: %d %s", w.Code, w.Body.String())
	}

	// Hand-rolled handshake: Sec-WebSocket-Accept for this fixed key is
	// deterministic, and QEMU sends the RFB version as its first frame, so one
	// exchange proves middleware, proxy, Docker's port publishing and QEMU.
	host := strings.TrimPrefix(srv.URL, "http://")
	c, e := net.DialTimeout("tcp", host, 5*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /api/vm/console HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==\r\n"+
		"Origin: %s\r\nX-Build-Proxy-Secret: %s\r\nX-Forwarded-User: alice\r\n\r\n", host, a.origin, a.secret)
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(c)
	line, e := br.ReadString('\n')
	if e != nil || !strings.Contains(line, "101") {
		t.Fatalf("no upgrade: %q %v", line, e)
	}
	accepted := false
	for {
		h, e := br.ReadString('\n')
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(h, "ICX+Yqv66kxgM0FcWaLWlFLwTAI=") {
			accepted = true
		}
		if strings.TrimSpace(h) == "" {
			break
		}
	}
	if !accepted {
		t.Fatal("missing or wrong Sec-WebSocket-Accept")
	}
	frame := make([]byte, 64)
	n, e := br.Read(frame)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(frame[:n]), "RFB 003.00") {
		t.Fatalf("not a VNC stream: %q", frame[:n])
	}
}

// vmContainerName reports the live container's name so a failed test still
// cleans up after itself.
func (a *App) vmContainerName(t *testing.T) string {
	t.Helper()
	a.vmMu.Lock()
	defer a.vmMu.Unlock()
	if a.vm == nil {
		return "ydfs-vm-none"
	}
	return a.vm.Container
}
