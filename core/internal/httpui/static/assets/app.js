/* GEX Suite frontend — vanilla JS, no build step.
   Talks to the Go backend: GET /api/state, GET /api/events (SSE: state|status),
   POST /api/streams {stream, connect}, GET/POST /api/sweeps (sweeper). */
"use strict";

/* ─────────────────────────── state ─────────────────────────── */
const S = {
  st: null,            // last State from the server
  pendingStatus: null, // status event that arrived before the first state
  view: "gex",         // gex | oi
  gexMode: "net",      // net | callput
  render: "graph",     // graph | table
  range: "all",        // all | near
  expiry: "all",       // all | yyyyMMdd
  heatAll: false,
};

const $ = (id) => document.getElementById(id);
const POS = "#22c55e", NEG = "#ef4444", GRID = "#1d2430", TXT = "#7d8997", ACCENT = "#3b82f6";
const MONO_FONT = "'JetBrains Mono', Consolas, monospace";
const SANS_FONT = "Inter, system-ui, sans-serif";

/* per-canvas hover x (CSS px), filled by wireHover */
S.hover = {};

/* ─────────────────────────── formatting ─────────────────────────── */
function fmtMoney(v, dp = 1) {
  if (v == null || !isFinite(v)) return "—";
  const a = Math.abs(v), sign = v < 0 ? "-" : "";
  if (a >= 1e12) return `${sign}$${(a / 1e12).toFixed(dp)}T`;
  if (a >= 1e9) return `${sign}$${(a / 1e9).toFixed(dp)}B`;
  if (a >= 1e6) return `${sign}$${(a / 1e6).toFixed(dp)}M`;
  if (a >= 1e3) return `${sign}$${(a / 1e3).toFixed(dp)}K`;
  return `${sign}$${a.toFixed(0)}`;
}
function fmtNum(v) {
  if (v == null || !isFinite(v)) return "—";
  const a = Math.abs(v);
  if (a >= 1e9) return `${(v / 1e9).toFixed(1)}B`;
  if (a >= 1e6) return `${(v / 1e6).toFixed(1)}M`;
  if (a >= 1e3) return `${(v / 1e3).toFixed(1)}K`;
  return `${Math.round(v)}`;
}
function fmtPct(v, dp = 2) {
  if (v == null || !isFinite(v)) return "—";
  return `${v >= 0 ? "+" : ""}${(v * 100).toFixed(dp)}%`;
}
function fmtStrike(v) {
  return Number.isInteger(v) ? String(v) : v.toFixed(1);
}
function fmtTime(ms) {
  if (!ms) return "—";
  return new Date(ms).toLocaleTimeString([], { hour12: false });
}
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
function expLabel(ymd) {
  return `${MONTHS[+ymd.slice(4, 6) - 1]} ${ymd.slice(6, 8)}`;
}
function dteOf(ymd) {
  const [y, m, d] = [ymd.slice(0, 4), ymd.slice(4, 6), ymd.slice(6, 8)].map(Number);
  return Math.round((Date.UTC(y, m - 1, d) - utcToday()) / 86400000);
}
function utcToday() {
  const n = new Date();
  return Date.UTC(n.getFullYear(), n.getMonth(), n.getDate());
}

/* ─────────────────────────── canvas helpers ─────────────────────────── */
function ctx2d(canvas) {
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth || canvas.parentElement.clientWidth;
  const h = parseInt(canvas.getAttribute("height"), 10) || 200;
  canvas.width = Math.round(w * dpr);
  canvas.height = Math.round(h * dpr);
  canvas.style.height = h + "px";
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { ctx, w, h };
}
function niceStep(span, n) {
  const raw = span / n, mag = Math.pow(10, Math.floor(Math.log10(raw))), norm = raw / mag;
  const nice = norm >= 5 ? 10 : norm >= 2.2 ? 5 : norm >= 1.2 ? 2.5 : norm >= 0.6 ? 1 : 0.5;
  return nice * mag;
}
function axisText(ctx, txt, x, y, align = "center", color = TXT) {
  ctx.fillStyle = color;
  ctx.textAlign = align;
  ctx.textBaseline = "middle";
  ctx.font = "10px Inter, system-ui, sans-serif";
  ctx.fillText(txt, x, y);
}

/* Diverging bar chart: rows of {x, pos, neg} drawn around a zero axis.
   opts.hoverX + opts.panelFor(i) add the top-left hover data panel; when not
   hovering, the panel shows opts.defaultIndex (e.g. the strike nearest spot). */
function drawDivergingBars(canvas, rows, { spot = null, posLabelFmt = fmtMoney, hoverX = null, defaultIndex = null, panelFor = null } = {}) {
  const { ctx, w, h } = ctx2d(canvas);
  ctx.clearRect(0, 0, w, h);
  const padL = 52, padR = 10, padT = 8, padB = 22;
  const pw = w - padL - padR, ph = h - padT - padB;
  if (!rows.length) { drawEmpty(ctx, w, h); return; }

  let maxAbs = 0;
  for (const r of rows) maxAbs = Math.max(maxAbs, Math.abs(r.pos), Math.abs(r.neg));
  if (maxAbs === 0) maxAbs = 1;

  const zero = padT + ph / 2;
  const yOf = (v) => zero - (v / maxAbs) * (ph / 2) * 0.94;

  // horizontal grid + y labels
  const step = niceStep(maxAbs, 4);
  ctx.strokeStyle = GRID; ctx.lineWidth = 1;
  for (let v = 0; Math.abs(v) <= maxAbs; v += step) {
    for (const sv of v === 0 ? [0] : [v, -v]) {
      if (Math.abs(sv) > maxAbs) continue;
      const y = yOf(sv);
      ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(w - padR, y); ctx.stroke();
      axisText(ctx, (sv >= 0 ? "" : "-") + posLabelFmt(Math.abs(sv)).replace("$-", "-$"), padL - 6, y, "right");
    }
  }

  // bars
  const bw = Math.max(2, (pw / rows.length) * 0.72);
  const xOf = (i) => padL + (i + 0.5) * (pw / rows.length);
  rows.forEach((r, i) => {
    const x = xOf(i) - bw / 2;
    if (r.pos > 0) { ctx.fillStyle = POS; ctx.fillRect(x, yOf(r.pos), bw, zero - yOf(r.pos)); }
    if (r.neg < 0) { ctx.fillStyle = NEG; ctx.fillRect(x, zero, bw, yOf(r.neg) - zero); }
  });

  // x labels (thin out)
  const every = Math.max(1, Math.ceil(rows.length / 12));
  ctx.fillStyle = TXT; ctx.textAlign = "center"; ctx.textBaseline = "top";
  ctx.font = "9.5px " + SANS_FONT;
  rows.forEach((r, i) => {
    if (i % every === 0 || i === rows.length - 1) ctx.fillText(fmtStrike(r.x), xOf(i), h - padB + 5);
  });

  // spot marker
  if (spot != null) {
    let si = rows.findIndex((r, i) => i + 1 >= rows.length || (rows[i].x <= spot && rows[i + 1].x > spot));
    if (rows[0].x > spot) si = 0;
    const sx = si < 0 ? padL : xOf(si) + (spot - rows[Math.max(si, 0)].x) / ((rows[Math.min(si + 1, rows.length - 1)].x - rows[Math.max(si, 0)].x) || 1) * (pw / rows.length);
    ctx.save();
    ctx.strokeStyle = ACCENT; ctx.setLineDash([4, 3]); ctx.lineWidth = 1.4;
    ctx.beginPath(); ctx.moveTo(sx, padT); ctx.lineTo(sx, padT + ph); ctx.stroke();
    ctx.restore();
    axisText(ctx, "Spot", sx, padT + 6, "center", ACCENT);
  }

  // hover: crosshair + bar highlight + top-left data panel
  const idx = hoverX != null
    ? Math.max(0, Math.min(rows.length - 1, Math.floor((hoverX - padL) / (pw / rows.length))))
    : defaultIndex;
  if (idx != null && idx >= 0 && idx < rows.length && panelFor) {
    const cx = xOf(idx);
    if (hoverX != null) {
      ctx.save();
      ctx.strokeStyle = "rgba(255,255,255,0.35)"; ctx.setLineDash([3, 3]); ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(cx, padT); ctx.lineTo(cx, padT + ph); ctx.stroke();
      ctx.restore();
      const r = rows[idx], bx = cx - bw / 2 - 0.5;
      ctx.strokeStyle = "rgba(255,255,255,0.6)"; ctx.lineWidth = 1;
      if (r.pos > 0) ctx.strokeRect(bx, yOf(r.pos) - 0.5, bw + 1, zero - yOf(r.pos) + 1);
      if (r.neg < 0) ctx.strokeRect(bx, zero - 0.5, bw + 1, yOf(r.neg) - zero + 1);
    }
    drawPanel(ctx, w, h, padL + 6, padT + 2, panelFor(idx).title, panelFor(idx).rows);
  }
}

/* Line chart for the spot→GEX profile with green/red fill halves.
   Draws 50%-opacity dashed verticals bracketing each negative→positive gamma
   crossing (the "gamma auction" zone), plus a hover data panel whose live
   section shows the total-GEX change over 5m/15m/30m/1h/2h + session. */
