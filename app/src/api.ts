/**
 * Client for the two backends behind the dashboard:
 *   * the Go crawl engine over loopback HTTP + SSE (stats, control, seeds)
 *   * the Rust/Tantivy index over Tauri IPC (search, index stats)
 */

import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";

// ------------------------------------------------------------------- types --

export interface Rates {
  pagesPerSec: number;
  bytesPerSec: number;
  errorsPerSec: number;
  pagesPerMin: number;
  avgLatencyMs: number;
  successRate: number;
  inflight: number;
  hostsLive: number;
  uptimeSeconds: number;
}

export interface CrawlStatus {
  running: boolean;
  paused: boolean;
  workers: number;
  buffered: number;
  hostsLive: number;
  hostsSeen: number;
  recrawled: number;
  discovered: number;
  discoveryLastRun: number;
  discoveryLastErr?: string;
}

export interface FrontierStats {
  pending: number;
  spoolDepth: number;
  spoolCursor: number;
  diskBytes: number;
  heapMb: number;
  rssMb: number;
}

/** Every runtime action the engine accepts while it is crawling. */
export type ControlAction =
  | "start"
  | "stop"
  | "pause"
  | "resume"
  | "clearQueue"
  | "reseed"
  | "discover"
  | "freeMemory"
  | "clearRobots"
  | "resetStats"
  | "clearSeen"
  | "compact";

export interface CountRow {
  label: string;
  n: number;
}

export interface HostRow {
  host: string;
  site: string;
  pages: number;
  errors: number;
  bytes: number;
  category: string;
  lastSeen: number;
}

export interface MinutePoint {
  ts: number;
  pages: number;
  bytes: number;
  errors: number;
}

export interface Aggregates {
  totals: Record<string, number>;
  hostCount: number;
  categories: CountRow[];
  langs: CountRow[];
  statuses: CountRow[];
  countries: CountRow[];
  topHosts: HostRow[];
  series: MinutePoint[];
  geo: { enabled: boolean; database?: string };
}

export interface CrawlEvent {
  ts: number;
  url: string;
  host: string;
  title: string;
  category: string;
  lang: string;
  status: number;
  bytes: number;
  latencyMs: number;
  depth: number;
  links: number;
  ok: boolean;
  err?: string;
  /** Present only when a GeoIP database is installed. */
  ip?: string;
  lat?: number;
  lon?: number;
  country?: string;
  city?: string;
  hasGeo?: boolean;
}

export interface Snapshot {
  rates: Rates;
  status: CrawlStatus;
  frontier: FrontierStats;
  agg: Aggregates;
  spark: number[];
  recent: CrawlEvent[];
  now: number;
}

export interface EngineConfig {
  workers: number;
  perHostDelayMs: number;
  perHostBurst: number;
  maxDepth: number;
  maxLinksPerDoc: number;
  requestTimeoutMs: number;
  maxPageBytes: number;
  userAgent: string;
  respectRobots: boolean;
  followRedirects: number;
  insecureTls: boolean;
  recrawlAfterHours: number;
  reseedWhenDry: boolean;
  crawlAdult: boolean;
  onlyHtml: boolean;
  paused: boolean;
  discoveryEnabled: boolean;
  discoveryIntervalMin: number;
  discoveryMaxPerCycle: number;
  discoverySources: string[];
}

export interface EngineInfo {
  port: number;
  token: string;
  running: boolean;
  restarts: number;
  dataDir: string;
}

export interface IndexStats {
  docs: number;
  segments: number;
  diskBytes: number;
  cursor: number;
  indexedTotal: number;
  pendingCommit: number;
  lastCommitUnix: number;
  indexing: boolean;
}

export interface Hit {
  url: string;
  host: string;
  site: string;
  title: string;
  description: string;
  excerpt: string;
  lang: string;
  category: string;
  status: number;
  depth: number;
  bytes: number;
  fetchedAt: number;
  score: number;
}

export interface SearchResponse {
  hits: Hit[];
  total: number;
  tookMs: number;
  query: string;
  offset: number;
  /** Strict all-terms search found nothing, so any-term matching was used. */
  relaxed: boolean;
}

export interface SearchParams {
  query: string;
  limit?: number;
  offset?: number;
  category?: string | null;
  host?: string | null;
  lang?: string | null;
  sort?: "relevance" | "recent";
}

// ------------------------------------------------------------------ engine --

/** Where the engine is; resolved once the shell reports it is up. */
let engine: EngineInfo | null = null;

/**
 * Re-checks the shell for the engine's current address. Used to recover from a
 * late start or a supervisor restart that moved the engine.
 */
async function refreshEngineInfo(): Promise<void> {
  try {
    const i = await invoke<EngineInfo>("engine_info");
    if (i.running) engine = i;
  } catch {
    /* shell may be mid-restart */
  }
}

