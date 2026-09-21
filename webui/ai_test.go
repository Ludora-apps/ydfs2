package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitWorkspace makes the App's repo a real checkout: every workspace read goes
// through git, so the tests have to as well.
func gitWorkspace(t *testing.T, a *App, files map[string]string) {
	t.Helper()
	for p, body := range files {
		putFile(t, filepath.Join(a.repo, p), body)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "2.12"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "Test"},
		{"add", "-A"},
		{"commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", append([]string{"-C", a.repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %v\n%s", args, e, out)
		}
	}
}

func aiWorkspace(t *testing.T) *App {
	t.Helper()
	a := testApp(t)
	gitWorkspace(t, a, map[string]string{
		"2.12/Makefile":         "all:\n\t$(CC) -o net 2.12/tools/src/net.c\n",
		"2.12/tools/src/net.c":  "#include <stdio.h>\nint net_init(void){\n\treturn 0;\n}\n",
		"2.12/scripts/make_net": "#!/bin/sh\necho building\n",
		".gitignore":            "build/\n*.o\n",
		"build/output.iso":      "ignored binary output",
		"logo.png":              "\x89PNG not text",
	})
	return a
}

// The first boundary: a path supplied by a browser or by a model is never a
// filesystem path. Nothing below may resolve to anything outside the checkout.
func TestWorkspacePathRejectsEscapes(t *testing.T) {
	for _, p := range []string{
		"../../etc/passwd", "/etc/passwd", "..", "../outside", "2.12/../../etc/passwd",
		"./../../etc/shadow", "", "   ", "C:\\windows\\system32", "..\\..\\etc\\passwd",
		".git/config", ".git/hooks/pre-commit", "2.12/x\x00.c",
	} {
		if got, e := workspacePath(p); e == nil {
			t.Fatalf("accepted unsafe path %q as %q", p, got)
		}
	}
	for _, p := range []string{"2.12/tools/src/net.c", "./2.12/Makefile", "2.12/a/../b.c"} {
		if _, e := workspacePath(p); e != nil {
			t.Fatalf("rejected ordinary path %q: %v", p, e)
		}
	}
}

// A symlink pointing out of the checkout is the same attack wearing a hat:
// openRoot refuses to follow it, which is why every read goes through it.
func TestWorkspaceReadRefusesSymlinkEscape(t *testing.T) {
	a := aiWorkspace(t)
	secret := filepath.Join(t.TempDir(), "secret.conf")
	putFile(t, secret, "api-key=super-secret\n")
	if e := os.Symlink(secret, filepath.Join(a.repo, "2.12", "escape.conf")); e != nil {
		t.Skip("symlinks unavailable here")
	}
	if body, e := a.readWorkspaceFile("2.12/escape.conf"); e == nil {
		t.Fatalf("followed a symlink out of the workspace: %q", body)
	}
}

// The listing is what the tree is: ignored output, the git directory and
// binaries are not part of it, so they can never reach a model.
func TestWorkspaceListingHonoursGitignore(t *testing.T) {
	a := aiWorkspace(t)
	list, e := a.workspaceFiles(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(list, "\n")
	for _, want := range []string{"2.12/Makefile", "2.12/tools/src/net.c", "2.12/scripts/make_net"} {
		if !offeredFile(list, want) {
			t.Fatalf("workspace is missing %s:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"build/output.iso", "logo.png", ".git/config"} {
		if offeredFile(list, unwanted) {
			t.Fatalf("workspace should not list %s:\n%s", unwanted, joined)
		}
	}
}

// Reading a file the listing does not contain is refused even when the path
// itself is perfectly well formed and the file is really there.
func TestReadFileRefusesUnlistedPath(t *testing.T) {
	a := aiWorkspace(t)
	w := request(a, "GET", "/api/ai/file?path=build/output.iso", "")
	if w.Code != 404 {
		t.Fatalf("read an ignored file: %d %s", w.Code, w.Body)
	}
	w = request(a, "GET", "/api/ai/file?path=../../etc/passwd", "")
	if w.Code != 400 {
		t.Fatalf("traversal was not rejected: %d %s", w.Code, w.Body)
	}
	if w = request(a, "GET", "/api/ai/file?path=2.12/tools/src/net.c", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "net_init") {
		t.Fatalf("cannot read a workspace file: %d %s", w.Code, w.Body)
	}
}

func stage(t *testing.T, a *App, reply string) *Patch {
	t.Helper()
	p, e := a.stagePatch(context.Background(), reply)
	if e != nil {
		t.Fatal(e)
	}
	return p
}

// A proposal is a diff, and applying it writes exactly that diff.
func TestPatchProposeReviewAndApply(t *testing.T) {
	a := aiWorkspace(t)
	p := stage(t, a, "Here is the fix.\n\n```ydfs-patch path=2.12/tools/src/net.c\n#include <stdio.h>\nint net_init(void){\n\treturn 1;\n}\n```\n")
	if p == nil || len(p.Files) != 1 {
		t.Fatalf("no proposal was staged: %#v", p)
	}
	f := p.Files[0]
	if f.Added != 1 || f.Removed != 1 {
		t.Fatalf("unexpected diff counts: +%d -%d\n%s", f.Added, f.Removed, f.Diff)
	}
	if !strings.HasPrefix(f.Diff, "--- a/2.12/tools/src/net.c\n+++ b/2.12/tools/src/net.c\n@@") {
		t.Fatalf("diff is not a readable unified diff:\n%s", f.Diff)
	}
	// Nothing is written until Apply.
	if body, _ := a.readWorkspaceFile("2.12/tools/src/net.c"); strings.Contains(body, "return 1") {
		t.Fatal("the proposal was written before it was applied")
	}
	if w := request(a, "POST", "/api/ai/patch/apply", `{"id":"`+p.ID+`"}`); w.Code != 200 {
		t.Fatalf("apply failed: %d %s", w.Code, w.Body)
	}
	body, e := a.readWorkspaceFile("2.12/tools/src/net.c")
	if e != nil || !strings.Contains(body, "return 1") {
		t.Fatalf("apply did not write the file: %v %q", e, body)
	}
	// And the undo puts back exactly what was there.
	if w := request(a, "POST", "/api/ai/patch/reject", `{}`); w.Code != 200 {
		t.Fatalf("revert failed: %d %s", w.Code, w.Body)
	}
	if body, _ := a.readWorkspaceFile("2.12/tools/src/net.c"); !strings.Contains(body, "return 0") {
		t.Fatalf("revert did not restore the original: %q", body)
	}
}

// A model naming a path outside the workspace must not be able to write
// through the patch mechanism either.
func TestPatchRefusesPathsOutsideTheWorkspace(t *testing.T) {
	a := aiWorkspace(t)
	for _, reply := range []string{
		"```ydfs-patch path=../../etc/passwd\nroot::0:0::/:/bin/sh\n```",
		"```ydfs-patch path=/etc/passwd\nroot::0:0::/:/bin/sh\n```",
		"```ydfs-patch path=.git/hooks/post-commit\n#!/bin/sh\ncurl evil\n```",
		"```ydfs-patch path=2.12/../../outside.c\nint main(){}\n```",
	} {
		if _, e := a.stagePatch(context.Background(), reply); e == nil {
			t.Fatalf("staged a proposal outside the workspace: %s", reply)
		}
	}
	if _, e := os.Stat(filepath.Join(filepath.Dir(a.repo), "outside.c")); e == nil {
		t.Fatal("a file was created outside the workspace")
	}
}

// A new file is a normal proposal; an ignored or binary path is not.
func TestPatchCreatesNewFilesButNotIgnoredOnes(t *testing.T) {
	a := aiWorkspace(t)
	p := stage(t, a, "```ydfs-patch path=2.12/tools/src/net_retry.c\nint retry(void){ return 0; }\n```")
	if p == nil || !p.Files[0].Created || !strings.HasPrefix(p.Files[0].Diff, "--- /dev/null") {
		t.Fatalf("a new file was not proposed as a creation: %#v", p)
	}
	if _, e := a.stagePatch(context.Background(), "```ydfs-patch path=build/output.iso\nx\n```"); e == nil {
		t.Fatal("staged a proposal over an ignored file")
	}
	if _, e := a.stagePatch(context.Background(), "```ydfs-patch path=logo.png\nx\n```"); e == nil {
		t.Fatal("staged a proposal over a binary file")
	}
}

// The review the operator read has to still describe the tree when they press
// Apply, or the change they approved is not the change that would land.
func TestApplyRefusesWhenTheFileMovedUnderIt(t *testing.T) {
	a := aiWorkspace(t)
	p := stage(t, a, "```ydfs-patch path=2.12/tools/src/net.c\nint net_init(void){ return 2; }\n```")
	putFile(t, filepath.Join(a.repo, "2.12/tools/src/net.c"), "edited at the command line\n")
	w := request(a, "POST", "/api/ai/patch/apply", `{"id":"`+p.ID+`"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "changed after the proposal") {
		t.Fatalf("stale proposal was applied: %d %s", w.Code, w.Body)
	}
	if body, _ := a.readWorkspaceFile("2.12/tools/src/net.c"); !strings.Contains(body, "edited at the command line") {
		t.Fatalf("the file was overwritten anyway: %q", body)
	}
}

// Text that merely talks about code is not a proposal.
func TestExplanationsStageNoPatch(t *testing.T) {
	a := aiWorkspace(t)
	for _, reply := range []string{
		"The crash is in net_init: it returns before the socket is closed.",
		"Run this:\n\n```sh\nrm -rf /\n```\n",
		"```c\nint net_init(void){ return 0; }\n```",
	} {
		p, e := a.stagePatch(context.Background(), reply)
		if e != nil || p != nil {
			t.Fatalf("staged a patch from prose: %v %#v", e, p)
		}
	}
}

// Feature 7's input: the errors come out of the build log the existing
// pipeline already wrote, and only files the workspace lists are pulled in.
func TestCompilerErrorsComeFromTheBuildLog(t *testing.T) {
	a := aiWorkspace(t)
	j := saveFixtureJob(t, a, "failed")
	code := 2
	j.ExitCode, j.Settings.Target = &code, "busybox"
	if e := a.saveJob(j); e != nil {
		t.Fatal(e)
	}
	putFile(t, a.logPath(j.ID), strings.Join([]string{
		"gcc -c net.c",
		"2.12/tools/src/net.c:2:9: error: implicit declaration of function 'socket'",
		"/usr/include/stdio.h:10:1: error: not our file",
		"\x1b[31m2.12/tools/src/net.c:3:1: warning: unused variable\x1b[0m",
		"make[1]: *** [Makefile:4: net] Error 1",
	}, "\n"))
	errs, e := a.compilerErrors(j.ID)
	if e != nil {
		t.Fatal(e)
	}
	if errs.Count < 4 || !strings.Contains(errs.Text, "implicit declaration") {
		t.Fatalf("diagnostics were not extracted: %#v", errs)
	}
	if strings.Contains(errs.Text, "\x1b[") {
		t.Fatalf("terminal escapes reached the context: %q", errs.Text)
	}
	if len(errs.Files) != 1 || errs.Files[0] != "2.12/tools/src/net.c" {
		t.Fatalf("wrong files pulled in: %v", errs.Files)
	}
	if errs.Exit != "2" || errs.Target != "busybox" {
		t.Fatalf("build metadata missing: %#v", errs)
	}
}

// Everything the assistant can do is behind the same authentication and CSRF
// rules as the rest of the manager.
func TestAIEndpointsRequireAuthentication(t *testing.T) {
	a := aiWorkspace(t)
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/ai"}, {"GET", "/api/ai/files"}, {"GET", "/api/ai/file?path=2.12/Makefile"},
		{"POST", "/api/ai/message"}, {"POST", "/api/ai/patch/apply"}, {"PUT", "/api/ai/config"},
	} {
		r := httptest.NewRequest(ep.method, ep.path, strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:2345"
		w := httptest.NewRecorder()
		a.handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s %s served without authentication: %d", ep.method, ep.path, w.Code)
		}
	}
	// Authenticated but without the UI's header: a mutating request is still
	// refused, so a cross-site form cannot drive the assistant.
	r := httptest.NewRequest("POST", "/api/ai/message", strings.NewReader(`{"prompt":"hi"}`))
	r.RemoteAddr = "127.0.0.1:2345"
	r.Header.Set("X-Build-Proxy-Secret", a.secret)
	r.Header.Set("X-Forwarded-User", "alice")
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("a cross-site POST reached the assistant: %d", w.Code)
	}
}

// With nothing configured the page must say what to do, not fail obscurely,
// and it must never be able to leak the key it holds.
func TestAIConfigurationNeverReturnsTheKey(t *testing.T) {
	a := aiWorkspace(t)
	w := request(a, "GET", "/api/ai", "")
	if w.Code != 200 {
		t.Fatalf("state unavailable: %d %s", w.Code, w.Body)
	}
	var st struct {
		Ready         bool   `json:"ready"`
		Message       string `json:"message"`
		KeyConfigured bool   `json:"keyConfigured"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Ready || !strings.Contains(st.Message, "provider") {
		t.Fatalf("unconfigured state is not actionable: %#v", st)
	}
	body := `{"provider":"anthropic","model":"claude-sonnet-5","baseUrl":"https://api.anthropic.com","contextLimit":200000,"inputPrice":0,"cachedInputPrice":0,"outputPrice":0,"currency":"USD","timeoutSeconds":120,"maxTokens":4000,"apiKey":"sk-secret-value-that-must-never-come-back"}`
	if w = request(a, "PUT", "/api/ai/config", body); w.Code != 200 {
		t.Fatalf("configuration rejected: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "sk-secret") {
		t.Fatalf("the API key came back to the browser: %s", w.Body)
	}
	for _, path := range []string{"/api/ai", "/api/ai/usage", "/api/capabilities"} {
		if w = request(a, "GET", path, ""); strings.Contains(w.Body.String(), "sk-secret") {
			t.Fatalf("%s leaked the API key: %s", path, w.Body)
		}
	}
	if a.aiKey() != "sk-secret-value-that-must-never-come-back" {
		t.Fatal("the key was not stored server-side")
	}
	// And a provider error quoting it back is scrubbed before anyone sees it.
	redactions = func() []string { return []string{a.aiKey()} }
	defer func() { redactions = func() []string { return nil } }()
	if got := sanitizeAIError("401 from https://api.anthropic.com with key sk-secret-value-that-must-never-come-back"); strings.Contains(got, "sk-secret") {
		t.Fatalf("a provider error leaked the key: %s", got)
	}
}

func TestAISettingsValidation(t *testing.T) {
	ok := AISettings{Provider: "openai-compatible", Model: "qwen2.5-coder", BaseURL: "http://127.0.0.1:8000/v1"}
	ok.normalize()
	if e := ok.validate(); e != nil {
		t.Fatal(e)
	}
	for _, s := range []AISettings{
		{Provider: "", Model: "m"},
		{Provider: "made-up", Model: "m"},
		{Provider: "anthropic", Model: ""},
		{Provider: "openai-compatible", Model: "m", BaseURL: "file:///etc/passwd"},
		{Provider: "anthropic", Model: "m", ContextLimit: -1},
		{Provider: "anthropic", Model: "m", InputPrice: -3},
	} {
		s.normalize()
		if s.validate() == nil {
			t.Fatalf("accepted unsafe settings: %#v", s)
		}
	}
}

// Sending with no provider configured must explain itself rather than 500.
func TestAIMessageWithoutAProviderIsActionable(t *testing.T) {
	a := aiWorkspace(t)
	w := request(a, "POST", "/api/ai/message", `{"prompt":"why does net_init crash?"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "provider") {
		t.Fatalf("unhelpful error: %d %s", w.Code, w.Body)
	}
	if w = request(a, "POST", "/api/ai/message", `{"prompt":"   "}`); w.Code != 400 {
		t.Fatalf("an empty instruction was accepted: %d %s", w.Code, w.Body)
	}
	if w = request(a, "POST", "/api/ai/cancel", `{}`); w.Code != 409 {
		t.Fatalf("cancelling nothing should conflict: %d %s", w.Code, w.Body)
	}
}

// The context layer sends what was asked for and nothing else — never the
// repository, never an ignored file, never a path outside it.
func TestContextIsScopedToWhatWasSelected(t *testing.T) {
	a := aiWorkspace(t)
	ctx := context.Background()
	body, _, e := a.buildContext(ctx, []string{"2.12/tools/src/net.c"}, "", "", false)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(body, "net_init") {
		t.Fatalf("the selected file is missing from the context:\n%s", body)
	}
	if strings.Contains(body, "make_net") || strings.Contains(body, "ignored binary output") {
		t.Fatalf("unselected content reached the context:\n%s", body)
	}
	if _, _, e = a.buildContext(ctx, []string{"../../etc/passwd"}, "", "", false); e == nil {
		t.Fatal("a path outside the workspace was accepted as context")
	}
	if _, _, e = a.buildContext(ctx, []string{"build/output.iso"}, "", "", false); e == nil {
		t.Fatal("an ignored file was accepted as context")
	}
	many := make([]string, maxContextFiles+1)
	for i := range many {
		many[i] = "2.12/Makefile"
	}
	if _, _, e = a.buildContext(ctx, many, "", "", false); e == nil {
		t.Fatal("an unbounded context was accepted")
	}
}

// Usage accounting reports what the provider said and nothing it did not say.
func TestUsageKeepsNullsAndLabelsEstimates(t *testing.T) {
	s := AISettings{InputPrice: 3, OutputPrice: 15, Currency: "USD"}
	u := &AIUsage{InputTokens: intp(1_000_000), OutputTokens: intp(100_000), CachedInputTokens: intp(400_000)}
	s.price(u)
	if u.Cost == nil || *u.Cost <= 0 {
		t.Fatalf("no cost computed from configured rates: %#v", u)
	}
	// With no rates configured, the cost is absent and says why.
	bare := AISettings{Currency: "USD"}
	v := &AIUsage{InputTokens: intp(10)}
	bare.price(v)
	if v.Cost != nil || v.CostNote == "" {
		t.Fatalf("a cost was invented without configured prices: %#v", v)
	}
	// A provider that reports nothing leaves nulls, never zeros.
	w := &AIUsage{}
	bare.price(w)
	if w.InputTokens != nil || w.OutputTokens != nil {
		t.Fatalf("token counts were invented: %#v", w)
	}
	var totals AITotals
	totals.add(u)
	totals.add(v)
	if totals.Requests != 2 || totals.InputTokens != 1_000_010 {
		t.Fatalf("session totals do not add up: %#v", totals)
	}
}

// The build step is the existing pipeline: the assistant records which job is
// testing its work, it does not start a compiler of its own.
func TestBuildAdoptionUsesTheExistingJob(t *testing.T) {
	a := aiWorkspace(t)
	if w := request(a, "POST", "/api/ai/build", `{"jobId":"nope"}`); w.Code != 404 {
		t.Fatalf("an unknown build was adopted: %d %s", w.Code, w.Body)
	}
	j := saveFixtureJob(t, a, "running")
	w := request(a, "POST", "/api/ai/build", `{"jobId":"`+j.ID+`"}`)
	if w.Code != 200 {
		t.Fatalf("adoption failed: %d %s", w.Code, w.Body)
	}
	w = request(a, "GET", "/api/ai", "")
	var st struct {
		Build *AIBuild `json:"build"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Build == nil || st.Build.JobID != j.ID {
		t.Fatalf("the build was not remembered: %s", w.Body)
	}
}

// A conversation survives a restart, and a new one starts empty.
func TestSessionPersistsAndResets(t *testing.T) {
	a := aiWorkspace(t)
	sess, e := a.aiSession()
	if e != nil {
		t.Fatal(e)
	}
	sess.Messages = append(sess.Messages, AIMessage{Role: "user", Content: "hello", At: now()})
	sess.Totals.add(&AIUsage{InputTokens: intp(5), Provider: "ollama", Model: "qwen"})
	if e := a.saveAISession(sess); e != nil {
		t.Fatal(e)
	}
	again, e := a.aiSession()
	if e != nil || len(again.Messages) != 1 || again.Totals.Requests != 1 {
		t.Fatalf("the conversation did not persist: %v %#v", e, again)
	}
	if w := request(a, "POST", "/api/ai/session", `{}`); w.Code != 200 {
		t.Fatalf("cannot start a new conversation: %d %s", w.Code, w.Body)
	}
	fresh, _ := a.aiSession()
	if len(fresh.Messages) != 0 || fresh.ID == again.ID {
		t.Fatalf("a new conversation kept the old one: %#v", fresh)
	}
}

// End to end against a stub provider speaking the OpenAI streaming shape: the
// reply streams, the usage is recorded, and the proposal it contains is staged
// for review without touching the tree.
func TestGenerationStreamsAndStagesAProposal(t *testing.T) {
	a := aiWorkspace(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(s string) {
			b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{"content": s}}}})
			w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		chunk("The initialiser returns before the socket closes.\n\n")
		chunk("```ydfs-patch path=2.12/tools/src/net.c\n")
		chunk("#include <stdio.h>\nint net_init(void){\n\treturn 1;\n}\n```\n")
		usage, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 90, "prompt_tokens_details": map[string]any{"cached_tokens": 200}}})
		w.Write([]byte("data: " + string(usage) + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	cfg := `{"provider":"openai-compatible","model":"qwen2.5-coder","baseUrl":"` + srv.URL + `","contextLimit":32000,"inputPrice":1,"cachedInputPrice":0.1,"outputPrice":3,"currency":"USD","timeoutSeconds":30,"maxTokens":2000,"apiKey":"test-key"}`
	if w := request(a, "PUT", "/api/ai/config", cfg); w.Code != 200 {
		t.Fatalf("configuration rejected: %d %s", w.Code, w.Body)
	}
	w := request(a, "POST", "/api/ai/message", `{"prompt":"net_init crashes, find it","files":["2.12/tools/src/net.c"]}`)
	if w.Code != 200 {
		t.Fatalf("generation failed: %d %s", w.Code, w.Body)
	}
	stream := w.Body.String()
	for _, want := range []string{"event: context", "event: text", "event: usage", "event: done", "socket closes"} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream is missing %q:\n%s", want, stream)
		}
	}
	if strings.Contains(stream, "test-key") {
		t.Fatalf("the stream leaked the API key:\n%s", stream)
	}
	// The proposal is waiting for review; the tree is untouched.
	w = request(a, "GET", "/api/ai/patch", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "2.12/tools/src/net.c") {
		t.Fatalf("no proposal staged: %d %s", w.Code, w.Body)
	}
	if body, _ := a.readWorkspaceFile("2.12/tools/src/net.c"); strings.Contains(body, "return 1") {
		t.Fatal("the reply was written to the tree without approval")
	}
	// And the turn was accounted for.
	w = request(a, "GET", "/api/ai/usage", "")
	var usage struct {
		Totals   AITotals `json:"totals"`
		Requests []any    `json:"requests"`
	}
	json.Unmarshal(w.Body.Bytes(), &usage)
	if usage.Totals.Requests != 1 || usage.Totals.InputTokens != 1200 || usage.Totals.OutputTokens != 90 || usage.Totals.CachedInputTokens != 200 {
		t.Fatalf("usage was not recorded as reported: %#v", usage.Totals)
	}
	if usage.Totals.Cost == nil || len(usage.Requests) != 1 {
		t.Fatalf("no cost or no usage row: %#v", usage)
	}
	// Applying it is what writes, and the file then really changed.
	if w = request(a, "POST", "/api/ai/patch/apply", `{}`); w.Code != 200 {
		t.Fatalf("apply failed: %d %s", w.Code, w.Body)
	}
	if body, _ := a.readWorkspaceFile("2.12/tools/src/net.c"); !strings.Contains(body, "return 1") {
		t.Fatal("apply did not change the file")
	}
}

// A provider that refuses the request is reported as the provider's refusal,
// not as an internal failure, and the key is not in the text.
func TestProviderFailureIsReportedCleanly(t *testing.T) {
	a := aiWorkspace(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limit exceeded for key test-key"}}`))
	}))
	defer srv.Close()
	cfg := `{"provider":"openai-compatible","model":"m","baseUrl":"` + srv.URL + `","timeoutSeconds":10,"maxTokens":100,"currency":"USD","apiKey":"test-key"}`
	if w := request(a, "PUT", "/api/ai/config", cfg); w.Code != 200 {
		t.Fatalf("configuration rejected: %d %s", w.Code, w.Body)
	}
	redactions = func() []string { return []string{a.aiKey()} }
	defer func() { redactions = func() []string { return nil } }()
	w := request(a, "POST", "/api/ai/message", `{"prompt":"hello"}`)
	stream := w.Body.String()
	if !strings.Contains(stream, "event: failed") || !strings.Contains(stream, "rate limit exceeded") {
		t.Fatalf("provider refusal was not surfaced:\n%s", stream)
	}
	if strings.Contains(stream, "test-key") {
		t.Fatalf("the failure leaked the key:\n%s", stream)
	}
}

