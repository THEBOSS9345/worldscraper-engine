/**
 * Canvas charts for the dashboard.
 *
 * Design rules applied here: one y-axis only, recessive grid and axes, 2px
 * data lines, no marker on every point, and a crosshair + tooltip on the time
 * series (an on-screen chart should be interactive by default).
 */

export interface Point {
  ts: number;
  value: number;
}

const INK_MUTED = "#61748f";
const GRID = "rgba(90, 170, 220, 0.10)";

/**
 * Prepares a canvas for drawing.
 *
 * Assigning canvas.width/height reallocates the backing bitmap even when the
 * value is unchanged, which at two renders a second is enough to make the whole
 * window feel unresponsive. The dimensions are only written when they actually
 * differ.
 */
function setupCanvas(canvas: HTMLCanvasElement): {
  ctx: CanvasRenderingContext2D;
  w: number;
  h: number;
} {
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new Error("2d canvas context unavailable");

  const rect = canvas.getBoundingClientRect();
  const dpr = Math.min(window.devicePixelRatio || 1, 2);
  const w = Math.max(1, rect.width);
  const h = Math.max(1, rect.height);
  const pxW = Math.round(w * dpr);
  const pxH = Math.round(h * dpr);

  if (canvas.width !== pxW || canvas.height !== pxH) {
    canvas.width = pxW;
    canvas.height = pxH;
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  return { ctx, w, h };
}

/** Rounds a maximum up to a friendly axis bound. */
function niceMax(v: number): number {
  if (v <= 0) return 1;
  const exp = Math.floor(Math.log10(v));
  const base = Math.pow(10, exp);
  const norm = v / base;
  const step = norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 5 ? 5 : 10;
  return step * base;
}

export function compactNumber(n: number): string {
  const abs = Math.abs(n);
  if (abs >= 1e12) return (n / 1e12).toFixed(abs >= 1e13 ? 0 : 1) + "T";
  if (abs >= 1e9) return (n / 1e9).toFixed(abs >= 1e10 ? 0 : 1) + "B";
  if (abs >= 1e6) return (n / 1e6).toFixed(abs >= 1e7 ? 0 : 1) + "M";
  if (abs >= 1e3) return (n / 1e3).toFixed(abs >= 1e4 ? 0 : 1) + "k";
  return String(Math.round(n));
}

export function formatBytes(n: number): string {
  if (n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / Math.pow(1024, i);
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`;
}

const timeFmt = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

/**
 * Time-series area chart with a hover crosshair.
 *
 * `unitLabel` names what the single series is, which is why no legend box is
 * needed — the panel title carries identity for a one-series chart.
 */
export class AreaChart {
  private data: Point[] = [];
  private hoverIndex = -1;
  private w = 0;
  private pad = { top: 12, right: 8, bottom: 22, left: 44 };

  constructor(
    private canvas: HTMLCanvasElement,
    private tooltip: HTMLElement,
    private unitLabel: string,
  ) {
    const move = (e: PointerEvent) => {
      const rect = this.canvas.getBoundingClientRect();
      this.updateHover(e.clientX - rect.left, e.clientX, e.clientY);
    };
    this.canvas.addEventListener("pointermove", move);
    this.canvas.addEventListener("pointerleave", () => {
      this.hoverIndex = -1;
      this.tooltip.classList.remove("tooltip--on");
      this.render();
    });
  }

  setData(points: Point[]): void {
    this.data = points;
    this.render();
  }

  private updateHover(localX: number, clientX: number, clientY: number): void {
    if (this.data.length === 0) return;
    const plotW = this.w - this.pad.left - this.pad.right;
    const f = (localX - this.pad.left) / Math.max(1, plotW);
    const idx = Math.round(f * (this.data.length - 1));
    this.hoverIndex = Math.max(0, Math.min(this.data.length - 1, idx));

    const p = this.data[this.hoverIndex];
    this.tooltip.innerHTML =
      `<div class="tooltip__head">${timeFmt.format(new Date(p.ts * 1000))}</div>` +
      `<div class="tooltip__row">` +
      `<span class="tooltip__swatch" style="background:#22e2ff"></span>` +
      `<span>${compactNumber(p.value)} ${this.unitLabel}</span></div>`;

    const host = this.canvas.parentElement;
    if (host) {
      const hostRect = host.getBoundingClientRect();
      const tw = this.tooltip.offsetWidth || 120;
      let left = clientX - hostRect.left + 12;
      if (left + tw > hostRect.width) left = clientX - hostRect.left - tw - 12;
      this.tooltip.style.left = `${Math.max(0, left)}px`;
      this.tooltip.style.top = `${Math.max(0, clientY - hostRect.top - 44)}px`;
    }
    this.tooltip.classList.add("tooltip--on");
    this.render();
  }

  render(): void {
    const { ctx, w, h } = setupCanvas(this.canvas);
    this.w = w;

    const { top, right, bottom, left } = this.pad;
    const plotW = w - left - right;
    const plotH = h - top - bottom;

    if (this.data.length === 0 || plotW <= 0 || plotH <= 0) {
      ctx.fillStyle = INK_MUTED;
      ctx.font = "11px ui-monospace, monospace";
      ctx.textAlign = "center";
      ctx.fillText("awaiting data", w / 2, h / 2);
      return;
    }

    const max = niceMax(Math.max(...this.data.map((d) => d.value), 1));
    const xAt = (i: number) => left + (i / Math.max(1, this.data.length - 1)) * plotW;
    const yAt = (v: number) => top + plotH - (v / max) * plotH;

    // Recessive grid + y labels.
    ctx.font = "10px ui-monospace, monospace";
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    for (let i = 0; i <= 3; i++) {
      const v = (max / 3) * i;
      const y = yAt(v);
      ctx.strokeStyle = GRID;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(left, Math.round(y) + 0.5);
      ctx.lineTo(left + plotW, Math.round(y) + 0.5);
      ctx.stroke();
      ctx.fillStyle = INK_MUTED;
      ctx.fillText(compactNumber(v), left - 8, y);
    }

    // Area fill.
    const grad = ctx.createLinearGradient(0, top, 0, top + plotH);
    grad.addColorStop(0, "rgba(34, 226, 255, 0.30)");
    grad.addColorStop(1, "rgba(34, 226, 255, 0.02)");
    ctx.fillStyle = grad;
    ctx.beginPath();
    ctx.moveTo(xAt(0), top + plotH);
    this.data.forEach((d, i) => ctx.lineTo(xAt(i), yAt(d.value)));
    ctx.lineTo(xAt(this.data.length - 1), top + plotH);
    ctx.closePath();
    ctx.fill();

    // Data line.
    ctx.strokeStyle = "#22e2ff";
    ctx.lineWidth = 2;
    ctx.lineJoin = "round";
    ctx.beginPath();
    this.data.forEach((d, i) => (i === 0 ? ctx.moveTo(xAt(i), yAt(d.value)) : ctx.lineTo(xAt(i), yAt(d.value))));
    ctx.stroke();

    // X labels at the ends and middle only — no dense tick clutter.
    ctx.textAlign = "center";
    ctx.textBaseline = "top";
    ctx.fillStyle = INK_MUTED;
    for (const i of [0, Math.floor(this.data.length / 2), this.data.length - 1]) {
      const p = this.data[i];
      if (!p) continue;
      ctx.fillText(timeFmt.format(new Date(p.ts * 1000)), xAt(i), top + plotH + 7);
    }

    // Crosshair.
    if (this.hoverIndex >= 0 && this.hoverIndex < this.data.length) {
      const x = xAt(this.hoverIndex);
      const y = yAt(this.data[this.hoverIndex].value);
      ctx.strokeStyle = "rgba(34, 226, 255, 0.4)";
      ctx.lineWidth = 1;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      ctx.moveTo(x, top);
      ctx.lineTo(x, top + plotH);
      ctx.stroke();
      ctx.setLineDash([]);

      ctx.fillStyle = "#05070d";
      ctx.strokeStyle = "#22e2ff";
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.arc(x, y, 4.5, 0, Math.PI * 2);
      ctx.fill();
      ctx.stroke();
    }
  }
}

/** Bare 60-second sparkline: shape only, no axes. */
export class Sparkline {
  constructor(private canvas: HTMLCanvasElement) {}

  render(values: number[]): void {
    const { ctx, w, h } = setupCanvas(this.canvas);
    if (values.length === 0) return;

    const max = Math.max(...values, 1);
    const step = w / Math.max(1, values.length - 1);
    const yAt = (v: number) => h - 2 - (v / max) * (h - 6);

    const grad = ctx.createLinearGradient(0, 0, 0, h);
    grad.addColorStop(0, "rgba(34, 226, 255, 0.34)");
    grad.addColorStop(1, "rgba(34, 226, 255, 0)");
    ctx.fillStyle = grad;
    ctx.beginPath();
    ctx.moveTo(0, h);
    values.forEach((v, i) => ctx.lineTo(i * step, yAt(v)));
    ctx.lineTo((values.length - 1) * step, h);
    ctx.closePath();
    ctx.fill();

    ctx.strokeStyle = "#22e2ff";
    ctx.lineWidth = 1.6;
    ctx.lineJoin = "round";
    ctx.beginPath();
    values.forEach((v, i) => (i === 0 ? ctx.moveTo(0, yAt(v)) : ctx.lineTo(i * step, yAt(v))));
    ctx.stroke();
  }
}

/**
 * Horizontal status bar: HTTP response classes.
 *
 * Uses the reserved status palette (good / warning / critical), never the
 * categorical hues, and every segment is labeled — color is not the only cue.
 */
export interface StatusBucket {
  label: string;
  n: number;
  color: string;
}

export function bucketStatuses(rows: { label: string; n: number }[]): StatusBucket[] {
  const buckets = { ok: 0, redirect: 0, client: 0, server: 0 };
  for (const r of rows) {
    const code = Number(r.label);
    if (code >= 200 && code < 300) buckets.ok += r.n;
    else if (code >= 300 && code < 400) buckets.redirect += r.n;
    else if (code >= 400 && code < 500) buckets.client += r.n;
    else buckets.server += r.n;
  }
  return [
    { label: "2xx success", n: buckets.ok, color: "#3e9e4a" },
    { label: "3xx redirect", n: buckets.redirect, color: "#5b84e8" },
    { label: "4xx blocked", n: buckets.client, color: "#9a6b12" },
    { label: "5xx failed", n: buckets.server, color: "#c2405c" },
  ].filter((b) => b.n > 0);
}
