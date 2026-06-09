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
let curMode = "loadbalance"; // current dispatch mode
let cfg = null; // persisted config from the backend
let rulesDraft = []; // editable copy of host rules

function backend() {
  return window.go && window.go.main && window.go.main.App;
}

function wireButtons() {
  // Attach EVERY click handler synchronously, before any async/await work, so a
  // slow or failing backend call can never leave buttons dead.
  on("primaryBtn", "click", onPrimary);
  on("stopBtn", "click", stop);
  on("refreshBtn", "click", loadInterfaces);
  // "Later": hide for this session only — the banner returns next launch.
  on("updateLater", "click", () => ($("updateBanner").hidden = true));
  // "Skip this version": persist so this exact version never nags again.
  on("updateSkip", "click", () => {
    if (updateInfo && updateInfo.latestVersion) {
      try { localStorage.setItem(skipUpdateKey(updateInfo.latestVersion), "1"); } catch (_) {}
    }
    $("updateBanner").hidden = true;
  });
  on("updatePage", "click", () => {
    if (updateInfo && updateInfo.htmlURL) Backend.OpenURL(updateInfo.htmlURL);
  });
  on("updateYes", "click", onUpdate);

  // Mode selector.
  document.querySelectorAll("#modeSel button").forEach((b) =>
    b.addEventListener("click", () => setMode(b.dataset.mode))
  );
  // Rules.
  on("addRuleBtn", "click", () => {
    rulesDraft.push({ hostGlob: "", port: 0, action: "direct", ifName: "" });
    renderRules();
  });
  // Settings toggles.
  on("autoAdd", "change", (e) => Backend.SetAutoAddNewLinks($("autoAdd").checked));
  on("startOnLogin", "change", () => Backend.SetStartOnLogin($("startOnLogin").checked).catch((err) => showError("Login item: " + err)));
  on("minTray", "change", () => Backend.SetMinimizeToTray($("minTray").checked));
  // Relay (Phase 3 BYO single-stream bonding) — save the three fields.
  on("relay-save", "click", onRelaySave);
  // Hotplug events.
  if (window.runtime && window.runtime.EventsOn) {
    window.runtime.EventsOn("links-changed", onLinksChanged);
  }
  on("hotplugDismiss", "click", () => ($("hotplugBanner").hidden = true));
  on("hotplugAdd", "click", () => {
    if (pendingLink) Backend.AddInterface(pendingLink).catch((e) => showError("Add link: " + e));
    $("hotplugBanner").hidden = true;
  });
}

// on accepts (id, event, handler); handler may ignore the event arg.
function setMode(mode) {
  curMode = mode === "failover" ? "failover" : "loadbalance";
  syncModeSelector();
  if (Backend) Backend.SetMode(curMode);
}

function syncModeSelector() {
  document.querySelectorAll("#modeSel button").forEach((b) =>
    b.classList.toggle("seg-on", b.dataset.mode === curMode)
  );
}

let pendingLink = null;
function onLinksChanged(ev) {
  if (!ev) return;
  if (ev.kind === "available") {
    pendingLink = ev.ifName;
    $("hotplugName").textContent = (ev.label || ev.ifName) + " — add it to the bond?";
    $("hotplugBanner").hidden = false;
  } else if (ev.kind === "removed") {
    // The status loop will drop it from the list; just refresh.
    if (pendingLink === ev.ifName) $("hotplugBanner").hidden = true;
  }
  // "added" links appear automatically via the next status tick.
}

// --- rules UI ---

