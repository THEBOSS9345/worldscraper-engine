//! Supervision of the Go crawl engine.
//!
//! The engine is a standalone daemon: it keeps crawling whether or not this
//! shell is open. On startup this module either *adopts* a running instance
//! (located via `runtime.json` in the data directory) or launches a detached
//! one, and while the app is open a supervisor health-checks it and restarts
//! it with backoff if it ever dies. Quitting the app never stops the engine.
//!
//! The engine writes its current port and API token to `runtime.json`, which
//! is what lets a fresh shell session reconnect to a daemon that outlived the
//! previous session.

use std::io::{Read, Seek, SeekFrom, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use anyhow::{anyhow, Result};
use rand::Rng;
use serde::{Deserialize, Serialize};

#[cfg(windows)]
use std::os::windows::process::CommandExt;

/// Keeps a console window from flashing up behind the app on Windows.
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

/// Lines of engine output kept for the in-app log view.
const LOG_CAPACITY: usize = 500;
/// File the engine's stdout/stderr (and shell notices) are appended to.
const LOG_FILE: &str = "engine.log";
/// How often to poll a healthy engine.
const HEALTH_INTERVAL: Duration = Duration::from_secs(3);
/// Longest pause between restart attempts after repeated spawn failures.
const MAX_BACKOFF: Duration = Duration::from_secs(30);
/// A freshly spawned engine gets this long to come up before a new process is
/// tried; without it, a slow start would look like a crash and spawn a second
/// engine that dies on the busy port.
const STARTUP_GRACE: Duration = Duration::from_secs(10);

/// Where the engine can be reached, once it is up.
pub struct Endpoint {
    inner: RwLock<Option<(u16, String)>>,
}

impl Endpoint {
    fn new() -> Self {
        Self {
            inner: RwLock::new(None),
        }
    }

    /// Returns (port, token) when the engine is running.
    pub fn get(&self) -> Option<(u16, String)> {
        self.inner.read().ok().and_then(|g| g.clone())
    }

    fn set(&self, port: u16, token: String) {
        if let Ok(mut g) = self.inner.write() {
            *g = Some((port, token));
        }
    }
}

/// Public view of the engine's state, surfaced to the UI.
#[derive(Debug, Serialize, Clone)]
#[serde(rename_all = "camelCase")]
pub struct EngineInfo {
    pub port: u16,
    pub token: String,
    pub running: bool,
    pub restarts: u32,
    pub data_dir: String,
}

/// The handoff file the engine writes so a new shell session can reconnect.
#[derive(Debug, Deserialize)]
struct RuntimeInfo {
    // Kept for diagnostics; adoption itself relies on the health probe.
    #[allow(dead_code)]
    pid: u64,
    #[serde(default)]
    port: u16,
    #[serde(default)]
    token: String,
}

/// Owns the engine process and its supervision thread.
pub struct EngineHandle {
    pub endpoint: Arc<Endpoint>,
    pub data_dir: PathBuf,
    binary: PathBuf,
    token: String,
    port: u16,
    child: Mutex<Option<Child>>,
    running: AtomicBool,
    restarts: AtomicU32,
    shutting_down: AtomicBool,
    /// Set when the user explicitly asked for the engine to stop; the
    /// supervisor then refuses to restart it until `start_engine` is called.
    stop_requested: AtomicBool,
}

impl EngineHandle {
    /// Prepares a supervisor. If a healthy engine is already running (left by
    /// a previous session, for example) it is adopted instead of a second
    /// instance being started.
    pub fn new(binary: PathBuf, data_dir: PathBuf) -> Result<Arc<Self>> {
        std::fs::create_dir_all(&data_dir).ok();

        if let Some((port, token)) = adopt_running(&data_dir) {
            log::info!("adopted running engine on 127.0.0.1:{port}");
            let endpoint = Arc::new(Endpoint::new());
            endpoint.set(port, token.clone());
            return Ok(Arc::new(Self {
                endpoint,
                data_dir,
                binary,
                port,
                token,
                child: Mutex::new(None),
                running: AtomicBool::new(true),
                restarts: AtomicU32::new(0),
                shutting_down: AtomicBool::new(false),
                stop_requested: AtomicBool::new(false),
            }));
        }

        let port = free_port()?;
        let token = mint_token();
        Ok(Arc::new(Self {
            endpoint: Arc::new(Endpoint::new()),
            data_dir,
            binary,
            port,
            token,
            child: Mutex::new(None),
            running: AtomicBool::new(false),
            restarts: AtomicU32::new(0),
            shutting_down: AtomicBool::new(false),
            stop_requested: AtomicBool::new(false),
        }))
    }

    /// Current engine state.
    pub fn info(&self) -> EngineInfo {
        EngineInfo {
            port: self.port,
            token: self.token.clone(),
            running: self.running.load(Ordering::Relaxed),
            restarts: self.restarts.load(Ordering::Relaxed),
            data_dir: self.data_dir.display().to_string(),
        }
    }

    /// Recent engine log lines, oldest first.
    pub fn logs(&self) -> Vec<String> {
        tail_log(&self.data_dir.join(LOG_FILE), LOG_CAPACITY)
    }

    /// Starts the supervision thread. Returns immediately.
    pub fn supervise(self: &Arc<Self>) {
        let this = Arc::clone(self);
        std::thread::Builder::new()
            .name("engine-supervisor".into())
            .spawn(move || this.supervise_loop())
            .ok();
    }

    fn supervise_loop(self: Arc<Self>) {
        let mut backoff = Duration::from_secs(1);
        let mut last_spawn: Option<Instant> = None;

        while !self.shutting_down.load(Ordering::Relaxed) {
            // A panic in one pass must not take the supervisor down with it.
            let step = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                self.supervise_step(&mut backoff, &mut last_spawn);
            }));
            if step.is_err() {
                log::error!("engine supervisor step panicked; continuing");
                backoff = (backoff * 2).min(MAX_BACKOFF);
            }
            std::thread::sleep(backoff);
        }
    }

    fn supervise_step(&self, backoff: &mut Duration, last_spawn: &mut Option<Instant>) {
        let healthy = self
            .endpoint
            .get()
            .map(|(port, _)| health_check(port))
            .unwrap_or(false);

        if healthy {
            self.running.store(true, Ordering::Relaxed);
            *backoff = HEALTH_INTERVAL;
            return;
        }

        // Unhealthy. An explicit user stop wins over auto-restart.
        if self.stop_requested.load(Ordering::Relaxed) {
            self.running.store(false, Ordering::Relaxed);
            return;
        }

        // Give a freshly spawned engine time to bind its socket before we even
        // consider that it failed.
        if let Some(t) = *last_spawn {
            if t.elapsed() < STARTUP_GRACE {
                return;
            }
        }

        self.reap();

        match self.spawn_once() {
            Ok(()) => {
                self.running.store(true, Ordering::Relaxed);
                self.endpoint.set(self.port, self.token.clone());
                self.log(format!(
                    "[shell] engine started on 127.0.0.1:{}",
                    self.port
                ));
                *last_spawn = Some(Instant::now());
                *backoff = Duration::from_secs(1);
            }
            Err(e) => {
                self.running.store(false, Ordering::Relaxed);
                *last_spawn = None;
                self.log(format!("[shell] could not start engine: {e}"));
                *backoff = (*backoff * 2).min(MAX_BACKOFF);
            }
        }
    }

    fn spawn_once(&self) -> Result<()> {
        if !self.binary.exists() {
            return Err(anyhow!(
                "engine binary not found at {}",
                self.binary.display()
            ));
        }

        // The engine must outlive this shell, so its output goes to a file, not
        // to pipes this process owns and will close.
        let log_path = self.data_dir.join(LOG_FILE);
        let file = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(&log_path)
            .map_err(|e| anyhow!("open engine log {}: {e}", log_path.display()))?;

        let mut cmd = Command::new(&self.binary);
        cmd.arg("-data")
            .arg(&self.data_dir)
            .arg("-listen")
            .arg(format!("127.0.0.1:{}", self.port))
            .arg("-token")
            .arg(&self.token)
            // Deliberately no -parent-pid: the engine keeps running when the
            // app quits, and reconnects when the app is next opened.
            .stdout(Stdio::from(file.try_clone()?))
            .stderr(Stdio::from(file));

        #[cfg(windows)]
        cmd.creation_flags(CREATE_NO_WINDOW);

        let child = cmd.spawn()?;

        let mut guard = self
            .child
            .lock()
            .map_err(|_| anyhow!("engine child lock poisoned"))?;
        *guard = Some(child);
        drop(guard);

        Ok(())
    }

    /// Takes the child handle and reaps (or kills) whatever is behind it. Only
    /// called when the engine is unhealthy, so an alive-but-hung process is
    /// killed to free its port for the replacement.
    fn reap(&self) {
        if let Ok(mut guard) = self.child.lock() {
            if let Some(mut child) = guard.take() {
                match child.try_wait() {
                    Ok(None) => {
                        let _ = child.kill();
                        let _ = child.wait();
                    }
                    _ => {
                        let _ = child.wait();
                    }
                }
            }
        }
    }

    fn log(&self, line: String) {
        log::info!("{line}");
        if let Ok(mut f) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(self.data_dir.join(LOG_FILE))
        {
            let _ = writeln!(f, "{line}");
        }
    }

    /// User asked to stop the engine entirely. The supervisor will not restart
    /// it until `start_engine` is called (or the app is reopened).
    pub fn stop_engine(&self) -> Result<()> {
        self.stop_requested.store(true, Ordering::Relaxed);
        if let Some((port, token)) = self.endpoint.get() {
            // Best effort: the process exits on its own once it has cleaned up.
            post_control(port, &token, "shutdown").ok();
        }
        self.running.store(false, Ordering::Relaxed);
        Ok(())
    }

    /// Clears the stop request so the supervisor brings the engine back.
    pub fn start_engine(&self) {
        self.stop_requested.store(false, Ordering::Relaxed);
        log::info!("[shell] start requested");
    }

    /// App is exiting. The engine is a daemon and keeps running; supervision
    /// is the only thing that stops here.
    pub fn shutdown(&self) {
        self.shutting_down.store(true, Ordering::Relaxed);
    }
}

