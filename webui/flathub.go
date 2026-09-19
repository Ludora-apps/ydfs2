package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The catalogue offered by the Flatpak page. Flathub is the only source: an
// operator must never be able to point it at another repository through the
// browser, so the URL is fixed exactly like upstreamURL in repo.go. The
// collection is ordered by popularity and paginated; the query is added by
// flathubURL, so a test server can stand in for the host on its own.
const flathubPopularURL = "https://flathub.org/api/v2/collection/popular"

// Flathub publishes ~3300 applications. They are fetched a page at a time and
// cached as id/name/summary triples (a few hundred kilobytes), so the Flatpak
// page can offer the whole store and search it in the browser with no further
// request. The cap is a bound on a remote list, not a target.
const (
	flathubPageSize      = 500
	flathubMaxPages      = 20
	flathubCatalogueSize = 5000
)

type FlathubApp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type FlathubCatalogue struct {
	Apps      []FlathubApp `json:"apps"`
	CheckedAt string       `json:"checkedAt,omitempty"`
	Source    string       `json:"source"`
	Error     string       `json:"error,omitempty"`
}

// Flathub's popular collection as of the last time this file was updated.
// Serves the form before anyone has refreshed, and on a host with no outbound
// network. It also keeps the allowlist non-empty, so a failed refresh can never
// make every saved profile unsubmittable.
var flathubFallback = []FlathubApp{
	{"org.mozilla.firefox", "Firefox", "Web browser"},
	{"com.brave.Browser", "Brave", "Privacy-oriented web browser"},
	{"com.google.Chrome", "Google Chrome", "Web browser"},
	{"com.discordapp.Discord", "Discord", "Voice and text chat"},
	{"org.telegram.desktop", "Telegram", "Messaging"},
	{"org.localsend.localsend_app", "LocalSend", "Share files on the local network"},
	{"com.spotify.Client", "Spotify", "Music streaming"},
	{"org.videolan.VLC", "VLC", "Media player"},
	{"com.obsproject.Studio", "OBS Studio", "Screen recording and streaming"},
	{"org.gimp.GIMP", "GIMP", "Image editor"},
	{"org.onlyoffice.desktopeditors", "ONLYOFFICE Desktop Editors", "Office suite"},
	{"md.obsidian.Obsidian", "Obsidian", "Markdown notes"},
	{"org.qbittorrent.qBittorrent", "qBittorrent", "BitTorrent client"},
	{"com.valvesoftware.Steam", "Steam", "Game store and launcher"},
	{"com.heroicgameslauncher.hgl", "Heroic", "Epic and GOG game launcher"},
	{"com.usebottles.bottles", "Bottles", "Run Windows software"},
	{"net.davidotek.pupgui2", "ProtonUp-Qt", "Manage Proton and Wine versions"},
	{"org.prismlauncher.PrismLauncher", "Prism Launcher", "Minecraft launcher"},
	{"org.libretro.RetroArch", "RetroArch", "Emulator frontend"},
	{"com.github.tchx84.Flatseal", "Flatseal", "Manage Flatpak permissions"},
}

// flathubSource is the fixed Flathub URL; only tests substitute another one.
func (a *App) flathubSource() string {
	if a.flathubFrom != "" {
		return a.flathubFrom
	}
	return flathubPopularURL
}

func (a *App) flathubCachePath() string { return filepath.Join(a.data, "flathub.json") }

// catalogue returns what the Flatpak page should offer, without any network
// call: the last refresh if there is one, otherwise the built-in list.
func (a *App) catalogue() FlathubCatalogue {
	a.flathubMu.Lock()
	defer a.flathubMu.Unlock()
	if a.flathub == nil {
		// First use since startup: pick up what an earlier run refreshed.
		var c FlathubCatalogue
		if b, e := os.ReadFile(a.flathubCachePath()); e == nil && json.Unmarshal(b, &c) == nil && len(c.Apps) > 0 {
			a.flathub = &c
		} else {
			a.flathub = &FlathubCatalogue{Apps: flathubFallback, Source: "built-in"}
		}
	}
	c := *a.flathub
	return c
}

