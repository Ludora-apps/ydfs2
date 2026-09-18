// Apply the saved choice before the page paints; dark is the first-visit default.
(() => {
  let theme = "dark";
  try {
    if (localStorage.getItem("ydfs-theme") === "light") theme = "light";
  } catch {
    // Storage may be disabled; the switch still works for this page.
  }
  document.documentElement.dataset.theme = theme;
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute("content", theme === "dark" ? "#0c141d" : "#f2f5f7");
})();