function drawProfile(canvas, pts, { spot = null, hoverX = null, hist = [], flip = null } = {}) {
  const { ctx, w, h } = ctx2d(canvas);
  ctx.clearRect(0, 0, w, h);
  const padL = 56, padR = 12, padT = 10, padB = 22;
  const pw = w - padL - padR, ph = h - padT - padB;
  if (!pts || pts.length < 2) { drawEmpty(ctx, w, h); return; }

  let lo = Infinity, hi = -Infinity, glo = Infinity, ghi = -Infinity;
  for (const p of pts) {
    if (p.totalGex < lo) lo = p.totalGex;
    if (p.totalGex > hi) hi = p.totalGex;
    if (p.spot < glo) glo = p.spot;
    if (p.spot > ghi) ghi = p.spot;
  }
  if (lo > 0) lo = 0; if (hi < 0) hi = 0;
  const xOf = (s) => padL + ((s - glo) / (ghi - glo)) * pw;
  const yOf = (v) => padT + (1 - (v - lo) / (hi - lo)) * ph;
  const zeroY = yOf(0);

  // grid
  ctx.strokeStyle = GRID; ctx.lineWidth = 1;
  const step = niceStep(hi - lo, 4);
  for (let v = Math.ceil(lo / step) * step; v <= hi; v += step) {
    const y = yOf(v);
    ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(w - padR, y); ctx.stroke();
    axisText(ctx, fmtMoney(Math.abs(v)).replace("$", v < 0 ? "-$" : "$"), padL - 6, y, "right");
  }
  ctx.save(); ctx.strokeStyle = TXT; ctx.setLineDash([3, 3]);
  ctx.beginPath(); ctx.moveTo(padL, zeroY); ctx.lineTo(w - padR, zeroY); ctx.stroke(); ctx.restore();

  // gamma auction zone: bracket the negative→positive gamma crossing nearest
  // the current spot with two 50%-opacity dashed verticals (smaller
  // noise-scale crossings elsewhere are ignored)
  let auctionZone = null;
  {
    const zones = [];
    for (let i = 0; i + 1 < pts.length; i++) {
      const a = pts[i], b = pts[i + 1];
      if (a.totalGex < 0 && b.totalGex > 0) zones.push([a, b]);
    }
    if (zones.length) {
      const ref = spot != null ? spot : (glo + ghi) / 2;
      zones.sort((p, q) =>
        Math.abs((p[0].spot + p[1].spot) / 2 - ref) - Math.abs((q[0].spot + q[1].spot) / 2 - ref));
      auctionZone = zones[0];
    }
  }
  if (auctionZone) {
    const xa = xOf(auctionZone[0].spot), xb = xOf(auctionZone[1].spot);
    ctx.save();
    ctx.globalAlpha = 0.5;
    ctx.strokeStyle = "#cbd5e1"; ctx.setLineDash([5, 4]); ctx.lineWidth = 1.2;
    ctx.beginPath(); ctx.moveTo(xa, padT); ctx.lineTo(xa, padT + ph); ctx.stroke();
    ctx.beginPath(); ctx.moveTo(xb, padT); ctx.lineTo(xb, padT + ph); ctx.stroke();
    ctx.restore();
    if (xb - xa > 34) {
      ctx.save(); ctx.globalAlpha = 0.6;
      axisText(ctx, "gamma auction", (xa + xb) / 2, padT + 18, "center", "#cbd5e1");
      ctx.restore();
    }
  }

  // area + line split at zero via clipping
  const trace = () => {
    ctx.beginPath();
    pts.forEach((p, i) => i ? ctx.lineTo(xOf(p.spot), yOf(p.totalGex)) : ctx.moveTo(xOf(p.spot), yOf(p.totalGex)));
  };
  const fillArea = (color, clipTop) => {
    ctx.save(); ctx.beginPath();
    if (clipTop) ctx.rect(padL, padT, pw, Math.max(0, zeroY - padT));
    else ctx.rect(padL, zeroY, pw, Math.max(0, padT + ph - zeroY));
    ctx.clip();
    ctx.beginPath(); ctx.moveTo(xOf(pts[0].spot), zeroY);
    pts.forEach((p) => ctx.lineTo(xOf(p.spot), yOf(p.totalGex)));
    ctx.lineTo(xOf(pts[pts.length - 1].spot), zeroY); ctx.closePath();
    ctx.fillStyle = color; ctx.globalAlpha = 0.16; ctx.fill(); ctx.globalAlpha = 1;
    trace(); ctx.strokeStyle = clipTop ? POS : NEG; ctx.lineWidth = 1.8; ctx.stroke();
    ctx.restore();
  };
  fillArea(POS, true);
  fillArea(NEG, false);

  // current spot marker (solid accent)
  if (spot != null && spot > glo && spot < ghi) {
    ctx.save();
    ctx.strokeStyle = ACCENT; ctx.setLineDash([4, 3]); ctx.lineWidth = 1.4;
    ctx.beginPath(); ctx.moveTo(xOf(spot), padT); ctx.lineTo(xOf(spot), padT + ph); ctx.stroke();
    ctx.restore();
    axisText(ctx, "Spot", xOf(spot), padT + 7, "center", ACCENT);
  }

  // x labels in $
  const stepX = niceStep(ghi - glo, 8);
  for (let s = Math.ceil(glo / stepX) * stepX; s <= ghi; s += stepX) {
    axisText(ctx, "$" + s.toFixed(0), xOf(s), h - padB + 8);
  }

  // hover: nearest profile point + panel (hover info, or live windows)
  let hoverP = null, inZone = false;
  if (hoverX != null && hoverX >= padL && hoverX <= w - padR) {
    let s = glo + ((hoverX - padL) / pw) * (ghi - glo);
    hoverP = pts.reduce((best, p) => Math.abs(p.spot - s) < Math.abs(best.spot - s) ? p : best, pts[0]);
    if (auctionZone && s >= auctionZone[0].spot && s <= auctionZone[1].spot) inZone = true;
    ctx.save();
    ctx.strokeStyle = "rgba(255,255,255,0.35)"; ctx.setLineDash([3, 3]); ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(xOf(hoverP.spot), padT); ctx.lineTo(xOf(hoverP.spot), padT + ph); ctx.stroke();
    ctx.restore();
    ctx.beginPath(); ctx.arc(xOf(hoverP.spot), yOf(hoverP.totalGex), 3.2, 0, Math.PI * 2);
    ctx.fillStyle = "#fff"; ctx.fill();
  }
  const rows = [];
  if (hoverP) {
    rows.push({ k: "Level", v: "$" + hoverP.spot.toFixed(0) });
    rows.push({ k: "Projected GEX", v: fmtMoney(hoverP.totalGex), c: hoverP.totalGex >= 0 ? POS : NEG });
    if (inZone) rows.push({ k: "Zone", v: "gamma auction", c: "#cbd5e1" });
    if (spot != null) rows.push({ k: "Δ vs spot", v: fmtMoney(hoverP.totalGex - (hist.length ? hist[hist.length - 1].gex : 0)).replace("$-", "-$"), c: TXT });
  } else {
    rows.push({ k: "Spot", v: spot != null ? "$" + spot.toFixed(2) : "—" });
    if (flip != null) rows.push({ k: "Zero gamma", v: "$" + fmtStrike(flip) });
  }
  const live = hist.length ? hist[hist.length - 1] : null;
  if (live) {
    rows.push({ k: "GEX now", v: fmtMoney(live.gex), c: live.gex >= 0 ? POS : NEG });
    rows.push(...windowRows(hist));
  }
  drawPanel(ctx, w, h, padL + 8, padT + 4, hoverP ? "PRICE PROFILE" : "TOTAL GEX — LIVE", rows);
}

/* Small line chart for intraday ΔGEX with a hover data panel showing the
   value and its change over 5m/15m/30m/1h/2h + session at the hovered time. */
function drawSpark(canvas, hist, { hoverX = null } = {}) {
  const { ctx, w, h } = ctx2d(canvas);
  ctx.clearRect(0, 0, w, h);
  if (!hist || hist.length < 2) { drawEmpty(ctx, w, h, "no intraday samples yet"); return; }
  const padL = 8, padR = 8, padT = 10, padB = 6;
  const pw = w - padL - padR, ph = h - padT - padB;
  const vals = hist.map((p) => p.gex);
  let lo = Math.min(...vals), hi = Math.max(...vals);
  if (lo === hi) { lo -= 1; hi += 1; }
  const xOf = (i) => padL + (i / (vals.length - 1)) * pw;
  const yOf = (v) => padT + (1 - (v - lo) / (hi - lo)) * ph;

  const crossesZero = lo < 0 && hi > 0;
  if (crossesZero) {
    ctx.save(); ctx.strokeStyle = TXT; ctx.setLineDash([3, 3]);
    ctx.beginPath(); ctx.moveTo(padL, yOf(0)); ctx.lineTo(w - padR, yOf(0)); ctx.stroke(); ctx.restore();
  }
  ctx.beginPath();
  vals.forEach((v, i) => (i ? ctx.lineTo(xOf(i), yOf(v)) : ctx.moveTo(xOf(i), yOf(v))));
  ctx.strokeStyle = vals[vals.length - 1] >= (crossesZero ? 0 : vals[0]) ? POS : NEG;
  ctx.lineWidth = 1.6; ctx.stroke();
  ctx.lineTo(xOf(vals.length - 1), padT + ph); ctx.lineTo(xOf(0), padT + ph); ctx.closePath();
  ctx.globalAlpha = 0.12; ctx.fillStyle = ctx.strokeStyle; ctx.fill(); ctx.globalAlpha = 1;

  // hover: nearest sample + panel (windows measured from the hovered point)
  let idx = hist.length - 1;
  if (hoverX != null) {
    idx = Math.max(0, Math.min(hist.length - 1, Math.round(((hoverX - padL) / pw) * (vals.length - 1))));
    ctx.save();
    ctx.strokeStyle = "rgba(255,255,255,0.4)"; ctx.setLineDash([3, 3]); ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(xOf(idx), padT); ctx.lineTo(xOf(idx), padT + ph); ctx.stroke();
    ctx.restore();
    ctx.beginPath(); ctx.arc(xOf(idx), yOf(vals[idx]), 3, 0, Math.PI * 2);
    ctx.fillStyle = "#fff"; ctx.fill();
  }
  const p = hist[idx];
  const rows = [
    { k: "GEX", v: fmtMoney(p.gex).replace("$-", "-$"), c: p.gex >= 0 ? POS : NEG },
    ...windowRows(hist, idx),
  ];
  drawPanel(ctx, w, h, padL + 6, padT + 2,
    hoverX != null ? fmtTime(p.t) : "LATEST " + fmtTime(p.t), rows);
}

function drawEmpty(ctx, w, h, msg = "no data — press Connect to start the feed") {
  ctx.fillStyle = TXT; ctx.textAlign = "center"; ctx.textBaseline = "middle";
  ctx.font = "12px " + SANS_FONT;
  ctx.fillText(msg, w / 2, h / 2);
}