/** Resolves when the engine endpoint is known. */
export async function connectEngine(): Promise<EngineInfo> {
  if (engine?.running) return engine;

  const info = await invoke<EngineInfo>("engine_info");
  if (info.running) {
    engine = info;
    return info;
  }

  // The supervisor emits this as soon as the child process is up.
  return new Promise<EngineInfo>((resolve, reject) => {
    let settled = false;
    const done = (i: EngineInfo) => {
      if (settled) return;
      settled = true;
      engine = i;
      resolve(i);
    };

    listen<EngineInfo>("engine-ready", (e) => done(e.payload)).catch(() => {});

    // Belt and braces: the event can fire before the listener attaches.
    const poll = setInterval(async () => {
      try {
        const i = await invoke<EngineInfo>("engine_info");
        if (i.running) {
          clearInterval(poll);
          done(i);
        }
      } catch {
        /* keep polling */
      }
    }, 400);

    // Give the supervisor a generous window, then give up so the UI can show
    // "engine unavailable" instead of spinning forever.
    window.setTimeout(() => {
      if (settled) return;
      settled = true;
      clearInterval(poll);
      reject(new Error("engine did not start in time"));
    }, 30_000);
  });
}

function requireEngine(): EngineInfo {
  if (!engine) throw new Error("engine not connected yet");
  return engine;
}

async function engineFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const e = requireEngine();
  const res = await fetch(`http://127.0.0.1:${e.port}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      "X-WS-Token": e.token,
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    throw new Error(`${path} failed: ${res.status} ${await res.text()}`);
  }
  return (await res.json()) as T;
}

export const getStats = () => engineFetch<Snapshot>("/api/stats");
export const getConfig = () => engineFetch<EngineConfig>("/api/config");

export const setConfig = (patch: Partial<EngineConfig>) =>
  engineFetch<EngineConfig>("/api/config", {
    method: "POST",
    body: JSON.stringify(patch),
  });

export const control = (action: ControlAction) =>
  engineFetch<{ status: CrawlStatus; message: string }>("/api/control", {
    method: "POST",
    body: JSON.stringify({ action }),
  });

export const addSeeds = (urls: string[]) =>
  engineFetch<{ added: number; submitted: number }>("/api/seeds", {
    method: "POST",
    body: JSON.stringify({ urls }),
  });

export const getHosts = (n = 50) => engineFetch<HostRow[]>(`/api/hosts?n=${n}`);

// --------------------------------------------------------------- live feed --

export interface LiveHandlers {
  onSnapshot?: (s: Snapshot) => void;
  onTick?: (t: {
    rates: Rates;
    status: CrawlStatus;
    frontier: FrontierStats;
    spark: number[];
    events: CrawlEvent[];
    now: number;
  }) => void;
  onAgg?: (a: Aggregates) => void;
  onState?: (connected: boolean) => void;
}

/**
 * Subscribes to the engine's server-sent-events stream.
 *
 * EventSource reconnects on its own, but not if the engine process restarted
 * and the socket died mid-stream, so connection state is surfaced to the UI.
 */
export function openLiveFeed(h: LiveHandlers): () => void {
  let source: EventSource | null = null;
  let closed = false;
  let retry: number | undefined;

  const connect = () => {
    if (closed) return;
    let e: EngineInfo;
    try {
      // The engine may have come up after the initial boot (or been replaced
      // by the supervisor on a restart), so refresh its address each attempt.
      e = requireEngine();
    } catch {
      void refreshEngineInfo().then(() => {
        try {
          e = requireEngine();
        } catch {
          if (!closed) retry = window.setTimeout(connect, 2000);
          return;
        }
        open(e);
      });
      return;
    }
    open(e);
  };

  const open = (e: EngineInfo) => {
    source = new EventSource(
      `http://127.0.0.1:${e.port}/api/live?token=${encodeURIComponent(e.token)}`,
    );

    source.addEventListener("open", () => h.onState?.(true));

    source.addEventListener("snapshot", (ev) => {
      try {
        h.onSnapshot?.(JSON.parse((ev as MessageEvent).data));
      } catch {
        /* ignore malformed frame */
      }
    });
    source.addEventListener("tick", (ev) => {
      try {
        h.onTick?.(JSON.parse((ev as MessageEvent).data));
      } catch {
        /* ignore malformed frame */
      }
    });
    source.addEventListener("agg", (ev) => {
      try {
        h.onAgg?.(JSON.parse((ev as MessageEvent).data));
      } catch {
        /* ignore malformed frame */
      }
    });

    source.addEventListener("error", () => {
      h.onState?.(false);
      source?.close();
      source = null;
      if (!closed) retry = window.setTimeout(connect, 2000);
    });
  };

  connect();

  return () => {
    closed = true;
    if (retry) window.clearTimeout(retry);
    source?.close();
  };
}

// ------------------------------------------------------------------ search --

export const search = (params: SearchParams) =>
  invoke<SearchResponse>("search", { params });

export const indexStats = () => invoke<IndexStats>("index_stats");

export const engineLogs = () => invoke<string[]>("engine_logs");

export const dataDir = () => invoke<string>("data_dir");

/** Brings the engine daemon back after an explicit stop. */
export const engineStart = () => invoke<EngineInfo>("engine_start");

/** Gracefully stops the engine daemon; it stays down until started again. */
export const engineStop = () => invoke<EngineInfo>("engine_stop");
