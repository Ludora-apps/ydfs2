// What both screens need from the server, in one place: the fetch helper, the
// end-of-session plumbing behind it, and the two shapes the shared dialogs are
// driven by. Moved here unchanged when the AI Code Assistant arrived as a
// second page — main.tsx was the only importer before.
// The session behind this page has ended.
//
// The reverse proxy answers a *background* request with 401 rather than
// redirecting it into a GitHub login: this page polls every few seconds, and a
// login started on every tick would leave several in flight at once, where the
// first callback to complete clears the CSRF cookie out from under the rest —
// a 403 on a sign-in that was working. So the page is told plainly, stops
// polling, and offers one Sign in button that navigates the whole tab.
//
// Set by App, so every request — including the plain fetch behind TextViewer —
// reaches the same banner without each call site knowing about it.
let sessionEnded: () => void = () => {};
// App registers the banner here; see the comment above.
export function onSessionEnded(fn: () => void) {
  sessionEnded = fn;
}
// For the one request that does not go through api(): the plain fetch behind
// TextViewer, which reads text rather than JSON.
export function sessionExpired() {
  sessionEnded();
}
// Signing in has to be a navigation of the whole tab, never a fetch, and it
// carries the screen being looked at so the login lands back on it.
export function signIn() {
  const here = location.pathname + location.search + location.hash;
  location.assign(`/oauth2/start?rd=${encodeURIComponent(here)}`);
}
export async function api<T>(
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
    if (res.status === 401) sessionEnded();
    throw new Error(err.error);
  }
  return res.json();
}

export type TextView = {
  // Changing key refetches; it is what the open/close effects key on.
  key: string;
  title: string;
  subtitle: string;
  url: string;
  // Logs fill the viewport; a config.ini is a few dozen lines and should not.
  compact?: boolean;
  deleteLabel?: string;
  onDelete?: () => void;
  // A conflicting file is the one text here that is written as well as read.
  // Editing lives in this viewer rather than in a dialog of its own, so there
  // stays exactly one reader — and now writer — for every file.
  editable?: boolean;
  saveLabel?: string;
  onSave?: (text: string) => void;
};

export type Confirmation = {
  title: string;
  body: string;
  confirm: string;
  danger?: boolean;
  onConfirm: () => void;
  // A second way to say yes, for a question with two answers rather than one —
  // a pull request carrying one commit, or that commit and its history.
  alternate?: { label: string; onPick: () => void };
};