/* ── hover data panel (top-left overlay inside a chart) ── */
function rrect(ctx, x, y, w, h, r) {
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}
/* rows: [{k, v, c}] — label left (muted), value right-aligned (color). */
function drawPanel(ctx, cw, ch, x, y, title, rows) {
  if (!rows || !rows.length) return;
  ctx.save();
  const pad = 8, rowH = 13, titleH = title ? 15 : 0;
  ctx.font = "600 10px " + SANS_FONT;
  let w = 0;
  for (const r of rows) w = Math.max(w, ctx.measureText(r.k).width + 10 + ctx.measureText(r.v).width);
  if (title) { ctx.font = "700 10.5px " + SANS_FONT; w = Math.max(w, ctx.measureText(title).width); }
  w += pad * 2;
  const h = titleH + rows.length * rowH + pad * 2 - 3;
  x = Math.max(6, Math.min(x, cw - w - 6));
  y = Math.max(4, Math.min(y, ch - h - 4));
  rrect(ctx, x, y, w, h, 6);
  ctx.fillStyle = "rgba(10,13,18,0.9)";
  ctx.fill();
  ctx.strokeStyle = GRID; ctx.lineWidth = 1; ctx.stroke();
  let ty = y + pad + 4;
  if (title) {
    ctx.font = "700 10.5px " + SANS_FONT;
    ctx.fillStyle = "#dbe2ea"; ctx.textAlign = "left"; ctx.textBaseline = "middle";
    ctx.fillText(title, x + pad, ty);
    ty += titleH;
  }
  ctx.font = "600 10px " + SANS_FONT;
  for (const r of rows) {
    ctx.fillStyle = TXT; ctx.textAlign = "left";
    ctx.fillText(r.k, x + pad, ty);
    ctx.fillStyle = r.c || "#dbe2ea"; ctx.textAlign = "right";
    ctx.fillText(r.v, x + w - pad, ty);
    ty += rowH;
  }
  ctx.restore();
}

/* % change over the last 5m/15m/30m/1h/2h + session, only where history
   reaches back far enough (rows auto-appear as the session ages). */
function windowRows(hist, tIdx) {
  if (!hist || hist.length < 2) return [];
  const end = hist[tIdx != null ? tIdx : hist.length - 1];
  const out = [];
  for (const [m, label] of [[5, "5m"], [15, "15m"], [30, "30m"], [60, "1h"], [120, "2h"]]) {
    const target = end.t - m * 60000;
    let base = null;
    for (const p of hist) { if (p.t <= target) base = p; else break; }
    if (!base) continue;
    out.push(deltaRow(label, end.gex - base.gex, base.gex));
  }
  out.push(deltaRow("session", end.gex - hist[0].gex, hist[0].gex));
  return out;
}
function deltaRow(label, d, base) {
  let pctS = "";
  if (base !== 0 && isFinite(base)) {
    const pct = Math.max(-999, Math.min(999, (d / Math.abs(base)) * 100));
    pctS = ` (${pct >= 0 ? "+" : ""}${pct.toFixed(1)}%)`;
  }
  return { k: label, v: fmtMoney(d).replace("$-", "-$") + pctS, c: d >= 0 ? POS : NEG };
}

/* hover wiring: track x, redraw just that chart */
function wireHover(id, redraw) {
  const c = $(id);
  c.addEventListener("mousemove", (e) => {
    const r = c.getBoundingClientRect();
    const x = e.clientX - r.left;
    if (S.hover[id] !== x) { S.hover[id] = x; redraw(); }
  });
  c.addEventListener("mouseleave", () => {
    if (S.hover[id] != null) { delete S.hover[id]; redraw(); }
  });
}

/* ─────────────────────────── rendering ─────────────────────────── */
function render() {
  const st = S.st;
  if (!st) return;
  renderStatus();
  renderQuote();
  renderTiles();
  renderStats();
  renderClasses();
  renderStrikeProfile();
  renderProfile();
  renderSkew();
  renderHeatmap();
  renderOI();
  renderDeltaGex();
  renderSignals();
  renderSqueeze();
}

/* setText touches the DOM only on change — the status re-renders once per
   live tick, and rewriting stable nodes would thrash layout (and any pointer
   aiming at the Connect/Disconnect button). */
function setText(el, v) {
  if (el.textContent !== v) el.textContent = v;
}
function setClass(el, v) {
  if (el.className !== v) el.className = v;
}

function renderStatus() {
  const s = S.st.status;
  const live = s.connected;
  setClass($("live-badge"), `badge ${live ? "badge-on" : "badge-off"}`);
  setText($("live-label"), live ? "LIVE" : "SNAPSHOT");
  const btn = $("master-btn");
  setText(btn, live ? "Disconnect" : "Connect");
  setClass(btn, `btn ${live ? "btn-disconnect" : "btn-connect"}`);
  for (const id of ["underlying", "options"]) {
    const on = s.streams.find((x) => x.id === id)?.connected;
    $(`stream-${id}`).checked = !!on;
    const el = $(`state-${id}`);
    setText(el, on ? "live" : "off");
    setClass(el, `stream-state ${on ? "on" : ""}`);
  }
  setText($("stream-mode"), s.mode || "—");
  setText($("stream-updates"), s.updates.toLocaleString());
  setText($("stream-last"), s.lastUpdateMs ? fmtTime(s.lastUpdateMs) : "—");
  setText($("stream-saved"), s.savedAsOfMs ? fmtTime(s.savedAsOfMs) : "—");
  setText($("foot-conn"), live
    ? `Connected · ${S.st.ticker} · ${S.st.contracts} contracts`
    : (s.savedAsOfMs
      ? `Disconnected · showing saved snapshot from ${new Date(s.savedAsOfMs).toLocaleString()}`
      : "Disconnected · no saved snapshot yet — press Connect to load the feed"));
  setText($("stream-hint"), live
    ? "Disconnect to freeze the view: the strike profile and the expiration table keep rendering from the saved state, no live feed needed."
    : (s.savedAsOfMs
      ? "Disconnected — rendering the saved snapshot. Press Connect to resume the live feed."
      : "No data yet — press Connect once to load the synthetic feed; the chain it builds is then saved for offline viewing."));
}

function renderQuote() {
  const snap = S.st.snapshot;
  $("quote-ticker").textContent = S.st.ticker;
  $("quote-contracts").textContent =
    (S.st.contracts ? ` · ${S.st.contracts} contracts` : "") +
    (S.st.tradingClass && S.st.tradingClass !== S.st.ticker ? ` · options: ${S.st.tradingClass}` : "");
  $("quote-iv").textContent = (S.st.baselineIv * 100).toFixed(1) + "%";
  const exps = expiryList();
  $("quote-expiries").textContent = exps.length ? exps.map((e) => `${expLabel(e.expiry)} (${Math.max(0, Math.round(e.dte))}d)`).join(", ") : "—";

  const live = S.st.status.connected;
  const updated = live && S.st.status.lastUpdateMs ? S.st.status.lastUpdateMs : snap?.asOfMs;
  $("quote-updated").textContent = updated ? fmtTime(updated) : "—";
  $("quote-updated").style.color = live ? "var(--pos)" : "var(--warn)";

  if (!snap) { $("quote-price").textContent = "—"; $("quote-chg").textContent = ""; return; }
  $("quote-price").textContent = "$" + snap.spot.toFixed(2);
  const hist = S.st.history || [];
  const open = hist.length ? hist[0].spot : null;
  const chgEl = $("quote-chg");
  if (open) {
    const chg = (snap.spot - open) / open;
    chgEl.textContent = `${chg >= 0 ? "+" : ""}${(chg * 100).toFixed(2)}% session`;
    chgEl.className = `quote-chg ${chg >= 0 ? "pos" : "neg"}`;
  } else chgEl.textContent = "";
}

function expiryList() {
  return S.st.snapshot?.perExpiry || [];
}

function renderStats() {
  const snap = S.st.snapshot;
  const set = (id, v, cls) => { const el = $(id); el.textContent = v; if (cls !== undefined) el.className = `stat-value ${cls}`; };
  if (!snap) {
    ["st-spot", "st-net", "st-call", "st-put", "st-total", "st-callwall", "st-putwall", "st-flip"].forEach((id) => set(id, "—"));
    $("st-regime").textContent = "";
    return;
  }
  set("st-spot", "$" + snap.spot.toFixed(2));
  set("st-net", fmtMoney(snap.totals.gex), snap.totals.gex >= 0 ? "pos" : "neg");
  $("st-regime").textContent = snap.regime === "POSITIVE_GAMMA" ? "long gamma · dampened" : "short gamma · amplified";
  set("st-call", fmtMoney(snap.perStrike.reduce((a, s) => a + Math.max(0, s.callGex), 0)), "pos");
  $("st-put").textContent = fmtMoney(snap.perStrike.reduce((a, s) => a + Math.min(0, s.putGex), 0));
  set("st-total", fmtMoney(snap.totals.gex), snap.totals.gex >= 0 ? "pos" : "neg");
  set("st-callwall", snap.callWall.hasWall ? "$" + fmtStrike(snap.callWall.strike) : "—", "pos");
  set("st-putwall", snap.putWall.hasWall ? "$" + fmtStrike(snap.putWall.strike) : "—", "neg");
  set("st-flip", snap.hasGammaFlip ? "$" + fmtStrike(snap.gammaFlipSpot) : "no flip");
  $("st-callwall-dist").textContent = snap.callWall.hasWall ? distPct(snap.spot, snap.callWall.strike) : "";
  $("st-putwall-dist").textContent = snap.putWall.hasWall ? distPct(snap.spot, snap.putWall.strike) : "";
  $("st-flip-dist").textContent = snap.hasGammaFlip ? distPct(snap.spot, snap.gammaFlipSpot) : "";
}
function distPct(spot, lvl) {
  const d = (lvl - spot) / spot;
  return `${d >= 0 ? "+" : ""}${(d * 100).toFixed(2)}%`;
}

/* Per-class segregation (Snapshot.Classes): present only on multi-class
   (index) chains — AM monthlies vs PM weeklies stay comparable instead of
   blended. Single-class chains keep the strip hidden. */
function renderClasses() {
  const strip = $("panel-classes");
  const cards = S.st.snapshot?.classes || [];
  if (!cards.length) { strip.classList.add("hidden"); return; }
  strip.classList.remove("hidden");
  $("class-cards").innerHTML = cards.map((c) => {
    const t = c.totals || {};
    const cls = t.gex >= 0 ? "pos" : "neg";
    const wall = (w) => (w && w.hasWall ? "$" + fmtStrike(w.strike) : "—");
    return `<div class="class-card">
      <div class="class-name">${c.tradingClass}
        ${c.settlement ? `<span class="class-badge">${c.settlement}</span>` : ""}
      </div>
      <div class="class-metrics">
        <div class="tile"><div class="tile-label">Contracts</div><div class="tile-value">${c.contracts}</div></div>
        <div class="tile"><div class="tile-label">Net GEX</div><div class="tile-value ${cls}">${fmtMoney(t.gex)}</div></div>
        <div class="tile"><div class="tile-label">Call Wall</div><div class="tile-value pos">${wall(c.callWall)}</div></div>
        <div class="tile"><div class="tile-label">Put Wall</div><div class="tile-value neg">${wall(c.putWall)}</div></div>
      </div>
    </div>`;
  }).join("");
}

