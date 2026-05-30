// InternetMerge frontend. Talks to the Go backend through the Wails-injected
// globals: window.go.main.App.<Method>() (Promises) and window.runtime.EventsOn.

const $ = (id) => document.getElementById(id);

let Backend = null;
let running = false;
let interfaces = []; // discovered interfaces
let selected = new Set(); // ifNames chosen by the user
let proxyServices = []; // OS proxy targets to route (from backend)
let lastBytes = {}; // ifName -> {up,down} for rate calc
let lastTs = 0;
let peakRate = 1; // for relative link bars

function backend() {
  return window.go && window.go.main && window.go.main.App;
}

async function init() {
  for (let i = 0; i < 60 && !backend(); i++) await new Promise((r) => setTimeout(r, 50));
  Backend = backend();
  if (!Backend) {
    showError("Backend not available.");
    return;
  }

  $("primaryBtn").addEventListener("click", onPrimary);
  $("stopBtn").addEventListener("click", stop);
  $("refreshBtn").addEventListener("click", loadInterfaces);

  await loadInterfaces();
  try {
    proxyServices = (await Backend.NetworkServices()) || [];
  } catch (_) {
    proxyServices = [];
  }

  if (window.runtime && window.runtime.EventsOn) {
    window.runtime.EventsOn("status", onStatus);
  }
  try { onStatus(await Backend.Status()); } catch (_) {}
}

// --- interface discovery / selection ---

async function loadInterfaces() {
  const box = $("linkList");
  if (!running) box.textContent = "Scanning…";
  try {
    interfaces = (await Backend.ListInterfaces()) || [];
  } catch (e) {
    box.textContent = "Failed to scan: " + e;
    return;
  }
  // Default selection: every usable interface, first time only.
  if (selected.size === 0) {
    interfaces.forEach((it) => {
      if (it.up && it.ipv4) selected.add(it.name);
    });
  }
  if (!running) renderLinkPicker();
}

// renderLinkPicker shows selectable cards before/while idle.
function renderLinkPicker() {
  const box = $("linkList");
  box.innerHTML = "";
  if (!interfaces.length) {
    box.textContent = "No network connections found.";
    return;
  }
  interfaces.forEach((it) => {
    const usable = it.up && it.ipv4;
    const sel = usable && selected.has(it.name);
    const row = document.createElement("div");
    row.className = "link" + (usable ? "" : " disabled") + (sel ? " sel" : "");
    row.innerHTML = `
      <div class="check">${sel ? "✓" : ""}</div>
      <div class="link-main">
        <div class="link-name">${escapeHtml(it.label || it.name)}
          ${usable ? "" : '<span class="badge unavail">unavailable</span>'}</div>
        <div class="link-meta">${it.name}${it.ipv4 ? " · " + it.ipv4 : " · no IP address"}</div>
      </div>
      <div class="link-stats"></div>`;
    if (usable) {
      row.addEventListener("click", () => {
        if (selected.has(it.name)) selected.delete(it.name);
        else selected.add(it.name);
        renderLinkPicker();
        updatePrimaryLabel();
      });
    }
    box.appendChild(row);
  });
  updatePrimaryLabel();
}

function updatePrimaryLabel() {
  const n = [...selected].filter((name) =>
    interfaces.some((it) => it.name === name && it.up && it.ipv4)
  ).length;
  const btn = $("primaryBtn");
  if (running) return;
  btn.disabled = n === 0;
  btn.textContent = n >= 2 ? `⚡ Merge ${n} connections` : n === 1 ? "⚡ Start (1 connection)" : "Select a connection";
  $("hint").textContent =
    n >= 2
      ? "Spreads your traffic across these connections for more combined speed."
      : n === 1
      ? "Only one connection selected — pick a second one on a different network to actually merge."
      : "Select at least one connection above.";
}

// --- start / stop ---

