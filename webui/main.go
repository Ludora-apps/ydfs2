package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"linuxconsole/webui/internal/assets"
)

type App struct {
	db                         *sql.DB
	repo, data, origin, secret string
	users                      map[string]bool
	dev                        bool
	minFree                    uint64
	mu                         sync.Mutex
	repoMu                     sync.Mutex
	upstreamFrom               string
	forkFrom                   string
	upstream                   *Commit
	upstreamAt                 time.Time
	originError                string
	flathubMu                  sync.Mutex
	flathubFrom                string
	flathub                    *FlathubCatalogue
	wake                       chan struct{}
	ctx                        context.Context
	docker                     string
	// The single test VM (see vm.go). vmMu is its own lock: a.mu serialises
	// build state, and booting an ISO must never wait on a build.
	// Lock order, wherever both are needed, is a.mu then a.vmMu.
	vmMu    sync.Mutex
	vm      *VM
	vmLast  *VM
	kvm     string
	vmImage string
	// The single merge or cherry-pick being resolved (see graft.go). Its own
	// lock for the same reason as vmMu: resolving a conflict happens in a
	// worktree of its own and must never wait on, or hold up, a build.
	// Lock order, wherever both are needed, is a.mu then a.graftMu.
	graftMu sync.Mutex
	graft   *Graft
	gh      string
}

