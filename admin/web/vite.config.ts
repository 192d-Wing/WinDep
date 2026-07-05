import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { version } from "./package.json";

// base './' so the built assets resolve regardless of mount path.
// dev proxy sends /api to a locally-running windep-admin backend.
export default defineConfig({
  plugins: [react()],
  base: "./",
  // Bake the package.json version into the bundle so the UI can show what it's running.
  define: { __APP_VERSION__: JSON.stringify(version) },
  build: { outDir: "dist" },
  server: {
    proxy: {
      "/api": { target: "https://localhost:8443", secure: false, changeOrigin: true },
    },
  },
});
