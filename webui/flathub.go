package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The catalogue offered by the build form. Flathub is the only source: an
// operator must never be able to point it at another repository through the
// browser, so the URL is fixed exactly like upstreamURL in repo.go.
const flathubPopularURL = "https://flathub.org/api/v2/collection/popular?page=1&per_page=20"

// Kept small on purpose. Every entry a user ticks is downloaded during the
// build and baked into the ISO, so the list is a starting point, not a store.
const flathubCatalogueSize = 20

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

// catalogue returns what the form should offer, without any network call:
// the last refresh if there is one, otherwise the built-in list.
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

// refresh fetches the current popular collection. A failure keeps the previous
// catalogue and reports the reason, so the form never empties out.
func (a *App) refreshCatalogue(ctx context.Context) FlathubCatalogue {
	current := a.catalogue()
	req, e := http.NewRequestWithContext(ctx, "GET", a.flathubSource(), nil)
	if e != nil {
		current.Error = e.Error()
		return current
	}
	resp, e := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if e != nil {
		current.Error = "cannot reach Flathub: " + e.Error()
		return current
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		current.Error = "Flathub returned " + resp.Status
		return current
	}
	var body struct {
		Hits []struct {
			AppID   string `json:"app_id"`
			Name    string `json:"name"`
			Summary string `json:"summary"`
		} `json:"hits"`
	}
	if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4*1024*1024)).Decode(&body) != nil {
		current.Error = "cannot read the Flathub response"
		return current
	}
	apps := []FlathubApp{}
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
		if len(apps) == flathubCatalogueSize {
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
	ctx, c := context.WithTimeout(r.Context(), 20*time.Second)
	defer c()
	respond(w, a.refreshCatalogue(ctx))
}
