import React, { useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
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
    : `${(n / 2 ** 20).toFixed(1)} MB`;
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
function App() {
  const [ask, setAsk] = useState<Confirmation | undefined>();
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
  const active = jobs.find((j) => !done(j.state) && j.state !== "queued");
  const queued = jobs.filter((j) => j.state === "queued").length;
  async function refresh() {
    const [c, j, p] = await Promise.all([
      api<Capabilities>("/capabilities"),
      api<Job[]>("/jobs"),
      api<Profile[]>("/profiles"),
    ]);
    setCaps(c);
    setJobs(j);
    setProfiles(p);
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
                        <a
                          href={`/api/jobs/${f.id}/config`}
                          target="_blank"
                          rel="noreferrer"
                        >
                          config.ini ↗
                        </a>
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
                    <a
                      href={`/api/jobs/${job.id}/config`}
                      target="_blank"
                      rel="noreferrer"
                    >
                      <span>⚙ config.ini</span>
                      <small>as built</small>
                    </a>
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
        <footer>
          LinuxConsole 2026{" "}
          <span>Builds continue when you close this page.</span>
        </footer>
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