/* strike rows for the bar chart honouring the expiry filter + near range */
function strikeRows() {
  const snap = S.st.snapshot;
  if (!snap) return [];
  let rows;
  if (S.expiry === "all") rows = snap.perStrike;
  else rows = expiryList().find((e) => e.expiry === S.expiry)?.perStrike || [];
  if (S.range === "near") {
    let i = rows.findIndex((r) => r.strike >= snap.spot);
    if (i < 0) i = rows.length - 1;
    rows = rows.slice(Math.max(0, i - 8), i + 9);
  }
  return rows;
}

function renderStrikeProfile() {
  const snap = S.st.snapshot;
  const rows = strikeRows();
  const canvas = $("chart-strike");
  const table = $("strike-table");
  $("panel-strike").querySelector('[data-range]').parentElement.style.display = S.render === "table" ? "none" : "";
  if (!snap || !rows.length) {
    if (S.render === "table") table.innerHTML = `<div class="muted small" style="padding:20px;text-align:center">no data yet</div>`;
    else { const { ctx, w, h } = ctx2d(canvas); ctx.clearRect(0, 0, w, h); drawEmpty(ctx, w, h); }
    return;
  }
  if (S.render === "table") {
    canvas.classList.add("hidden"); table.classList.remove("hidden");
    const fmt = (v) => fmtMoney(v).replace("$-", "-$");
    table.innerHTML = `<table class="data"><thead><tr><th>Strike</th><th>Call GEX</th><th>Put GEX</th><th>Net GEX</th></tr></thead><tbody>` +
      rows.map((r) => `<tr><td>${fmtStrike(r.strike)}${nearSpot(r.strike) ? ' <span class="accent">◆</span>' : ""}</td>` +
        `<td class="pos">${fmt(r.callGex)}</td><td class="neg">${fmt(r.putGex)}</td>` +
        `<td class="${r.netGex >= 0 ? "pos" : "neg"}">${fmt(r.netGex)}</td></tr>`).join("") + `</tbody></table>`;
  } else {
    canvas.classList.remove("hidden"); table.classList.add("hidden");
    const panelFor = (i) => {
      const r = rows[i];
      return { title: "STRIKE " + fmtStrike(r.strike) + (nearSpot(r.strike) ? " — SPOT" : ""), rows: [
        { k: "Call GEX", v: fmtMoney(r.callGex), c: POS },
        { k: "Put GEX", v: fmtMoney(r.putGex), c: NEG },
        { k: "Net GEX", v: fmtMoney(r.netGex), c: r.netGex >= 0 ? POS : NEG },
      ] };
    };
    // default panel: the strike nearest spot
    let def = 0, best = Infinity;
    rows.forEach((r, i) => { const d = Math.abs(r.strike - snap.spot); if (d < best) { best = d; def = i; } });
    if (S.gexMode === "net") {
      drawDivergingBars(canvas, rows.map((r) => ({ x: r.strike, pos: Math.max(0, r.netGex), neg: Math.min(0, r.netGex) })),
        { spot: snap.spot, hoverX: S.hover["chart-strike"], defaultIndex: def, panelFor });
    } else {
      drawDivergingBars(canvas, rows.map((r) => ({ x: r.strike, pos: r.callGex, neg: r.putGex })),
        { spot: snap.spot, hoverX: S.hover["chart-strike"], defaultIndex: def, panelFor });
    }
  }
}
function nearSpot(k) { return S.st.snapshot && Math.abs(k - S.st.snapshot.spot) < 1e-9; }

function renderProfile() {
  const snap = S.st.snapshot;
  drawProfile($("chart-profile"), snap?.spotProfile || [], {
    spot: snap?.spot || null,
    hoverX: S.hover["chart-profile"],
    hist: S.st.history || [],
    flip: snap?.hasGammaFlip ? snap.gammaFlipSpot : null,
  });
  $("profile-note").textContent = snap?.spotProfile?.length
    ? `Projected total GEX across ±20% of spot · ${snap.spotProfile.length} grid points`
    : "Projected total GEX at different spot levels (constant IV)";
}

function renderHeatmap() {
  const snap = S.st.snapshot;
  const tbl = $("heat-table"), foot = $("heat-foot");
  if (!snap || !snap.perExpiry?.length) {
    tbl.innerHTML = ""; foot.textContent = "no data yet"; return;
  }
  const exps = snap.perExpiry;
  // collect all strikes across expiries, descending like the reference
  const strikeSet = new Set();
  exps.forEach((e) => e.perStrike.forEach((r) => strikeSet.add(r.strike)));
  const all = [...strikeSet].sort((a, b) => b - a);
  let rows = all;
  if (!S.heatAll) {
    let i = all.findIndex((k) => k <= snap.spot);
    if (i < 0) i = all.length - 1;
    rows = all.slice(Math.max(0, i - 8), i + 9);
  }
  const cellOf = (expiry, k) => expiry.perStrike.find((r) => r.strike === k)?.netGex;
  let maxAbs = 0;
  exps.forEach((e) => e.perStrike.forEach((r) => { maxAbs = Math.max(maxAbs, Math.abs(r.netGex)); }));
  if (!maxAbs) maxAbs = 1;

  const head = `<tr><th>Strike</th>` + exps.map((e) => {
    const d = Math.max(0, Math.round(e.dte));
    return `<th>${expLabel(e.expiry)}<small>${d}d · ${(e.sigmaBar * 100).toFixed(0)}%</small></th>`;
  }).join("") + "</tr>";

  const body = rows.map((k) => {
    const spotRow = nearSpot(k);
    const cls = spotRow ? ' class="spot-row"' : "";
    return `<tr${cls}><td>${fmtStrike(k)}${spotRow ? " ←" : ""}</td>` +
      exps.map((e) => {
        const v = cellOf(e, k);
        if (v == null) return `<td></td>`;
        const a = Math.min(1, Math.abs(v) / maxAbs);
        const bg = v >= 0 ? `rgba(34,197,94,${(a * 0.75).toFixed(3)})` : `rgba(239,68,68,${(a * 0.75).toFixed(3)})`;
        return `<td style="background:${bg}">${fmtMoney(v)}</td>`;
      }).join("") + "</tr>";
  }).join("");

  tbl.innerHTML = `<thead>${head}</thead><tbody>${body}</tbody>`;
  foot.innerHTML = `Showing ${rows.length} of ${all.length} strikes around spot · <a id="heat-link">${S.heatAll ? "around spot" : "view all"}</a>`;
  const link = $("heat-link");
  if (link) link.onclick = () => { S.heatAll = !S.heatAll; renderHeatmap(); syncHeatToggle(); };
}
function syncHeatToggle() {
  $("heat-toggle").textContent = S.heatAll ? "around spot" : "view all";
}

function renderOI() {
  const oi = S.st.perStrikeOi || [];
  const snap = S.st.snapshot;
  let rows = oi;
  if (S.expiry !== "all" && snap) {
    // per-expiry OI isn't aggregated server-side; filter by strikes present in that expiry
    const keep = new Set(expiryList().find((e) => e.expiry === S.expiry)?.perStrike.map((r) => r.strike) || []);
    rows = oi.filter((r) => keep.has(r.strike));
  }
  drawDivergingBars($("chart-oi"), rows.map((r) => ({ x: r.strike, pos: r.callOi, neg: -r.putOi })),
    {
      spot: snap?.spot || null, posLabelFmt: (v) => fmtNum(v), hoverX: S.hover["chart-oi"],
      panelFor: (i) => {
        const r = rows[i];
        return { title: "STRIKE " + fmtStrike(r.strike), rows: [
          { k: "Call OI", v: fmtNum(r.callOi), c: POS },
          { k: "Put OI", v: fmtNum(r.putOi), c: NEG },
          { k: "Net OI", v: fmtNum(r.callOi - r.putOi), c: r.callOi - r.putOi >= 0 ? POS : NEG },
        ] };
      },
    });
}

function renderDeltaGex() {
  const hist = S.st.history || [];
  if (hist.length >= 2) {
    const first = hist[0], last = hist[hist.length - 1];
    const recent = hist.find((p) => p.t >= last.t - 15 * 60000) || first;
    const ds = last.gex - first.gex, dr = last.gex - recent.gex;
    const el1 = $("dgex-session"), el2 = $("dgex-recent");
    el1.textContent = fmtMoney(ds); el1.style.color = ds >= 0 ? POS : NEG;
    el2.textContent = fmtMoney(dr); el2.style.color = dr >= 0 ? POS : NEG;
  } else {
    $("dgex-session").textContent = "—"; $("dgex-recent").textContent = "—";
  }
  drawSpark($("chart-dgex"), hist, { hoverX: S.hover["chart-dgex"] });
}

