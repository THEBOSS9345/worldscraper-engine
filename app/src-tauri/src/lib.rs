//! WorldScraper desktop shell.
//!
//! Responsibilities:
//!   * run or adopt the Go crawl engine, which lives on as a daemon even after
//!     this window closes,
//!   * own the Tantivy search index and drain the engine's document spool,
//!   * host the dashboard window and keep running in the tray.

mod engine;
mod indexer;

use std::path::PathBuf;
use std::sync::Arc;

use tauri::menu::{Menu, MenuItem};
use tauri::tray::TrayIconBuilder;
use tauri::{Emitter, Manager, State, WindowEvent};

use engine::{EngineHandle, EngineInfo};
use indexer::{IndexStats, SearchIndex, SearchParams, SearchResponse};

/// Injected into every page before it renders. The window is created
/// manually (see `create: false` in `tauri.conf.json`) so the webview can
/// be locked down: DevTools off at the WebView2 level, no context menu to
/// right-click "Inspect", and no DevTools/zoom hotkeys.
const SHELL_HARDENING_SCRIPT: &str = r#"
  (() => {
    const swallow = (e) => { e.preventDefault(); e.stopPropagation(); };
    document.addEventListener('contextmenu', swallow, true);
    document.addEventListener('keydown', (e) => {
      if (e.key === 'F12') return swallow(e);
      if (e.ctrlKey && e.shiftKey && ['I', 'J', 'C', 'K'].includes(e.key.toUpperCase())) return swallow(e);
      if (e.ctrlKey && e.key.toUpperCase() === 'U') return swallow(e);
    }, true);
  })();
"#;

/// Shared application state.
struct AppState {
    engine: Arc<EngineHandle>,
    index: Arc<SearchIndex>,
}

// ------------------------------------------------------------------ commands --

/// Runs a search against the Tantivy index.
#[tauri::command]
async fn search(state: State<'_, AppState>, params: SearchParams) -> Result<SearchResponse, String> {
    let index = Arc::clone(&state.index);
    // Search is CPU-bound; keep it off the async runtime's worker threads.
    tauri::async_runtime::spawn_blocking(move || index.search(&params))
        .await
        .map_err(|e| format!("search task failed: {e}"))?
        .map_err(|e| e.to_string())
}

/// Live index statistics.
#[tauri::command]
fn index_stats(state: State<'_, AppState>) -> IndexStats {
    state.index.stats()
}

/// Where the engine is listening, and the token needed to talk to it.
#[tauri::command]
fn engine_info(state: State<'_, AppState>) -> EngineInfo {
    state.engine.info()
}

/// Recent engine log lines for the in-app log panel.
#[tauri::command]
fn engine_logs(state: State<'_, AppState>) -> Vec<String> {
    state.engine.logs()
}

/// Brings the engine back after an explicit stop.
#[tauri::command]
fn engine_start(state: State<'_, AppState>) -> EngineInfo {
    state.engine.start_engine();
    state.engine.info()
}

/// Gracefully stops the engine process. It stays down until started again or
/// the app is reopened.
#[tauri::command]
fn engine_stop(state: State<'_, AppState>) -> Result<EngineInfo, String> {
    state
        .engine
        .stop_engine()
        .map_err(|e| e.to_string())?;
    Ok(state.engine.info())
}

/// Opens the data directory in the system file manager.
#[tauri::command]
fn data_dir(state: State<'_, AppState>) -> String {
    state.engine.data_dir.display().to_string()
}

// --------------------------------------------------------------------- setup --