func main() {
	repo := flag.String("repo", "..", "repository checkout")
	data := flag.String("data", "", "persistent data directory")
	port := flag.Int("port", 8080, "HTTP port")
	host := flag.String("host", "127.0.0.1", "listen address")
	origin := flag.String("origin", "", "public origin, e.g. https://build.example.org")
	dev := flag.Bool("dev", false, "local development only: no authentication")
	min := flag.Uint64("min-free-gb", 10, "minimum free GiB required to start builds")
	flag.Parse()
	if *data == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			log.Fatal(e)
		}
		*data = filepath.Join(h, ".local/share/ydfs-web")
	}
	absRepo, e := filepath.Abs(*repo)
	if e != nil {
		log.Fatal(e)
	}
	absData, e := filepath.Abs(*data)
	if e != nil {
		log.Fatal(e)
	}
	absRepo, e = filepath.EvalSymlinks(absRepo)
	if e != nil {
		log.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(absRepo, "2.12", "Makefile")); e != nil {
		log.Fatal("--repo must contain 2.12/Makefile")
	}
	if e = os.MkdirAll(absData, 0700); e != nil {
		log.Fatal(e)
	}
	absData, e = filepath.EvalSymlinks(absData)
	if e != nil {
		log.Fatal(e)
	}
	if absData == absRepo || strings.HasPrefix(absData, absRepo+string(os.PathSeparator)) {
		log.Fatal("--data must be outside the repository")
	}
	if *dev {
		ip := net.ParseIP(*host)
		if ip == nil || !ip.IsLoopback() {
			log.Fatal("--dev requires a loopback listen address")
		}
		if *origin == "" {
			*origin = fmt.Sprintf("http://%s:%d", *host, *port)
		}
	} else {
		if len(os.Getenv("YDFS_PROXY_SECRET")) < 32 {
			log.Fatal("YDFS_PROXY_SECRET must contain at least 32 characters")
		}
		u, e := url.Parse(*origin)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
			log.Fatal("--origin must be an HTTPS origin")
		}
	}
	lock, e := os.OpenFile(filepath.Join(absData, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		log.Fatal(e)
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		log.Fatal("another server owns this data directory")
	}
	// A second instance with another data directory must not build this checkout concurrently.
	repoLock, e := os.OpenFile(filepath.Join(absRepo, ".git", "ydfs-web.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		log.Fatal(e)
	}
	defer repoLock.Close()
	if e = syscall.Flock(int(repoLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		log.Fatal("another build manager owns this checkout")
	}
	db, e := openDB(filepath.Join(absData, "state.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &App{db: db, repo: absRepo, data: absData, origin: *origin, secret: os.Getenv("YDFS_PROXY_SECRET"), dev: *dev, minFree: *min * 1024 * 1024 * 1024, users: map[string]bool{}, wake: make(chan struct{}, 1), ctx: ctx, docker: "docker", kvm: "/dev/kvm", vmImage: vmImageTag, gh: "gh"}
	for _, u := range strings.Split(os.Getenv("YDFS_ALLOWED_USERS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			a.users[strings.ToLower(u)] = true
		}
	}
	if !a.dev && len(a.users) == 0 {
		log.Fatal("YDFS_ALLOWED_USERS must list allowed GitHub usernames")
	}
	// Apply retention once at startup, so a policy change (or builds made
	// while the service was down) takes effect without waiting for a new build.
	a.migrateLogs()
	a.mu.Lock()
	a.pruneLocked()
	a.mu.Unlock()
	// Test VMs are ephemeral: one that outlived its server holds 4 GiB and
	// belongs to a session nobody can reach any more.
	reapCtx, reapCancel := context.WithTimeout(context.Background(), 30*time.Second)
	a.reapVMs(reapCtx)
	// A conflict resolution is just as ephemeral, and its worktree is hundreds
	// of megabytes belonging to a session nobody can reach any more.
	a.reapGrafts(reapCtx)
	reapCancel()
	done := make(chan struct{})
	go func() { defer close(done); a.worker() }()
	srv := &http.Server{Addr: net.JoinHostPort(*host, strconv.Itoa(*port)), Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("LinuxConsole build manager listening on %s (data: %s)", srv.Addr, a.data)
	if e = srv.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
		log.Print(e)
		stop()
	}
	<-done
	// Belt and braces: vmWatch stops its own container on shutdown, but this
	// does not depend on a goroutine having got there first.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	a.reapVMs(shutdownCtx)
	shutdownCancel()
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, s string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": s})
}

// Large enough for a job submission or profile carrying a full package-list
// override (see maxPackageListTextLen), plus JSON overhead.
const maxRequestBody = 512 * 1024

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "invalid JSON request")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "expected one JSON value")
		return false
	}
	return true
}
func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/capabilities", a.capabilities)
	mux.HandleFunc("GET /api/repository", a.repository)
	mux.HandleFunc("POST /api/repository/check", a.repositoryCheck)
	mux.HandleFunc("POST /api/repository/update", a.repositoryUpdate)
	mux.HandleFunc("GET /api/repository/commits", a.repositoryCommits)
	mux.HandleFunc("POST /api/repository/checkout", a.repositoryCheckout)
	mux.HandleFunc("POST /api/repository/merge/preview", a.repositoryMergePreview)
	mux.HandleFunc("POST /api/repository/graft/resolve", a.graftResolve)
	mux.HandleFunc("POST /api/repository/graft/apply", a.graftApply)
	mux.HandleFunc("DELETE /api/repository/graft", a.graftAbort)
	mux.HandleFunc("GET /api/repository/graft/file", a.graftFile)
	mux.HandleFunc("POST /api/repository/push", a.repositoryPush)
	mux.HandleFunc("POST /api/repository/pr", a.repositoryPR)
	mux.HandleFunc("GET /api/flathub", a.flathubCatalogue)
	mux.HandleFunc("POST /api/flathub/refresh", a.flathubRefresh)
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		js, e := a.jobs()
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		respond(w, js)
	})
	mux.HandleFunc("POST /api/jobs", a.submit)
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, e := a.job(r.PathValue("id"))
		if e != nil {
			fail(w, 404, "build not found")
			return
		}
		respond(w, j)
	})
	mux.HandleFunc("POST /api/jobs/{id}/cancel", a.cancel)
	mux.HandleFunc("POST /api/jobs/{id}/favorite", a.setFavorite)
	mux.HandleFunc("DELETE /api/jobs/{id}/favorite", a.setFavorite)
	mux.HandleFunc("GET /api/jobs/{id}/config", a.downloadConfig)
	mux.HandleFunc("DELETE /api/jobs/{id}", a.deleteJob)
	mux.HandleFunc("GET /api/jobs/{id}/events", a.events)
	mux.HandleFunc("GET /api/jobs/{id}/log", a.downloadLog)
	mux.HandleFunc("DELETE /api/jobs/{id}/log", a.deleteLog)
	mux.HandleFunc("GET /api/logs", a.logs)
	mux.HandleFunc("GET /api/jobs/{id}/artifacts/{name}", a.downloadArtifact)
	mux.HandleFunc("GET /api/vm", a.vmStatus)
	mux.HandleFunc("GET /api/vm/console", a.vmConsole)
	mux.HandleFunc("POST /api/jobs/{id}/vm", a.vmStart)
	mux.HandleFunc("DELETE /api/jobs/{id}/vm", a.vmStop)
	mux.HandleFunc("GET /api/packages", a.packageLists)
	mux.HandleFunc("GET /api/packages/{name}", a.packageListContent)
	mux.HandleFunc("GET /api/profiles", a.profiles)
	mux.HandleFunc("PUT /api/profiles", a.putProfile)
	mux.HandleFunc("DELETE /api/profiles/{name}", func(w http.ResponseWriter, r *http.Request) {
		_, e := a.db.Exec("DELETE FROM profiles WHERE name=?", r.PathValue("name"))
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		respond(w, map[string]bool{"ok": true})
	})
	files, _ := fs.Sub(assets.Files, "dist")
	static := http.FileServer(http.FS(files))
	mux.Handle("GET /", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Cache-Control", "no-store")
		if !a.dev {
			h, _, _ := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(h)
			if ip == nil || !ip.IsLoopback() || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Build-Proxy-Secret")), []byte(a.secret)) != 1 || !a.users[strings.ToLower(r.Header.Get("X-Forwarded-User"))] {
				fail(w, 401, "authentication required")
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if r.Header.Get("Origin") != a.origin || r.Header.Get("X-Requested-With") != "ydfs-web" {
				fail(w, 403, "request origin rejected")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *App) profiles(w http.ResponseWriter, r *http.Request) {
	rows, e := a.db.Query("SELECT data FROM profiles ORDER BY name")
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	defer rows.Close()
	ps := []Profile{}
	for rows.Next() {
		var b string
		var p Profile
		if e = rows.Scan(&b); e != nil {
			fail(w, 500, e.Error())
			return
		}
		if e = json.Unmarshal([]byte(b), &p); e != nil {
			fail(w, 500, e.Error())
			return
		}
		p.Settings.normalize()
		ps = append(ps, p)
	}
	respond(w, ps)
}
func (a *App) putProfile(w http.ResponseWriter, r *http.Request) {
	var p Profile
	if !decode(w, r, &p) {
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 80 {
		fail(w, 400, "profile name must contain 1–80 bytes")
		return
	}
	if e := p.Settings.validate(); e != nil {
		fail(w, 400, e.Error())
		return
	}
	b, _ := json.Marshal(p)
	_, e := a.db.Exec("INSERT INTO profiles VALUES(?,?) ON CONFLICT(name) DO UPDATE SET data=excluded.data", p.Name, string(b))
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, p)
}