function renderSignals() {
  const snap = S.st.snapshot;
  const body = $("signals-body");
  if (!snap) { body.innerHTML = `<div class="muted small">Connect a stream or load a saved snapshot.</div>`; return; }
  const cards = [];
  if (snap.totals.gex < 0) {
    cards.push(["Volatility", "STRONG", "tag-strong",
      `Short gamma: dealer hedging amplifies price movements. Net GEX ${fmtMoney(snap.totals.gex)}.`]);
  } else {
    cards.push(["Stability", "CALM", "tag-info",
      `Long gamma: dealer hedging dampens moves and mean-reverts price. Net GEX ${fmtMoney(snap.totals.gex)}.`]);
  }
  if (snap.callWall.hasWall) {
    const d = (snap.callWall.strike - snap.spot) / snap.spot;
    const tag = Math.abs(d) < 0.015 ? "MODERATE" : "STRONG";
    cards.push(["Resistance", tag, tag === "STRONG" ? "tag-strong" : "tag-moderate",
      `Call wall at $${fmtStrike(snap.callWall.strike)} (${distPct(snap.spot, snap.callWall.strike)}); pinning likely below it.`]);
  }
  if (snap.putWall.hasWall) {
    cards.push(["Support", "MODERATE", "tag-moderate",
      `Put wall at $${fmtStrike(snap.putWall.strike)} (${distPct(snap.spot, snap.putWall.strike)}); hedging flips to buying if breached.`]);
  }
  if (snap.hasGammaFlip) {
    const d = Math.abs(snap.spot - snap.gammaFlipSpot) / snap.spot;
    cards.push(["Zero Gamma", d < 0.01 ? "NEAR" : "WATCH", d < 0.01 ? "tag-moderate" : "tag-info",
      `Flip at $${fmtStrike(snap.gammaFlipSpot)} (${distPct(snap.spot, snap.gammaFlipSpot)}). Vol regime changes on a clean break.`]);
  }
  body.innerHTML = cards.map(([title, tag, cls, text]) =>
    `<div class="signal"><div class="signal-head"><span>${title}</span><span class="signal-tag ${cls}">${tag}</span></div><p>${text}</p></div>`
  ).join("");
}

function renderSqueeze() {
  const snap = S.st.snapshot;
  const body = $("squeeze-body");
  if (!snap) { body.innerHTML = `<div class="muted small">Heuristic score — needs a snapshot.</div>`; return; }

  const oi = S.st.perStrikeOi || [];
  const totCall = oi.reduce((a, r) => a + r.callOi, 0), totPut = oi.reduce((a, r) => a + r.putOi, 0);
  const callShare = totCall + totPut > 0 ? totCall / (totCall + totPut) : 0.5;

  const hist = S.st.history || [];
  const dGex = hist.length >= 2 ? hist[hist.length - 1].gex - hist[0].gex : 0;

  const fGamma = snap.totals.gex < 0 ? 18 : 5;
  let fWall = 4;
  if (snap.callWall.hasWall) {
    const d = (snap.callWall.strike - snap.spot) / snap.spot;
    fWall = d < 0.01 ? 25 : d < 0.02 ? 18 : d < 0.035 ? 10 : 4;
  }
  const fFlow = Math.min(25, 6 + Math.abs(dGex) / Math.max(1e6, Math.abs(snap.totals.gex) * 0.25) * 19);
  const fOI = Math.round(Math.abs(callShare - 0.5) * 40);
  const score = Math.min(100, Math.round(fGamma + fWall + fFlow + fOI));
  const label = score >= 75 ? ["IMMINENT", "var(--neg)"] : score >= 50 ? ["LIKELY", "var(--warn)"] : score >= 25 ? ["POSSIBLE", "var(--warn)"] : ["UNLIKELY", "var(--muted)"];

  const factor = (name, v, max) =>
    `<div class="factor"><div class="factor-head"><span class="muted">${name}</span><span>${Math.round(v)}/${max}</span></div>` +
    `<div class="factor-bar"><div class="factor-fill" style="width:${(v / max) * 100}%"></div></div></div>`;

  const trigger = snap.hasGammaFlip ? snap.gammaFlipSpot : (snap.putWall.hasWall ? snap.putWall.strike : null);
  const checks = [];
  checks.push(snap.totals.gex < 0
    ? `Short gamma environment (${fmtMoney(snap.totals.gex)} net GEX)`
    : `Long gamma environment (${fmtMoney(snap.totals.gex)} net GEX)`);
  if (snap.callWall.hasWall) checks.push(`Call wall at $${fmtStrike(snap.callWall.strike)}`);
  if (snap.hasGammaFlip) checks.push(`Zero gamma at $${fmtStrike(snap.gammaFlipSpot)} (${distPct(snap.spot, snap.gammaFlipSpot)})`);
  checks.push(callShare > 0.5 ? `Call OI heavier (${(callShare * 100).toFixed(0)}% of book)` : `Put OI heavier (${((1 - callShare) * 100).toFixed(0)}% of book)`);

  body.innerHTML = `
    <div class="signal"><div class="signal-head"><span>${snap.totals.gex < 0 ? "Bullish Squeeze" : "Squeeze Setup"}</span>
      <span class="signal-tag ${score >= 50 ? "tag-strong" : "tag-moderate"}">${label[0]}</span></div>
      <div class="squeeze-score"><span class="muted small">PROBABILITY SCORE</span><span class="score" style="color:${label[1]}">${score}<span class="muted small">/100</span></span></div>
    </div>
    ${factor("Gamma Regime", fGamma, 25)}
    ${factor("Call Wall Proximity", fWall, 25)}
    ${factor("Flow Alignment", fFlow, 25)}
    ${factor("Delta OI Alignment", fOI, 20)}
    <div class="muted small" style="margin-top:10px;letter-spacing:.5px">KEY LEVELS</div>
    <div class="stream-stats">
      <div><span>Current</span><span>$${snap.spot.toFixed(2)}</span></div>
      <div><span>Call Wall</span><span>${snap.callWall.hasWall ? "$" + fmtStrike(snap.callWall.strike) : "—"}</span></div>
      <div><span>Trigger</span><span>${trigger ? "$" + fmtStrike(trigger) : "—"}</span></div>
    </div>
    <div class="muted small" style="margin-top:10px;letter-spacing:.5px">SETUP ANALYSIS</div>
    <ul class="check-list">${checks.map((c) => `<li>${c}</li>`).join("")}</ul>
    <div class="hint muted small" style="margin-top:8px">Heuristic demo score from regime, wall distance, ΔGEX flow and OI mix — not investment advice.</div>`;
}

/* expiry tiles: ODTE / Weekly / Monthly share of |GEX| */
function renderTiles() {
  const exps = expiryList();
  const tot = exps.reduce((a, e) => a + Math.abs(e.totals.gex), 0);
  const share = (lo, hi) => {
    const s = exps.filter((e) => e.dte >= lo && e.dte < hi).reduce((a, e) => a + Math.abs(e.totals.gex), 0);
    return tot > 0 ? (100 * s / tot).toFixed(1) + "%" : "—";
  };
  $("tile-odte").textContent = exps.length ? share(0, 2) : "—";
  $("tile-weekly").textContent = exps.length ? share(2, 9) : "—";
  $("tile-monthly").textContent = exps.length ? share(9, 46) : "—";
}

/* ─────────────────────────── server wiring ─────────────────────────── */
async function fetchState(ticker) {
  const q = ticker ? `?ticker=${encodeURIComponent(ticker)}` : "";
  const r = await fetch("/api/state" + q);
  if (r.ok) {
    S.st = await r.json();
    if (S.pendingStatus) { S.st.status = S.pendingStatus; S.pendingStatus = null; }
    S.active = S.st.ticker;
    S.watch = S.st.watchlist || [];
    syncExpirySelect(); renderWatchlist(); render();
  }
}

async function postJSON(url, body) {
  const r = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!r.ok) { console.error("POST failed", url, await r.text()); return null; }
  return r.json();
}

/* one SSE connection per displayed ticker; the server filters `state`
   events to that ticker (status/watchlist always flow) */
let es = null;
function connectEvents(ticker) {
  if (es) es.close();
  es = new EventSource("/api/events?ticker=" + encodeURIComponent(ticker));
  es.addEventListener("state", (e) => {
    const d = JSON.parse(e.data);
    if (S.active && d.ticker !== S.active) return; // stale tick right after a switch
    S.st = d;
    if (S.pendingStatus) { S.st.status = S.pendingStatus; S.pendingStatus = null; }
    syncExpirySelect(); render();
  });
  es.addEventListener("status", (e) => {
    const status = JSON.parse(e.data);
    if (!S.st) S.pendingStatus = status;
    else { S.st.status = status; render(); }
  });
  es.addEventListener("watchlist", (e) => {
    S.watch = JSON.parse(e.data);
    renderWatchlist();
    // another client (or a custom-slot swap) moved the active ticker
    const act = S.watch.find((x) => x.active);
    if (act && act.ticker !== S.active) switchTicker(act.ticker);
  });
  es.onerror = () => { $("live-label").textContent = "RECONNECTING"; };
}

async function activateTicker(t) {
  const wl = await postJSON("/api/watchlist", { action: "activate", ticker: t });
  if (wl) { S.watch = wl; renderWatchlist(); }
  switchTicker(t);
}

async function setCustomTicker(t) {
  const wl = await postJSON("/api/watchlist", { action: "custom", ticker: t });
  if (!wl) return;
  S.watch = wl;
  renderWatchlist();
  const act = wl.find((x) => x.active);
  if (act) switchTicker(act.ticker);
}

function switchTicker(t) {
  if (t === S.active) { render(); return; }
  S.active = t;
  S.st = null;
  S.pendingStatus = null;
  fetchState(t);
  connectEvents(t);
}

async function postStream(id, connect) {
  const r = await fetch("/api/streams", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ stream: id, connect }),
    keepalive: true, // the pagehide disconnect must outlive the closing tab
  });
  if (!r.ok) {
    console.error("stream control failed", await r.text());
    // revert the optimistic toggle; a successful switch re-syncs the UI
    // through the next status broadcast anyway
    const el = $(`stream-${id}`);
    if (el) el.checked = !connect;
  }
}

function syncExpirySelect() {
  const sel = $("expiry-select");
  const exps = expiryList();
  const cur = sel.value;
  if (sel.dataset.sig === exps.map((e) => e.expiry).join(",")) return;
  sel.dataset.sig = exps.map((e) => e.expiry).join(",");
  sel.innerHTML = `<option value="all">All expirations</option>` +
    exps.map((e) => `<option value="${e.expiry}">${expLabel(e.expiry)} · ${Math.max(0, Math.round(e.dte))}d</option>`).join("");
  sel.value = exps.some((e) => e.expiry === cur) ? cur : "all";
  S.expiry = sel.value;
}

