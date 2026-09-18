import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
// Type only: the client itself is imported lazily, when a console is opened.
import type RFB from "@novnc/novnc";
import "./style.css";

type Settings = {
  target: string;
  verbose: boolean;
  kernel: string;
  configOverrides: string;
  packageList: string;
  packageListText: string;
  flatpaks: string[];
};
type FlathubApp = { id: string; name: string; summary: string };
type FlathubCatalogue = {
  apps: FlathubApp[];
  checkedAt?: string;
  source: string;
  error?: string;
};
type Artifact = { name: string; size: number };
type Job = {
  id: string;
  state: string;
  user: string;
  settings: Settings;
  created: string;
  started?: string;
  finished?: string;
  revision: string;
  tag: string;
  container: string;
  exitCode?: number;
  error?: string;
  artifacts: Artifact[];
  favorite: boolean;
  prunedAt?: string;
};
type Profile = { name: string; settings: Settings };
type Commit = {
  revision: string;
  subject: string;
  author: string;
  date: string;
};
type Repository = {
  url: string;
  branch: string;
  tag: string;
  dirty: boolean;
  local: Commit;
  upstream?: Commit;
  ahead: number;
  behind: number;
  fastForward: boolean;
  checkedAt?: string;
};
type Capabilities = {
  targets: string[];
  packageLists: string[];
  flathub?: FlathubCatalogue;
  architecture: string;
  distribution: string;
  user: string;
  docker: boolean;
  freeBytes: number;
  minFreeBytes: number;
  ready: boolean;
  message: string;
  development: boolean;
  vm: boolean;
  vmMessage: string;
};
// The single test machine (see vm.go). `state` is "stopped" both when nothing
// ever ran and when the last session ended; `error` then says why it ended.
type VMStatus = {
  session?: string;
  jobId?: string;
  target?: string;
  iso?: string;
  state: "stopped" | "starting" | "running" | "stopping";
  started?: string;
  user?: string;
  viewers: number;
  idleFor: number;
  idleLimit?: number;
  password?: string;
  error?: string;
};
const names: Record<string, string> = {
  "fast-iso": "Fast ISO",
  "full-iso": "Full ISO",
  kernel: "Linux kernel",
  busybox: "BusyBox",
  initramfs: "Initramfs",
  updates: "Update module",
  mate: "Mate desktop",
  kde: "KDE desktop",
  cinnamon: "Cinnamon desktop",
};
const blankSettings: Settings = {
  target: "fast-iso",
  verbose: true,
  kernel: "",
  configOverrides: "",
  packageList: "",
  packageListText: "",
  flatpaks: [],
};
const isIso = (t: string) => t === "fast-iso" || t === "full-iso";
// Mirrors keepBuilds in retention.go; used only in explanatory copy.
const keepBuilds = 3;
const done = (s: string) => ["succeeded", "failed", "cancelled"].includes(s);
const bytes = (n: number) =>
  n >= 2 ** 30
    ? `${(n / 2 ** 30).toFixed(1)} GB`
    : n >= 2 ** 20
      ? `${(n / 2 ** 20).toFixed(1)} MB`
      : // Logs are routinely kilobytes; without this they all read "0.0 MB".
        `${Math.max(1, Math.round(n / 2 ** 10))} KB`;
const date = (s?: string) => (s ? new Date(s).toLocaleString() : "—");
const commitCard = (label: string, c?: Commit, note?: React.ReactNode) => (
  <div className="commit">
    <p className="eyebrow">{label}</p>
    {c ? (
      <>
        <strong title={c.subject}>{c.subject}</strong>
        <p>
          <code>{c.revision.slice(0, 12)}</code> · {date(c.date)}
        </p>
        <small>
          {c.author}
          {note}
        </small>
      </>
    ) : (
      <p className="pending">Not checked yet.</p>
    )}
  </div>
);
type LogEntry = {
  id: string;
  target: string;
  state: string;
  created: string;
  user: string;
  size: number;
  present: boolean;
};

// Highlighting every match of a common substring in a multi-megabyte log would
// create more nodes than it is worth; past this the search still counts matches
// but stops marking them.
const maxHighlights = 4000;

type TextView = {
  // Changing key refetches; it is what the open/close effects key on.
  key: string;
  title: string;
  subtitle: string;
  url: string;
  // Logs fill the viewport; a config.ini is a few dozen lines and should not.
  compact?: boolean;
  deleteLabel?: string;
  onDelete?: () => void;
};

