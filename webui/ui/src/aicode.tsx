import React, {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { api } from "./shared";
import type { Confirmation, TextView } from "./shared";

// The AI Code Assistant.
//
// The loop this page exists for: describe a problem, let the assistant read
// only the files you picked, review the diff it proposes, apply it, build it
// with the pipeline that was already here, and hand the compiler errors back.
//
// Two things are deliberate. Nothing the assistant writes reaches the checkout
// until Apply is pressed — the server stages a proposal and this page shows the
// real diff for it. And the build is an ordinary job: this page posts to
// /api/jobs and streams /api/jobs/{id}/events like the rest of the manager, so
// there is one build queue, one log and one history.

type Usage = {
  inputTokens: number | null;
  cachedInputTokens: number | null;
  outputTokens: number | null;
  reasoningTokens: number | null;
  contextTokens: number | null;
  contextLimit: number | null;
  estimated: boolean;
  cost: number | null;
  currency?: string;
  costNote?: string;
  latencyMs: number;
  provider: string;
  model: string;
};
type Totals = {
  requests: number;
  toolCalls: number;
  inputTokens: number;
  cachedInputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  cost: number | null;
  currency?: string;
  latencyMs: number;
};
type Message = {
  role: "user" | "assistant";
  content: string;
  at: string;
  usage?: Usage;
  patchId?: string;
  cancelled?: boolean;
};
type PatchFile = {
  path: string;
  diff: string;
  added: number;
  removed: number;
  created: boolean;
  bytes: number;
};
type Patch = {
  id: string;
  at: string;
  files: PatchFile[];
  applied: boolean;
  appliedAt?: string;
};
type AISettings = {
  provider: string;
  model: string;
  baseUrl: string;
  contextLimit: number;
  inputPrice: number;
  cachedInputPrice: number;
  outputPrice: number;
  currency: string;
  timeoutSeconds: number;
  maxTokens: number;
};
type ProviderInfo = {
  ID: string;
  Label: string;
  DefaultBase: string;
  NeedsKey: boolean;
};
type QuickAction = { ID: string; Label: string; Prompt: string };
type AIBuild = { jobId: string; target: string; at: string };
type AIState = {
  providers: ProviderInfo[];
  settings: AISettings;
  keyConfigured: boolean;
  keyFromEnv: boolean;
  ready: boolean;
  message: string;
  session: {
    id: string;
    started: string;
    messages: Message[];
    totals: Totals;
    build: AIBuild | null;
  };
  generating: boolean;
  patch: Patch | null;
  build: AIBuild | null;
  quickActions: QuickAction[];
  targets: string[];
};
type CompilerErrors = {
  jobId: string;
  target: string;
  exit: string;
  state: string;
  text: string;
  files: string[];
  count: number;
};
// Only what this page reads from a build; the full shape lives in main.tsx,
// which owns the build screens.
type BuildJob = {
  id: string;
  state: string;
  exitCode?: number;
  settings: { target: string };
};

const done = (s: string) =>
  s === "succeeded" || s === "failed" || s === "cancelled";
// Mirrors maxContextFiles in aiworkspace.go: the server refuses more.
const maxContextFiles = 24;
// This page's own arithmetic, never presented as a measurement.
const estimateTokens = (s: string) => Math.ceil(s.length / 4);
const num = (n: number | null | undefined) =>
  n === null || n === undefined ? "—" : n.toLocaleString();

// The patch fence is the protocol between the server and the model. It is
// stripped from what is displayed: the proposal is shown as a reviewable diff
// below, and repeating the whole file in the transcript hides the explanation.
const patchFence = /```ydfs-patch[ \t]+path=([^\n`]+)\n[\s\S]*?```/g;

// ------------------------------------------------------------------- hooks

// useAIChat owns the conversation: the server state, the streaming reply and
// the cancellation. The stream is a POST rather than an EventSource because
// every mutating request has to carry the headers the CSRF rule checks, and
// EventSource cannot set headers.
function useAIChat(onError: (m: string) => void) {
  const [state, setState] = useState<AIState>();
  const [streaming, setStreaming] = useState("");
  const [busy, setBusy] = useState(false);
  const [contextNote, setContextNote] = useState("");
  const [lastUsage, setLastUsage] = useState<Usage>();
  // After a reload there is no stream to learn from, but the conversation
  // carries what the last request cost: show that rather than nothing.
  const restored = useMemo(() => {
    const turns = state?.session.messages ?? [];
    for (let i = turns.length - 1; i >= 0; i--)
      if (turns[i].usage) return turns[i].usage;
    return undefined;
  }, [state?.session.messages]);
  const abort = useRef<AbortController>(null);

  const reload = useCallback(async () => {
    try {
      setState(await api<AIState>("/ai"));
    } catch (e) {
      onError((e as Error).message);
    }
  }, [onError]);
  useEffect(() => {
    reload();
  }, [reload]);

  const send = useCallback(
    async (body: {
      prompt: string;
      files: string[];
      includeDiff: boolean;
      buildId: string;
      search: string;
    }) => {
      setBusy(true);
      setStreaming("");
      setContextNote("");
      const controller = new AbortController();
      abort.current = controller;
      try {
        const res = await fetch("/api/ai/message", {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "X-Requested-With": "ydfs-web",
          },
          body: JSON.stringify(body),
          signal: controller.signal,
        });
        if (!res.ok || !res.body) {
          const err = await res
            .json()
            .catch(() => ({ error: `Request failed (${res.status})` }));
          throw new Error(err.error);
        }
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        for (;;) {
          const { done: finished, value } = await reader.read();
          if (finished) break;
          buffer += decoder.decode(value, { stream: true });
          // Server-sent events are separated by a blank line; a partial one
          // stays in the buffer until the rest of it arrives.
          const parts = buffer.split("\n\n");
          buffer = parts.pop() ?? "";
          for (const part of parts) {
            let event = "message";
            let data = "";
            for (const line of part.split("\n")) {
              if (line.startsWith("event:")) event = line.slice(6).trim();
              else if (line.startsWith("data:")) data += line.slice(5).trim();
            }
            if (!data) continue;
            const payload = JSON.parse(data);
            if (event === "text") setStreaming((old) => old + payload);
            else if (event === "context")
              setContextNote(
                `${payload.tokens.toLocaleString()} estimated context tokens${payload.note ? ` · ${payload.note}` : ""}`,
              );
            else if (event === "usage") setLastUsage(payload);
            else if (event === "failed" || event === "patchError")
              onError(payload.error);
            else if (event === "cancelled") onError(payload.error);
          }
        }
      } catch (e) {
        if ((e as Error).name !== "AbortError") onError((e as Error).message);
      } finally {
        abort.current = null;
        setBusy(false);
        setStreaming("");
        await reload();
      }
    },
    [onError, reload],
  );

  // Stop both halves: the server drops the provider connection, and the fetch
  // stops reading. Either alone would leave the other running.
  const stop = useCallback(async () => {
    try {
      await api("/ai/cancel", "POST", {});
    } catch {
      /* it may have finished between the click and the call */
    }
    abort.current?.abort();
  }, []);

  return {
    state,
    reload,
    send,
    stop,
    busy,
    streaming,
    contextNote,
    lastUsage: lastUsage ?? restored,
  };
}

// useWorkspaceFiles is the context-selection layer's read side: the tree the
// server is willing to show, narrowed by a substring.
function useWorkspaceFiles(onError: (m: string) => void) {
  const [files, setFiles] = useState<string[]>([]);
  const [more, setMore] = useState(0);
  const [total, setTotal] = useState(0);
  const [filter, setFilter] = useState("");
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    let live = true;
    const t = window.setTimeout(async () => {
      try {
        const r = await api<{ files: string[]; more: number; total: number }>(
          `/ai/files?search=${encodeURIComponent(filter)}`,
        );
        if (!live) return;
        setFiles(r.files);
        setMore(r.more);
        setTotal(r.total);
      } catch (e) {
        if (live) onError((e as Error).message);
      } finally {
        if (live) setLoading(false);
      }
    }, 200);
    return () => {
      live = false;
      window.clearTimeout(t);
    };
  }, [filter, onError]);
  return { files, more, total, filter, setFilter, loading };
}

// useAIBuild drives the existing pipeline. It queues an ordinary job, tells the
// server which one is testing the assistant's work, and follows the same SSE
// log stream the build screens use.
function useAIBuild(onError: (m: string) => void, onFinish: () => void) {
  const [job, setJob] = useState<BuildJob>();
  const [log, setLog] = useState("");
  const [elapsed, setElapsed] = useState(0);
  const [starting, setStarting] = useState(false);
  const startedAt = useRef(0);

  const follow = useCallback(
    (id: string) => {
      startedAt.current = Date.now();
      const stream = new EventSource(`/api/jobs/${id}/events`);
      stream.addEventListener("log", (e) =>
        setLog((old) =>
          (old + JSON.parse((e as MessageEvent).data)).slice(-200_000),
        ),
      );
      stream.addEventListener("done", (e) => {
        setJob(JSON.parse((e as MessageEvent).data));
        stream.close();
        onFinish();
      });
      return stream;
    },
    [onFinish],
  );

  // Pick an already-running or finished build back up after a reload.
  const adopt = useCallback(
    async (id: string) => {
      try {
        const j = await api<BuildJob>(`/jobs/${id}`);
        setJob(j);
        setLog("");
        if (!done(j.state)) follow(id);
        else {
          const text = await fetch(`/api/jobs/${id}/log`, {
            headers: { "X-Requested-With": "ydfs-web" },
          }).then((r) => (r.ok ? r.text() : ""));
          setLog(text.slice(-200_000));
        }
      } catch (e) {
        onError((e as Error).message);
      }
    },
    [follow, onError],
  );

  const start = useCallback(
    async (target: string) => {
      setStarting(true);
      setLog("");
      try {
        const j = await api<BuildJob>("/jobs", "POST", {
          target,
          verbose: true,
          kernel: "",
          configOverrides: "",
          packageList: "",
          packageListText: "",
          flatpaks: [],
        });
        setJob(j);
        await api("/ai/build", "POST", { jobId: j.id });
        follow(j.id);
      } catch (e) {
        onError((e as Error).message);
      } finally {
        setStarting(false);
      }
    },
    [follow, onError],
  );

  useEffect(() => {
    if (!job || done(job.state)) return;
    const t = setInterval(
      () => setElapsed(Math.floor((Date.now() - startedAt.current) / 1000)),
      1000,
    );
    // The stream reports completion, but a poll is the fallback the rest of
    // the manager uses too, and it is what notices a cancellation.
    const p = setInterval(async () => {
      try {
        const j = await api<BuildJob>(`/jobs/${job.id}`);
        setJob(j);
        if (done(j.state)) onFinish();
      } catch {
        /* the next tick tries again */
      }
    }, 5000);
    return () => {
      clearInterval(t);
      clearInterval(p);
    };
  }, [job, onFinish]);

  return { job, log, elapsed, starting, start, adopt, setJob };
}

// ------------------------------------------------------------- presentation

// Assistant text, with fenced code shown as code. No highlighter: the project
// carries none, and one would be a dependency for this page alone.
function AIMessageBody({ text }: { text: string }) {
  const blocks = useMemo(() => {
    const cleaned = text.replace(
      patchFence,
      (_m, p) => `\n→ Proposed a change to ${String(p).trim()}, shown below.\n`,
    );
    const out: { code: boolean; lang: string; text: string }[] = [];
    const parts = cleaned.split(/```/);
    parts.forEach((part, i) => {
      if (i % 2 === 0) {
        if (part.trim()) out.push({ code: false, lang: "", text: part });
        return;
      }
      const nl = part.indexOf("\n");
      const lang = nl > 0 ? part.slice(0, nl).trim() : "";
      out.push({ code: true, lang, text: nl > 0 ? part.slice(nl + 1) : part });
    });
    return out;
  }, [text]);
  return (
    <>
      {blocks.map((b, i) =>
        b.code ? (
          <pre className="ai-code" key={i}>
            {b.lang && <span className="ai-code-lang">{b.lang}</span>}
            <code>{b.text.replace(/\n$/, "")}</code>
          </pre>
        ) : (
          <p key={i}>{b.text.trim()}</p>
        ),
      )}
    </>
  );
}

