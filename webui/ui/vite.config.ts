import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
export default defineConfig({
  plugins: [react()],
  // Match Vite 7 browser targets; noVNC requires top-level await.
  build: { target: ["chrome107", "edge107", "firefox104", "safari16"], outDir: "../internal/assets/dist", emptyOutDir: true },
  server: { proxy: { "/api": "http://127.0.0.1:8080" } },
});
