import { test, expect, Page } from "@playwright/test";
async function fixture(
  page: Page,
  scenario: "success" | "cancel" | "failure" = "success",
) {
  let jobs: any[] = [];
  let profiles: any[] = [];
  let logs = true;
  // Who else has the page open; the server reports this with capabilities.
  const others: string[] = [];
  const commit = (revision: string, subject: string) => ({
    revision,
    subject,
    author: "tester",
    date: "2026-09-18T10:00:00Z",
  });
  const repository = {
    url: "https://github.com/linuxconsole-org/ydfs2",
    branch: "2.12",
    detached: false,
    tag: "2026",
    dirty: false,
    local: commit("a".repeat(40), "local head"),
    ahead: 0,
    behind: 0,
    fastForward: true,
    sources: [
      {
        name: "local",
        label: "This checkout",
        url: "",
        selected: "2.12",
        branches: [
          { name: "2.12", ref: "2.12", current: true, buildable: true },
        ],
        commits: [commit("a".repeat(40), "local head")],
      },
      {
        name: "origin",
        label: "Ludora-apps/ydfs2",
        url: "git@github.com:Ludora-apps/ydfs2.git",
        selected: "origin/2.12",
        branches: [
          { name: "2.12", ref: "origin/2.12", current: false, buildable: true },
          { name: "3.0", ref: "origin/3.0", current: false, buildable: false },
        ],
        commits: [
          commit("b".repeat(40), "fork commit"),
          commit("c".repeat(40), "older fork commit"),
        ],
      },
      {
        name: "upstream",
        label: "linuxconsole-org/ydfs2",
        url: "https://github.com/linuxconsole-org/ydfs2",
        selected: "upstream/2.12",
        branches: [
          {
            name: "2.12",
            ref: "upstream/2.12",
            current: false,
            buildable: true,
          },
        ],
        commits: [commit("d".repeat(40), "upstream commit")],
      },
    ],
  };
  await page.route("**/api/**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    const method = req.method();
    const json = (v: unknown, status = 200) =>
      route.fulfill({
        status,
        contentType: "application/json",
        body: JSON.stringify(v),
      });
    if (path === "/api/capabilities")
      return json({
        targets: [
          "fast-iso",
          "full-iso",
          "kernel",
          "busybox",
          "initramfs",
          "updates",
          "mate",
          "kde",
          "cinnamon",
        ],
        user: "alice",
        others,
        docker: true,
        ready: true,
        freeBytes: 240 * 2 ** 30,
        minFreeBytes: 10 * 2 ** 30,
        message: "",
        development: false,
        vm: false,
        vmMessage: "No /dev/kvm on this host",
      });
    if (path === "/api/vm")
      return json({ state: "stopped", viewers: 0, idleFor: 0 });
    if (path === "/api/flathub")
      return json({
        source: "built-in",
        apps: [
          { id: "org.videolan.VLC", name: "VLC", summary: "Media player" },
          {
            id: "com.github.tchx84.Flatseal",
            name: "Flatseal",
            summary: "Manage Flatpak permissions",
          },
        ],
      });
    if (path === "/api/flathub/refresh")
      return json({
        source: "flathub",
        checkedAt: "2026-09-05T12:00:00Z",
        apps: [
          { id: "org.videolan.VLC", name: "VLC", summary: "Media player" },
          {
            id: "com.github.tchx84.Flatseal",
            name: "Flatseal",
            summary: "Manage Flatpak permissions",
          },
        ],
      });
    if (path === "/api/profiles") {
      if (method === "PUT") {
        profiles = [req.postDataJSON()];
        return json(profiles[0]);
      }
      return json(profiles);
    }
    if (path === "/api/jobs" && method === "POST") {
      jobs = [
        {
          id: "build-01",
          state: scenario === "cancel" ? "running" : "queued",
          user: "alice",
          settings: req.postDataJSON(),
          created: "2026-09-05T12:00:00Z",
          revision: "acb125cdb721",
          tag: "v2.12.1",
          container: "fixture",
          artifacts: [],
          favorite: false,
        },
      ];
      return json(jobs[0], 202);
    }
    // Order matters: the log routes are more specific than the job routes.
    if (path === "/api/logs")
      return json(
        jobs.map((j: any) => ({
          id: j.id,
          target: j.settings.target,
          state: j.state,
          created: j.created,
          user: j.user,
          size: 4096,
          present: logs,
        })),
      );
    if (path.endsWith("/log")) {
      if (method === "DELETE") {
        logs = false;
        return json({ ok: true });
      }
      // Mirrors downloadLog, which serves the log as an attachment.
      return route.fulfill({
        status: 200,
        contentType: "text/plain",
        headers: { "content-disposition": 'attachment; filename="build.log"' },
        body: "g++ -Wall fixture.cpp -o fixture\nCompiling LinuxConsole…\nlinking fixture\n",
      });
    }
    if (path.startsWith("/api/jobs/") && method === "DELETE") {
      jobs = [];
      return json({ ok: true });
    }
    if (path === "/api/jobs") return json(jobs);
    if (path.endsWith("/favorite")) {
      jobs[0].favorite = method === "POST";
      return json(jobs[0]);
    }
    if (path.endsWith("/config"))
      return route.fulfill({
        status: 200,
        contentType: "text/plain",
        body: 'ARCH=x86_64\nKERNEL3=6.18.29\nDISTRONAME=linuxconsole\nMODULES="x86_64 mate-x86_64"\nBUILDYDFS=fast\n',
      });
    if (path.endsWith("/cancel")) {
      jobs[0].state = "cancelled";
      return json(jobs[0]);
    }
    if (path.endsWith("/events")) {
      let body =
        'id: 44\nevent: log\ndata: "g++ -Wall fixture.cpp -o fixture\\nCompiling LinuxConsole…\\n"\n\n';
      if (scenario !== "cancel") {
        jobs[0].state = scenario === "failure" ? "failed" : "succeeded";
        jobs[0].exitCode = scenario === "failure" ? 2 : 0;
        jobs[0].finished = "2026-09-05T12:10:00Z";
        if (scenario === "success")
          jobs[0].artifacts = [{ name: "linuxconsole.iso", size: 2 ** 30 }];
        else jobs[0].error = "Build exited with code 2.";
        body += "event: done\ndata: " + JSON.stringify(jobs[0]) + "\n\n";
      }
      return route.fulfill({ contentType: "text/event-stream", body });
    }
    if (path.includes("/artifacts/"))
      return route.fulfill({
        contentType: "application/octet-stream",
        headers: {
          "Content-Disposition": 'attachment; filename="linuxconsole.iso"',
        },
        body: "ISO fixture",
      });
    if (path.startsWith("/api/repository")) {
      if (path === "/api/repository/commits") {
        const ref = new URL(req.url()).searchParams.get("ref");
        const known = repository.sources.find((s) => s.selected === ref);
        return json({
          selected: ref,
          commits: known
            ? known.commits
            : [commit("e".repeat(40), "rewritten layout")],
        });
      }
      return json(repository);
    }
    return json({ error: "not found" }, 404);
  });
  await page.goto("/");
  // The menu decides what the content area shows; the repository screen is
  // first, and every build assertion below lives on "New build".
  await expect(page.getByRole("heading", { name: "Repository" })).toBeVisible();
  await open(page, "New build");
  await expect(page.getByText("Build host ready")).toBeVisible();
  return others;
}
// Click one entry of the left-hand menu.
async function open(page: Page, label: string) {
  await page
    .getByRole("navigation", { name: "Workspace sections" })
    .getByRole("button", { name: label })
    .click();
}
test("profile to build, live log, completion and downloads", async ({
  page,
}) => {
  await fixture(page);
  await open(page, "Saved profiles");
  await page.getByPlaceholder("Profile name").fill("Daily ISO");
  await page.getByRole("button", { name: "Save current settings" }).click();
  await expect(page.getByText("Profile saved.")).toBeVisible();
  await open(page, "New build");
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("Build log")).toBeHidden();
  await page.getByRole("button", { name: "Open log", exact: true }).click();
  await expect(page.getByLabel("Build log")).toContainText("g++ -Wall");
  await page.getByLabel("Filter log").fill("g++");
  await expect(page.getByLabel("Build log")).not.toContainText("Compiling");
  const download = page.waitForEvent("download");
  await page
    .locator("section.details")
    .getByRole("link", { name: "Download log" })
    .click();
  expect((await download).suggestedFilename()).toBe("build.log");
  await page.getByRole("button", { name: "Close log", exact: true }).click();
  await expect(page.getByLabel("Build log")).toBeHidden();
  await expect(
    page.getByRole("button", { name: "Open log", exact: true }),
  ).toHaveAttribute("aria-expanded", "false");
  await expect(
    page.getByRole("link", { name: /linuxconsole.iso/ }),
  ).toBeVisible();
  await page.screenshot({ path: "/tmp/ydfs-web-ui.png", fullPage: true });
});
test("running build can be cancelled", async ({ page }) => {
  await fixture(page, "cancel");
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.getByRole("button", { name: "Cancel build", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Cancel build", exact: true }).click();
  await expect(
    page.locator("section.history").getByText("cancelled", { exact: true }),
  ).toBeVisible();
});
test("failed builds show exit status and logs", async ({ page }) => {
  await fixture(page, "failure");
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(page.getByText("Build exited with code 2.")).toBeVisible();
  await expect(page.getByLabel("Build log")).toBeHidden();
  await page.getByRole("button", { name: "Open log", exact: true }).click();
  await expect(page.getByLabel("Build log")).toContainText("g++");
  await expect(
    page.getByRole("link", { name: /linuxconsole.iso/ }),
  ).toHaveCount(0);
});
test("Flathub applications are selected on their own screen", async ({
  page,
}) => {
  await fixture(page);
  const summary = page.locator(".flatpak-summary");
  await expect(summary).toContainText("0 Flathub applications");
  await open(page, "Flatpak");
  const box = page.locator("section.flathub-box");
  await expect(box).toContainText("0 / 40 selected");
  // Every application is listed and searchable, not just the first few.
  await page.getByLabel("Search applications").fill("permissions");
  await expect(page.getByRole("checkbox", { name: "VLC" })).toHaveCount(0);
  await page.getByLabel("Search applications").fill("");
  await page.getByRole("checkbox", { name: "VLC" }).check();
  await expect(box).toContainText("1 / 40 selected");
  await expect(box.getByRole("button", { name: "Remove VLC" })).toBeVisible();

  await open(page, "New build");
  await expect(summary).toContainText("1 Flathub application");
  await expect(summary).toContainText("VLC");
  // Only an ISO can carry applications.
  await page.getByLabel("Build target").selectOption("busybox");
  await expect(summary).toBeHidden();
  await page.getByLabel("Build target").selectOption("fast-iso");
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("1 Flathub app(s)")).toBeVisible();
});
test("the menu switches screens and survives a reload", async ({ page }) => {
  await fixture(page);
  await open(page, "Logs");
  await expect(page.locator("section.logs-box")).toBeVisible();
  await expect(page.locator("section.history")).toHaveCount(0);
  expect(new URL(page.url()).hash).toBe("#/logs");
  await page.reload();
  await expect(page.locator("section.logs-box")).toBeVisible();
  // The entry for the screen being shown is the current one.
  await expect(
    page.getByRole("navigation").getByRole("button", { name: "Logs" }),
  ).toHaveAttribute("aria-current", "page");
  await open(page, "Launch");
  await expect(page.locator("section.launch")).toContainText(
    "No /dev/kvm on this host",
  );
});
test("a succeeded build can be kept and released", async ({ page }) => {
  await fixture(page);
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();
  const box = page.locator("section.favorites");
  await open(page, "Kept builds");
  await expect(box).toContainText("Nothing is pinned");
  // Pin it: the kept-builds screen then offers the ISO and archived config.
  await open(page, "New build");
  await page
    .getByRole("button", { name: "Keep", exact: false })
    .first()
    .click();
  await open(page, "Kept builds");
  await expect(
    box.getByRole("link", { name: /linuxconsole.iso/ }),
  ).toBeVisible();
  await expect(box.getByRole("button", { name: "config.ini" })).toBeVisible();
  await expect(box.getByRole("button", { name: "Delete" })).toBeVisible();
  // Releasing it empties the screen again.
  await box.getByRole("button", { name: "Release" }).click();
  await expect(box).toContainText("Nothing is pinned");
});
test("destructive actions confirm in a modal, never a native dialog", async ({
  page,
}) => {
  // A native confirm() would fire this and auto-dismiss; nothing must.
  let native = 0;
  page.on("dialog", (d) => {
    native++;
    d.dismiss();
  });
  await fixture(page);
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();

  const modal = page.getByRole("dialog");
  await expect(modal).toBeHidden();
  await page.getByRole("button", { name: "Delete build" }).click();
  await expect(modal).toBeVisible();
  await expect(modal).toContainText("logs, source snapshot, and artifacts");

  // Escape dismisses, and the build survives.
  await page.keyboard.press("Escape");
  await expect(modal).toBeHidden();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();

  // Cancel dismisses too, and holds focus for a destructive action.
  await page.getByRole("button", { name: "Delete build" }).click();
  await expect(modal).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel" })).toBeFocused();
  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(modal).toBeHidden();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();

  // Confirming actually deletes.
  await page.getByRole("button", { name: "Delete build" }).click();
  await modal.getByRole("button", { name: "Delete build" }).click();
  await expect(modal).toBeHidden();
  await expect(page.getByText("Your first build starts here")).toBeVisible();
  expect(native).toBe(0);
});
test("build logs are listed, searchable with highlight, and deletable", async ({
  page,
}) => {
  await fixture(page);
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();

  await open(page, "Logs");
  const box = page.locator("section.logs-box");
  await expect(box.getByRole("heading", { name: "Build logs" })).toBeVisible();
  const row = box.locator(".log-row");
  await expect(row).toHaveCount(1);
  await expect(row.getByRole("link")).toHaveAttribute(
    "href",
    "/api/jobs/build-01/log",
  );

  // Opening a listed log gives the whole file in a full-page reader.
  await box.locator(".log-open").click();
  const viewer = page.getByRole("dialog");
  await expect(viewer).toBeVisible();
  await expect(page.getByLabel("File contents")).toContainText(
    "linking fixture",
  );

  // Under two characters it refuses to search rather than matching everything.
  await page.getByLabel("Search text").fill("g");
  await expect(viewer).toContainText("2+ characters");

  // A real search highlights every match and steps through them.
  await page.getByLabel("Search text").fill("fixture");
  await expect(viewer).toContainText("1 / 3");
  await expect(page.locator("mark")).toHaveCount(3);
  await expect(page.locator("mark.current")).toHaveCount(1);
  await page.getByRole("button", { name: "Next match" }).click();
  await expect(viewer).toContainText("2 / 3");
  // And wraps around.
  await page.getByRole("button", { name: "Previous match" }).click();
  await page.getByRole("button", { name: "Previous match" }).click();
  await expect(viewer).toContainText("3 / 3");

  await page.getByLabel("Search text").fill("nothing-matches-this");
  await expect(viewer).toContainText("No match");
  await expect(page.locator("mark")).toHaveCount(0);

  // Deleting the log confirms in a modal, keeps the build, and empties the box.
  await viewer.getByRole("button", { name: "Delete log" }).click();
  await expect(page.getByText("only the log file is removed")).toBeVisible();
  await page.getByRole("button", { name: "Delete log" }).last().click();
  await expect(box.locator(".log-row")).toHaveCount(0);
  await open(page, "Build activity");
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();
});
test("the as-built config.ini opens in a modal, not a browser tab", async ({
  page,
}) => {
  await fixture(page);
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();

  const details = page.locator("section.details");
  const opener = details.getByRole("button", { name: /config.ini/ });
  // A button, not a link: nothing here should navigate away or open a tab.
  await expect(opener).toBeVisible();
  await expect(details.getByRole("link", { name: /config.ini/ })).toHaveCount(
    0,
  );

  await opener.click();
  const viewer = page.getByRole("dialog");
  await expect(viewer).toBeVisible();
  await expect(viewer).toContainText("config.ini");
  await expect(viewer).toContainText("as built");
  await expect(page.getByLabel("File contents")).toContainText(
    'MODULES="x86_64 mate-x86_64"',
  );
  // Sized to its content rather than filling the viewport like a log. A modal
  // <dialog> is fixed with inset 0, so `height:auto` would stretch to the full
  // screen; this guards that regression and the overflow that follows it.
  await expect(page.locator("dialog.logviewer.compact")).toHaveCount(1);
  const size = await page.evaluate(() => {
    const d = document.querySelector("dialog.logviewer") as HTMLElement;
    const t = d.querySelector(".logviewer-text") as HTMLElement;
    return {
      dialog: d.getBoundingClientRect().height,
      viewport: window.innerHeight,
      overflows: t.scrollHeight > t.clientHeight,
    };
  });
  expect(size.dialog).toBeLessThan(size.viewport * 0.75);
  expect(size.overflows).toBe(false);
  // It is a config, so there is nothing to delete from here.
  await expect(viewer.getByRole("button", { name: /Delete/ })).toHaveCount(0);
  await expect(viewer.getByRole("link", { name: "Download" })).toHaveAttribute(
    "href",
    "/api/jobs/build-01/config",
  );

  // Search works here too.
  await page.getByLabel("Search text").fill("x86_64");
  await expect(viewer).toContainText("1 / 3");
  await expect(page.locator("mark")).toHaveCount(3);

  await page.keyboard.press("Escape");
  await expect(viewer).toBeHidden();
});
test("mobile layout has no horizontal overflow", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await fixture(page);
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(
    page.locator("section.history").getByText("succeeded", { exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

// Every repository the checkout can be moved to gets a box of its own, and a
// switch is confirmed through the shared dialog — never window.confirm.
test("repository boxes list commits and confirm a checkout", async ({
  page,
}) => {
  await fixture(page);
  page.on("dialog", () => {
    throw new Error("the app opened a native browser dialog");
  });
  let checkouts = 0;
  await page.route("**/api/repository/checkout", (route) => {
    checkouts++;
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        url: "https://github.com/linuxconsole-org/ydfs2",
        branch: "2.12",
        detached: true,
        tag: "2026",
        dirty: false,
        local: {
          revision: "b".repeat(40),
          subject: "fork commit",
          author: "tester",
          date: "2026-09-18T10:00:00Z",
        },
        ahead: 0,
        behind: 0,
        fastForward: true,
        sources: [],
      }),
    });
  });
  await open(page, "Repository");
  const fork = page.locator(".repo-source", { hasText: "Ludora-apps/ydfs2" });
  await expect(fork.locator(".commit-list li")).toHaveCount(2);
  await expect(fork.getByText("fork commit", { exact: true })).toBeVisible();
  await expect(
    page.locator(".repo-source", { hasText: "linuxconsole-org/ydfs2" }),
  ).toContainText("upstream commit");
  // A branch whose tree has no 2.12/ is readable but not buildable.
  await fork.getByRole("combobox").selectOption("origin/3.0");
  await expect(fork.locator(".commit-list")).toContainText("rewritten layout");
  await expect(fork.getByRole("button", { name: "Checkout" })).toBeDisabled();
  // Switching branches asks first, and Cancel sends nothing.
  await fork.getByRole("combobox").selectOption("origin/2.12");
  await fork.getByRole("button", { name: "Checkout" }).click();
  await expect(page.getByRole("dialog")).toContainText("Switch to origin/2.12");
  await page.getByRole("button", { name: "Cancel" }).click();
  expect(checkouts).toBe(0);
  // A bare commit warns that the checkout ends up detached.
  await fork
    .getByRole("button", { name: `Check out ${"b".repeat(12)}` })
    .click();
  await expect(page.getByRole("dialog")).toContainText("detached HEAD");
  await page.getByRole("button", { name: "Check out", exact: true }).click();
  await expect(page.getByText("Checkout detached at")).toBeVisible();
  expect(checkouts).toBe(1);
});

// The merge resolution screen: a conflicting merge is prepared, its files are
// resolved one at a time, and only then can it be applied.
test("a conflicting merge is resolved before it can be applied", async ({
  page,
}) => {
  await fixture(page);
  const commit = (revision: string, subject: string) => ({
    revision,
    subject,
    author: "tester",
    date: "2026-09-18T10:00:00Z",
  });
  const github = {
    available: true,
    origin: "Ludora-apps/ydfs2",
    upstream: "linuxconsole-org/ydfs2",
    base: "2.12",
  };
  const conflict = (resolved: string) => ({
    path: "2.12/packages/list-x86_64",
    kind: "content",
    ours: true,
    theirs: true,
    binary: false,
    resolved,
  });
  const graft = (resolved: string) => ({
    kind: "merge",
    base: "a".repeat(40),
    branch: "2.12",
    revision: "d".repeat(40),
    subject: "upstream commit",
    clean: false,
    files: [conflict(resolved)],
    oursLabel: "This checkout",
    theirsLabel: "Upstream",
    openedAt: "2026-09-19T10:00:00Z",
  });
  const state = (g?: unknown) => ({
    url: "https://github.com/linuxconsole-org/ydfs2",
    branch: "2.12",
    detached: false,
    tag: "2026",
    dirty: false,
    local: commit("a".repeat(40), "local head"),
    upstream: commit("d".repeat(40), "upstream commit"),
    ahead: 1,
    behind: 1,
    fastForward: false,
    github,
    graft: g,
    sources: [
      {
        name: "local",
        label: "This checkout",
        url: "",
        selected: "2.12",
        branches: [
          { name: "2.12", ref: "2.12", current: true, buildable: true },
        ],
        commits: [commit("a".repeat(40), "local head")],
      },
    ],
  });
  let resolved = "";
  let applied = 0;
  const json = (route: any, v: unknown) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(v),
    });
  await page.route("**/api/repository**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/repository/merge/preview")
      return json(route, state(graft("")));
    if (path === "/api/repository/graft/resolve") {
      resolved = (route.request().postDataJSON() as any).choice;
      return json(route, state(graft(resolved)));
    }
    if (path === "/api/repository/graft/apply") {
      applied++;
      return json(route, state(undefined));
    }
    return json(route, state(resolved && applied ? undefined : undefined));
  });
  // The app read the repository when it mounted; reload so it reads the state
  // these routes describe.
  await page.reload();
  await open(page, "Repository");
  // Nothing is being resolved yet.
  await expect(page.locator(".graft")).toHaveCount(0);
  await page.getByRole("button", { name: "Check merge" }).click();
  const panel = page.locator(".graft");
  await expect(panel).toContainText("Merge being prepared");
  await expect(panel).toContainText("2.12/packages/list-x86_64");
  await expect(panel).toContainText("changed on both sides");
  await expect(panel).toContainText("1 of 1 file(s) left to resolve");
  // Nothing has moved in the checkout, and it cannot be applied yet.
  await expect(
    panel.getByRole("button", { name: "Apply merge" }),
  ).toBeDisabled();
  // While a resolution is open the boxes below must not move the checkout.
  await expect(
    page.locator(".repo-source").getByRole("button", { name: "Checkout" }),
  ).toBeDisabled();
  await panel.getByRole("button", { name: "Keep Upstream" }).click();
  expect(resolved).toBe("theirs");
  await expect(panel).toContainText("kept theirs");
  const apply = panel.getByRole("button", { name: "Apply merge" });
  await expect(apply).toBeEnabled();
  await apply.click();
  await expect(page.locator(".graft")).toHaveCount(0);
  expect(applied).toBe(1);
});

