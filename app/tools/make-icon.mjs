// Generates the source app icon as a PNG, with no image-library dependency.
//
// `npx tauri icon` takes this file and produces every size and format the
// bundler needs, so this only has to draw one 1024x1024 master.

import { deflateSync } from "node:zlib";
import { writeFileSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const SIZE = 1024;
const OUT = resolve(dirname(fileURLToPath(import.meta.url)), "..", "src-tauri", "app-icon.png");

const px = new Uint8Array(SIZE * SIZE * 4);

const set = (x, y, r, g, b, a) => {
  if (x < 0 || y < 0 || x >= SIZE || y >= SIZE) return;
  const i = (y * SIZE + x) * 4;
  // Simple source-over blend against whatever is already there.
  const sa = a / 255;
  px[i] = Math.round(px[i] * (1 - sa) + r * sa);
  px[i + 1] = Math.round(px[i + 1] * (1 - sa) + g * sa);
  px[i + 2] = Math.round(px[i + 2] * (1 - sa) + b * sa);
  px[i + 3] = Math.max(px[i + 3], Math.round(a));
};

const cx = SIZE / 2;
const cy = SIZE / 2;
const R = SIZE * 0.40;

// Rounded-square background with a vertical gradient.
const radius = SIZE * 0.22;
for (let y = 0; y < SIZE; y++) {
  for (let x = 0; x < SIZE; x++) {
    const dx = Math.max(radius - x, 0, x - (SIZE - radius));
    const dy = Math.max(radius - y, 0, y - (SIZE - radius));
    if (dx * dx + dy * dy > radius * radius) continue;
    const t = y / SIZE;
    set(x, y, Math.round(8 + 6 * t), Math.round(12 + 14 * t), Math.round(24 + 30 * t), 255);
  }
}

// Outer glow around the globe.
for (let y = 0; y < SIZE; y++) {
  for (let x = 0; x < SIZE; x++) {
    const d = Math.hypot(x - cx, y - cy);
    if (d > R * 1.55 || d < R * 0.2) continue;
    const g = Math.exp(-Math.pow((d - R) / (R * 0.42), 2));
    if (g < 0.02) continue;
    set(x, y, 34, 226, 255, Math.round(70 * g));
  }
}

// Filled sphere.
for (let y = 0; y < SIZE; y++) {
  for (let x = 0; x < SIZE; x++) {
    const d = Math.hypot(x - cx, y - cy);
    if (d > R) continue;
    // Light from the upper left.
    const nx = (x - cx) / R;
    const ny = (y - cy) / R;
    const nz = Math.sqrt(Math.max(0, 1 - nx * nx - ny * ny));
    const light = Math.max(0, -0.55 * nx - 0.55 * ny + 0.62 * nz);
    const edge = Math.min(1, (R - d) / (R * 0.06));
    const base = 0.10 + 0.55 * light;
    set(
      x, y,
      Math.round(10 + 26 * base),
      Math.round(30 + 150 * base),
      Math.round(60 + 190 * base),
      Math.round(255 * edge),
    );
  }
}

// Latitude bands.
for (let i = -3; i <= 3; i++) {
  const yOff = (i / 4) * R;
  const rx = Math.sqrt(Math.max(0, R * R - yOff * yOff));
  const ry = Math.max(2, rx * 0.16);
  for (let a = 0; a < 2200; a++) {
    const th = (a / 2200) * Math.PI * 2;
    const x = cx + Math.cos(th) * rx;
    const y = cy + yOff + Math.sin(th) * ry;
    if (Math.hypot(x - cx, y - cy) > R - 1) continue;
    for (let w = -1; w <= 1; w++) set(Math.round(x), Math.round(y + w), 120, 245, 255, w === 0 ? 190 : 80);
  }
}

// Longitude bands.
for (let i = 0; i < 6; i++) {
  const f = Math.cos((i / 6) * Math.PI);
  const rx = Math.max(2, Math.abs(f) * R);
  for (let a = 0; a < 2200; a++) {
    const th = (a / 2200) * Math.PI * 2;
    const x = cx + Math.cos(th) * rx;
    const y = cy + Math.sin(th) * R;
    if (Math.hypot(x - cx, y - cy) > R - 1) continue;
    for (let w = -1; w <= 1; w++) set(Math.round(x + w), Math.round(y), 120, 245, 255, w === 0 ? 150 : 60);
  }
}

// Node pings scattered over the surface.
const nodes = [
  [-0.42, -0.36], [0.12, -0.55], [0.48, -0.14], [-0.20, 0.10],
  [0.30, 0.34], [-0.55, 0.16], [0.02, 0.62], [-0.10, -0.12],
];
for (const [nx, ny] of nodes) {
  const x0 = cx + nx * R;
  const y0 = cy + ny * R;
  for (let y = -18; y <= 18; y++) {
    for (let x = -18; x <= 18; x++) {
      const d = Math.hypot(x, y);
      if (d > 18) continue;
      const core = d <= 6 ? 1 : Math.exp(-Math.pow((d - 6) / 7, 2));
      set(Math.round(x0 + x), Math.round(y0 + y), 190, 255, 255, Math.round(235 * core));
    }
  }
}

writeFileSync(OUT, encodePNG(px, SIZE, SIZE));
console.log(`wrote ${OUT}`);

// ------------------------------------------------------------------ encoding --

function encodePNG(rgba, width, height) {
  const raw = Buffer.alloc(height * (width * 4 + 1));
  for (let y = 0; y < height; y++) {
    const rowStart = y * (width * 4 + 1);
    raw[rowStart] = 0; // filter: none
    Buffer.from(rgba.buffer, y * width * 4, width * 4).copy(raw, rowStart + 1);
  }

  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // RGBA
  ihdr[10] = 0;
  ihdr[11] = 0;
  ihdr[12] = 0;

  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw, { level: 9 })),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length, 0);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body) >>> 0, 0);
  return Buffer.concat([len, body, crc]);
}

// `var` on purpose: this file runs top-to-bottom and encodePNG is called above
// this line, so a let/const binding would still be in its dead zone.
var CRC_TABLE;

function crc32(buf) {
  if (!CRC_TABLE) {
    CRC_TABLE = new Int32Array(256);
    for (let n = 0; n < 256; n++) {
      let c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      CRC_TABLE[n] = c;
    }
  }
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return c ^ 0xffffffff;
}
