/**
 * The rotating globe on the overview.
 *
 * Every ping is a real page fetch that just completed. When a GeoIP database is
 * installed the position is the server's actual location; without one the host
 * name is hashed to a stable point and the panel says the positions are
 * approximate.
 */

import { CONTINENTS, ISLANDS, type Ring } from "./land";

const TAU = Math.PI * 2;

// Bounded working sets: a fast crawl must not make the frame cost grow.
const MAX_PINGS = 140;
const MAX_ARCS = 16;
const PING_LIFE = 2400;
const ARC_LIFE = 1500;

interface Ping {
  lat: number;
  lon: number;
  born: number;
  ok: boolean;
}

interface Arc {
  from: [number, number];
  to: [number, number];
  born: number;
}

interface Projected {
  x: number;
  y: number;
  z: number;
}

export class Globe {
  private ctx: CanvasRenderingContext2D;
  private raf = 0;
  private rotation = 0;
  private pings: Ping[] = [];
  private arcs: Arc[] = [];
  private w = 0;
  private h = 0;
  private cx = 0;
  private cy = 0;
  private r = 0;
  private dpr = 1;
  private reduced: boolean;
  private lastFrame = 0;
  private stars: HTMLCanvasElement | null = null;

  constructor(private canvas: HTMLCanvasElement) {
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("2d canvas context unavailable");
    this.ctx = ctx;
    this.reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    this.resize();
  }

  resize(): void {
    const rect = this.canvas.getBoundingClientRect();
    this.dpr = Math.min(window.devicePixelRatio || 1, 2);
    this.w = Math.max(1, rect.width);
    this.h = Math.max(1, rect.height);
    this.canvas.width = Math.round(this.w * this.dpr);
    this.canvas.height = Math.round(this.h * this.dpr);
    this.ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0);