/// Looks for a live engine left behind by a previous session and adopts it.
fn adopt_running(data_dir: &Path) -> Option<(u16, String)> {
    let path = data_dir.join("runtime.json");
    let raw = std::fs::read_to_string(&path).ok()?;
    let info: RuntimeInfo = serde_json::from_str(&raw).ok()?;
    if info.port == 0 || info.token.is_empty() {
        // A tokenless engine cannot be driven by the shell; start our own.
        return None;
    }
    if health_check(info.port) {
        Some((info.port, info.token))
    } else {
        None
    }
}

/// A cheap liveness probe: connect and ask for /api/health, which is served
/// unauthenticated and identifies the daemon in its body.
fn health_check(port: u16) -> bool {
    let Ok(mut stream) = connect(port) else {
        return false;
    };
    let req = "GET /api/health HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n";
    if stream.write_all(req.as_bytes()).is_err() {
        return false;
    }
    let _ = stream.set_read_timeout(Some(Duration::from_secs(2)));
    let mut buf = [0u8; 2048];
    let n = stream.read(&mut buf).unwrap_or(0);
    let resp = String::from_utf8_lossy(&buf[..n]);
    resp.starts_with("HTTP/1.1 200") && resp.contains("wsengine")
}

/// Fire-and-forget POST to an engine control endpoint (used for shutdown).
fn post_control(port: u16, token: &str, action: &str) -> Result<()> {
    let mut stream = connect(port)?;
    let body = format!(r#"{{"action":"{action}"}}"#);
    let req = format!(
        "POST /api/control HTTP/1.1\r\n\
         Host: 127.0.0.1\r\n\
         Content-Type: application/json\r\n\
         X-WS-Token: {token}\r\n\
         Content-Length: {}\r\n\
         Connection: close\r\n\r\n\
         {body}",
        body.len()
    );
    stream.write_all(req.as_bytes())?;
    let _ = stream.set_read_timeout(Some(Duration::from_secs(3)));
    let mut buf = [0u8; 1024];
    let _ = stream.read(&mut buf);
    Ok(())
}

fn connect(port: u16) -> Result<TcpStream> {
    let addr: SocketAddr = format!("127.0.0.1:{port}").parse()?;
    Ok(TcpStream::connect_timeout(&addr, Duration::from_secs(2))?)
}

/// Reads the last `capacity` lines of a (possibly large) log file.
fn tail_log(path: &Path, capacity: usize) -> Vec<String> {
    let Ok(mut f) = std::fs::File::open(path) else {
        return Vec::new();
    };
    let len = f.metadata().map(|m| m.len()).unwrap_or(0);
    let skip = len.saturating_sub(128 * 1024);
    if skip > 0 {
        let _ = f.seek(SeekFrom::Start(skip));
    }
    let mut raw = Vec::new();
    if f.read_to_end(&mut raw).is_err() {
        return Vec::new();
    }
    let text = String::from_utf8_lossy(&raw);
    let mut lines: Vec<String> = text.lines().map(str::to_string).collect();
    if skip > 0 && !lines.is_empty() {
        // The first line was cut in half by the seek; drop it.
        lines.remove(0);
    }
    if lines.len() > capacity {
        let split = lines.len() - capacity;
        lines.drain(..split);
    }
    lines
}

/// Asks the OS for an unused loopback port by binding and immediately closing.
fn free_port() -> Result<u16> {
    let listener = std::net::TcpListener::bind("127.0.0.1:0")?;
    Ok(listener.local_addr()?.port())
}

/// A per-run secret so other local processes (including any web page the user
/// visits) cannot drive the engine's API.
fn mint_token() -> String {
    let mut rng = rand::rng();
    (0..32)
        .map(|_| {
            let n: u8 = rng.random_range(0..16);
            char::from_digit(n as u32, 16).unwrap_or('0')
        })
        .collect()
}

/// Locates the engine binary: next to the app in a bundled install, or in the
/// repository layout when running from a dev build.
pub fn locate_binary(resource_dir: Option<PathBuf>) -> PathBuf {
    let name = if cfg!(windows) {
        "wsengine.exe"
    } else {
        "wsengine"
    };

    let mut candidates: Vec<PathBuf> = Vec::new();

    if let Some(dir) = resource_dir {
        candidates.push(dir.join("binaries").join(name));
        candidates.push(dir.join(name));
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            candidates.push(dir.join(name));
            candidates.push(dir.join("binaries").join(name));
            // Dev layout: target/debug/worldscraper.exe -> ../../../engine
            candidates.push(dir.join("../../../engine").join(name));
            candidates.push(dir.join("../../../../engine").join(name));
        }
    }

    for c in &candidates {
        if c.exists() {
            return c.canonicalize().unwrap_or_else(|_| c.clone());
        }
    }
    candidates.into_iter().next().unwrap_or_else(|| name.into())
}
