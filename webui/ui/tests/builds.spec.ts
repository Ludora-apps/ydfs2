import { test, expect, Page } from "@playwright/test";
async function fixture(
  page: Page,
  scenario: "success" | "cancel" | "failure" = "success",
) {
  let jobs: any[] = [];
  let profiles: any[] = [];
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
        docker: true,
        ready: true,
        freeBytes: 240 * 2 ** 30,
        minFreeBytes: 10 * 2 ** 30,
        message: "",
        development: false,
        flathub: {
          source: "built-in",
          apps: [
            { id: "org.videolan.VLC", name: "VLC", summary: "Media player" },
            {
              id: "com.github.tchx84.Flatseal",
              name: "Flatseal",
              summary: "Manage Flatpak permissions",
            },
          ],
        },
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
        },
      ];
      return json(jobs[0], 202);
    }
    if (path === "/api/jobs") return json(jobs);
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
    if (path.endsWith("/log"))
      return route.fulfill({
        contentType: "text/plain",
        headers: { "Content-Disposition": 'attachment; filename="build.log"' },
        body: "complete build log",
      });
    if (path.includes("/artifacts/"))
      return route.fulfill({
        contentType: "application/octet-stream",
        headers: {
          "Content-Disposition": 'attachment; filename="linuxconsole.iso"',
        },
        body: "ISO fixture",
      });
    return json({ error: "not found" }, 404);
  });
  await page.goto("/");
  await expect(page.getByText("Build host ready")).toBeVisible();
}
test("profile to build, live log, completion and downloads", async ({
  page,
}) => {
  await fixture(page);
  await page.getByPlaceholder("Profile name").fill("Daily ISO");
  await page.getByRole("button", { name: "Save current settings" }).click();
  await expect(page.getByText("Profile saved.")).toBeVisible();
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(page.getByText("succeeded", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Build log")).toBeHidden();
  await page.getByRole("button", { name: "Open log", exact: true }).click();
  await expect(page.getByLabel("Build log")).toContainText("g++ -Wall");
  await page.getByLabel("Filter log").fill("g++");
  await expect(page.getByLabel("Build log")).not.toContainText("Compiling");
  const download = page.waitForEvent("download");
  await page.getByRole("link", { name: "Download log" }).click();
  expect((await download).suggestedFilename()).toBe("build.log");
  await page.getByRole("button", { name: "Close log", exact: true }).click();
  await expect(page.getByLabel("Build log")).toBeHidden();
  await expect(page.getByRole("button", { name: "Open log", exact: true })).toHaveAttribute("aria-expanded", "false");
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
  await expect(page.getByText("cancelled", { exact: true })).toBeVisible();
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
test("Flathub applications are selected into the build", async ({ page }) => {
  await fixture(page);
  const picker = page.getByRole("group", { name: "Flathub applications" });
  await expect(picker).toBeVisible();
  await expect(picker).toContainText("0 selected");
  await page.getByRole("checkbox", { name: "VLC" }).check();
  await expect(picker).toContainText("1 selected");
  // Only an ISO can carry applications.
  await page.getByLabel("Build target").selectOption("busybox");
  await expect(picker).toBeHidden();
  await page.getByLabel("Build target").selectOption("fast-iso");
  await expect(page.getByRole("checkbox", { name: "VLC" })).toBeChecked();
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(page.getByText("succeeded", { exact: true })).toBeVisible();
  await expect(page.getByText("1 Flathub app(s)")).toBeVisible();
});
test("mobile layout has no horizontal overflow", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await fixture(page);
  await page.getByRole("button", { name: "Queue build" }).click();
  await expect(page.getByText("succeeded", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});
