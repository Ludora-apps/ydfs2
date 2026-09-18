package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerLifecycle(t *testing.T) {
	if os.Getenv("YDFS_DOCKER_TEST") != "1" {
		t.Skip("set YDFS_DOCKER_TEST=1 for real Docker integration")
	}
	a := testApp(t)
	build := t.TempDir()
	image := "ydfs-web-fixture:test"
	putFile(t, filepath.Join(build, "Dockerfile"), "FROM alpine:3.22\nRUN apk add --no-cache bash make g++ && adduser -D -u 1000 builder\nUSER builder\n")
	cmd := exec.Command("docker", "build", "-q", "-t", image, build)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	source := filepath.Join(a.repo, "2.12")
	putFile(t, filepath.Join(source, "docker-compose.yml"), "services:\n  ydfs2.12:\n    image: "+image+"\n")
	generator, e := os.ReadFile("../2.12/scripts/make_config_ini")
	if e != nil {
		t.Fatal(e)
	}
	putFile(t, filepath.Join(source, "scripts/make_config_ini"), string(generator))
	putFile(t, filepath.Join(source, "init-x86/etc/profile"), "# fixture\n")
	putFile(t, filepath.Join(source, "fixture.cpp"), "#include <iostream>\nint main(){std::cout << \"C++ compilation fixture passed\\n\";}\n")
	putFile(t, filepath.Join(source, "Makefile"), "iso:\n\tg++ -Wall -Werror fixture.cpp -o fixture\n\t./fixture\n\tsleep $$(cat delay)\n\tcp fixture /web-output/fixture.iso\nlinux:\n\t@echo 'intentional compiler failure' >&2\n\t@exit 17\n")
	putFile(t, filepath.Join(source, "delay"), "6")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.org", "commit", "-m", "fixture"}, {"tag", "vfixture"}} {
		cmd := exec.Command("git", append([]string{"-C", a.repo}, args...)...)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatal(e, string(b))
		}
	}
	submit := func(target string) *Job {
		w := request(a, "POST", "/api/jobs", fmt.Sprintf(`{"target":%q,"verbose":true,"kernel":""}`, target))
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		var j Job
		if e := json.Unmarshal(w.Body.Bytes(), &j); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", j.Container).Run() })
		return &j
	}
	first := submit("fast-iso")
	putFile(t, filepath.Join(source, "delay"), "30")
	second := submit("fast-iso")
	ctx, stop := context.WithCancel(context.Background())
	a.ctx = ctx
	workerDone := make(chan struct{})
	go func() { a.worker(); close(workerDone) }()
	t.Cleanup(func() { stop(); <-workerDone })
	awaitJob(t, a, first.ID, func(j *Job) bool { return j.State == "running" })
	queued, _ := a.job(second.ID)
	if queued.State != "queued" {
		t.Fatal("queue ran concurrently")
	}
	stop()
	<-workerDone
	ctx, stop = context.WithCancel(context.Background())
	a.ctx = ctx
	workerDone = make(chan struct{})
	go func() { a.worker(); close(workerDone) }()
	defer func() { stop(); <-workerDone }()
	finished := awaitJob(t, a, first.ID, func(j *Job) bool { return terminal(j.State) })
	if finished.State != "succeeded" || len(finished.Artifacts) != 1 {
		t.Fatalf("first failed: %#v", finished)
	}
	b, e := os.ReadFile(filepath.Join(a.dir(first), "build.log"))
	if e != nil || strings.Count(string(b), "C++ compilation fixture passed") != 1 {
		t.Fatal("log replay duplicated or lost output", e, string(b))
	}
	awaitJob(t, a, second.ID, func(j *Job) bool { return j.State == "running" })
	request(a, "POST", "/api/jobs/"+second.ID+"/cancel", `{}`)
	cancelled := awaitJob(t, a, second.ID, func(j *Job) bool { return terminal(j.State) })
	if cancelled.State != "cancelled" {
		t.Fatal(cancelled.State)
	}
	st, e := a.inspect(context.Background(), second)
	if e != nil || st.Running {
		t.Fatal("cancelled container still running", e)
	}
	failure := submit("kernel")
	failed := awaitJob(t, a, failure.ID, func(j *Job) bool { return terminal(j.State) })
	if failed.State != "failed" || failed.ExitCode == nil || *failed.ExitCode == 0 {
		t.Fatal(failed)
	}
	w := request(a, "GET", "/api/jobs/"+first.ID+"/artifacts/fixture.iso", "")
	if w.Code != 200 || w.Body.Len() == 0 {
		t.Fatal("first build artifact was lost")
	}
	t.Log("Actual C++ compilation, queue serialization, server restart, log replay, cancellation, failure, and artifact download passed")
	time.Sleep(10 * time.Millisecond)
}