// allowedFlatpak reports whether an application may be built into an ISO. The
// browser list is never authoritative: the union of the cached catalogue and
// the built-in list means a refresh cannot lock out an already-saved profile.
func (a *App) allowedFlatpak(id string) bool {
	for _, app := range a.catalogue().Apps {
		if app.ID == id {
			return true
		}
	}
	for _, app := range flathubFallback {
		if app.ID == id {
			return true
		}
	}
	return false
}

// flathubURL addresses one page of the collection. The base carries no query
// of its own, so the pagination is always ours.
func (a *App) flathubURL(page int) string {
	sep := "?"
	if strings.Contains(a.flathubSource(), "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%spage=%d&per_page=%d", a.flathubSource(), sep, page, flathubPageSize)
}

// flathubPage reads one page, returning the applications it holds and how many
// pages the collection has in total (0 when the response does not say).
func (a *App) flathubPage(ctx context.Context, client *http.Client, page int) ([]FlathubApp, int, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", a.flathubURL(page), nil)
	if e != nil {
		return nil, 0, e
	}
	resp, e := client.Do(req)
	if e != nil {
		return nil, 0, errors.New("cannot reach Flathub: " + e.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, errors.New("Flathub returned " + resp.Status)
	}
	var body struct {
		Hits []struct {
			AppID   string `json:"app_id"`
			Name    string `json:"name"`
			Summary string `json:"summary"`
		} `json:"hits"`
		TotalPages int `json:"totalPages"`
	}
	// A page of 500 entries carries their descriptions too: about a megabyte.
	if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 16*1024*1024)).Decode(&body) != nil {
		return nil, 0, errors.New("cannot read the Flathub response")
	}
	apps := make([]FlathubApp, 0, len(body.Hits))
	for _, h := range body.Hits {
		// Flathub is upstream data, not input we control: drop anything that
		// is not a plain application ID rather than offering it for ticking.
		if h.AppID == "" || !flatpakIDPattern.MatchString(h.AppID) {
			continue
		}
		name := h.Name
		if name == "" {
			name = h.AppID
		}
		apps = append(apps, FlathubApp{ID: h.AppID, Name: name, Summary: h.Summary})
	}
	return apps, body.TotalPages, nil
}

// refreshCatalogue fetches the whole collection, most popular first. A failure
// on the first page keeps the previous catalogue and reports the reason, so the
// Flatpak page never empties out; a failure partway keeps the pages already
// read, since a shorter catalogue is still usable.
func (a *App) refreshCatalogue(ctx context.Context) FlathubCatalogue {
	current := a.catalogue()
	client := &http.Client{Timeout: 30 * time.Second}
	apps := []FlathubApp{}
	seen := map[string]bool{}
	for page := 1; page <= flathubMaxPages && len(apps) < flathubCatalogueSize; page++ {
		got, total, e := a.flathubPage(ctx, client, page)
		if e != nil {
			if page == 1 {
				current.Error = e.Error()
				return current
			}
			break
		}
		for _, app := range got {
			// Paging a collection that is being reordered can repeat an entry.
			if seen[app.ID] {
				continue
			}
			seen[app.ID] = true
			apps = append(apps, app)
			if len(apps) == flathubCatalogueSize {
				break
			}
		}
		// A short page, or the last one the collection says it has, ends it.
		if len(got) < flathubPageSize || (total > 0 && page >= total) {
			break
		}
	}
	if len(apps) == 0 {
		current.Error = "Flathub returned no usable applications"
		return current
	}
	c := FlathubCatalogue{Apps: apps, CheckedAt: now(), Source: "flathub"}
	a.flathubMu.Lock()
	a.flathub = &c
	a.flathubMu.Unlock()
	if b, e := json.Marshal(c); e == nil {
		os.WriteFile(a.flathubCachePath(), b, 0600)
	}
	return c
}

func (a *App) flathubCatalogue(w http.ResponseWriter, r *http.Request) {
	respond(w, a.catalogue())
}
func (a *App) flathubRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, c := context.WithTimeout(r.Context(), 3*time.Minute)
	defer c()
	respond(w, a.refreshCatalogue(ctx))
}
