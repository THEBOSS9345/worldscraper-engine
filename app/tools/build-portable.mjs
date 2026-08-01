// Builds a portable WorldScraper: a plain folder with two executables and no
// installer.
//
// Steps: compile the Go engine, build the front end, build the Tauri binary
// with --no-bundle (which skips the NSIS/MSI installers), then stage everything
// into dist-portable/.

import { execFileSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, rmSync, writeFileSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const appDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repoDir = resolve(appDir, "..");
const engineDir = join(repoDir, "engine");
const outDir = join(repoDir, "dist-portable");

const isWindows = process.platform === "win32";
const engineExe = isWindows ? "wsengine.exe" : "wsengine";
const appExe = isWindows ? "WorldScraper.exe" : "worldscraper";

// npm and npx are shell scripts on Windows and need a shell; real executables
// like `go` must not use one, or quoted arguments get re-split.
const run = (cmd, args, cwd, { shell = false } = {}) => {
  console.log(`\n> ${cmd} ${args.join(" ")}`);
  execFileSync(cmd, args, { cwd, stdio: "inherit", shell });
};

// 1. Crawl engine.
run("go", ["build", "-trimpath", "-ldflags=-s -w", "-o", engineExe, "./cmd/wsengine"], engineDir);

// 2. Ship the freshly built engine as a bundled resource too, so a packaged
//    build and a portable build stay in step.
const binariesDir = join(appDir, "src-tauri", "binaries");
mkdirSync(binariesDir, { recursive: true });
copyFileSync(join(engineDir, engineExe), join(binariesDir, engineExe));

// 3. Front end + Tauri binary, without any installer.
run("npm", ["run", "build"], appDir, { shell: true });
run("npx", ["tauri", "build", "--no-bundle"], appDir, { shell: true });

// 4. Stage.
const releaseDir = join(appDir, "src-tauri", "target", "release");
const builtApp = join(releaseDir, appExe);
if (!existsSync(builtApp)) {
  throw new Error(`expected ${builtApp} to exist after the build`);
}

rmSync(outDir, { recursive: true, force: true });
mkdirSync(outDir, { recursive: true });

copyFileSync(builtApp, join(outDir, appExe));
copyFileSync(join(engineDir, engineExe), join(outDir, engineExe));

writeFileSync(
  join(outDir, "README.txt"),
  [
    "WorldScraper (portable)",
    "",
    `Run ${appExe}. It starts ${engineExe} itself — keep the two files together.`,
    "",
    "Data is stored outside this folder, in:",
    isWindows ? "  %APPDATA%\\WorldScraper" : "  ~/.config/WorldScraper",
    "",
    "Optional: drop a GeoLite2-City.mmdb into that folder for real server",
    "locations on the globe.",
    "",
    "Closing the window keeps the crawl running in the tray. Quitting the app",
    "keeps the engine crawling too - it runs as its own daemon and the next",
    "launch reconnects to it. Use Control > Stop engine to halt it for good,",
    "or Start engine to bring it back.",
    "",
  ].join("\n"),
);

const mb = (p) => (statSync(p).size / (1024 * 1024)).toFixed(1);
console.log(`\nPortable build ready: ${outDir}`);
console.log(`  ${appExe}     ${mb(join(outDir, appExe))} MB`);
console.log(`  ${engineExe}  ${mb(join(outDir, engineExe))} MB`);