// A commit goes upstream as itself or with its history; both answers live in
// the one confirmation dialog, never in a native browser prompt.
test("a listed commit can be proposed upstream two ways", async ({ page }) => {
  await fixture(page);
  const commit = (revision: string, subject: string) => ({
    revision,
    subject,
    author: "tester",
    date: "2026-09-18T10:00:00Z",
  });
  const state = {
    url: "https://github.com/linuxconsole-org/ydfs2",
    branch: "2.12",
    detached: false,
    tag: "2026",
    dirty: false,
    local: commit("a".repeat(40), "local head"),
    ahead: 2,
    behind: 0,
    fastForward: true,
    github: {
      available: true,
      origin: "Ludora-apps/ydfs2",
      upstream: "linuxconsole-org/ydfs2",
      base: "2.12",
    },
    sources: [
      {
        name: "local",
        label: "This checkout",
        url: "",
        selected: "2.12",
        branches: [
          { name: "2.12", ref: "2.12", current: true, buildable: true },
        ],
        commits: [commit("b".repeat(40), "a fix worth sending")],
      },
    ],
  };
  let sent: { revision: string; mode: string } | null = null;
  await page.route("**/api/repository**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/repository/pr")
      sent = route.request().postDataJSON() as typeof sent;
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(
        path === "/api/repository/pr"
          ? { ...state, pullRequest: "https://github.com/up/ydfs2/pull/7" }
          : state,
      ),
    });
  });
  await page.reload();
  await open(page, "Repository");
  const row = page.locator(".commit-list li", {
    hasText: "a fix worth sending",
  });
  await row.getByRole("button", { name: /Propose/ }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("linuxconsole-org/ydfs2");
  await expect(dialog).toContainText("Ludora-apps/ydfs2");
  // The second answer: this commit and everything before it.
  await dialog.getByRole("button", { name: "…and those before it" }).click();
  await expect.poll(() => sent).not.toBeNull();
  expect(sent!.mode).toBe("through");
  expect(sent!.revision).toBe("b".repeat(40));
  // Where it landed is shown as a link, not swallowed by a transient notice.
  await expect(page.getByRole("link", { name: /pull\/7/ })).toBeVisible();
});

// The checkout, the profiles and the queue are shared, so a second operator
// with the page open has to be visible to the first.
test("a second operator connected is warned about", async ({ page }) => {
  const others = await fixture(page);
  await expect(page.locator(".alert.presence")).toHaveCount(0);
  others.push("yledoare");
  const banner = page.locator(".alert.presence");
  await expect(banner).toContainText("yledoare", { timeout: 15000 });
  await expect(banner).toContainText("is also connected");
  // It is not an error and carries no dismiss button: it stands until they go.
  await expect(banner.getByRole("button")).toHaveCount(0);
  // It clears on its own once they stop polling (the page polls every 5s).
  others.length = 0;
  await expect(page.locator(".alert.presence")).toHaveCount(0, {
    timeout: 15000,
  });
});