/* ─────────────────────────── watchlist strip ─────────────────────────── */
function renderWatchlist() {
  const strip = $("ticker-strip");
  if (!S.watch) return;
  const sig = S.watch.map((w) => `${w.ticker}:${w.active}:${w.class}:${sweepDotSig(w.ticker)}`).join("|");
  if (strip.dataset.sig === sig && !$("custom-form").classList.contains("hidden") === S.customOpen) return;
  strip.dataset.sig = sig;
  strip.innerHTML = "";
  for (const w of S.watch) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = `tick-btn ${w.active ? "active" : ""} ${w.custom ? "custom" : ""}`;
    const sw = sweepRowOf(w.ticker);
    const dot = sw && sw.armed
      ? `<span class="sweep-dot ${sw.intervalSeconds === sweepFastSeconds() ? "fast" : ""} ${sw.sweeping ? "live" : ""}"></span>`
      : "";
    b.innerHTML = w.ticker + (w.custom ? `<span class="cls">✎</span>` : "") + dot;
    b.title = (w.class && w.class !== w.ticker ? `options: ${w.class}` : w.ticker) +
      (sw && sw.armed ? ` · sweep armed every ${sw.intervalSeconds}s${sw.sweeping ? " (sweeping)" : ""}` : "");
    b.onclick = () => (w.custom ? openCustomForm(w.ticker) : activateTicker(w.ticker));
    strip.appendChild(b);
  }
  const plus = document.createElement("button");
  plus.type = "button";
  plus.className = "tick-btn custom";
  plus.textContent = S.watch.some((w) => w.custom) ? "" : "＋ free";
  plus.title = "Point the free slot at any ticker";
  plus.onclick = () => openCustomForm("");
  if (plus.textContent) strip.appendChild(plus);
}

function openCustomForm(current) {
  S.customOpen = true;
  const form = $("custom-form");
  form.classList.remove("hidden");
  const input = $("custom-input");
  input.value = current || "";
  input.focus();
}
function closeCustomForm() {
  S.customOpen = false;
  $("custom-form").classList.add("hidden");
  renderWatchlist();
}

/* ─────────────────────────── volatility skew ─────────────────────────── */
function skewExpiry() {
  const exps = expiryList();
  if (!exps.length) return null;
  if (S.expiry !== "all") {
    const hit = exps.find((x) => x.expiry === S.expiry);
    if (hit) return hit;
  }
  return exps[0]; // skew is per-expiry; "All" falls back to the front month
}

function ivAtDelta(skew, right, target) {
  let best = 2, iv = -1;
  for (const p of skew) {
    if (p.right !== right) continue;
    const d = Math.abs(p.delta - target);
    if (d < best) { best = d; iv = p.iv; }
  }
  return iv;
}
function pointAtDelta(skew, right, target) {
  let best = 2, hit = null;
  for (const p of skew) {
    if (p.right !== right) continue;
    const d = Math.abs(p.delta - target);
    if (d < best) { best = d; hit = p; }
  }
  return hit;
}

function renderSkew() {
  const snap = S.st.snapshot;
  const exp = snap ? skewExpiry() : null;
  const stats = $("skew-stats");
  if (!exp || !exp.skew || exp.skew.length < 3) {
    stats.innerHTML = "";
    $("skew-note").textContent = "no quoted IVs for this expiry";
    const { ctx, w, h } = ctx2d($("chart-skew"));
    ctx.clearRect(0, 0, w, h);
    drawEmpty(ctx, w, h, "no quoted IVs — the skew panel needs an IV feed");
    return;
  }

  // ── stat tiles ──
  const ivP25 = ivAtDelta(exp.skew, "P", -0.25), ivC25 = ivAtDelta(exp.skew, "C", 0.25);
  const ivP50 = ivAtDelta(exp.skew, "P", -0.50), ivC50 = ivAtDelta(exp.skew, "C", 0.50);
  const atm = (ivP50 + ivC50) / 2;
  const skew25 = (ivP25 - ivC25) * 100;             // vol points
  const bfly25 = (ivP25 + ivC25 - 2 * atm) * 100;   // vol points
  // least-squares slope of IV (pts) vs moneyness (% of strike)
  let sx = 0, sy = 0, sxx = 0, sxy = 0;
  for (const p of exp.skew) {
    const x = (p.strike / snap.spot - 1) * 100, y = p.iv * 100;
    sx += x; sy += y; sxx += x * x; sxy += x * y;
  }
  const n = exp.skew.length;
  const slope = (n * sxy - sx * sy) / Math.max(1e-9, n * sxx - sx * sx);
  // term slope: ATM IV of this expiry vs the next one out
  const exps = expiryList();
  const idx = exps.indexOf(exp);
  let term = null, termNote = "—";
  if (idx >= 0 && idx + 1 < exps.length) {
    const nx = exps[idx + 1];
    const atmNext = (ivAtDelta(nx.skew || [], "P", -0.5) + ivAtDelta(nx.skew || [], "C", 0.5)) / 2;
    if (atmNext > 0) {
      term = (atm - atmNext) * 100;
      termNote = `front vs ${expLabel(nx.expiry)}`;
    }
  }
  const tile = (l, v, cls, s) =>
    `<div class="sk-tile"><div class="l">${l}</div><div class="v ${cls || ""}">${v}</div><div class="s">${s || ""}</div></div>`;
  stats.innerHTML =
    tile("ATM IV", (atm * 100).toFixed(1) + "%", "", `strike ${snap.spot.toFixed(0)}`) +
    tile("25Δ Skew", `${skew25 >= 0 ? "+" : ""}${skew25.toFixed(1)} pts`, skew25 >= 0 ? "neg" : "pos",
      skew25 >= 0 ? "puts rich — crash hedge" : "calls rich — upside chase") +
    tile("25Δ Butterfly", `${bfly25 >= 0 ? "+" : ""}${bfly25.toFixed(1)} pts`, "", "smile curvature") +
    tile("Skew Slope", `${slope >= 0 ? "+" : ""}${slope.toFixed(2)} pts`, "", "per 1% of strike") +
    tile("Term Slope", term == null ? "—" : `${term >= 0 ? "+" : ""}${term.toFixed(1)} pts`, "", termNote);

  $("skew-note").textContent =
    `${expLabel(exp.expiry)} · ${Math.max(0, Math.round(exp.dte))}d · ${exp.skew.length} quoted contracts` +
    (S.expiry === "all" ? " · front month" : "");
  drawSkewChart($("chart-skew"), exp, snap.spot, { hoverX: S.hover["chart-skew"] });
}

/* IV-vs-strike smile with the 25Δ/50Δ/75Δ contracts marked on each wing,
   plus a hover panel: exact strike, quoted call/put IV and their deltas. */
function drawSkewChart(canvas, exp, spot, { hoverX = null } = {}) {
  const { ctx, w, h } = ctx2d(canvas);
  ctx.clearRect(0, 0, w, h);
  const padL = 46, padR = 12, padT = 16, padB = 22;
  const pw = w - padL - padR, ph = h - padT - padB;
  const pts = exp.skew.filter((p) => p.iv > 0);
  if (pts.length < 3) { drawEmpty(ctx, w, h); return; }

  let klo = Infinity, khi = -Infinity, ilo = Infinity, ihi = -Infinity;
  for (const p of pts) {
    klo = Math.min(klo, p.strike); khi = Math.max(khi, p.strike);
    ilo = Math.min(ilo, p.iv); ihi = Math.max(ihi, p.iv);
  }
  const pad = (khi - klo) * 0.04 || 1;
  klo -= pad; khi += pad;
  const ivPad = Math.max((ihi - ilo) * 0.15, 0.002);
  ilo -= ivPad; ihi += ivPad;
  const xOf = (k) => padL + ((k - klo) / (khi - klo)) * pw;
  const yOf = (iv) => padT + (1 - (iv - ilo) / (ihi - ilo)) * ph;

  // grid + y labels in vol %
  ctx.strokeStyle = GRID; ctx.lineWidth = 1;
  const step = niceStep(ihi - ilo, 4);
  for (let v = Math.ceil(ilo / step) * step; v <= ihi; v += step) {
    const y = yOf(v);
    ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(w - padR, y); ctx.stroke();
    axisText(ctx, (v * 100).toFixed(1) + "%", padL - 6, y, "right");
  }
  // x labels
  const stepX = niceStep(khi - klo, 8);
  for (let k = Math.ceil(klo / stepX) * stepX; k <= khi; k += stepX) {
    axisText(ctx, fmtStrike(k), xOf(k), h - padB + 8);
  }

  // spot marker
  if (spot > klo && spot < khi) {
    ctx.save();
    ctx.strokeStyle = ACCENT; ctx.setLineDash([4, 3]); ctx.lineWidth = 1.4;
    ctx.beginPath(); ctx.moveTo(xOf(spot), padT); ctx.lineTo(xOf(spot), padT + ph); ctx.stroke();
    ctx.restore();
    axisText(ctx, "Spot", xOf(spot), padT + 6, "center", ACCENT);
  }

  // curves
  const curve = (right, color) => {
    const line = pts.filter((p) => p.right === right).sort((a, b) => a.strike - b.strike);
    if (line.length < 2) return;
    ctx.beginPath();
    line.forEach((p, i) => (i ? ctx.lineTo(xOf(p.strike), yOf(p.iv)) : ctx.moveTo(xOf(p.strike), yOf(p.iv))));
    ctx.strokeStyle = color; ctx.lineWidth = 1.8; ctx.stroke();
  };
  curve("P", NEG);
  curve("C", POS);

  // Δ25 / Δ50 / Δ75 markers on each wing
  const mark = (right, color) => {
    for (const [target, label] of [[0.25, "25Δ"], [0.50, "50Δ"], [0.75, "75Δ"]]) {
      const p = pointAtDelta(pts, right, right === "P" ? -target : target);
      if (!p) continue;
      const x = xOf(p.strike), y = yOf(p.iv);
      ctx.beginPath(); ctx.arc(x, y, 4.2, 0, Math.PI * 2);
      ctx.fillStyle = "#0a0d12"; ctx.fill();
      ctx.strokeStyle = "#fff"; ctx.lineWidth = 1.6; ctx.stroke();
      ctx.strokeStyle = color; ctx.lineWidth = 1; ctx.beginPath(); ctx.arc(x, y, 5.8, 0, Math.PI * 2); ctx.stroke();
      axisText(ctx, label, x, right === "C" ? y - 14 : y + 15, "center", color);
    }
  };
  mark("C", POS);
  mark("P", NEG);

  // hover: nearest quoted strike + panel
  if (hoverX != null && hoverX >= padL && hoverX <= w - padR) {
    const s = klo + ((hoverX - padL) / pw) * (khi - klo);
    const hit = pts.reduce((best, p) => Math.abs(p.strike - s) < Math.abs(best.strike - s) ? p : best, pts[0]);
    ctx.save();
    ctx.strokeStyle = "rgba(255,255,255,0.35)"; ctx.setLineDash([3, 3]); ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(xOf(hit.strike), padT); ctx.lineTo(xOf(hit.strike), padT + ph); ctx.stroke();
    ctx.restore();
    const at = (right) => pts.find((p) => p.right === right && p.strike === hit.strike);
    const c = at("C"), pu = at("P");
    const rows = [];
    if (c) rows.push({ k: "Call IV", v: (c.iv * 100).toFixed(2) + "%", c: POS }, { k: "Call Δ", v: c.delta.toFixed(2) });
    if (pu) rows.push({ k: "Put IV", v: (pu.iv * 100).toFixed(2) + "%", c: NEG }, { k: "Put Δ", v: pu.delta.toFixed(2) });
    if (rows.length) drawPanel(ctx, w, h, padL + 8, padT + 4, "STRIKE " + fmtStrike(hit.strike), rows);
  }
}