function renderRules() {
  const box = $("rulesList");
  box.innerHTML = "";
  if (!rulesDraft.length) {
    box.innerHTML = '<div class="rules-empty">No rules — every connection goes through the bond. Add a rule to send a site/port direct, to a specific link, or to block it.</div>';
  }
  const ifOptions = (interfaces || [])
    .filter((it) => it.up && it.ipv4)
    .map((it) => `<option value="${it.name}">${escapeHtml(it.label || it.name)}</option>`)
    .join("");
  rulesDraft.forEach((r, i) => {
    const row = document.createElement("div");
    row.className = "rule";
    row.innerHTML = `
      <input type="text" placeholder="*.example.com (host)" value="${escapeHtml(r.hostGlob || "")}" data-i="${i}" data-k="hostGlob" />
      <input type="number" placeholder="port" value="${r.port || ""}" data-i="${i}" data-k="port" />
      <select data-i="${i}" data-k="action">
        <option value="direct" ${r.action === "direct" ? "selected" : ""}>Direct</option>
        <option value="bond" ${r.action === "bond" ? "selected" : ""}>Bond</option>
        <option value="link" ${r.action === "link" ? "selected" : ""}>Link…</option>
        <option value="block" ${r.action === "block" ? "selected" : ""}>Block</option>
      </select>
      <select data-i="${i}" data-k="ifName" ${r.action === "link" ? "" : "disabled"}>
        <option value="">(interface)</option>${ifOptions}
      </select>
      <button class="rm" data-i="${i}">✕</button>`;
    box.appendChild(row);
  });
  // Wire rule inputs.
  box.querySelectorAll("input,select").forEach((el) => {
    el.addEventListener("change", () => {
      const i = +el.dataset.i, k = el.dataset.k;
      if (k === "port") rulesDraft[i][k] = parseInt(el.value, 10) || 0;
      else rulesDraft[i][k] = el.value;
      if (k === "action") renderRules();
      saveRules();
    });
  });
  box.querySelectorAll(".rm").forEach((el) =>
    el.addEventListener("click", () => {
      rulesDraft.splice(+el.dataset.i, 1);
      renderRules();
      saveRules();
    })
  );
  // Pre-select the link dropdown values.
  box.querySelectorAll('select[data-k="ifName"]').forEach((el) => {
    el.value = rulesDraft[+el.dataset.i].ifName || "";
  });
}

function saveRules() {
  if (Backend) Backend.SetRules(rulesDraft, cfg ? cfg.appRules || [] : []);
}

// --- relay (Phase 3 BYO single-stream bonding) ---

function relayStatus(text, kind) {
  const el = $("relay-status");
  if (!el) return;
  el.textContent = text || "";
  el.className = "adv-note" + (kind ? " " + kind : "");
  el.hidden = !text;
}

async function onRelaySave() {
  if (!Backend) return;
  const enabled = $("relay-enabled").checked;
  const address = $("relay-address").value.trim();
  const key = $("relay-key").value.trim();
  try {
    await Backend.SetRelay(enabled, address, key);
    relayStatus("Saved — applies on next Merge.", "ok");
  } catch (e) {
    relayStatus("Save failed: " + e, "err");
  }
}

// on safely attaches a listener, logging (not throwing) if the element is absent.
function on(id, event, handler) {
  const el = $(id);
  if (el) el.addEventListener(event, handler);
  else console.error("missing element:", id);
}

async function init() {
  wireButtons(); // first — never blocked by backend readiness

  for (let i = 0; i < 60 && !backend(); i++) await new Promise((r) => setTimeout(r, 50));
  Backend = backend();
  if (!Backend) {
    showError("Backend not available.");
    return;
  }

  await loadInterfaces();
  try {
    proxyServices = (await Backend.NetworkServices()) || [];
  } catch (_) {
    proxyServices = [];
  }

  // Load persisted config and reflect it in the UI.
  try {
    cfg = (await Backend.GetConfig()) || {};
    curMode = cfg.mode || "loadbalance";
    rulesDraft = (cfg.rules || []).map((r) => ({ ...r }));
    $("autoAdd").checked = !!cfg.autoAddNewLinks;
    $("startOnLogin").checked = !!cfg.startOnLogin;
    $("minTray").checked = !!cfg.minimizeToTray;
    if (typeof cfg.routeSystem === "boolean") $("routeSystem").checked = cfg.routeSystem;
    syncModeSelector();
    renderRules();
    if (isWindows()) $("appRuleNote").hidden = false;
  } catch (_) {}

  // Load the relay (Phase 3) settings into the three fields.
  try {
    const r = (await Backend.GetRelay()) || {};
    $("relay-enabled").checked = !!r.enabled;
    $("relay-address").value = r.address || "";
    $("relay-key").value = r.key || "";
  } catch (_) {}

  if (window.runtime && window.runtime.EventsOn) {
    window.runtime.EventsOn("status", onStatus);
  }
  try { onStatus(await Backend.Status()); } catch (_) {}

  // Version label + background update check.
  try {
    const v = await Backend.AppVersion();
    if (v && v !== "dev") $("verTag").textContent = v;
  } catch (_) {}
  checkForUpdate();
}