function AIMessage({
  message,
  onRetry,
}: {
  message: Message;
  onRetry?: () => void;
}) {
  const mine = message.role === "user";
  // The context block the server prepended is not what the operator typed;
  // the instruction is the last paragraph after the separator.
  const text = mine
    ? message.content.split("\n\n---\n\n").slice(-1)[0]
    : message.content;
  const context = mine
    ? message.content.slice(0, message.content.length - text.length).trim()
    : "";
  return (
    <article className={`ai-message ${mine ? "mine" : "theirs"}`}>
      <header>
        <strong>{mine ? "You" : "Assistant"}</strong>
        {message.cancelled && <span className="ai-tag">stopped</span>}
        {message.usage && (
          <span className="muted">
            {message.usage.model} ·{" "}
            {(message.usage.latencyMs / 1000).toFixed(1)}s
          </span>
        )}
        {onRetry && (
          <button type="button" className="quiet" onClick={onRetry}>
            ↻ Retry
          </button>
        )}
      </header>
      {context && (
        <details className="ai-context-sent">
          <summary>Context sent with this message</summary>
          <pre>
            <code>{context}</code>
          </pre>
        </details>
      )}
      <AIMessageBody text={text} />
    </article>
  );
}

// The context-selection panel: what the assistant is allowed to read.
function ContextPanel({
  repo,
  files,
  more,
  total,
  filter,
  setFilter,
  loading,
  selected,
  toggle,
  clear,
  includeDiff,
  setIncludeDiff,
  search,
  setSearch,
  contextTokens,
  onView,
}: {
  repo?: { branch: string; detached: boolean; tag: string; dirty: boolean };
  files: string[];
  more: number;
  total: number;
  filter: string;
  setFilter: (s: string) => void;
  loading: boolean;
  selected: string[];
  toggle: (p: string) => void;
  clear: () => void;
  includeDiff: boolean;
  setIncludeDiff: (v: boolean) => void;
  search: string;
  setSearch: (s: string) => void;
  contextTokens: number;
  onView: (v: TextView) => void;
}) {
  return (
    <aside className="ai-context">
      <section>
        <h2>Workspace</h2>
        <p className="muted">
          The checkout this manager builds from
          {repo && (
            <>
              {" — "}
              <strong>
                {repo.detached ? "detached HEAD" : repo.branch || "unknown"}
              </strong>
              {repo.tag && ` · ${repo.tag}`}
              {repo.dirty && " · uncommitted changes"}
            </>
          )}
          .
        </p>
        <label className="ai-check">
          <input
            type="checkbox"
            checked={includeDiff}
            onChange={(e) => setIncludeDiff(e.target.checked)}
          />
          Include the uncommitted diff
        </label>
      </section>
      <section>
        <h2>Files in context</h2>
        <p className="muted">
          Only the files ticked here are sent, never the repository. At most{" "}
          {maxContextFiles}.
        </p>
        <input
          type="search"
          placeholder="Search file names"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          aria-label="Search file names"
        />
        <input
          type="search"
          placeholder="Search file contents (git grep)"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search file contents"
        />
        {!!selected.length && (
          <div className="ai-selected">
            <div className="ai-selected-head">
              <strong>
                {selected.length} selected
                {selected.length >= maxContextFiles && " (limit reached)"}
              </strong>
              <button type="button" className="quiet" onClick={clear}>
                Clear
              </button>
            </div>
            {selected.map((p) => (
              <span className="ai-chip" key={p}>
                <button
                  type="button"
                  className="quiet"
                  title="Open this file"
                  onClick={() =>
                    onView({
                      key: `ai-file-${p}`,
                      title: p,
                      subtitle: "Workspace file",
                      url: `/api/ai/file?path=${encodeURIComponent(p)}`,
                    })
                  }
                >
                  {p}
                </button>
                <button
                  type="button"
                  className="quiet"
                  aria-label={`Remove ${p} from context`}
                  onClick={() => toggle(p)}
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        )}
        <div className="ai-filelist">
          {loading && <p className="muted">Reading the workspace…</p>}
          {!loading && !files.length && (
            <p className="muted">No file name matches that search.</p>
          )}
          {files.map((p) => (
            <label key={p} className="ai-file">
              <input
                type="checkbox"
                checked={selected.includes(p)}
                disabled={
                  !selected.includes(p) && selected.length >= maxContextFiles
                }
                onChange={() => toggle(p)}
              />
              <span>{p}</span>
            </label>
          ))}
          {more > 0 && (
            <p className="muted">
              {more} more match — narrow the search to see them.
            </p>
          )}
        </div>
        <p className="muted">
          {total.toLocaleString()} files in the workspace · about{" "}
          <strong>{contextTokens.toLocaleString()}</strong> context tokens
          (estimated)
        </p>
      </section>
    </aside>
  );
}

// The review gate. Nothing here has touched the checkout until Apply.
function PatchReview({
  patch,
  busy,
  onApply,
  onReject,
  onRevise,
  onView,
}: {
  patch: Patch;
  busy: boolean;
  onApply: () => void;
  onReject: () => void;
  onRevise: () => void;
  onView: (v: TextView) => void;
}) {
  const added = patch.files.reduce((n, f) => n + f.added, 0);
  const removed = patch.files.reduce((n, f) => n + f.removed, 0);
  return (
    <section className={`ai-patch ${patch.applied ? "applied" : ""}`}>
      <header>
        <h2>
          {patch.applied
            ? "Applied to the working tree"
            : "Proposed changes — review before applying"}
        </h2>
        <span className="muted">
          {patch.files.length} file{patch.files.length > 1 ? "s" : ""} ·{" "}
          <span className="ai-added">+{added}</span>{" "}
          <span className="ai-removed">−{removed}</span>
        </span>
      </header>
      <ul className="ai-patch-files">
        {patch.files.map((f) => (
          <li key={f.path}>
            <button
              type="button"
              className="quiet ai-patch-open"
              onClick={() =>
                onView({
                  key: `ai-diff-${patch.id}-${f.path}`,
                  title: f.path,
                  subtitle: patch.applied
                    ? "Applied change"
                    : "Proposed change — not yet applied",
                  url: `/api/ai/patch?path=${encodeURIComponent(f.path)}`,
                })
              }
            >
              {f.path}
            </button>
            {f.created && <span className="ai-tag">new file</span>}
            <span className="ai-added">+{f.added}</span>
            <span className="ai-removed">−{f.removed}</span>
          </li>
        ))}
      </ul>
      <div className="ai-patch-actions">
        {!patch.applied && (
          <button
            type="button"
            className="primary"
            disabled={busy}
            onClick={onApply}
          >
            Apply changes
          </button>
        )}
        <button
          type="button"
          className="quiet"
          disabled={busy}
          onClick={onRevise}
        >
          Ask AI to revise
        </button>
        <button
          type="button"
          className="danger"
          disabled={busy}
          onClick={onReject}
        >
          {patch.applied ? "Undo these changes" : "Reject"}
        </button>
      </div>
      <p className="muted">
        {patch.applied
          ? "These are ordinary uncommitted changes now: the Repository screen stages, commits and reverts them like any other edit."
          : "Nothing has been written to the checkout yet."}
      </p>
    </section>
  );
}

// The build panel drives the existing pipeline and, on a failure, hands the
// compiler's own words back to the assistant.
function BuildOutput({
  targets,
  target,
  setTarget,
  build,
  onAskAI,
  busy,
  onView,
}: {
  targets: string[];
  target: string;
  setTarget: (t: string) => void;
  build: ReturnType<typeof useAIBuild>;
  onAskAI: (errors: CompilerErrors) => void;
  busy: boolean;
  onView: (v: TextView) => void;
}) {
  const [errors, setErrors] = useState<CompilerErrors>();
  const job = build.job;
  const failed = job?.state === "failed";
  useEffect(() => {
    setErrors(undefined);
    if (!job || !failed) return;
    api<CompilerErrors>(`/ai/errors?job=${encodeURIComponent(job.id)}`)
      .then(setErrors)
      .catch(() => setErrors(undefined));
  }, [job, failed]);
  const label: Record<string, string> = {
    queued: "Queued",
    starting: "Starting",
    running: "Building",
    succeeded: "Success",
    failed: "Failed",
    cancelled: "Cancelled",
  };
  return (
    <section className="ai-build">
      <header>
        <h2>Build / Test changes</h2>
        <label>
          Target
          <select value={target} onChange={(e) => setTarget(e.target.value)}>
            {targets.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </label>
        <button
          type="button"
          className="primary"
          disabled={busy || build.starting || (!!job && !done(job.state))}
          onClick={() => build.start(target)}
        >
          {build.starting ? "Queueing…" : "Run build"}
        </button>
      </header>
      {!job && (
        <p className="muted">
          Queues an ordinary build through the same queue, container and log as
          every other build on this server.
        </p>
      )}
      {job && (
        <>
          <p className="ai-build-state">
            <span className={`ai-state ${job.state}`}>
              {label[job.state] ?? job.state}
            </span>
            <span className="muted">
              {job.settings?.target} ·{" "}
              {done(job.state)
                ? `exit ${job.exitCode ?? "—"}`
                : `${build.elapsed}s elapsed`}
            </span>
            <button
              type="button"
              className="quiet"
              onClick={() =>
                onView({
                  key: `ai-build-${job.id}`,
                  title: "Build log",
                  subtitle: `${job.settings?.target} · ${job.state}`,
                  url: `/api/jobs/${job.id}/log`,
                })
              }
            >
              Open full log
            </button>
          </p>
          <pre className="ai-buildlog">
            <code>
              {build.log
                .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "")
                .split("\n")
                .slice(-200)
                .join("\n") || "Waiting for output…"}
            </code>
          </pre>
          {failed && errors && (
            <div className="ai-build-errors">
              <p>
                <strong>{errors.count}</strong> compiler diagnostic
                {errors.count === 1 ? "" : "s"}
                {errors.files.length > 0 && <> in {errors.files.join(", ")}</>}.
              </p>
              <button
                type="button"
                className="primary"
                disabled={busy}
                onClick={() => onAskAI(errors)}
              >
                Ask AI to fix errors
              </button>
            </div>
          )}
          {failed && !errors && (
            <p className="muted">
              The build failed but printed nothing this page recognises as a
              compiler diagnostic. Open the full log to see what happened.
            </p>
          )}
        </>
      )}
    </section>
  );
}

// Provider, model and what the last request cost. Collapsible: it matters
// while tuning a setup and is noise the rest of the time.
function UsagePanel({
  state,
  usage,
  contextNote,
  onSave,
  saving,
}: {
  state: AIState;
  usage?: Usage;
  contextNote: string;
  onSave: (s: AISettings, apiKey: string) => void;
  saving: boolean;
}) {
  const [open, setOpen] = useState(!state.ready);
  const [form, setForm] = useState<AISettings>(state.settings);
  const [apiKey, setApiKey] = useState("");
  useEffect(() => setForm(state.settings), [state.settings]);
  const totals = state.session.totals;
  const provider = state.providers.find((p) => p.ID === form.provider);
  const used = usage?.contextTokens ?? null;
  const limit = usage?.contextLimit ?? (form.contextLimit || null);
  // Enough places to show a cheap request without padding an expensive one.
  const money = (v: number | null | undefined, currency?: string) =>
    v === null || v === undefined
      ? "—"
      : `${v.toLocaleString(undefined, {
          minimumFractionDigits: 2,
          maximumFractionDigits: 5,
        })} ${currency ?? ""}`.trim();
  return (
    <aside className="ai-usage">
      <section>
        <h2>This request</h2>
        {!usage && <p className="muted">No request has been made yet.</p>}
        {usage && (
          <dl>
            <dt>Provider</dt>
            <dd>{usage.provider}</dd>
            <dt>Model</dt>
            <dd>{usage.model}</dd>
            <dt>Context tokens</dt>
            <dd>
              {num(usage.contextTokens)} <em>estimated</em>
            </dd>
            <dt>Input tokens</dt>
            <dd>
              {num(usage.inputTokens)}
              {usage.estimated && <em> estimated</em>}
            </dd>
            <dt>Cached input</dt>
            <dd>
              {usage.cachedInputTokens === null ? (
                <span className="muted">not reported</span>
              ) : (
                num(usage.cachedInputTokens)
              )}
            </dd>
            <dt>Output tokens</dt>
            <dd>
              {num(usage.outputTokens)}
              {usage.estimated && <em> estimated</em>}
            </dd>
            <dt>Reasoning tokens</dt>
            <dd>
              {usage.reasoningTokens === null ? (
                <span className="muted">not reported</span>
              ) : (
                num(usage.reasoningTokens)
              )}
            </dd>
            <dt>Total tokens</dt>
            <dd>
              {num(
                usage.inputTokens === null && usage.outputTokens === null
                  ? null
                  : (usage.inputTokens ?? 0) + (usage.outputTokens ?? 0),
              )}
            </dd>
            <dt>Context window</dt>
            <dd>
              {limit && used ? (
                <>
                  {Math.round((used / limit) * 100)}% of {num(limit)}{" "}
                  <em>estimated</em>
                </>
              ) : (
                <span className="muted">no context limit configured</span>
              )}
            </dd>
            <dt>Cost</dt>
            <dd>
              {usage.cost === null ? (
                <span className="muted">{usage.costNote || "not priced"}</span>
              ) : (
                money(usage.cost, usage.currency)
              )}
            </dd>
            <dt>Latency</dt>
            <dd>{(usage.latencyMs / 1000).toFixed(2)}s</dd>
          </dl>
        )}
        {contextNote && <p className="muted">{contextNote}</p>}
      </section>
      <section>
        <h2>This session</h2>
        <dl>
          <dt>Requests</dt>
          <dd>{totals.requests}</dd>
          <dt>Input tokens</dt>
          <dd>{totals.inputTokens.toLocaleString()}</dd>
          <dt>Cached input</dt>
          <dd>{totals.cachedInputTokens.toLocaleString()}</dd>
          <dt>Output tokens</dt>
          <dd>{totals.outputTokens.toLocaleString()}</dd>
          <dt>Cost</dt>
          <dd>
            {totals.cost === null ? (
              <span className="muted">not priced</span>
            ) : (
              money(totals.cost, totals.currency)
            )}
          </dd>
          <dt>Time in requests</dt>
          <dd>{(totals.latencyMs / 1000).toFixed(1)}s</dd>
        </dl>
      </section>
      <section className="ai-settings">
        <h2>
          <button
            type="button"
            className="quiet"
            aria-expanded={open}
            onClick={() => setOpen(!open)}
          >
            {open ? "▾" : "▸"} Provider settings
          </button>
        </h2>
        {open && (
          <form
            onSubmit={(e) => {
              e.preventDefault();
              onSave(form, apiKey);
              setApiKey("");
            }}
          >
            <label>
              Provider
              <select
                value={form.provider}
                onChange={(e) => {
                  const p = state.providers.find(
                    (x) => x.ID === e.target.value,
                  );
                  setForm({
                    ...form,
                    provider: e.target.value,
                    baseUrl: p?.DefaultBase ?? "",
                  });
                }}
              >
                <option value="">Choose a provider…</option>
                {state.providers.map((p) => (
                  <option key={p.ID} value={p.ID}>
                    {p.Label}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Model
              <input
                value={form.model}
                placeholder="e.g. claude-sonnet-5, gpt-4.1, qwen2.5-coder"
                onChange={(e) => setForm({ ...form, model: e.target.value })}
              />
            </label>
            <label>
              Base URL
              <input
                value={form.baseUrl}
                placeholder="http://127.0.0.1:11434/v1"
                onChange={(e) => setForm({ ...form, baseUrl: e.target.value })}
              />
            </label>
            <label>
              API key
              <input
                type="password"
                autoComplete="off"
                value={apiKey}
                placeholder={
                  state.keyFromEnv
                    ? "set in the service environment"
                    : state.keyConfigured
                      ? "stored — leave blank to keep it"
                      : provider?.NeedsKey
                        ? "required for this provider"
                        : "not needed for this provider"
                }
                disabled={state.keyFromEnv}
                onChange={(e) => setApiKey(e.target.value)}
              />
            </label>
            <p className="muted">
              The key is held on the server and is never sent back to this page.
              {state.keyFromEnv &&
                " It comes from YDFS_AI_API_KEY in the service environment, which wins over anything set here."}
            </p>
            <div className="ai-grid">
              <label>
                Context limit
                <input
                  type="number"
                  min={0}
                  value={form.contextLimit}
                  onChange={(e) =>
                    setForm({ ...form, contextLimit: +e.target.value })
                  }
                />
              </label>
              <label>
                Max output tokens
                <input
                  type="number"
                  min={0}
                  value={form.maxTokens}
                  onChange={(e) =>
                    setForm({ ...form, maxTokens: +e.target.value })
                  }
                />
              </label>
              <label>
                Timeout (s)
                <input
                  type="number"
                  min={0}
                  value={form.timeoutSeconds}
                  onChange={(e) =>
                    setForm({ ...form, timeoutSeconds: +e.target.value })
                  }
                />
              </label>
              <label>
                Currency
                <input
                  value={form.currency}
                  onChange={(e) =>
                    setForm({ ...form, currency: e.target.value })
                  }
                />
              </label>
              <label>
                Input / Mtok
                <input
                  type="number"
                  step="0.01"
                  min={0}
                  value={form.inputPrice}
                  onChange={(e) =>
                    setForm({ ...form, inputPrice: +e.target.value })
                  }
                />
              </label>
              <label>
                Cached / Mtok
                <input
                  type="number"
                  step="0.01"
                  min={0}
                  value={form.cachedInputPrice}
                  onChange={(e) =>
                    setForm({ ...form, cachedInputPrice: +e.target.value })
                  }
                />
              </label>
              <label>
                Output / Mtok
                <input
                  type="number"
                  step="0.01"
                  min={0}
                  value={form.outputPrice}
                  onChange={(e) =>
                    setForm({ ...form, outputPrice: +e.target.value })
                  }
                />
              </label>
            </div>
            <p className="muted">
              Cost is only ever computed from the rates set here. Leave them at
              zero and this page reports no cost rather than a guessed one.
            </p>
            <button type="submit" className="primary" disabled={saving}>
              {saving ? "Saving…" : "Save provider settings"}
            </button>
          </form>
        )}
      </section>
    </aside>
  );
}

// ------------------------------------------------------------------ the page

export function AICodePage({
  onView,
  onAsk,
  repo,
}: {
  onView: (v: TextView) => void;
  onAsk: (a: Confirmation) => void;
  repo?: { branch: string; detached: boolean; tag: string; dirty: boolean };
}) {
  const [error, setError] = useState("");
  const onError = useCallback((m: string) => setError(m), []);
  const chat = useAIChat(onError);
  const workspace = useWorkspaceFiles(onError);
  const [selected, setSelected] = useState<string[]>([]);
  const [includeDiff, setIncludeDiff] = useState(false);
  const [search, setSearch] = useState("");
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);
  const [saving, setSaving] = useState(false);
  const [target, setTarget] = useState("busybox");
  const [lastSent, setLastSent] = useState("");
  const transcript = useRef<HTMLDivElement>(null);
  const reload = chat.reload;
  const onBuildFinished = useCallback(() => {
    reload();
  }, [reload]);
  const build = useAIBuild(onError, onBuildFinished);

  const state = chat.state;
  const adopt = build.adopt;
  const adopted = useRef("");
  useEffect(() => {
    // Pick the build this conversation started back up after a reload.
    const id = state?.build?.jobId;
    if (id && adopted.current !== id) {
      adopted.current = id;
      adopt(id);
    }
  }, [state?.build?.jobId, adopt]);

  useEffect(() => {
    if (transcript.current)
      transcript.current.scrollTop = transcript.current.scrollHeight;
  }, [state?.session.messages.length, chat.streaming]);

  const toggle = (p: string) =>
    setSelected((old) =>
      old.includes(p)
        ? old.filter((x) => x !== p)
        : old.length >= maxContextFiles
          ? old
          : [...old, p],
    );

  // What the composer will cost, before it is sent. This page's own count, so
  // it is labelled estimated everywhere it is shown.
  const contextTokens =
    estimateTokens(prompt) + selected.length * 400 + (includeDiff ? 600 : 0);

  async function send(text: string, buildId = "") {
    const instruction = text.trim();
    if (!instruction || busy || chat.busy) return;
    setError("");
    setLastSent(instruction);
    setPrompt("");
    await chat.send({
      prompt: instruction,
      files: selected,
      includeDiff,
      buildId,
      search: search.trim(),
    });
  }

  async function act(fn: () => Promise<void>) {
    setBusy(true);
    setError("");
    try {
      await fn();
      await chat.reload();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  if (!state)
    return (
      <section className="box">
        <h1>AI Code Assistant</h1>
        <p className="muted">Loading…</p>
      </section>
    );

  const patch = state.patch;
  const generating = chat.busy || state.generating;

  return (
    <div className="ai-page">
      <ContextPanel
        repo={repo}
        {...workspace}
        selected={selected}
        toggle={toggle}
        clear={() => setSelected([])}
        includeDiff={includeDiff}
        setIncludeDiff={setIncludeDiff}
        search={search}
        setSearch={setSearch}
        contextTokens={contextTokens}
        onView={onView}
      />
      <div className="ai-main">
        <section className="box ai-chat">
          <header className="ai-head">
            <div>
              <h1>AI Code Assistant</h1>
              <p className="muted">
                Describe a problem, review the change it proposes, build it, and
                hand the compiler errors back.
              </p>
            </div>
            <button
              type="button"
              className="quiet"
              disabled={generating}
              onClick={() =>
                onAsk({
                  title: "Start a new conversation?",
                  body: "The current conversation is kept in the usage history, but the assistant will no longer see any of it.",
                  confirm: "Start a new conversation",
                  onConfirm: () =>
                    act(async () => {
                      await api("/ai/session", "POST", {});
                    }),
                })
              }
            >
              New conversation
            </button>
          </header>

          {!state.ready && (
            <div className="alert presence" role="status">
              <span>{state.message}</span>
            </div>
          )}
          {error && (
            <div className="alert" role="alert">
              {error}
              <button onClick={() => setError("")} aria-label="Dismiss error">
                ×
              </button>
            </div>
          )}

          <div className="ai-transcript" ref={transcript}>
            {!state.session.messages.length && !chat.streaming && (
              <p className="muted">
                Nothing yet. Tick the files the assistant should read, then
                describe what is wrong — for example, “the network
                initialisation sometimes crashes; find the bug and propose a
                fix”.
              </p>
            )}
            {state.session.messages.map((m, i) => (
              <AIMessage
                key={i}
                message={m}
                onRetry={
                  m.role === "assistant" &&
                  i === state.session.messages.length - 1 &&
                  lastSent &&
                  !generating
                    ? () => send(lastSent)
                    : undefined
                }
              />
            ))}
            {chat.streaming && (
              <article className="ai-message theirs">
                <header>
                  <strong>Assistant</strong>
                  <span className="muted">generating…</span>
                </header>
                <AIMessageBody text={chat.streaming} />
              </article>
            )}
            {generating && !chat.streaming && (
              <p className="muted">Waiting for the first tokens…</p>
            )}
          </div>

          {patch && (
            <PatchReview
              patch={patch}
              busy={busy || generating}
              onView={onView}
              onApply={() =>
                onAsk({
                  title: `Apply ${patch.files.length} file${patch.files.length > 1 ? "s" : ""} to the checkout?`,
                  body: "The files are written into the working tree as uncommitted changes. Nothing is committed, and this page can undo it.",
                  confirm: "Apply changes",
                  onConfirm: () =>
                    act(async () => {
                      await api("/ai/patch/apply", "POST", { id: patch.id });
                    }),
                })
              }
              onReject={() =>
                onAsk({
                  title: patch.applied
                    ? "Undo these changes?"
                    : "Reject this proposal?",
                  body: patch.applied
                    ? "Every file in this proposal goes back to the content it had before it was applied."
                    : "The proposal is discarded. The conversation is kept, so you can ask for a different approach.",
                  confirm: patch.applied ? "Undo changes" : "Reject",
                  danger: true,
                  onConfirm: () =>
                    act(async () => {
                      await api("/ai/patch/reject", "POST", {});
                    }),
                })
              }
              onRevise={() =>
                setPrompt(
                  "That change is not right. Revise it — explain what you are changing and why, then emit the corrected files.",
                )
              }
            />
          )}

          <BuildOutput
            targets={state.targets}
            target={target}
            setTarget={setTarget}
            build={build}
            busy={busy || generating}
            onView={onView}
            onAskAI={(errors) => {
              setPrompt("");
              send(
                "The build failed. Work out the cause from the compiler output and propose a fix.",
                errors.jobId,
              );
            }}
          />

          <div className="ai-composer">
            <div className="ai-actions">
              {state.quickActions.map((q) => (
                <button
                  type="button"
                  key={q.ID}
                  className="quiet"
                  title="Prepares an instruction; nothing is sent until you press Send"
                  onClick={() => setPrompt(q.Prompt)}
                >
                  {q.Label}
                </button>
              ))}
            </div>
            <textarea
              value={prompt}
              rows={4}
              placeholder="Describe the bug, or what you want improved…"
              disabled={!state.ready}
              onChange={(e) => setPrompt(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) send(prompt);
              }}
            />
            <div className="ai-composer-actions">
              <span className="muted">
                {selected.length} file{selected.length === 1 ? "" : "s"} in
                context · about {contextTokens.toLocaleString()} tokens
                (estimated) · Ctrl+Enter sends
              </span>
              {generating ? (
                <button type="button" className="danger" onClick={chat.stop}>
                  Stop generation
                </button>
              ) : (
                <button
                  type="button"
                  className="primary"
                  disabled={!state.ready || !prompt.trim()}
                  onClick={() => send(prompt)}
                >
                  Send
                </button>
              )}
            </div>
          </div>
        </section>
      </div>
      <UsagePanel
        state={state}
        usage={chat.lastUsage}
        contextNote={chat.contextNote}
        saving={saving}
        onSave={(s, apiKey) => {
          setSaving(true);
          act(async () => {
            await api("/ai/config", "PUT", { ...s, apiKey });
          }).finally(() => setSaving(false));
        }}
      />
    </div>
  );
}