/* ─────────────────────────── controls ─────────────────────────── */
$("master-btn").onclick = () => postStream("all", !(S.st?.status.connected));
$("stream-underlying").onchange = (e) => postStream("underlying", e.target.checked);
$("stream-options").onchange = (e) => postStream("options", e.target.checked);

$("custom-form").onsubmit = (e) => {
  e.preventDefault();
  const t = $("custom-input").value.trim().toUpperCase();
  if (!t) return;
  setCustomTicker(t);
  closeCustomForm();
};
$("custom-cancel").onclick = closeCustomForm;

document.querySelectorAll(".tab").forEach((t) => t.onclick = () => {
  document.querySelectorAll(".tab").forEach((x) => x.classList.toggle("active", x === t));
  S.view = t.dataset.view;
  $("panel-strike").classList.toggle("hidden", S.view !== "gex");
  $("panel-profile").classList.toggle("hidden", S.view !== "gex");
  $("panel-skew").classList.toggle("hidden", S.view !== "gex");
  $("panel-heatmap").classList.toggle("hidden", S.view !== "gex");
  $("panel-oi").classList.toggle("hidden", S.view !== "oi");
  render();
});

document.querySelectorAll("[data-gexmode]").forEach((b) => b.onclick = () => {
  document.querySelectorAll("[data-gexmode]").forEach((x) => x.classList.toggle("active", x === b));
  S.gexMode = b.dataset.gexmode; renderStrikeProfile();
});
document.querySelectorAll("[data-render]").forEach((b) => b.onclick = () => {
  document.querySelectorAll("[data-render]").forEach((x) => x.classList.toggle("active", x === b));
  S.render = b.dataset.render; renderStrikeProfile();
});
document.querySelectorAll("[data-range]").forEach((b) => b.onclick = () => {
  document.querySelectorAll("[data-range]").forEach((x) => x.classList.toggle("active", x === b));
  S.range = b.dataset.range; renderStrikeProfile();
});
$("expiry-select").onchange = (e) => { S.expiry = e.target.value; renderStrikeProfile(); renderOI(); };
$("heat-toggle").onclick = () => { S.heatAll = !S.heatAll; syncHeatToggle(); renderHeatmap(); };

/* keep charts crisp on layout changes (16:9 ↔ 1:1 reflow) */
const ro = new ResizeObserver(() => render());
["chart-strike", "chart-profile", "chart-skew", "chart-dgex", "chart-oi"].forEach((id) => ro.observe($(id).parentElement));

/* hover tooltips: one data panel per chart, top-left */
wireHover("chart-strike", renderStrikeProfile);
wireHover("chart-oi", renderOI);
wireHover("chart-profile", renderProfile);
wireHover("chart-skew", renderSkew);
wireHover("chart-dgex", renderDeltaGex);

fetchState().then(() => connectEvents(S.active || ""));

/* ── Ingest diagnostics panel (edge mode) ─────────────────────────────
   Polls /api/diagnostics; the panel stays hidden when the edge boundary
   is not running (the endpoint answers {}). The anomaly ring is the
   forensic trail: parameter changes old→new, seq gaps, rejected events. */
const DIAG_KINDS = {
  seq_gap: "seq gap", malformed: "malformed", param_change: "param change",
  identity_reject: "identity reject", iv_jump: "iv jump", unknown_contract: "unknown conId",
  unknown_ticker: "unknown ticker", chain_replace: "chain re-discovery",
  apply_failed: "apply failed", write_failed: "write failed", stale_event: "stale event",
};
async function pollDiagnostics() {
  let d;
  try {
    const r = await fetch("/api/diagnostics");
    if (!r.ok) return;
    d = await r.json();
  } catch { return; }
  const edge = d && d.edge;
  $("panel-diag").classList.toggle("hidden", !edge);
  if (!edge) return;

  $("diag-events").textContent = fmtNum(edge.events);
  $("diag-gaps").textContent = fmtNum(edge.seqGaps);
  $("diag-malformed").textContent = fmtNum(edge.malformed);
  $("diag-rejected").textContent = fmtNum(edge.rejected);
  const lat = edge.latency && edge.latency.n > 0 ? edge.latency : null;
  $("diag-latency").textContent = lat ? `${lat.ewmaMs.toFixed(0)} ms avg` : "—";
  $("diag-db").textContent = fmtNum(d.storeFailedBatches || 0);
  $("diag-uptime").textContent = `up ${Math.round((edge.uptimeMs || 0) / 1000)}s · ${edge.sessions || 0} session(s)`;

  const anoms = (edge.anomalies || []).slice(-8).reverse();
  const box = $("diag-anoms");
  if (anoms.length === 0) {
    box.innerHTML = `<div class="muted small">No anomalies.</div>`;
  } else {
    box.innerHTML = anoms.map((a) => {
      const kind = DIAG_KINDS[a.kind] || a.kind;
      return `<div class="diag-anom"><span class="diag-kind">${kind}</span>` +
        (a.ticker ? `<span class="diag-tk">${a.ticker}</span>` : "") +
        `<span class="muted small">${a.detail}</span></div>`;
    }).join("");
  }
}
setInterval(pollDiagnostics, 2000);
pollDiagnostics();

/* ── Snapshot sweeper (GUI-controlled, max 2, cost-guarded) ───────────
   Everything renders from GET /api/sweeps (knobs live in the core's
   sweep_policy.go — intervals, costs, cap, windows). Sweeps never start on
   their own: the roster changes only through these toggles, and every POST
   sends the FULL roster (set semantics). Rejects come back 409 with the
   operator reason, shown inline; the roster is untouched on reject.
   Windows are sticky: a closed window clears the armed row server-side and
   nothing re-arms it until toggled again. */
S.sweeps = null;       // last SweepsView
S.sweepPick = {};      // ticker → interval chosen on an un-armed row
S.sweepErr = {};       // ticker → inline refusal ("" key = panel-level)
S.sweepBusy = false;   // a POST is in flight

function sweepRowOf(ticker) {
  return S.sweeps?.rows?.find((r) => r.ticker === ticker) || null;
}
function sweepFastSeconds() {
  return S.sweeps?.intervals?.find((i) => i.min)?.seconds ?? 15;
}
function sweepDefaultSeconds() {
  return S.sweeps?.intervals?.find((i) => !i.min)?.seconds ?? 60;
}
function sweepDotSig(ticker) {
  const r = sweepRowOf(ticker);
  return r && r.armed ? `${r.intervalSeconds}${r.sweeping ? "L" : ""}` : "";
}
function sweepIntervalOf(seconds) {
  return S.sweeps?.intervals?.find((i) => i.seconds === seconds) || null;
}
function fmtCost(v) {
  return "$" + (v || 0).toFixed(2);
}
/* opensAtMs rendered in ET (the windows are ET regardless of the viewer's tz) */
function fmtET(ms) {
  if (!ms) return "";
  try {
    return new Date(ms).toLocaleString("en-US", {
      timeZone: "America/New_York", weekday: "short", hour: "2-digit", minute: "2-digit", hour12: false,
    }) + " ET";
  } catch { return new Date(ms).toLocaleString(); }
}
function escHTML(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

async function pollSweeps() {
  let v;
  try {
    const r = await fetch("/api/sweeps");
    if (!r.ok) return;
    v = await r.json();
  } catch { return; }
  applySweepsView(v);
}

function applySweepsView(v) {
  S.sweeps = v && v.enabled ? v : null;
  // drop inline refusals for tickers that are now armed (the refusal is stale)
  for (const r of S.sweeps?.rows || []) if (r.armed) delete S.sweepErr[r.ticker];
  renderSweeper();
  renderSweepBanner();
  renderMasterCountdown();
  renderWatchlist();
}

function armedRoster() {
  return (S.sweeps?.rows || [])
    .filter((r) => r.armed)
    .map((r) => ({ ticker: r.ticker, intervalSeconds: r.intervalSeconds }));
}

/* POST the full roster (or {kill:true}); 409 → inline reason, roster unchanged */
async function postSweeps(body, errKey) {
  if (S.sweepBusy) return;
  S.sweepBusy = true;
  renderSweeper();
  try {
    const r = await fetch("/api/sweeps", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    });
    let d = null;
    try { d = await r.json(); } catch {}
    if (!r.ok) {
      const msg = d?.error || `sweep control failed (HTTP ${r.status})`;
      S.sweepErr[d?.ticker || errKey || ""] = msg;
    } else {
      delete S.sweepErr[errKey || ""];
      delete S.sweepErr[""];
      if (d) { S.sweepBusy = false; applySweepsView(d); return; }
    }
  } catch (e) {
    S.sweepErr[errKey || ""] = "sweep control unreachable: " + e.message;
  } finally {
    S.sweepBusy = false;
  }
  renderSweeper();
  pollSweeps();
}

function toggleSweep(ticker) {
  const row = sweepRowOf(ticker);
  if (!row) return;
  const roster = armedRoster();
  if (row.armed) {
    postSweeps({ tickers: roster.filter((e) => e.ticker !== ticker) }, ticker);
    return;
  }
  // refused client-side before any POST: blocked window or cap (the core
  // re-validates both — this only keeps the reason inline and instant)
  if (!row.allowed) {
    S.sweepErr[ticker] = row.reason || "sweeps blocked right now";
    renderSweeper();
    return;
  }
  const max = S.sweeps.max || 2;
  if (roster.length >= max) {
    S.sweepErr[ticker] = `max ${max} sweeps — turn one off first`;
    renderSweeper();
    return;
  }
  const secs = S.sweepPick[ticker] || sweepDefaultSeconds();
  postSweeps({ tickers: [...roster, { ticker, intervalSeconds: secs }] }, ticker);
}