// One reader for every build text file: the log tail and the archived
// config.ini both land here rather than in a browser tab, so the whole file is
// scrollable and searchable with matches highlighted. Same <dialog> primitive
// as ConfirmDialog, so focus trap, Escape and backdrop come for free.
function TextViewer({
  view,
  onClose,
}: {
  view?: TextView;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const body = useRef<HTMLDivElement>(null);
  const [text, setText] = useState("");
  const [loadError, setLoadError] = useState("");
  const [query, setQuery] = useState("");
  const [at, setAt] = useState(0);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (view && !d.open) d.showModal();
    else if (!view && d.open) d.close();
  }, [view]);
  const url = view?.url;
  useEffect(() => {
    if (!url) return;
    setText("");
    setQuery("");
    setAt(0);
    setLoadError("");
    let live = true;
    fetch(url, { headers: { "X-Requested-With": "ydfs-web" } })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error("Not available"))))
      .then((t) => live && setText(t))
      .catch((e) => live && setLoadError((e as Error).message));
    return () => {
      live = false;
    };
  }, [url]);
  // Single-character searches match almost every line and help nobody.
  const matches = useMemo(() => {
    const out: number[] = [];
    if (query.length < 2) return out;
    const hay = text.toLowerCase();
    const needle = query.toLowerCase();
    for (let i = hay.indexOf(needle); i !== -1; i = hay.indexOf(needle, i + needle.length))
      out.push(i);
    return out;
  }, [text, query]);
  const rendered = useMemo(() => {
    if (matches.length === 0) return text;
    const marked = matches.slice(0, maxHighlights);
    const parts: React.ReactNode[] = [];
    let last = 0;
    marked.forEach((m, n) => {
      if (m > last) parts.push(text.slice(last, m));
      parts.push(
        <mark key={n} className={n === at ? "current" : ""}>
          {text.slice(m, m + query.length)}
        </mark>,
      );
      last = m + query.length;
    });
    parts.push(text.slice(last));
    return parts;
  }, [text, query, matches, at]);
  useEffect(() => {
    body.current?.querySelector("mark.current")?.scrollIntoView({
      block: "center",
    });
  }, [at, rendered]);
  const step = (by: number) => {
    if (matches.length === 0) return;
    const n = Math.min(matches.length, maxHighlights);
    setAt((old) => (old + by + n) % n);
  };
  return (
    <dialog
      className={`modal logviewer ${view?.compact ? "compact" : ""}`}
      ref={ref}
      aria-labelledby="logviewer-title"
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      {view && (
        <div className="logviewer-body">
          <header>
            <div>
              <h2 id="logviewer-title">{view.title}</h2>
              <small>{view.subtitle}</small>
            </div>
            <form
              className="log-search"
              onSubmit={(e) => {
                e.preventDefault();
                step(1);
              }}
            >
              <label className="sr-only" htmlFor="logviewer-search">
                Search text
              </label>
              <input
                id="logviewer-search"
                placeholder="Search…"
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value);
                  setAt(0);
                }}
              />
              <span className="matches">
                {query.length < 2
                  ? "2+ characters"
                  : matches.length === 0
                    ? "No match"
                    : `${at + 1} / ${matches.length}${matches.length > maxHighlights ? " (first " + maxHighlights + " marked)" : ""}`}
              </span>
              <button
                type="button"
                className="quiet"
                aria-label="Previous match"
                disabled={matches.length === 0}
                onClick={() => step(-1)}
              >
                ↑
              </button>
              <button
                type="button"
                className="quiet"
                aria-label="Next match"
                disabled={matches.length === 0}
                onClick={() => step(1)}
              >
                ↓
              </button>
            </form>
            <div className="logviewer-actions">
              <a href={view.url}>Download ↓</a>
              {view.onDelete && (
                <button type="button" className="quiet" onClick={view.onDelete}>
                  {view.deleteLabel || "Delete"}
                </button>
              )}
              <button type="button" className="quiet" onClick={onClose}>
                Close
              </button>
            </div>
          </header>
          <div
            className="logviewer-text"
            ref={body}
            tabIndex={0}
            aria-label="File contents"
          >
            <pre>{loadError || (text ? rendered : "Loading…")}</pre>
          </div>
        </div>
      )}
    </dialog>
  );
}

type Confirmation = {
  title: string;
  body: string;
  confirm: string;
  danger?: boolean;
  onConfirm: () => void;
};

