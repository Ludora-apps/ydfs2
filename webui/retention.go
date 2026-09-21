package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// How many recent builds keep their artifacts, counted separately for ISOs and
// for component builds so a run of component builds cannot evict the ISOs.
const keepBuilds = 3

func isoTarget(t string) bool { return t == "fast-iso" || t == "full-iso" }

// retentionClass separates the two independent rolling windows.
func retentionClass(j *Job) string {
	if isoTarget(j.Settings.Target) {
		return "iso"
	}
	return "component"
}

// pruneLocked reclaims the artifacts of builds that have fallen out of their
// rolling window. An ISO is ~3.3 GB while the rest of a job directory is ~70 MB,
// so only the artifacts go: the job row, its build log, its config.ini and its
// source snapshot stay, and the build remains visible with its configuration
// readable.
//
// Kept = the keepBuilds most recent builds of each class, plus every favourite.
// A favourite inside the window still occupies its slot, so starring a recent
// build does not silently grow the window; once it ages out it is kept anyway.
//
// The caller must hold a.mu. Errors are reported but never fatal: failing to
// reclaim space must not fail the build that triggered it.
func (a *App) pruneLocked() {
	js, e := a.jobs()
	if e != nil {
		fmt.Fprintln(os.Stderr, "retention:", e)
		return
	}
	window := map[string]int{}
	// jobs() returns newest first (ORDER BY id DESC), and the id is a UTC
	// timestamp, so this walks builds from newest to oldest.
	for i := range js {
		j := &js[i]
		if j.State != "succeeded" || j.PrunedAt != "" || len(j.Artifacts) == 0 {
			continue
		}
		class := retentionClass(j)
		window[class]++
		if window[class] <= keepBuilds || j.Favorite {
			continue
		}
		// A build being booted right now keeps its files: reclaiming them
		// under a live console would not even free the space (QEMU holds the
		// descriptor) and would tell the viewer their ISO is gone.
		if a.vmHolds(j.ID) {
			continue
		}
		if e := a.pruneArtifacts(j); e != nil {
			fmt.Fprintln(os.Stderr, "retention:", j.ID, e)
		}
	}
}

// pruneArtifacts deletes exactly the files recorded as this build's artifacts.
// config.ini is never among them (collect only records ISOs and the component
// output), so the build's configuration survives.
func (a *App) pruneArtifacts(j *Job) error {
	output := filepath.Join(a.dir(j), "output")
	for _, v := range j.Artifacts {
		if filepath.Base(v.Name) != v.Name {
			continue
		}
		if e := os.Remove(filepath.Join(output, v.Name)); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	j.Artifacts = []Artifact{}
	j.PrunedAt = now()
	return a.saveJob(j)
}

// setFavorite pins a build's artifacts against retention, or releases them.
func (a *App) setFavorite(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, e := a.job(pathValue(r, "id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	on := r.Method == "POST"
	if on && j.PrunedAt != "" {
		fail(w, 409, "this build's artifacts were already reclaimed and cannot be kept")
		return
	}
	if on && j.State != "succeeded" {
		fail(w, 409, "only a succeeded build can be kept")
		return
	}
	j.Favorite = on
	if e = a.saveJob(j); e != nil {
		fail(w, 500, e.Error())
		return
	}
	// Releasing one can make it, or a build it was holding a slot for, eligible.
	a.pruneLocked()
	latest, e := a.job(j.ID)
	if e != nil {
		fail(w, 500, e.Error())
		return
	}
	respond(w, latest)
}

// downloadConfig serves the config.ini the build actually ran with, archived
// into the job's output directory by the container script in launch().
func (a *App) downloadConfig(w http.ResponseWriter, r *http.Request) {
	j, e := a.job(pathValue(r, "id"))
	if e != nil {
		fail(w, 404, "build not found")
		return
	}
	b, e := os.ReadFile(filepath.Join(a.dir(j), "output", "config.ini"))
	if e != nil {
		fail(w, 404, "no configuration was archived for this build")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}
