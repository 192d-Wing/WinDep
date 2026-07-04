import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// base './' so the built assets resolve regardless of mount path.
// dev proxy sends /api to a locally-running windep-admin backend.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: { outDir: "dist" },
  server: {
    proxy: {
      "/api": { target: "https://localhost:8443", secure: false, changeOrigin: true },
    },
  },
});