async function onPrimary() {
  hideError();
  const ifaces = [...selected].filter((name) =>
    interfaces.some((it) => it.name === name && it.up && it.ipv4)
  );
  if (!ifaces.length) {
    showError("Select at least one usable connection.");
    return;
  }
  const route = $("routeSystem").checked;
  try {
    await Backend.Start({
      interfaces: ifaces,
      addr: $("addr").value.trim() || "127.0.0.1:1080",
      proxyServices: route ? proxyServices : [],
    });
    lastBytes = {};
    lastTs = 0;
    peakRate = 1;
  } catch (e) {
    showError("Couldn't start: " + e);
  }
}

async function stop() {
  hideError();
  try {
    await Backend.Stop();
  } catch (e) {
    showError("Couldn't stop: " + e);
  }
}

// --- live status ---

function onStatus(st) {
  if (!st) return;
  running = !!st.running;

  $("statusPill").textContent = st.error ? "Error" : running ? "Merging" : "Idle";
  $("statusPill").className = "pill " + (st.error ? "pill-error" : running ? "pill-running" : "pill-idle");

  $("primaryBtn").hidden = running;
  $("stopBtn").hidden = !running;
  $("refreshBtn").hidden = running;

  if (st.error) showError(st.error);

  if (!running) {
    // Back to the picker.
    $("heroDown").textContent = "0";
    $("heroDownUnit").textContent = "KB/s";
    $("heroUp").textContent = "0 KB/s";
    $("heroLinks").textContent = "0";
    $("heroConns").textContent = "0";
    renderLinkPicker();
    return;
  }

  const links = st.links || [];
  const now = Date.now();
  const dt = lastTs ? Math.max((now - lastTs) / 1000, 0.001) : 1;
  lastTs = now;

  let totDown = 0, totUp = 0, totConns = 0, active = 0;
  const rows = links.map((l) => {
    const prev = lastBytes[l.ifName] || { up: l.bytesUp, down: l.bytesDown };
    const dRate = Math.max(0, (l.bytesDown - prev.down) / dt);
    const uRate = Math.max(0, (l.bytesUp - prev.up) / dt);
    lastBytes[l.ifName] = { up: l.bytesUp, down: l.bytesDown };
    totDown += dRate; totUp += uRate; totConns += l.connections;
    if (l.alive) active++;
    peakRate = Math.max(peakRate, dRate);
    return { l, dRate, uRate };
  });

  const [hv, hu] = splitRate(totDown);
  $("heroDown").textContent = hv;
  $("heroDownUnit").textContent = hu;
  $("heroUp").textContent = rate(totUp);
  $("heroLinks").textContent = active;
  $("heroConns").textContent = totConns;

  renderLiveLinks(rows);
}

function renderLiveLinks(rows) {
  const box = $("linkList");
  box.innerHTML = "";
  rows
    .sort((a, b) => b.dRate - a.dRate)
    .forEach(({ l, dRate, uRate }) => {
      const state = l.alive ? "up" : "down";
      const badge = l.alive
        ? '<span class="badge active">active</span>'
        : '<span class="badge dead">no internet</span>';
      const barPct = Math.min(100, (dRate / peakRate) * 100);
      const div = document.createElement("div");
      div.className = "link" + (l.alive ? "" : " off");
      div.innerHTML = `
        <span class="dot ${state}"></span>
        <div class="link-main">
          <div class="link-name">${escapeHtml(l.label || l.ifName)} ${badge}</div>
          <div class="link-meta">${l.ifName} · ${l.connections} conn · weight ${l.weight}</div>
        </div>
        <div class="link-stats">
          <div class="link-rate">${rate(dRate)} <span class="u">↓</span> &nbsp; ${rate(uRate)} <span class="u">↑</span></div>
          <div class="link-bar"><span style="width:${barPct}%"></span></div>
        </div>`;
      box.appendChild(div);
    });
}

// --- helpers ---

function splitRate(bps) {
  const units = ["B", "KB", "MB", "GB"];
  let v = bps, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return [i === 0 ? String(Math.round(v)) : v.toFixed(1), units[i] + "/s"];
}
function rate(bps) {
  const [v, u] = splitRate(bps);
  return v + " " + u;
}
function showError(msg) { const e = $("errmsg"); e.textContent = msg; e.hidden = false; }
function hideError() { $("errmsg").hidden = true; }
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

window.addEventListener("DOMContentLoaded", init);