    this.cx = this.w / 2;
    this.cy = this.h / 2;
    this.r = Math.min(this.w, this.h) * 0.42;
    this.stars = this.makeStars();
  }

  /** Registers a completed fetch. `geo` is the server's real position, if known. */
  hit(host: string, ok: boolean, geo?: { lat: number; lon: number } | null): void {
    const { lat, lon } = geo ?? hostToLatLon(host);
    const now = performance.now();

    if (this.pings.length >= MAX_PINGS) {
      this.pings.splice(0, this.pings.length - MAX_PINGS + 1);
    }
    this.pings.push({ lat, lon, born: now, ok });

    if (ok && this.arcs.length < MAX_ARCS && Math.random() < 0.35) {
      const prev = this.pings[this.pings.length - 2];
      if (prev) {
        this.arcs.push({ from: [prev.lat, prev.lon], to: [lat, lon], born: now });
      }
    }
  }

  start(): void {
    if (this.raf) return;
    // 30 fps is indistinguishable for a slowly rotating globe and halves the
    // cost, which matters when the crawler is already using the CPU hard.
    const minFrameMs = 1000 / 30;

    const loop = (t: number) => {
      this.raf = requestAnimationFrame(loop);

      // Nothing to do while the window is hidden or minimised.
      if (document.hidden) {
        this.lastFrame = t;
        return;
      }
      const elapsed = t - (this.lastFrame || t);
      if (this.lastFrame && elapsed < minFrameMs) return;

      const dt = Math.min(elapsed, 100);
      this.lastFrame = t;
      if (!this.reduced) this.rotation += dt * 0.00004 * TAU;
      this.draw(t);
    };
    this.raf = requestAnimationFrame(loop);
  }

  stop(): void {
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
    this.lastFrame = 0;
  }

  // ------------------------------------------------------------ projection --

  private project(lat: number, lon: number): Projected {
    const phi = (lat * Math.PI) / 180;
    const theta = (lon * Math.PI) / 180 + this.rotation;

    const x = Math.cos(phi) * Math.sin(theta);
    const y = Math.sin(phi);
    const z = Math.cos(phi) * Math.cos(theta);

    // A slight axial tilt reads as a globe rather than a spinning disc.
    const tilt = -0.36;
    const y2 = y * Math.cos(tilt) - z * Math.sin(tilt);
    const z2 = y * Math.sin(tilt) + z * Math.cos(tilt);

    return { x: this.cx + x * this.r, y: this.cy - y2 * this.r, z: z2 };
  }

  /**
   * Projects a point, pushing anything on the far side out to the limb.
   *
   * Filling a polygon that straddles the horizon otherwise produces a spike
   * across the sphere; clamping to the edge gives a clean silhouette instead.
   */
  private projectClamped(lat: number, lon: number): Projected {
    const p = this.project(lat, lon);
    if (p.z >= 0) return p;

    const dx = p.x - this.cx;
    const dy = p.y - this.cy;
    const d = Math.hypot(dx, dy) || 1;
    return { x: this.cx + (dx / d) * this.r, y: this.cy + (dy / d) * this.r, z: p.z };
  }

  // --------------------------------------------------------------- drawing --

  private draw(t: number): void {
    const ctx = this.ctx;
    ctx.clearRect(0, 0, this.w, this.h);

    if (this.stars) ctx.drawImage(this.stars, 0, 0, this.w, this.h);

    this.drawAtmosphere();
    this.drawOcean();

    // Everything from here on belongs to the sphere.
    ctx.save();
    ctx.beginPath();
    ctx.arc(this.cx, this.cy, this.r, 0, TAU);
    ctx.clip();

    this.drawGraticule();
    this.drawLand();
    this.drawShading();
    this.drawArcs(t);
    this.drawPings(t);

    ctx.restore();

    // Rim.
    ctx.strokeStyle = "rgba(130, 235, 255, 0.55)";
    ctx.lineWidth = 1.2;
    ctx.beginPath();
    ctx.arc(this.cx, this.cy, this.r, 0, TAU);
    ctx.stroke();
  }

  private drawAtmosphere(): void {
    const ctx = this.ctx;
    const glow = ctx.createRadialGradient(
      this.cx, this.cy, this.r * 0.94,
      this.cx, this.cy, this.r * 1.4,
    );
    glow.addColorStop(0, "rgba(56, 190, 255, 0.30)");
    glow.addColorStop(0.45, "rgba(40, 150, 230, 0.10)");
    glow.addColorStop(1, "rgba(30, 120, 200, 0)");
    ctx.fillStyle = glow;
    ctx.beginPath();
    ctx.arc(this.cx, this.cy, this.r * 1.4, 0, TAU);
    ctx.fill();
  }

  private drawOcean(): void {
    const ctx = this.ctx;
    const sea = ctx.createRadialGradient(
      this.cx - this.r * 0.32, this.cy - this.r * 0.38, this.r * 0.05,
      this.cx, this.cy, this.r,
    );
    sea.addColorStop(0, "#14456b");
    sea.addColorStop(0.55, "#0c2b46");
    sea.addColorStop(1, "#04121f");
    ctx.fillStyle = sea;
    ctx.beginPath();
    ctx.arc(this.cx, this.cy, this.r, 0, TAU);
    ctx.fill();
  }

  private drawGraticule(): void {
    const ctx = this.ctx;
    ctx.lineWidth = 1;
    ctx.strokeStyle = "rgba(120, 200, 240, 0.10)";

    for (let lat = -60; lat <= 60; lat += 30) {
      ctx.beginPath();
      let started = false;
      for (let lon = -180; lon <= 180; lon += 6) {
        const p = this.project(lat, lon);
        if (p.z < 0) {
          started = false;
          continue;
        }
        started ? ctx.lineTo(p.x, p.y) : ctx.moveTo(p.x, p.y);
        started = true;
      }
      ctx.stroke();
    }

    for (let lon = -180; lon < 180; lon += 30) {
      ctx.beginPath();
      let started = false;
      for (let lat = -90; lat <= 90; lat += 6) {
        const p = this.project(lat, lon);
        if (p.z < 0) {
          started = false;
          continue;
        }
        started ? ctx.lineTo(p.x, p.y) : ctx.moveTo(p.x, p.y);
        started = true;
      }
      ctx.stroke();
    }
  }

  private drawLand(): void {
    const ctx = this.ctx;

    const fillRing = (ring: Ring, fill: string, stroke: string, lineWidth: number) => {
      // Skip a landmass entirely when every point faces away.
      let anyFront = false;
      for (const [lat, lon] of ring) {
        if (this.project(lat, lon).z >= -0.05) {
          anyFront = true;
          break;
        }
      }
      if (!anyFront) return;

      ctx.beginPath();
      ring.forEach(([lat, lon], i) => {
        const p = this.projectClamped(lat, lon);
        i === 0 ? ctx.moveTo(p.x, p.y) : ctx.lineTo(p.x, p.y);
      });
      ctx.closePath();
      ctx.fillStyle = fill;
      ctx.fill();
      ctx.strokeStyle = stroke;
      ctx.lineWidth = lineWidth;
      ctx.stroke();
    };

    for (const ring of CONTINENTS) {
      fillRing(ring, "rgba(38, 118, 122, 0.92)", "rgba(150, 250, 255, 0.55)", 1.1);
    }
    for (const ring of ISLANDS) {
      fillRing(ring, "rgba(38, 118, 122, 0.88)", "rgba(150, 250, 255, 0.45)", 0.9);
    }
  }

  /** Limb darkening plus a soft specular highlight, so the disc reads as a ball. */
  private drawShading(): void {
    const ctx = this.ctx;

    const shade = ctx.createRadialGradient(
      this.cx - this.r * 0.3, this.cy - this.r * 0.35, this.r * 0.1,
      this.cx, this.cy, this.r * 1.02,
    );
    shade.addColorStop(0, "rgba(255, 255, 255, 0.10)");
    shade.addColorStop(0.45, "rgba(0, 0, 0, 0)");
    shade.addColorStop(1, "rgba(0, 6, 14, 0.72)");
    ctx.fillStyle = shade;
    ctx.beginPath();
    ctx.arc(this.cx, this.cy, this.r, 0, TAU);
    ctx.fill();
  }

  private drawArcs(t: number): void {
    const ctx = this.ctx;
    this.arcs = this.arcs.filter((a) => t - a.born < ARC_LIFE);

    for (const a of this.arcs) {
      const age = (t - a.born) / ARC_LIFE;
      const alpha = Math.sin(age * Math.PI) * 0.7;
      if (alpha <= 0.02) continue;

      ctx.strokeStyle = `rgba(255, 110, 235, ${alpha})`;
      ctx.lineWidth = 1.2;
      ctx.beginPath();

      let started = false;
      const steps = 24;
      for (let i = 0; i <= steps; i++) {
        const f = i / steps;
        const lat = a.from[0] + (a.to[0] - a.from[0]) * f;
        const lon = a.from[1] + (a.to[1] - a.from[1]) * f;
        const p = this.project(lat, lon);
        if (p.z < 0) {
          started = false;
          continue;
        }
        const lift = 1 + Math.sin(f * Math.PI) * 0.16;
        const x = this.cx + (p.x - this.cx) * lift;
        const y = this.cy + (p.y - this.cy) * lift;
        started ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
        started = true;
      }
      ctx.stroke();
    }
  }

  private drawPings(t: number): void {
    const ctx = this.ctx;
    this.pings = this.pings.filter((p) => t - p.born < PING_LIFE);

    for (const ping of this.pings) {
      const p = this.project(ping.lat, ping.lon);
      if (p.z < 0) continue;

      const age = (t - ping.born) / PING_LIFE;
      const fade = 1 - age;
      // Fade points near the limb so they sink round the edge rather than
      // vanishing abruptly.
      const edge = Math.min(1, p.z * 4);
      const rgb = ping.ok ? "125, 240, 150" : "255, 125, 146";

      const ringR = 2 + age * 15;
      ctx.strokeStyle = `rgba(${rgb}, ${fade * 0.45 * edge})`;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.arc(p.x, p.y, ringR, 0, TAU);
      ctx.stroke();

      // Bloom around the light.
      const bloom = ctx.createRadialGradient(p.x, p.y, 0, p.x, p.y, 7);
      bloom.addColorStop(0, `rgba(${rgb}, ${0.55 * fade * edge})`);
      bloom.addColorStop(1, `rgba(${rgb}, 0)`);
      ctx.fillStyle = bloom;
      ctx.beginPath();
      ctx.arc(p.x, p.y, 7, 0, TAU);
      ctx.fill();

      ctx.fillStyle = `rgba(240, 255, 245, ${Math.min(1, fade * 1.5) * edge})`;
      ctx.beginPath();
      ctx.arc(p.x, p.y, 1.8, 0, TAU);
      ctx.fill();
    }
  }

  /** Renders the star field once; it never changes between resizes. */
  private makeStars(): HTMLCanvasElement | null {
    const c = document.createElement("canvas");
    c.width = Math.max(1, Math.round(this.w));
    c.height = Math.max(1, Math.round(this.h));
    const g = c.getContext("2d");
    if (!g) return null;

    const count = Math.round((this.w * this.h) / 5200);
    for (let i = 0; i < count; i++) {
      const x = Math.random() * this.w;
      const y = Math.random() * this.h;
      // Keep the disc itself clear of stars.
      if (Math.hypot(x - this.cx, y - this.cy) < this.r * 1.06) continue;
      const a = 0.12 + Math.random() * 0.5;
      const s = Math.random() < 0.9 ? 0.7 : 1.3;
      g.fillStyle = `rgba(210, 235, 255, ${a})`;
      g.beginPath();
      g.arc(x, y, s, 0, TAU);
      g.fill();
    }
    return c;
  }
}

/**
 * Maps a hostname to a fixed point on the sphere.
 *
 * Only used when no GeoIP database is installed. This is a visual placement,
 * not geolocation — biased toward populated latitudes so the display still
 * looks like Earth.
 */
function hostToLatLon(host: string): { lat: number; lon: number } {
  let h1 = 0x811c9dc5;
  let h2 = 0x1000193;
  for (let i = 0; i < host.length; i++) {
    h1 ^= host.charCodeAt(i);
    h1 = Math.imul(h1, 0x01000193) >>> 0;
    h2 = Math.imul(h2 ^ host.charCodeAt(i), 0x85ebca6b) >>> 0;
  }
  const a = (h1 % 10000) / 10000;
  const b = (h2 % 10000) / 10000;

  const lat = 62 - Math.pow(a, 1.35) * 118;
  const lon = b * 360 - 180;
  return { lat, lon };
}