/// Entry point used by `main.rs`.
///
/// # Panics
/// Panics only if Tauri itself cannot build the application, which is not
/// recoverable.
pub fn run() {
    let mut builder = tauri::Builder::default()
        .plugin(tauri_plugin_opener::init());

    // Autostart is what makes "crawl 24/7" true across reboots. It is
    // registered here but stays disabled until the user turns it on.
    builder = builder.plugin(tauri_plugin_autostart::init(
        tauri_plugin_autostart::MacosLauncher::LaunchAgent,
        Some(vec!["--minimized"]),
    ));

    builder
        .invoke_handler(tauri::generate_handler![
            search,
            index_stats,
            engine_info,
            engine_logs,
            engine_start,
            engine_stop,
            data_dir,
        ])
        .setup(|app| {
            let handle = app.handle().clone();

            // The window is built here (not by the config auto-creator) so
            // the webview can be hardened against right-click Inspect and
            // DevTools shortcuts.
            if let Some(cfg) = app.config().app.windows.first() {
                tauri::WebviewWindowBuilder::from_config(app.handle(), cfg)?
                    .devtools(false)
                    .zoom_hotkeys_enabled(false)
                    .initialization_script(SHELL_HARDENING_SCRIPT)
                    .build()?;
            }

            let data = resolve_data_dir();
            std::fs::create_dir_all(&data)?;

            let resource_dir = app.path().resource_dir().ok();
            let binary = engine::locate_binary(resource_dir);
            log::info!("engine binary: {}", binary.display());

            let engine = EngineHandle::new(binary, data.clone())?;
            engine.supervise();

            let index = SearchIndex::open(&data.join("index"))?;

            // Drain the spool into the index for as long as the app lives.
            let ingest_index = Arc::clone(&index);
            let endpoint = Arc::clone(&engine.endpoint);
            tauri::async_runtime::spawn(async move {
                indexer::ingest_loop(ingest_index, endpoint).await;
            });

            app.manage(AppState {
                engine: Arc::clone(&engine),
                index: Arc::clone(&index),
            });

            build_tray(&handle)?;

            // Let the UI know the engine's address as soon as it is known.
            let notify = Arc::clone(&engine);
            let emit_handle = handle.clone();
            tauri::async_runtime::spawn(async move {
                loop {
                    if notify.endpoint.get().is_some() {
                        let _ = emit_handle.emit("engine-ready", notify.info());
                        break;
                    }
                    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
                }
            });

            Ok(())
        })
        .on_window_event(|window, event| {
            // Closing the window must not stop the crawl; hide to tray instead.
            if let WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .build(tauri::generate_context!())
        .expect("failed to build WorldScraper")
        .run(|app, event| {
            if let tauri::RunEvent::ExitRequested { .. } = event {
                if let Some(state) = app.try_state::<AppState>() {
                    // Stop supervising, but leave the engine running — it is a
                    // daemon that outlives the app and is adopted next launch.
                    state.engine.shutdown();
                    // Flush anything staged so a clean exit loses no documents.
                    let cursor = state.index.cursor();
                    if let Err(e) = state.index.commit(cursor) {
                        log::warn!("final index commit failed: {e}");
                    }
                }
            }
        });
}

/// Builds the tray icon and its menu.
fn build_tray(app: &tauri::AppHandle) -> tauri::Result<()> {
    let show = MenuItem::with_id(app, "show", "Open dashboard", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Quit WorldScraper", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&show, &quit])?;

    TrayIconBuilder::with_id("main")
        .icon(app.default_window_icon().cloned().ok_or_else(|| {
            tauri::Error::AssetNotFound("default window icon".into())
        })?)
        .tooltip("WorldScraper — crawling")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "show" => reveal(app),
            "quit" => app.exit(0),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let tauri::tray::TrayIconEvent::Click { button, .. } = event {
                if button == tauri::tray::MouseButton::Left {
                    reveal(tray.app_handle());
                }
            }
        })
        .build(app)?;

    Ok(())
}

fn reveal(app: &tauri::AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.show();
        let _ = w.unminimize();
        let _ = w.set_focus();
    }
}

/// The crawl data lives beside the user's other application data.
fn resolve_data_dir() -> PathBuf {
    dirs::config_dir()
        .map(|d| d.join("WorldScraper"))
        .unwrap_or_else(|| PathBuf::from("worldscraper-data"))
}