// The database now holds the provider credential, so it must not be readable
// by anyone but the service account whatever the umask was.
func TestDatabaseIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, e := openDB(path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e := db.Exec("INSERT INTO app_settings VALUES('ai-key', '\"sk-test\"')"); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, e := os.Stat(p)
		if e != nil {
			continue
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is readable outside the service account: %v", filepath.Base(p), st.Mode().Perm())
		}
	}
}

// Once files have been written, the content they replaced is the only way
// back: an applied proposal has to survive a restart of the service.
func TestAppliedPatchSurvivesARestart(t *testing.T) {
	a := aiWorkspace(t)
	p := stage(t, a, "```ydfs-patch path=2.12/tools/src/net.c\nint net_init(void){ return 7; }\n```")
	if w := request(a, "POST", "/api/ai/patch/apply", `{"id":"`+p.ID+`"}`); w.Code != 200 {
		t.Fatalf("apply failed: %d %s", w.Code, w.Body)
	}
	// A new process: same data directory, nothing in memory.
	restarted := &App{seen: a.seen, db: a.db, data: a.data, repo: a.repo, ctx: a.ctx, origin: a.origin, secret: a.secret, users: a.users, wake: a.wake, docker: "docker"}
	restarted.restorePatch()
	if restarted.patch == nil || !restarted.patch.Applied {
		t.Fatal("the applied proposal was forgotten across a restart")
	}
	w := request(restarted, "POST", "/api/ai/patch/reject", `{}`)
	if w.Code != 200 {
		t.Fatalf("undo after a restart failed: %d %s", w.Code, w.Body)
	}
	body, _ := restarted.readWorkspaceFile("2.12/tools/src/net.c")
	if !strings.Contains(body, "return 0") {
		t.Fatalf("undo did not restore the original: %q", body)
	}
	// And the record is gone, so a later restart does not resurrect it.
	again := &App{seen: a.seen, db: a.db, data: a.data, repo: a.repo, ctx: a.ctx, origin: a.origin, secret: a.secret, users: a.users, wake: a.wake, docker: "docker"}
	again.restorePatch()
	if again.patch != nil {
		t.Fatal("an undone proposal came back")
	}
	// An unapplied proposal is not kept: it changed nothing and can be asked
	// for again.
	stage(t, a, "```ydfs-patch path=2.12/tools/src/net.c\nint net_init(void){ return 8; }\n```")
	fresh := &App{seen: a.seen, db: a.db, data: a.data, repo: a.repo, ctx: a.ctx, origin: a.origin, secret: a.secret, users: a.users, wake: a.wake, docker: "docker"}
	fresh.restorePatch()
	if fresh.patch != nil {
		t.Fatal("an unapplied proposal was restored")
	}
}