let updateInfo = null;
const updateSkipPrefix = "internetmerge.skipUpdate.";

function skipUpdateKey(version) {
  return updateSkipPrefix + String(version || "").trim();
}

function updMsg(text, kind) {
  const el = $("updateMsg");
  el.textContent = text || "";
  el.className = "update-msg" + (kind ? " " + kind : "");
}

async function checkForUpdate() {
  try {
    const info = await Backend.CheckForUpdate();
    if (!info || info.error) return;
    if (!info.available) return;
    if (localStorage.getItem(skipUpdateKey(info.latestVersion)) === "1") return;
    updateInfo = info;
    $("updateVer").textContent = `${info.currentVersion} → ${info.latestVersion}`;
    updMsg(info.hasAsset ? "" : "No installer for this system — use Open page.", "");
    // If no direct asset matched, the green button just opens the page.
    $("updateYes").textContent = info.hasAsset ? "Download & install" : "Open page";
    $("updateBanner").hidden = false;
  } catch (_) {
    /* offline or rate-limited — silently ignore on startup */
  }
}

async function onUpdate() {
  if (!updateInfo) return;
  // No matched asset → open the release page.
  if (!updateInfo.hasAsset) {
    Backend.OpenURL(updateInfo.htmlURL);
    return;
  }
  const btn = $("updateYes");
  btn.disabled = true;
  btn.textContent = "Downloading…";
  updMsg("Downloading " + updateInfo.assetName + " …", "");
  try {
    await Backend.DownloadAndApplyUpdate();
    btn.textContent = "Done";
    updMsg("Downloaded. The installer/file should now be open — follow its steps, then reopen InternetMerge.", "ok");
  } catch (e) {
    btn.disabled = false;
    btn.textContent = "Download & install";
    updMsg("Download failed (" + e + "). Opening the release page instead…", "err");
    Backend.OpenURL(updateInfo.htmlURL);
  }
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
    $("bondLine").hidden = true;
    renderLinkPicker();
    return;
  }

  // The "bond" sample is bonded single-stream traffic (relay), not a NIC —
  // pull it out so it gets its own labeled line instead of a per-link card.
  const allLinks = st.links || [];
  const bondSample = allLinks.find((l) => l.ifName === "bond");
  const links = allLinks.filter((l) => l.ifName !== "bond");

  const now = Date.now();
  const dt = lastTs ? Math.max((now - lastTs) / 1000, 0.001) : 1;
  lastTs = now;

  renderBondLine(bondSample, dt);

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

  curMode = st.mode || curMode;
  syncModeSelector();
  renderLiveLinks(rows);
}

// renderBondLine shows bonded single-stream throughput from the "bond" sample.
// It only appears once bonded traffic flows; absent until then.
function renderBondLine(sample, dt) {
  const line = $("bondLine");
  if (!line) return;
  if (!sample) {
    line.hidden = true;
    return;
  }
  const prev = lastBytes[sample.ifName] || { up: sample.bytesUp, down: sample.bytesDown };
  const dRate = Math.max(0, (sample.bytesDown - prev.down) / dt);
  const uRate = Math.max(0, (sample.bytesUp - prev.up) / dt);
  lastBytes[sample.ifName] = { up: sample.bytesUp, down: sample.bytesDown };
  $("bondRate").innerHTML = `${rate(dRate)} <span class="u">↓</span> &nbsp; ${rate(uRate)} <span class="u">↑</span>`;
  line.hidden = false;
}

