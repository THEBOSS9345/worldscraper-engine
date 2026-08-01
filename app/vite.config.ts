import { defineConfig } from "vite";

export default defineConfig({
  clearScreen: false,
  server: {
    port: 5173,
    strictPort: true,
    watch: {
      // Cargo rewrites target/ constantly and locks the DLL it is linking,
      // which makes the dev watcher die with EBUSY on Windows.
      ignored: ["**/src-tauri/**"],
    },
  },
  build: {
    // Matches the WebView2 / modern Chromium runtime Tauri ships against.
    target: "chrome110",
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