// The app never uses window.confirm/alert: a native dialog cannot be themed,
// and blocks the whole page. <dialog> gives the focus trap, Escape handling and
// backdrop for free, while staying styled like the rest of the UI.
function ConfirmDialog({
  ask,
  onClose,
}: {
  ask?: Confirmation;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (ask && !d.open) d.showModal();
    else if (!ask && d.open) d.close();
  }, [ask]);
  return (
    <dialog
      className="modal"
      ref={ref}
      aria-labelledby="confirm-title"
      aria-describedby="confirm-body"
      // Escape must not close it natively, or React state would still hold the
      // request and the effect above would immediately reopen it.
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      // A click landing on the element itself is a click on the backdrop.
      onClick={(e) => {
        if (e.target === ref.current) onClose();
      }}
    >
      {ask && (
        <div className="modal-body">
          <h2 id="confirm-title">{ask.title}</h2>
          <p id="confirm-body">{ask.body}</p>
          <div className="modal-actions">
            <button
              type="button"
              className="quiet"
              onClick={onClose}
              // For a destructive action the safe choice takes focus.
              autoFocus={ask.danger}
            >
              Cancel
            </button>
            <button
              type="button"
              className={ask.danger ? "danger" : "primary"}
              autoFocus={!ask.danger}
              onClick={() => {
                ask.onConfirm();
                onClose();
              }}
            >
              {ask.confirm}
            </button>
          </div>
        </div>
      )}
    </dialog>
  );
}
async function api<T>(
  path: string,
  method = "GET",
  body?: unknown,
): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method,
    headers: {
      "Content-Type": "application/json",
      "X-Requested-With": "ydfs-web",
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    const err = await res
      .json()
      .catch(() => ({ error: `Request failed (${res.status})` }));
    throw new Error(err.error);
  }
  return res.json();
}
// The live screen of the test machine, over noVNC.
//
// noVNC is loaded on demand: it is ~90 kB that most sessions never open, and a
// same-origin dynamic import satisfies the app's script-src 'self' policy.
function VmConsole({
  vm,
  onStop,
  busy,
}: {
  vm: VMStatus;
  onStop: () => void;
  busy: boolean;
}) {
  const screen = useRef<HTMLDivElement>(null);
  const rfb = useRef<RFB | null>(null);
  const [phase, setPhase] = useState("Connecting");
  const [fit, setFit] = useState(true);
  const live = vm.state === "running";
  useEffect(() => {
    if (!live || !screen.current) return;
    let cancelled = false;
    let client: RFB | undefined;
    (async () => {
      const { default: RFB } = await import("@novnc/novnc");
      if (cancelled || !screen.current) return;
      const url = new URL("/api/vm/console", location.href);
      url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
      client = new RFB(screen.current, url.toString(), {
        shared: true,
        credentials: { password: vm.password || "" },
      });
      client.scaleViewport = true;
      client.resizeSession = false;
      client.background = "#0b1220";
      client.addEventListener("connect", () => setPhase("Live"));
      client.addEventListener("disconnect", (e) =>
        setPhase(
          (e as CustomEvent).detail?.clean ? "Disconnected" : "Connection lost",
        ),
      );
      client.addEventListener("securityfailure", () =>
        setPhase("Console refused the connection"),
      );
      rfb.current = client;
    })();
    return () => {
      cancelled = true;
      rfb.current = null;
      client?.disconnect();
    };
    // Keyed on the server's session id, not the job: the job list refreshes
    // every 5 seconds and re-dialling the console each time would be visible.
  }, [vm.session, live]);
  useEffect(() => {
    if (rfb.current) rfb.current.scaleViewport = fit;
  }, [fit]);
  return (
    <section className="panel console">
      <div className="panel-heading">
        <div className="panel-heading-title">
          <h2>Test machine</h2>
          <small>
            {vm.iso || "ISO"} · {vm.state === "starting" ? "booting" : phase}
            {vm.viewers === 0 && vm.state === "running" ? " · nobody watching" : ""}
          </small>
        </div>
        <div className="detail-actions">
          <button
            className="quiet"
            disabled={!live}
            onClick={() => rfb.current?.sendKey(0xff0d, "Enter")}
            title="The boot menu waits for a keypress before it starts"
          >
            Send Enter
          </button>
          <button
            className="quiet"
            disabled={!live}
            onClick={() => rfb.current?.sendCtrlAltDel()}
          >
            Ctrl+Alt+Del
          </button>
          <button className="quiet" onClick={() => setFit((f) => !f)}>
            {fit ? "1:1" : "Fit"}
          </button>
          <button
            className="quiet"
            onClick={() => screen.current?.requestFullscreen()}
          >
            Fullscreen
          </button>
          <button className="danger" disabled={busy} onClick={onStop}>
            Stop machine
          </button>
        </div>
      </div>
      <div
        className="vnc-screen"
        ref={screen}
        onClick={() => rfb.current?.focus()}
      />
      <p className="console-hint">
        This ISO stops at its boot menu and waits for a keypress — press Enter
        (or use the button) to start it. Click the screen before typing, so the
        keyboard goes to the machine. Nothing here is saved: the machine is
        discarded when you stop it, after 30 minutes with nobody watching, or
        when you close the build manager.
      </p>
    </section>
  );
}
function App() {
  const [ask, setAsk] = useState<Confirmation | undefined>();
  const [logList, setLogList] = useState<LogEntry[]>([]);
  const [viewing, setViewing] = useState<TextView | undefined>();
  // Both openers build a TextView; the reader itself is agnostic.
  function viewLog(l: LogEntry) {
    setViewing({
      key: `log:${l.id}`,
      title: `${names[l.target] || l.target} · build log`,
      subtitle: `${l.user} · ${date(l.created)} · ${bytes(l.size)}`,
      url: `/api/jobs/${l.id}/log`,
      deleteLabel: "Delete log",
      onDelete: () => removeLog(l),
    });
  }
  function viewConfig(j: Job) {
    setViewing({
      key: `config:${j.id}`,
      title: `${names[j.settings.target] || j.settings.target} · config.ini`,
      subtitle: `as built · ${date(j.created)} · ${j.revision.slice(0, 12)} · ${j.tag || "untagged"}`,
      url: `/api/jobs/${j.id}/config`,
      compact: true,
    });
  }
  const [theme, setTheme] = useState<"light" | "dark">(() =>
    document.documentElement.dataset.theme === "light" ? "light" : "dark",
  );
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document
      .querySelector('meta[name="theme-color"]')
      ?.setAttribute("content", theme === "dark" ? "#0c141d" : "#f2f5f7");
    try {
      localStorage.setItem("ydfs-theme", theme);
    } catch {
      /* Optional persistence. */
    }
  }, [theme]);
  const [caps, setCaps] = useState<Capabilities>();
  const [vm, setVm] = useState<VMStatus>({
    state: "stopped",
    viewers: 0,
    idleFor: 0,
  });
  const [jobs, setJobs] = useState<Job[]>([]);
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [repo, setRepo] = useState<Repository>();
  const [repoBusy, setRepoBusy] = useState("");
  const [repoError, setRepoError] = useState("");
  async function loadRepo(check: boolean) {
    setRepoError("");
    if (check) setRepoBusy("check");
    try {
      setRepo(
        check
          ? await api<Repository>("/repository/check", "POST", {})
          : await api<Repository>("/repository"),
      );
    } catch (e) {
      setRepoError((e as Error).message);
    } finally {
      if (check) setRepoBusy("");
    }
  }
  function updateRepo() {
    const merging = !!repo && !repo.fastForward;
    setAsk({
      title: merging ? "Merge upstream changes?" : "Update this checkout?",
      body: merging
        ? "The latest upstream commit is merged into this checkout. Local commits are kept; a conflicting merge is rolled back. Queued and running builds keep the snapshot they were submitted with."
        : "This checkout fast-forwards to the latest upstream commit. Queued and running builds keep the snapshot they were submitted with.",
      confirm: merging ? "Merge" : "Update",
      onConfirm: runUpdateRepo,
    });
  }
  async function runUpdateRepo() {
    setRepoBusy("update");
    setRepoError("");
    try {
      const r = await api<Repository>("/repository/update", "POST", {});
      setRepo(r);
      setNotice(`Checkout updated to ${r.local.revision.slice(0, 12)}.`);
      await refresh();
    } catch (e) {
      setRepoError((e as Error).message);
    } finally {
      setRepoBusy("");
    }
  }
  const [settings, setSettings] = useState<Settings>(blankSettings);
  const [flathubBusy, setFlathubBusy] = useState(false);
  const [flathubError, setFlathubError] = useState("");
  function toggleApp(id: string, on: boolean) {
    setSettings((s) => ({
      ...s,
      flatpaks: on
        ? [...s.flatpaks, id]
        : s.flatpaks.filter((x) => x !== id),
    }));
  }
  async function refreshFlathub() {
    setFlathubBusy(true);
    setFlathubError("");
    try {
      const c = await api<FlathubCatalogue>("/flathub/refresh", "POST");
      setFlathubError(c.error || "");
      // The catalogue reaches the form through /api/capabilities.
      await refresh();
      if (!c.error) setNotice("Flathub list refreshed.");
    } catch (e) {
      setFlathubError((e as Error).message);
    } finally {
      setFlathubBusy(false);
    }
  }
  const [loadingList, setLoadingList] = useState(false);
  async function loadPackageList(name: string) {
    if (!name) {
      setSettings((s) => ({ ...s, packageList: "", packageListText: "" }));
      return;
    }
    setLoadingList(true);
    try {
      const data = await api<{ name: string; content: string }>(
        `/packages/${encodeURIComponent(name)}`,
      );
      setSettings((s) => ({
        ...s,
        packageList: name,
        packageListText: data.content,
      }));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoadingList(false);
    }
  }
  const [profileName, setProfileName] = useState("");
  const [selected, setSelected] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  useEffect(() => {
    if (!notice) return;
    const timeout = window.setTimeout(() => setNotice(""), 4000);
    return () => window.clearTimeout(timeout);
  }, [notice]);
  const [log, setLog] = useState("");
  const [logOpen, setLogOpen] = useState(false);
  useEffect(() => {
    setLogOpen(false);
  }, [selected]);
  const [search, setSearch] = useState("");
  const [follow, setFollow] = useState(false);
  const [streamState, setStreamState] = useState("");
  const terminal = useRef<HTMLDivElement>(null);
  const job = jobs.find((j) => j.id === selected);
  const favorites = jobs.filter((j) => j.favorite);
  function toggleFavorite(j: Job) {
    action(async () => {
      await api(`/jobs/${j.id}/favorite`, j.favorite ? "DELETE" : "POST");
      setNotice(
        j.favorite
          ? "Build released; it can now be reclaimed."
          : "Build kept: its files are safe from cleanup.",
      );
    });
  }
  function removeLog(entry: LogEntry) {
    setAsk({
      title: "Delete this log?",
      body: "The build, its configuration and its artifacts are kept; only the log file is removed. This cannot be undone.",
      confirm: "Delete log",
      danger: true,
      onConfirm: () =>
        action(async () => {
          await api(`/jobs/${entry.id}/log`, "DELETE");
          setViewing((old) => (old?.key === `log:${entry.id}` ? undefined : old));
        }),
    });
  }
  function removeBuild(j: Job) {
    setAsk({
      title: "Delete this build?",
      body: "Its logs, source snapshot, and artifacts are deleted with it. This cannot be undone.",
      confirm: "Delete build",
      danger: true,
      onConfirm: () =>
        action(async () => {
          await api(`/jobs/${j.id}`, "DELETE");
          setSelected((old) => (old === j.id ? "" : old));
        }),
    });
  }
  // Booting an ISO is not destructive and is easy to undo, so it starts on one
  // click; stopping discards a machine someone may be using, so it asks.
  function startVm(j: Job) {
    action(async () => {
      setVm(await api<VMStatus>(`/jobs/${j.id}/vm`, "POST", {}));
    });
  }
  function stopVm(jobId: string) {
    setAsk({
      title: "Stop the test machine?",
      body: "The virtual machine is discarded immediately. Nothing inside it is saved; the ISO itself is untouched.",
      confirm: "Stop machine",
      danger: true,
      onConfirm: () =>
        action(async () => {
          setVm(await api<VMStatus>(`/jobs/${jobId}/vm`, "DELETE"));
        }),
    });
  }
  // One machine at a time, so every button consults the same live session.
  const vmBusy = vm.state === "starting" || vm.state === "stopping";
  const vmLive = vm.state !== "stopped";
  function vmButton(j: Job) {
    if (!isIso(j.settings.target) || j.state !== "succeeded" || j.prunedAt)
      return null;
    if (vmLive && vm.jobId === j.id)
      return (
        <button
          className="danger"
          disabled={busy || vmBusy}
          onClick={() => stopVm(j.id)}
        >
          {vm.state === "starting" ? "Booting…" : "Stop machine"}
        </button>
      );
    const blocked = !caps?.vm
      ? caps?.vmMessage || "Test machines are unavailable on this server."
      : vmLive
        ? "A test machine is already running for another build."
        : "";
    return (
      <button
        className="quiet"
        disabled={busy || !!blocked}
        title={blocked || "Boot this ISO and watch it in the browser"}
        onClick={() => startVm(j)}
      >
        ▶ Test in browser
      </button>
    );
  }
  const active = jobs.find((j) => !done(j.state) && j.state !== "queued");
  const queued = jobs.filter((j) => j.state === "queued").length;
  async function refresh() {
    const [c, j, p, l, v] = await Promise.all([
      api<Capabilities>("/capabilities"),
      api<Job[]>("/jobs"),
      api<Profile[]>("/profiles"),
      api<LogEntry[]>("/logs"),
      api<VMStatus>("/vm"),
    ]);
    setCaps(c);
    setJobs(j);
    setProfiles(p);
    setLogList(l);
    setVm(v);
    setSelected((old) => old || j[0]?.id || "");
  }
  useEffect(() => {
    let mounted = true;
    const load = () =>
      refresh().catch((e) => {
        if (mounted) setError(e.message);
      });
    load();
    const t = setInterval(load, 5000);
    return () => {
      mounted = false;
      clearInterval(t);
    };
  }, []);
  useEffect(() => {
    let mounted = true;
    // The local commit is cheap; the upstream check reaches GitHub, so it
    // follows separately instead of holding up the first paint.
    loadRepo(false).then(() => {
      if (mounted) loadRepo(true);
    });
    return () => {
      mounted = false;
    };
  }, []);
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState === "visible") {
        refresh().catch((e) => setError(e.message));
      }
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, []);
  useEffect(() => {
    setLog("");
    setSearch("");
    if (!selected) return;
    setStreamState("Connecting");
    const stream = new EventSource(`/api/jobs/${selected}/events`);
    stream.onopen = () => setStreamState("Live");
    stream.onerror = () => setStreamState("Reconnecting…");
    stream.addEventListener("log", (e) =>
      setLog((old) =>
        (old + JSON.parse((e as MessageEvent).data)).slice(-500_000),
      ),
    );
    stream.addEventListener("done", () => {
      setStreamState("Complete");
      stream.close();
      refresh().catch((e) => setError(e.message));
    });
    return () => stream.close();
  }, [selected]);
  useEffect(() => {
    // A direct jump, not scrollIntoView: turning Follow on while scrolled far
    // up must land at the bottom instantly, not via a long animated scroll.
    if (follow && terminal.current) {
      terminal.current.scrollTop = terminal.current.scrollHeight;
    }
  }, [log, follow]);
  async function action(fn: () => Promise<void>) {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await fn();
      await refresh();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  const visibleLog = log
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/\n$/, "")
    .split("\n")
    .slice(-300)
    .filter(
      (line) => !search || line.toLowerCase().includes(search.toLowerCase()),
    )
    .join("\n");
  return (
    <div className="app">
      <header>
        <a className="brand" href="/">
          <img className="logo" src="/logo.png" alt="LinuxConsole" />
        </a>
        <div className="brand-copy">
          <p className="eyebrow">YOUR DISTRO, FROM SOURCE</p>
          <h1>Build workspace</h1>
          <p className="muted">
            Configure LinuxConsole, launch a build, and follow its progress.
          </p>
        </div>
        <div className="header-actions">
          <div className="theme-switch" role="group" aria-label="Color theme">
            <button
              type="button"
              aria-label="Light theme"
              aria-pressed={theme === "light"}
              onClick={() => setTheme("light")}
            >
              Light
            </button>
            <button
              type="button"
              aria-label="Dark theme"
              aria-pressed={theme === "dark"}
              onClick={() => setTheme("dark")}
            >
              Dark
            </button>
          </div>
          <div className="identity">
            <span className="status-dot" />
            {caps?.development
              ? "Local development"
              : caps?.user || "Connecting"}
            {caps && !caps.development && (
              <a href="/oauth2/sign_out">Sign out</a>
            )}
          </div>
        </div>
      </header>
      <main>
        <section className="panel repository">
          <div className="panel-heading">
            <div className="panel-heading-title">
              <h2>Repository</h2>
              {repo && (
                <span className="architecture">
                  {repo.branch}
                  {repo.tag && ` · ${repo.tag}`}
                </span>
              )}
            </div>
            <div className="repo-actions">
              <button
                type="button"
                className="quiet"
                disabled={!!repoBusy}
                onClick={() => loadRepo(true)}
              >
                {repoBusy === "check" ? "Checking…" : "↻ Check upstream"}
              </button>
              <button
                type="button"
                className="update"
                disabled={
                  !!repoBusy ||
                  !repo?.upstream ||
                  repo.behind === 0 ||
                  repo.dirty
                }
                onClick={updateRepo}
              >
                {repoBusy === "update"
                  ? "Updating…"
                  : repo && repo.behind > 0
                    ? `↓ Update (${repo.behind})`
                    : "Up to date"}
              </button>
            </div>
          </div>
          <div className="commits">
            {commitCard(
              "THIS CHECKOUT",
              repo?.local,
              repo?.dirty ? " · uncommitted changes" : undefined,
            )}
            {commitCard(
              "UPSTREAM · LINUXCONSOLE-ORG/YDFS2",
              repo?.upstream,
              repo?.checkedAt ? ` · checked ${date(repo.checkedAt)}` : undefined,
            )}
          </div>
          <p className={`repo-status ${repoError ? "error" : ""}`}>
            {repoError ||
              (!repo
                ? "Reading the checkout…"
                : !repo.upstream
                  ? "Check upstream to compare this checkout with GitHub."
                  : repo.dirty
                    ? "The checkout has uncommitted changes; commit or discard them before updating."
                    : repo.behind === 0
                      ? `Up to date with ${repo.branch} on GitHub.${repo.ahead > 0 ? ` ${repo.ahead} local commit(s) ahead.` : ""}`
                      : `${repo.behind} new commit(s) available on ${repo.branch}.${repo.ahead > 0 ? ` Updating merges them into your ${repo.ahead} local commit(s).` : ""} Only builds queued afterwards are affected.`)}{" "}
            <a href={repo?.url || "https://github.com/linuxconsole-org/ydfs2"} target="_blank" rel="noreferrer">
              View on GitHub ↗
            </a>
          </p>
        </section>
        {error && (
          <div className="alert" role="alert">
            {error}
            <button onClick={() => setError("")} aria-label="Dismiss error">
              ×
            </button>
          </div>
        )}
        {notice && (
          <div className="notice" role="status">
            {notice}
          </div>
        )}
        <div className="workspace">
          <aside>
            <section className="panel">
              <div className="panel-heading">
                <div className="panel-heading-title">
                  <h2>New build</h2>
                  <span className="architecture">Linux · x86_64</span>
                </div>
                <span className="step">01</span>
              </div>
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  action(async () => {
                    const j = await api<Job>("/jobs", "POST", settings);
                    setSelected(j.id);
                    setNotice("Build added to the queue.");
                  });
                }}
              >
                <label>
                  Build target
                  <select
                    value={settings.target}
                    onChange={(e) =>
                      setSettings({
                        ...settings,
                        target: e.target.value,
                        kernel: "",
                      })
                    }
                  >
                    {(caps?.targets || Object.keys(names)).map((t) => (
                      <option key={t} value={t}>
                        {names[t] || t}
                      </option>
                    ))}
                  </select>
                </label>
                <p className="hint">
                  {settings.target === "fast-iso"
                    ? "Uses prebuilt core and kernel files, then builds updates and the ISO."
                    : settings.target === "full-iso"
                      ? "Compiles from source using cached work where available. A full build can take hours or days."
                      : "Builds this component using the existing LinuxConsole scripts and cache."}
                </p>
                {(settings.target === "full-iso" ||
                  settings.target === "kernel") && (
                  <label>
                    Kernel version <span className="optional">optional</span>
                    <input
                      placeholder="Use repository default"
                      value={settings.kernel}
                      pattern="[0-9]+\.[0-9]+\.[0-9]+"
                      onChange={(e) =>
                        setSettings({ ...settings, kernel: e.target.value })
                      }
                    />
                    <span className="hint">
                      An explicit version uses the repository’s kernel
                      configuration.
                    </span>
                  </label>
                )}
                <label className="check">
                  <input
                    type="checkbox"
                    checked={settings.verbose}
                    onChange={(e) =>
                      setSettings({ ...settings, verbose: e.target.checked })
                    }
                  />
                  <span>
                    Verbose compilation
                    <small>Show compiler output in the live log.</small>
                  </span>
                </label>
                <label>
                  Config.ini overrides{" "}
                  <span className="optional">optional</span>
                  <textarea
                    className="code"
                    rows={4}
                    placeholder={"KERNEL3=6.18.29\nBUILDMODULES=YES"}
                    value={settings.configOverrides}
                    onChange={(e) =>
                      setSettings({
                        ...settings,
                        configOverrides: e.target.value,
                      })
                    }
                  />
                  <span className="hint">
                    One <code>NAME=value</code> line per setting, appended
                    after the repository’s generated config.ini. Values may
                    only contain letters, digits, and{" "}
                    <code>{" _./:+-"}</code>, optionally wrapped in double
                    quotes for a space-separated list. Build-critical keys
                    (<code>ARCH</code>, <code>DISTRONAME</code>,{" "}
                    <code>BUILDYDFS</code>, <code>ISOTMP</code>,{" "}
                    <code>SEND_BUILD_LOG</code>, <code>SEND_OPKG</code>,{" "}
                    <code>MENUCONFIG</code>) are managed by the build manager
                    and cannot be overridden.
                  </span>
                </label>
                <label>
                  Package list <span className="optional">optional</span>
                  <select
                    disabled={loadingList}
                    value={settings.packageList}
                    onChange={(e) => loadPackageList(e.target.value)}
                  >
                    <option value="">Use the repository’s list</option>
                    {(caps?.packageLists || []).map((n) => (
                      <option key={n} value={n}>
                        {n}
                      </option>
                    ))}
                  </select>
                  {settings.packageList && (
                    <textarea
                      className="code"
                      rows={8}
                      value={settings.packageListText}
                      onChange={(e) =>
                        setSettings({
                          ...settings,
                          packageListText: e.target.value,
                        })
                      }
                    />
                  )}
                  <span className="hint">
                    Replaces <code>packages/list-{settings.packageList || "…"}</code>{" "}
                    for this build only; the shared repository checkout is
                    never modified.
                  </span>
                </label>
                {isIso(settings.target) && (
                  <fieldset className="apps-field">
                    <legend>
                      Flathub applications{" "}
                      <span className="optional">optional</span>
                    </legend>
                    <div className="apps">
                      {(caps?.flathub?.apps || []).map((app) => (
                        <label className="check" key={app.id}>
                          <input
                            type="checkbox"
                            checked={settings.flatpaks.includes(app.id)}
                            onChange={(e) =>
                              toggleApp(app.id, e.target.checked)
                            }
                          />
                          <span>
                            {app.name}
                            <small>{app.summary || app.id}</small>
                          </span>
                        </label>
                      ))}
                    </div>
                    <div className="apps-actions">
                      <button
                        type="button"
                        className="secondary"
                        disabled={flathubBusy}
                        onClick={refreshFlathub}
                      >
                        {flathubBusy ? "Refreshing…" : "Refresh from Flathub"}
                      </button>
                      <small>
                        {settings.flatpaks.length} selected
                        {caps?.flathub?.checkedAt
                          ? ` · updated ${date(caps.flathub.checkedAt)}`
                          : " · built-in list"}
                      </small>
                    </div>
                    <span className={`hint ${flathubError ? "error" : ""}`}>
                      {flathubError ||
                        "Downloaded during the build and baked into the ISO, so they work with no network on first boot. Each application adds hundreds of megabytes to several gigabytes once its runtime is counted."}
                    </span>
                  </fieldset>
                )}
                <div className="config-summary">
                  <span>
                    Distribution<strong>linuxconsole</strong>
                  </span>
                  <span>
                    Architecture<strong>x86_64</strong>
                  </span>
                  <span>
                    Build environment<strong>Docker</strong>
                  </span>
                </div>
                <button
                  className="primary"
                  disabled={busy || !caps?.ready}
                  type="submit"
                >
                  {busy ? "Working…" : "＋ Queue build"}
                </button>
              </form>
            </section>
            <section className="panel">
              <h2>Saved profiles</h2>
              <div className="profiles">
                {profiles.length === 0 && (
                  <p className="hint">Save settings you use regularly.</p>
                )}
                {profiles.map((p) => (
                  <div key={p.name}>
                    <button
                      onClick={() => {
                        setSettings({
                          ...blankSettings,
                          ...p.settings,
                          flatpaks: p.settings.flatpaks ?? [],
                        });
                        setProfileName(p.name);
                        setNotice(`Loaded profile “${p.name}”.`);
                      }}
                    >
                      {p.name}
                      <small>{names[p.settings.target]}</small>
                    </button>
                    <button
                      aria-label={`Delete profile ${p.name}`}
                      onClick={() =>
                        action(async () => {
                          await api(
                            `/profiles/${encodeURIComponent(p.name)}`,
                            "DELETE",
                          );
                        })
                      }
                    >
                      ×
                    </button>
                  </div>
                ))}
              </div>
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  action(async () => {
                    await api("/profiles", "PUT", {
                      name: profileName,
                      settings,
                    });
                    setNotice("Profile saved.");
                  });
                }}
              >
                <label className="sr-only" htmlFor="profile">
                  Profile name
                </label>
                <input
                  id="profile"
                  placeholder="Profile name"
                  maxLength={80}
                  required
                  value={profileName}
                  onChange={(e) => setProfileName(e.target.value)}
                />
                <button
                  className="secondary"
                  disabled={busy || !profileName.trim()}
                >
                  Save current settings
                </button>
              </form>
            </section>
          </aside>
          <div className="builds">
            <div className="metrics">
              <div>
                <span className={`status-dot ${caps?.ready ? "" : "warning"}`} />
                <strong>
                  {caps?.ready ? "Build host ready" : "Build host needs attention"}
                </strong>
                <small>{caps?.message || "Docker environment"}</small>
              </div>
              <div>
                <strong>{caps ? bytes(caps.freeBytes) : "—"}</strong>
                <small>Available storage</small>
              </div>
              <div>
                <strong>
                  {active ? "1" : "0"} running <span>/ {queued} queued</span>
                </strong>
                <small>One build at a time · shared cache</small>
              </div>
            </div>
            {favorites.length > 0 && (
              <section className="panel favorites">
                <div className="panel-heading">
                  <div className="panel-heading-title">
                    <h2>Kept builds</h2>
                    <span className="architecture">never cleaned up</span>
                  </div>
                  <span className="count">{favorites.length}</span>
                </div>
                <div className="fav-list">
                  {favorites.map((f) => (
                    <div className="fav" key={f.id}>
                      <div className="fav-head">
                        <button
                          className="fav-title"
                          onClick={() => setSelected(f.id)}
                        >
                          {names[f.settings.target] || f.settings.target}
                        </button>
                        <small>
                          {date(f.created)} · {f.revision.slice(0, 12)} ·{" "}
                          {f.tag || "untagged"}
                        </small>
                      </div>
                      <div className="fav-actions">
                        {f.artifacts.map((v) => (
                          <a
                            key={v.name}
                            href={`/api/jobs/${f.id}/artifacts/${encodeURIComponent(v.name)}`}
                          >
                            ↓ {v.name} <small>{bytes(v.size)}</small>
                          </a>
                        ))}
                        <button
                          type="button"
                          className="quiet"
                          onClick={() => viewConfig(f)}
                        >
                          config.ini
                        </button>
                        {vmButton(f)}
                        <button
                          className="quiet"
                          disabled={busy}
                          onClick={() => toggleFavorite(f)}
                        >
                          Release
                        </button>
                        <button
                          className="quiet"
                          disabled={busy}
                          onClick={() => removeBuild(f)}
                        >
                          Delete
                        </button>
                      </div>
                    </div>
                  ))}
                </div>
              </section>
            )}
            <section className="panel history">
              <div className="panel-heading">
                <h2>Build activity</h2>
                <span className="count">{jobs.length}</span>
              </div>
              {jobs.length === 0 ? (
                <div className="empty">
                  <span>⌘</span>
                  <h3>Your first build starts here</h3>
                  <p>
                    Choose a target and queue a build.
                    <br />
                    Progress and results will appear here.
                  </p>
                </div>
              ) : (
                <div className="job-list">
                  {jobs.map((j) => (
                    <button
                      className={`job-row ${selected === j.id ? "selected" : ""}`}
                      key={j.id}
                      onClick={() => setSelected(j.id)}
                    >
                      <span className={`job-icon ${j.state}`}>
                        {j.state === "succeeded"
                          ? "✓"
                          : j.state === "failed"
                            ? "!"
                            : done(j.state)
                              ? "–"
                              : "›"}
                      </span>
                      <span className="job-name">
                        <strong>{names[j.settings.target]}</strong>
                        <small>
                          {j.user} · {date(j.created)}
                        </small>
                      </span>
                      {j.favorite && (
                        <span className="pin" title="Kept build">
                          ★
                        </span>
                      )}
                      <span className={`badge ${j.state}`}>{j.state}</span>
                    </button>
                  ))}
                </div>
              )}
            </section>
            {job && (
              <section className="panel details">
                <div className="panel-heading">
                  <div>
                    <p className="eyebrow">BUILD DETAILS</p>
                    <h2>{names[job.settings.target]}</h2>
                  </div>
                  {!done(job.state) ? (
                    <button
                      className="danger"
                      disabled={busy || job.state === "cancelling"}
                      onClick={() =>
                        action(async () => {
                          await api(`/jobs/${job.id}/cancel`, "POST", {});
                        })
                      }
                    >
                      {job.state === "cancelling"
                        ? "Cancelling…"
                        : "Cancel build"}
                    </button>
                  ) : (
                    <div className="detail-actions">
                      {vmButton(job)}
                      {job.state === "succeeded" && !job.prunedAt && (
                        <button
                          className="quiet"
                          disabled={busy}
                          aria-pressed={job.favorite}
                          onClick={() => toggleFavorite(job)}
                        >
                          {job.favorite ? "★ Kept" : "☆ Keep"}
                        </button>
                      )}
                      <button
                        className="quiet"
                        disabled={busy}
                        onClick={() => removeBuild(job)}
                      >
                        Delete build
                      </button>
                    </div>
                  )}
                </div>
                <dl>
                  <div>
                    <dt>Revision</dt>
                    <dd>
                      {job.revision.slice(0, 12)} · {job.tag || "untagged"}
                    </dd>
                  </div>
                  <div>
                    <dt>Configuration</dt>
                    <dd>
                      {job.settings.kernel || "Default kernel"} ·{" "}
                      {job.settings.verbose ? "Verbose" : "Summary"} logs
                      {job.settings.configOverrides && " · config overrides"}
                      {job.settings.packageList &&
                        ` · package list: ${job.settings.packageList}`}
                      {(job.settings.flatpaks?.length ?? 0) > 0 &&
                        ` · ${job.settings.flatpaks.length} Flathub app(s)`}
                    </dd>
                  </div>
                  <div>
                    <dt>Started</dt>
                    <dd>{date(job.started)}</dd>
                  </div>
                  <div>
                    <dt>Finished</dt>
                    <dd>
                      {date(job.finished)}
                      {job.exitCode !== undefined && ` · exit ${job.exitCode}`}
                    </dd>
                  </div>
                </dl>
                {job.error && <p className="build-error">{job.error}</p>}
                {job.artifacts.length > 0 && (
                  <div className="artifacts">
                    {job.artifacts.map((f) => (
                      <a
                        key={f.name}
                        href={`/api/jobs/${job.id}/artifacts/${encodeURIComponent(f.name)}`}
                      >
                        <span>↓ {f.name}</span>
                        <small>{bytes(f.size)}</small>
                      </a>
                    ))}
                  </div>
                )}
                {job.prunedAt && (
                  <p className="hint reclaimed">
                    Files removed {date(job.prunedAt)} to make room: only the
                    {` ${keepBuilds} `}most recent builds of each kind keep
                    theirs. The log and configuration below are still here. Use
                    “Keep” on a build to pin its files permanently.
                  </p>
                )}
                {job.state === "succeeded" && (
                  <div className="artifacts">
                    <button
                      type="button"
                      className="artifact-open"
                      onClick={() => viewConfig(job)}
                    >
                      <span>⚙ config.ini</span>
                      <small>as built</small>
                    </button>
                  </div>
                )}
                <button
                  type="button"
                  className="quiet"
                  aria-expanded={logOpen}
                  aria-controls="build-log-panel"
                  onClick={() => setLogOpen((open) => !open)}
                >
                  {logOpen ? "Close log" : "Open log"}
                </button>
                <div id="build-log-panel" hidden={!logOpen}>
                  <div className="log-toolbar">
                    <span>
                      <span
                        className={`status-dot ${done(job.state) ? "idle" : ""}`}
                      />
                      {streamState}
                    </span>
                    <label className="sr-only" htmlFor="log-search">
                      Filter log
                    </label>
                    <input
                      id="log-search"
                      placeholder="Filter visible log…"
                      value={search}
                      onChange={(e) => setSearch(e.target.value)}
                    />
                    <label className="follow">
                      <input
                        type="checkbox"
                        checked={follow}
                        onChange={(e) => setFollow(e.target.checked)}
                      />
                      Follow
                    </label>
                    <a href={`/api/jobs/${job.id}/log`}>Download log ↓</a>
                  </div>
                  <div
                    className="terminal"
                    ref={terminal}
                    tabIndex={0}
                    aria-label="Build log"
                  >
                    <pre>{visibleLog || "Waiting for build output…"}</pre>
                  </div>
                  <p className="log-footnote">
                    Showing the latest 300 lines. Download the log for
                    the complete output.
                  </p>
                </div>
              </section>
            )}
          </div>
        </div>
        <section className="panel logs-box">
          <div className="panel-heading">
            <div className="panel-heading-title">
              <h2>Build logs</h2>
              <span className="architecture">logs-build/</span>
            </div>
            <span className="count">
              {logList.filter((l) => l.present).length}
            </span>
          </div>
          {logList.filter((l) => l.present).length === 0 ? (
            <p className="hint">
              Every build writes its log here. They are kept when a build's
              artifacts are reclaimed, and removed when the build is deleted.
            </p>
          ) : (
            <div className="log-rows">
              {logList
                .filter((l) => l.present)
                .map((l) => (
                  <div className="log-row" key={l.id}>
                    <button
                      className="log-open"
                      onClick={() => viewLog(l)}
                    >
                      <strong>{names[l.target] || l.target}</strong>
                      <small>
                        {l.user} · {date(l.created)} · {bytes(l.size)}
                      </small>
                    </button>
                    <span className={`badge ${l.state}`}>{l.state}</span>
                    <a
                      href={`/api/jobs/${l.id}/log`}
                      aria-label={`Download log for ${names[l.target] || l.target}`}
                    >
                      ↓
                    </a>
                    <button
                      className="quiet"
                      disabled={busy || !done(l.state)}
                      aria-label={`Delete log for ${names[l.target] || l.target}`}
                      onClick={() => removeLog(l)}
                    >
                      ×
                    </button>
                  </div>
                ))}
            </div>
          )}
        </section>
        {/* Shown whatever build is selected: clicking another build must not
            tear down a machine someone is watching. */}
        {vmLive && vm.jobId && (
          <VmConsole
            vm={vm}
            busy={busy || vmBusy}
            onStop={() => stopVm(vm.jobId!)}
          />
        )}
        {vm.state === "stopped" && vm.error && (
          <p className="notice">Test machine: {vm.error}</p>
        )}
        <footer>
          LinuxConsole 2026{" "}
          <span>Builds continue when you close this page.</span>
        </footer>
        <TextViewer view={viewing} onClose={() => setViewing(undefined)} />
        <ConfirmDialog ask={ask} onClose={() => setAsk(undefined)} />
      </main>
    </div>
  );
}
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