function renderLiveLinks(rows) {
  const box = $("linkList");
  box.innerHTML = "";
  rows
    .sort((a, b) => b.dRate - a.dRate)
    .forEach(({ l, dRate, uRate }) => {
      const state = !l.enabled ? "idle" : l.alive ? "up" : "down";
      const badge = !l.enabled
        ? '<span class="badge unavail">off</span>'
        : l.alive
        ? '<span class="badge active">active</span>'
        : '<span class="badge dead">no internet</span>';
      const barPct = Math.min(100, (dRate / peakRate) * 100);
      const div = document.createElement("div");
      div.className = "link" + (l.enabled && l.alive ? "" : " off");

      // Controls: enable toggle, Auto/Manual + slider (load-balance) or priority (failover).
      let ctrl = "";
      if (curMode === "failover") {
        ctrl = `<div class="prio">prio <input type="number" min="0" max="99" value="${l.priority}" data-if="${l.ifName}" class="prio-in" /></div>`;
      } else {
        const slider =
          `<input type="range" min="1" max="10" value="${l.weight}" class="wslider" data-if="${l.ifName}" ${l.manual ? "" : "disabled"} />` +
          `<span class="wval">${l.weight}</span>`;
        ctrl = `<div class="wmode">
            <select class="wmode-sel" data-if="${l.ifName}">
              <option value="auto" ${l.manual ? "" : "selected"}>Auto</option>
              <option value="manual" ${l.manual ? "selected" : ""}>Manual</option>
            </select>${slider}
          </div>`;
      }

      div.innerHTML = `
        <span class="dot ${state}"></span>
        <div class="link-main">
          <div class="link-name">${escapeHtml(l.label || l.ifName)} ${badge}</div>
          <div class="link-meta">${l.ifName} · ${l.connections} conn</div>
        </div>
        <div class="link-stats">
          <div class="link-rate">${rate(dRate)} <span class="u">↓</span> &nbsp; ${rate(uRate)} <span class="u">↑</span></div>
          <div class="link-bar"><span style="width:${barPct}%"></span></div>
        </div>
        <div class="link-controls">
          ${ctrl}
          <label class="toggle" title="Enable / disable">
            <input type="checkbox" class="en-in" data-if="${l.ifName}" ${l.enabled ? "checked" : ""} />
            <span class="track"></span>
          </label>
        </div>`;
      box.appendChild(div);
    });
  wireLinkControls();
}

// wireLinkControls attaches handlers to the per-link inputs just rendered.
function wireLinkControls() {
  document.querySelectorAll(".en-in").forEach((el) =>
    el.addEventListener("change", () => Backend.SetLinkEnabled(el.dataset.if, el.checked))
  );
  document.querySelectorAll(".wmode-sel").forEach((el) =>
    el.addEventListener("change", () => {
      Backend.SetLinkManual(el.dataset.if, el.value === "manual");
    })
  );
  document.querySelectorAll(".wslider").forEach((el) => {
    el.addEventListener("input", () => {
      const v = el.nextElementSibling;
      if (v) v.textContent = el.value;
    });
    el.addEventListener("change", () => Backend.SetLinkWeight(el.dataset.if, parseInt(el.value, 10)));
  });
  document.querySelectorAll(".prio-in").forEach((el) =>
    el.addEventListener("change", () => Backend.SetLinkPriority(el.dataset.if, parseInt(el.value, 10) || 0))
  );
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
function isWindows() {
  return /win/i.test(navigator.platform || navigator.userAgent || "");
}
function showError(msg) { const e = $("errmsg"); e.textContent = msg; e.hidden = false; }
function hideError() { $("errmsg").hidden = true; }
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

window.addEventListener("DOMContentLoaded", init);
