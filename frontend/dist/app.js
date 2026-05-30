// InternetMerge frontend. Talks to the Go backend through the Wails-injected
// globals: window.go.main.App.<Method>() (Promises) and window.runtime.EventsOn.

const $ = (id) => document.getElementById(id);

// Backend handles, resolved once the Wails runtime is ready.
let Backend = null;

let lastStats = {}; // interface -> previous cumulative Sample, for rate calc
let lastTs = 0;

function backend() {
  return window.go && window.go.main && window.go.main.App;
}

async function init() {
  // Wait briefly for the Wails runtime bindings to be injected.
  for (let i = 0; i < 50 && !backend(); i++) {
    await new Promise((r) => setTimeout(r, 50));
  }
  Backend = backend();
  if (!Backend) {
    $("errmsg").textContent = "Wails runtime not available.";
    return;
  }

  $("refreshIfaces").addEventListener("click", loadInterfaces);
  $("autoBtn").addEventListener("click", autoStart);
  $("startBtn").addEventListener("click", start);
  $("stopBtn").addEventListener("click", stop);

  await loadInterfaces();
  await loadServices();

  if (window.runtime && window.runtime.EventsOn) {
    window.runtime.EventsOn("status", onStatus);
  }
  // Prime current status (in case already running).
  try { onStatus(await Backend.Status()); } catch (_) {}
}

async function loadInterfaces() {
  const box = $("interfaces");
  box.textContent = "Loading interfaces…";
  try {
    const ifaces = await Backend.ListInterfaces();
    box.innerHTML = "";
    (ifaces || []).forEach((it) => {
      const usable = it.up && it.ipv4;
      const row = document.createElement("label");
      row.className = "ifrow" + (usable ? "" : " disabled");
      row.innerHTML = `
        <input type="checkbox" value="${it.name}" ${usable ? "" : "disabled"} />
        <div class="ifmain">
          <div class="ifname">${escapeHtml(it.label || it.name)} <span class="ifmeta">(${it.name})</span></div>
          <div class="ifmeta">${it.ipv4 ? "IPv4 " + it.ipv4 : "no IPv4 address"}</div>
        </div>
        <span class="pill ${usable ? "" : "no"}">${usable ? "ready" : "unavailable"}</span>`;
      box.appendChild(row);
    });
    if (!box.children.length) box.textContent = "No interfaces found.";
  } catch (e) {
    box.textContent = "Failed to load interfaces: " + e;
  }
}

async function loadServices() {
  const box = $("services");
  try {
    const svcs = await Backend.NetworkServices();
    box.innerHTML = "";
    (svcs || []).forEach((name) => {
      const el = document.createElement("label");
      el.className = "svc";
      el.innerHTML = `<input type="checkbox" value="${escapeHtml(name)}" /> ${escapeHtml(name)}`;
      box.appendChild(el);
    });
    if (!box.children.length) box.textContent = "none";
  } catch (e) {
    box.textContent = "unavailable";
  }
}

function selectedInterfaces() {
  return Array.from($("interfaces").querySelectorAll("input:checked")).map((c) => c.value);
}
function selectedServices() {
  return Array.from($("services").querySelectorAll("input:checked")).map((c) => c.value);
}

async function start() {
  $("errmsg").textContent = "";
  const interfaces = selectedInterfaces();
  if (interfaces.length === 0) {
    $("errmsg").textContent = "Select at least one interface.";
    return;
  }
  try {
    await Backend.Start({
      interfaces,
      addr: $("addr").value.trim() || "127.0.0.1:1080",
      proxyServices: selectedServices(),
    });
    lastStats = {};
    lastTs = 0;
  } catch (e) {
    $("errmsg").textContent = "Start failed: " + e;
  }
}

async function autoStart() {
  $("errmsg").textContent = "";
  try {
    await Backend.AutoStart();
    lastStats = {};
    lastTs = 0;
  } catch (e) {
    $("errmsg").textContent = "Auto-bond failed: " + e;
  }
}

async function stop() {
  try {
    await Backend.Stop();
  } catch (e) {
    $("errmsg").textContent = "Stop failed: " + e;
  }
}

function onStatus(st) {
  if (!st) return;
  const running = !!st.running;
  $("statusBadge").textContent = running ? "Running" : "Stopped";
  $("statusBadge").className = "badge " + (running ? "running" : "stopped");
  $("startBtn").disabled = running;
  $("autoBtn").disabled = running;
  $("stopBtn").disabled = !running;
  if (st.error) $("errmsg").textContent = st.error;

  const body = $("statsBody");
  const stats = st.stats || [];
  const links = {};
  (st.links || []).forEach((l) => (links[l.ifName] = l));

  // Compute rates from cumulative byte diffs.
  const now = Date.now();
  const dt = lastTs ? Math.max((now - lastTs) / 1000, 0.001) : 1;
  lastTs = now;

  let totalDown = 0, totalUp = 0, totalConns = 0;

  if (!running || stats.length === 0) {
    body.innerHTML = `<tr><td colspan="6" class="muted">${running ? "No traffic yet." : "Not running."}</td></tr>`;
  } else {
    body.innerHTML = "";
    stats
      .sort((a, b) => a.interface.localeCompare(b.interface))
      .forEach((s) => {
        const prev = lastStats[s.interface] || { bytesDown: s.bytesDown, bytesUp: s.bytesUp };
        const downRate = Math.max(0, (s.bytesDown - prev.bytesDown) / dt);
        const upRate = Math.max(0, (s.bytesUp - prev.bytesUp) / dt);
        lastStats[s.interface] = s;
        totalDown += downRate; totalUp += upRate; totalConns += s.connections;

        const li = links[s.interface] || {};
        const label = li.label && li.label !== s.interface ? `${li.label} (${s.interface})` : s.interface;
        const tr = document.createElement("tr");
        tr.innerHTML = `
          <td>${escapeHtml(label)}</td>
          <td>${li.weight ?? "-"}</td>
          <td>${li.alive ? '<span class="dot up"></span>yes' : '<span class="dot down"></span>no'}</td>
          <td>${s.connections}</td>
          <td>${rate(downRate)}</td>
          <td>${rate(upRate)}</td>`;
        body.appendChild(tr);
      });
  }

  $("totalDown").textContent = rate(totalDown);
  $("totalUp").textContent = rate(totalUp);
  $("totalConns").textContent = totalConns;
}

function rate(bps) {
  const units = ["B", "KB", "MB", "GB"];
  let v = bps, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? Math.round(v) : v.toFixed(1)) + " " + units[i] + "/s";
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

window.addEventListener("DOMContentLoaded", init);
