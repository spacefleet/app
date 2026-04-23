import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// In dev, Vite runs on :5173 and proxies /api/* to the Go server on :8080.
// In prod, `vite build` writes to ./dist, which is embedded into the Go binary
// (see ./embed.go) and served from the same origin — no proxy needed.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: false,
      },
      // `/config.js` is served by the Go backend and exposes `window.appConfig`
      // (see lib/server/routes.go). Proxy it so the dev server gets real values.
      "/config.js": {
        target: "http://localhost:8080",
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
