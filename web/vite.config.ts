import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/static/dist",
    emptyOutDir: false
  },
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8080"
    }
  }
});