function pickSweepInterval(ticker, secs) {
  S.sweepPick[ticker] = secs;
  const row = sweepRowOf(ticker);
  if (row && row.armed && row.intervalSeconds !== secs) {
    // an armed row's cadence change re-installs the roster with the new interval
    postSweeps({ tickers: armedRoster().map((e) => (e.ticker === ticker ? { ticker, intervalSeconds: secs } : e)) }, ticker);
  } else {
    renderSweeper();
  }
}

function renderSweeper() {
  const panel = $("panel-sweeper");
  const v = S.sweeps;
  panel.classList.toggle("hidden", !v);
  if (!v) return;

  const max = v.max || 2;
  const armedN = (v.rows || []).filter((r) => r.armed).length;
  setText($("sweep-cap-hint"), `${armedN}/${max} armed · never auto-starts`);

  const box = $("sweeper-rows");
  const sig = JSON.stringify([v.rows, v.intervals, S.sweepPick, S.sweepErr, S.sweepBusy, v.edge?.connected]);
  if (box.dataset.sig !== sig) {
    box.dataset.sig = sig;
    const fast = sweepFastSeconds();
    box.innerHTML = (v.rows || []).map((r) => {
      const t = escHTML(r.ticker);
      const secs = r.armed ? r.intervalSeconds : (S.sweepPick[r.ticker] || sweepDefaultSeconds());
      const isFast = secs === fast;
      const blocked = !r.allowed;
      const capped = !r.armed && armedN >= max;
      const opts = (v.intervals || []).map((i) =>
        `<option value="${i.seconds}" ${i.seconds === secs ? "selected" : ""}>${escHTML(i.label)}</option>`).join("");
      let live = "", liveCls = "";
      if (r.armed) {
        if (r.sweeping) { live = "sweeping"; liveCls = "on"; }
        else if (!v.edge?.connected) { live = "edge off"; liveCls = "wait"; }
        else { live = "arming"; liveCls = "wait"; }
      }
      const err = S.sweepErr[r.ticker];
      let sub = "";
      if (err) sub = `<div class="sweep-sub err">${escHTML(err)}</div>`;
      else if (blocked) sub = `<div class="sweep-sub">${escHTML(r.reason || "blocked")}${r.opensAtMs ? ` · reopens ${escHTML(fmtET(r.opensAtMs))}` : ""}</div>`;
      else if (r.armed) {
        const iv = sweepIntervalOf(secs);
        sub = `<div class="sweep-sub">every ${secs}s · ≈${fmtCost(iv?.costPerHour)}/hr${iv?.min ? " min" : ""}</div>`;
      } else if (capped) sub = `<div class="sweep-sub">max ${max} armed</div>`;
      return `<div class="sweep-row ${r.armed ? "armed" : ""} ${r.armed && isFast ? "fast" : ""} ${blocked ? "blocked" : ""}">
        <div class="sweep-row-main">
          <button type="button" class="sweep-toggle ${r.armed ? "on" : ""} ${isFast ? "fast" : ""}" data-sweep-toggle="${t}"
            ${S.sweepBusy ? "disabled" : ""} aria-pressed="${r.armed}"
            title="${blocked ? escHTML(r.reason || "blocked") : r.armed ? "Turn this sweep off" : "Arm a metered snapshot sweep"}">
            Sweep ${t} — <span class="st">${r.armed ? "ON" : "OFF"}</span>
          </button>
          <select data-sweep-interval="${t}" aria-label="Sweep interval for ${t}" ${S.sweepBusy ? "disabled" : ""}>${opts}</select>
          <span class="sweep-live ${liveCls}">${live}</span>
        </div>
        ${sub}
      </div>`;
    }).join("") || `<div class="muted small">No watchlist tickers.</div>`;
  }

  const e = v.edge;
  setText($("sweep-spend"), e ? fmtCost(e.snapshotSpend) : "—");
  setText($("sweep-live"), !e ? "no edge heartbeat"
    : !e.connected ? "edge disconnected"
    : (e.sweepActive && e.sweepActive.length ? e.sweepActive.join(", ") : "nothing")
      + (e.sweepWindowOpen === false ? " · window closed" : ""));

  const panelErr = S.sweepErr[""];
  const errEl = $("sweep-error");
  errEl.classList.toggle("hidden", !panelErr);
  setText(errEl, panelErr || "");
}

/* delegated handlers survive the signature-driven row rebuilds */
$("sweeper-rows").addEventListener("click", (ev) => {
  const b = ev.target.closest("[data-sweep-toggle]");
  if (b && !b.disabled) toggleSweep(b.dataset.sweepToggle);
});
$("sweeper-rows").addEventListener("change", (ev) => {
  const sel = ev.target.closest("[data-sweep-interval]");
  if (sel) pickSweepInterval(sel.dataset.sweepInterval, parseInt(sel.value, 10));
});

/* Fixed top banner: yellow at 60s cadences, red when any 15s ticker is armed
   (its figure is a floor → "min"). Two tickers add per-ticker "each" and a
   combined total. Renders from the armed roster, not the heartbeat — armed
   means money can be spent. */
function renderSweepBanner() {
  const el = $("sweep-banner");
  const armed = (S.sweeps?.rows || []).filter((r) => r.armed);
  const kill = $("sweep-kill");
  // the pill also shows if the edge heartbeat still reports a live loop with
  // an empty roster (stale edge) — one click pushes the empty roster again
  const edgeLive = (S.sweeps?.edge?.sweepActive || []).length > 0;
  kill.classList.toggle("hidden", !armed.length && !edgeLive);
  kill.disabled = S.sweepBusy;
  if (!armed.length) {
    el.classList.add("hidden");
    document.body.classList.remove("has-sweep-banner");
    return;
  }
  const costOf = (r) => sweepIntervalOf(r.intervalSeconds) || { costPerHour: 0, min: false };
  const anyMin = armed.some((r) => costOf(r).min);
  const total = S.sweeps.costPerHour ?? armed.reduce((a, r) => a + costOf(r).costPerHour, 0);
  const minTag = (m) => (m ? " min" : "");
  let text;
  const sameCadence = armed.every((r) => r.intervalSeconds === armed[0].intervalSeconds);
  if (armed.length === 1) {
    const c = costOf(armed[0]);
    text = `SWEEPER ON — ${armed[0].ticker} · every ${armed[0].intervalSeconds}s · ≈${fmtCost(c.costPerHour)}/hr${minTag(c.min)}`;
  } else if (sameCadence) {
    const c = costOf(armed[0]);
    text = `SWEEPER ON — ${armed.map((r) => r.ticker).join(", ")} · every ${armed[0].intervalSeconds}s · ` +
      `≈${fmtCost(c.costPerHour)}/hr${minTag(c.min)} each · ${fmtCost(total)}/hr${minTag(anyMin)} total`;
  } else {
    text = "SWEEPER ON — " + armed.map((r) => {
      const c = costOf(r);
      return `${r.ticker} · every ${r.intervalSeconds}s · ≈${fmtCost(c.costPerHour)}/hr${minTag(c.min)}`;
    }).join(" | ") + ` · ${fmtCost(total)}/hr${minTag(anyMin)} total`;
  }
  setText(el, text);
  setClass(el, `sweep-banner ${S.sweeps.anyFast || anyMin ? "fast" : ""}`);
  document.body.classList.add("has-sweep-banner");
}

$("sweep-kill").onclick = () => {
  // kill never needs the cap/window checks — it only ever removes
  postSweeps({ kill: true }, "");
};

/* Bottom-right master-log countdown: "data save in MM:SS" to the next
   wall-clock-aligned CSV flush (server-provided nextFlushMs), ticked locally. */
function renderMasterCountdown() {
  const el = $("master-countdown");
  const ml = S.sweeps?.masterLog;
  if (!ml || !ml.enabled || !ml.nextFlushMs) { el.classList.add("hidden"); return; }
  el.classList.remove("hidden");
  const left = Math.max(0, Math.floor((ml.nextFlushMs - Date.now()) / 1000));
  const mm = String(Math.floor(left / 60)).padStart(2, "0");
  const ss = String(left % 60).padStart(2, "0");
  setText(el, `data save in ${mm}:${ss}`);
  el.title = [
    ml.path ? `master CSV: ${ml.path}` : "",
    ml.lastFlushMs ? `last save ${new Date(ml.lastFlushMs).toLocaleString()}` : "no save yet",
    `${(ml.pendingRows || 0).toLocaleString()} row(s) pending`,
  ].filter(Boolean).join(" · ");
}

setInterval(pollSweeps, 2000);
setInterval(renderMasterCountdown, 1000);
pollSweeps();

/* ───────────────── hidden-tab meter guard ─────────────────
   GUI Disconnect now carries to the edge (core pause → the edge tears its
   TWS session down, snapshot meter included). A hidden tab means nobody is
   watching: auto-disconnect after a grace period, auto-reconnect when
   visible again. A MANUAL Disconnect is never overridden — auto-resume
   only reverses what the auto-pause did. Closing the tab disconnects too
   (pagehide), and a reload restores the previous live state. */
let autoPausedByHide = false;
let hideTimer = null;
document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    hideTimer = setTimeout(() => {
      if (!document.hidden || autoPausedByHide) return;
      if (S.st?.status.connected) {
        autoPausedByHide = true;
        postStream("all", false);
      }
    }, 10000); // grace: brief tab switches don't cycle the feed
  } else {
    clearTimeout(hideTimer);
    if (autoPausedByHide) {
      autoPausedByHide = false;
      if (!S.st?.status.connected) postStream("all", true);
    }
  }
});
window.addEventListener("pagehide", () => {
  clearTimeout(hideTimer);
  if (S.st?.status.connected) {
    try { sessionStorage.setItem("gex-was-live", "1"); } catch {}
    postStream("all", false);
  } else {
    try { sessionStorage.removeItem("gex-was-live"); } catch {}
  }
});
if (sessionStorage.getItem("gex-was-live") === "1") {
  try { sessionStorage.removeItem("gex-was-live"); } catch {}
  postStream("all", true); // reload after a live session: come back live
}
