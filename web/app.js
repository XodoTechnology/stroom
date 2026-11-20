// Stroom web UI — talks to /api/* over SSE + fetch.
"use strict";

const state = { data: null, view: "torrents", servers: [], editingServer: null, updateDismissed: false,
  speedHist: [], prevJobs: {}, expanded: {}, filesCache: {} };
const $ = (s) => document.querySelector(s);
// UI token passed as ?token= (EventSource can't set headers).
const urlToken = new URLSearchParams(location.search).get("token") || "";
function withToken(path) {
  if (!urlToken) return path;
  return path + (path.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(urlToken);
}

// ---------- utils ----------
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function fmtBytes(n) {
  if (n == null) return "—";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : Math.round(n)) + " " + u[i];
}
function fmtSpeed(n) { return n > 0 ? fmtBytes(n) + "/s" : "—"; }
function fmtEta(sec) {
  if (sec == null || sec < 0 || !isFinite(sec)) return "—";
  if (sec < 60) return Math.round(sec) + "s";
  if (sec < 3600) return Math.round(sec / 60) + "m";
  if (sec < 86400) return Math.round(sec / 3600) + "h";
  return Math.round(sec / 86400) + "d";
}
function pct(d, t) { return t > 0 ? Math.min(100, (d / t) * 100) : 0; }
function toast(msg) {
  const t = $("#toast"); t.textContent = msg; t.classList.remove("hidden");
  setTimeout(() => t.classList.add("hidden"), 3500);
}
async function api(path, opts) {
  const r = await fetch(withToken(path), opts);
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(j.error || r.statusText);
  return j;
}
function postJSON(path, body) {
  return api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
}

// ---------- rendering ----------
function render() {
  const d = state.data; if (!d) return;
  $("#dlSpeed").textContent = fmtSpeed(d.global.downloadSpeed);
  $("#ulSpeed").textContent = fmtSpeed(d.global.uploadSpeed);
  $("#peerCount").textContent = (d.global.peers || 0) + " peers · " + (d.global.torrents || 0) + " torrents · " + (d.global.nzbJobs || 0) + " nzb";
  renderTorrents(d.torrents || []);
  renderNZB(d.nzb || []);
  renderServers(d.servers || []);
  renderUpdate(d.update);
  pushSpeed(d.global);
  drawSpeedGraph();
  checkNotifications(d);
  restoreExpanded();
  $("#verTag").textContent = d.version ? "v" + d.version.replace(/^v+/, "") : "";
  if (!state.settingsLoaded) loadSettingsIntoForm(d.config);
}

function progBar(done, total, err) {
  const p = pct(done, total);
  const cls = err ? "err" : p >= 100 ? "done" : "";
  return `<div class="prog ${cls}"><i style="width:${p}%"></i><span>${p.toFixed(1)}%</span></div>`;
}

function jobActionBtns(kind, id, paused, status) {
  const done = status === "completed" || status === "seeding" || status === "error";
  const key = kind + ":" + id;
  const open = state.expanded[key] ? "▾" : "▸";
  let b = `<button class="mini expbtn" data-files="${esc(key)}" title="files">${open}</button>`;
  if (!done) b += `<button class="mini" data-act="${paused ? "resume" : "pause"}" data-kind="${kind}" data-id="${esc(id)}">${paused ? "▶" : "⏸"}</button>`;
  b += `<button class="mini danger" data-act="remove" data-kind="${kind}" data-id="${esc(id)}">✕</button>`;
  return `<td class="col-act">${b}</td>`;
}

function renderTorrents(list) {
  const f = $("#filterT").value.toLowerCase();
  const rows = $("#torrentRows");
  const filtered = list.filter(t => !f || (t.name || "").toLowerCase().includes(f) || (t.hash || "").includes(f));
  $("#torrentsEmpty").style.display = filtered.length ? "none" : "block";
  rows.innerHTML = filtered.map(t => {
    const st = t.status || "";
    const paused = st === "paused";
    const badges = `<span class="badge t">BT</span>` +
      (paused ? `<span class="badge paused">paused</span>` : "") +
      (st === "seeding" ? `<span class="badge done">seed</span>` : "") +
      (st === "metadata" ? `<span class="badge">meta</span>` : "") +
      (st === "error" ? `<span class="badge err">${esc(t.error || "err")}</span>` : "");
    const dl = t.downSpeed ? fmtSpeed(t.downSpeed) : "—";
    const ul = t.upSpeed ? fmtSpeed(t.upSpeed) : "—";
    return `<tr data-job="torrent:${esc(t.id)}">
      <td><div class="namecell" title="${esc(t.name)}">${badges}${esc(t.name)}</div></td>
      <td>${fmtBytes(t.size)}</td>
      <td>${progBar(t.done || 0, t.size, st === "error")}</td>
      <td class="dim">${dl}</td><td class="dim">${ul}</td>
      <td>${t.peers ?? "—"}${t.seeds ? "/" + t.seeds : ""}</td><td>${fmtEta(t.eta)}</td>
      ${jobActionBtns("torrent", t.id, paused, st)}
    </tr>`;
  }).join("");
}

function renderNZB(list) {
  const f = $("#filterN").value.toLowerCase();
  const rows = $("#nzbRows");
  const filtered = list.filter(j => !f || (j.name || "").toLowerCase().includes(f));
  $("#nzbEmpty").style.display = filtered.length ? "none" : "block";
  rows.innerHTML = filtered.map(j => {
    const st = j.status || "";
    const paused = st === "paused";
    const badges = `<span class="badge n">NZB</span>` +
      (paused ? `<span class="badge paused">paused</span>` : "") +
      (st === "done" ? `<span class="badge done">done</span>` : "") +
      (st === "error" ? `<span class="badge err">${esc(j.error || "err")}</span>` : "");
    const segTxt = j.segmentsTotal ? `${j.segmentsDone || 0}/${j.segmentsTotal}` : "—";
    const failTxt = j.segmentsFailed ? ` <span class="badge err">-${j.segmentsFailed} fail</span>` : "";
    return `<tr data-job="nzb:${esc(j.id)}">
      <td><div class="namecell" title="${esc(j.name)}">${badges}${esc(j.name)}</div></td>
      <td>${fmtBytes(j.totalBytes)}</td>
      <td>${progBar(j.doneBytes || 0, j.totalBytes, st === "error")}</td>
      <td class="dim">${fmtSpeed(j.downSpeed)}</td>
      <td>${segTxt}${failTxt}</td>
      <td>—</td>
      ${jobActionBtns("nzb", j.id, paused, st)}
    </tr>`;
  }).join("");
}

function renderServers(list) {
  state.servers = list;
  $("#serversEmpty").style.display = list.length ? "none" : "block";
  $("#serverRows").innerHTML = list.map(s => `<tr>
    <td>${esc(s.name || s.host)}</td><td>${esc(s.host)}</td><td>${s.port}</td>
    <td>${s.tls ? "TLS" : "—"}</td><td>${esc(s.username || "—")}</td><td>${s.connections}</td>
    <td><button class="mini" data-srvtest="${esc(s.id)}">test</button>
        <button class="mini" data-srvedit="${esc(s.id)}">edit</button>
        <button class="mini danger" data-srvdel="${esc(s.id)}">✕</button></td>
  </tr>`).join("");
  // refresh add-dialog server select
  const sel = $("#addServer");
  sel.innerHTML = `<option value="">auto</option>` + list.map(s =>
    `<option value="${esc(s.id)}">${esc(s.name || s.host)}</option>`).join("");
}

// ---------- update bar ----------
function renderUpdate(u) {
  const bar = $("#updateBar");
  if (!u || !u.configured) { bar.classList.add("hidden"); return; }
  if (u.available && !state.updateDismissed) {
    $("#updateMsg").innerHTML = `Update available: <b>v${esc(u.latest)}</b> (you have v${esc(u.current)})`;
    bar.classList.remove("hidden");
  } else if (u.error && !u.available) {
    $("#updateMsg").textContent = "Update check failed: " + u.error;
    bar.classList.remove("hidden");
  } else {
    bar.classList.add("hidden");
  }
}

// ---------- settings form ----------
let settingsLoaded = false;
function loadSettingsIntoForm(c) {
  if (!c) return;
  const f = $("#settingsForm");
  f.downloadDir.value = c.downloadDir || "";
  f.listenAddr.value = c.listenAddr || "";
  f.btListenPort.value = c.btListenPort ?? "";
  f.downloadLimit.value = c.downloadLimit ? Math.round(c.downloadLimit / 1024) : "";
  f.uploadLimit.value = c.uploadLimit ? Math.round(c.uploadLimit / 1024) : "";
  f.nzbMaxConnections.value = c.nzbMaxConnections || 12;
  f.nzbMissingAbortPct.value = Math.round((c.nzbMissingAbortPct ?? 0.20) * 100);
  f.enableDHT.checked = !!c.enableDHT;
  f.enableUpnp.checked = !!c.enableUpnp;
  f.enablePex.checked = !!c.enablePex;
  f.uiToken.value = "";
  f.uiToken.placeholder = c.hasToken ? "•••• (set)" : "leave empty for no auth";
  f.webUI.checked = !c.disableWebUI;
  f.watchDir.value = c.watchDir || "";
  state.settingsLoaded = true; settingsLoaded = true;
}

// ---------- events ----------
function wireEvents() {
  wireDragDrop();
  // delegated actions
  document.addEventListener("contextmenu", (e) => {
    const tr = e.target.closest("tr[data-job]");
    if (!tr) { hideCtx(); return; }
    e.preventDefault();
    const key = tr.dataset.job, i = key.indexOf(":");
    const kind = key.slice(0, i), id = key.slice(i + 1);
    showCtx(e.clientX, e.clientY, ctxItems(kind, id, findJob(kind, id)));
  });
  // single click on a job row → details dialog
  document.addEventListener("click", (e) => {
    if (e.target.closest("button,select,a,input,textarea")) return;
    const tr = e.target.closest("tr[data-job]");
    if (!tr) return;
    const key = tr.dataset.job, i = key.indexOf(":");
    const kind = key.slice(0, i), id = key.slice(i + 1);
    showDetails(kind, id, findJob(kind, id));
  });

  // details dialog tabs
  document.querySelectorAll("#detTabs [data-dtab]").forEach(b => b.onclick = () => setDetTab(b.dataset.dtab));
  document.addEventListener("click", hideCtx);
  document.addEventListener("scroll", hideCtx, true);
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") hideCtx(); });

  document.addEventListener("click", async (e) => {
    const b = e.target.closest("button[data-act]");
    if (b) {
      const { act, kind, id } = b.dataset;
      if (act === "remove") { showRemoveModal(kind, id); return; }
      try {
        await postJSON(`/api/jobs/${kind}/${id}/${act}`, {});
      } catch (err) { toast(err.message); }
      return;
    }
    const of = e.target.closest("a[data-openfile]");
    if (of) {
      e.preventDefault();
      const p = of.dataset.openfile.split("|");
      postJSON(`/api/jobs/${p[0]}/${p[1]}/open`, { path: p.slice(2).join("|") })
        .then(() => toast("opened on server")).catch(err => toast(err.message));
      return;
    }
    const fb = e.target.closest("button[data-files]");
    if (fb) {
      const i = fb.dataset.files.indexOf(":");
      toggleFiles(fb.dataset.files.slice(0, i), fb.dataset.files.slice(i + 1));
      return;
    }
    const st = e.target.closest("button[data-srvtest]");
    if (st) {
      st.disabled = true; st.textContent = "…";
      try {
        const r = await api(`/api/servers/${encodeURIComponent(st.dataset.srvtest)}/test`, { method: "POST" });
        toast(r.ok ? `server OK (${r.ms}ms)` : `server failed: ${r.error}`);
      } catch (err) { toast("server test: " + err); }
      st.disabled = false; st.textContent = "test";
      return;
    }
    const se = e.target.closest("button[data-srvedit]");
    if (se) { openServerModal(se.dataset.srvedit); return; }
    const sd = e.target.closest("button[data-srvdel]");
    if (sd) {
      if (!confirm("Delete this news server?")) return;
      try { await api(`/api/servers/${encodeURIComponent(sd.dataset.srvdel)}`, { method: "DELETE" }); toast("server deleted"); }
      catch (err) { toast(err.message); }
      return;
    }
  });

  document.addEventListener("change", async (e) => {
    const sel = e.target.closest("select[data-prio]");
    if (!sel) return;
    try {
      await postJSON(`/api/jobs/${sel.dataset.kind}/${sel.dataset.id}/files/prio`,
        { path: sel.dataset.prio, priority: parseInt(sel.value, 10) });
      const key = fileKey(sel.dataset.kind, sel.dataset.id);
      delete state.filesCache[key]; // refresh priorities on next expand
      toast("priority updated");
    } catch (err) { toast(err.message); }
  });

  $("#filterT").addEventListener("input", render);
  $("#filterN").addEventListener("input", render);

  document.querySelectorAll("#sidebar a").forEach(a => a.addEventListener("click", (e) => {
    e.preventDefault();
    document.querySelectorAll("#sidebar a").forEach(x => x.classList.remove("active"));
    a.classList.add("active");
    state.view = a.dataset.view;
    document.querySelectorAll(".view").forEach(v => v.classList.remove("active"));
    $("#view-" + state.view).classList.add("active");
  }));

  // pause/resume all
  $("#btnPauseAll").onclick = () => bulk("pause");
  $("#btnResumeAll").onclick = () => bulk("resume");
  async function bulk(act) {
    const jobs = (state.data?.torrents || []).filter(t => act === "pause" ? t.status !== "paused" && t.status !== "done" : t.status === "paused");
    for (const t of jobs) { try { await postJSON(`/api/jobs/torrent/${t.id}/${act}`, {}); } catch {} }
  }

  // add modal
  $("#btnAdd").onclick = () => $("#addModal").classList.remove("hidden");
  $("#btnAddCancel").onclick = $("#btnAddCancel2").onclick = () => $("#addModal").classList.add("hidden");
  document.querySelectorAll(".tabs [data-atab]").forEach(b => b.onclick = () => {
    document.querySelectorAll(".tabs button").forEach(x => x.classList.remove("active"));
    b.classList.add("active");
    document.querySelectorAll(".atab").forEach(x => x.classList.remove("active"));
    $("#atab-" + b.dataset.atab).classList.add("active");
  });
  $("#btnAddGo").onclick = async () => {
    const uri = $("#addUri").value.trim();
    if (!uri) return;
    try {
      const body = { uri };
      const sid = $("#addServer").value; if (sid) body.serverID = sid;
      const r = await postJSON("/api/add", body);
      toast(`added ${r.kind} job`);
      $("#addUri").value = ""; $("#addModal").classList.add("hidden");
    } catch (err) { toast(err.message); }
  };
  $("#btnAddFileGo").onclick = async () => {
    const f = $("#addFile").files[0]; if (!f) return;
    const fd = new FormData(); fd.append("file", f);
    const sid = $("#addServer").value; if (sid) fd.append("serverID", sid);
    try {
      const r = await api("/api/add", { method: "POST", body: fd });
      toast(`added ${r.kind} job`);
      $("#addFile").value = ""; $("#addModal").classList.add("hidden");
    } catch (err) { toast(err.message); }
  };

  // servers
  $("#btnAddServer").onclick = () => openServerModal(null);
  $("#btnServerCancel").onclick = () => $("#serverModal").classList.add("hidden");
  $("#serverForm").onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const srv = {
      name: fd.get("name"), host: fd.get("host"),
      port: parseInt(fd.get("port") || "0", 10),
      tls: fd.get("tls") === "on",
      username: fd.get("username") || "",
      password: fd.get("password") || "",
      connections: parseInt(fd.get("connections") || "12", 10),
    };
    if (state.editingServer) srv.id = state.editingServer;
    const servers = (state.data?.servers || []).filter(s => s.id !== state.editingServer);
    servers.push(srv);
    try {
      await postJSON("/api/servers", { servers });
      toast("server saved"); $("#serverModal").classList.add("hidden");
    } catch (err) { toast(err.message); }
  };

  // update bar
  $("#btnUpdateX").onclick = () => { state.updateDismissed = true; $("#updateBar").classList.add("hidden"); };
  $("#btnUpdateCheck").onclick = async () => {
    try { const u = await postJSON("/api/update/check", {}); renderUpdate(u); toast(u.available ? "update available: v" + u.latest : "up to date"); }
    catch (e) { toast(e.message); }
  };
  $("#btnUpdate").onclick = async () => {
    if (!confirm("Download and install the update now? Stroom will need a restart after.")) return;
    try {
      const r = await postJSON("/api/update/apply", {});
      toast(r.message || "updated — restart to apply");
      state.updateDismissed = true; $("#updateBar").classList.add("hidden");
    } catch (e) { toast(e.message); }
  };

  // remove modal
  $("#btnRemoveCancel").onclick = () => $("#removeModal").classList.add("hidden");
  $("#btnRemoveGo").onclick = async () => {
    const rj = state.removeTarget;
    $("#removeModal").classList.add("hidden");
    if (!rj) return;
    const delFiles = $("#removeDelFiles").checked;
    try {
      await postJSON(`/api/jobs/${rj.kind}/${rj.id}/remove`, { deleteFiles: delFiles });
      toast(delFiles ? "removed + files deleted" : "removed");
    } catch (err) { toast(err.message); }
  };

  // tracker modal
  $("#btnTrackerCancel").onclick = () => $("#trackerModal").classList.add("hidden");
  $("#btnTrackerGo").onclick = async () => {
    const ids = $("#trackerList").value.split(/\r?\n/).map(x => x.trim()).filter(Boolean);
    if (!ids.length) return;
    try {
      await postJSON(`/api/jobs/torrent/${state.ctxJobId}/trackers`, { trackers: ids });
      toast("trackers added"); $("#trackerModal").classList.add("hidden");
    } catch (e) { toast(e.message); }
  };
  $("#btnDetailsClose").onclick = () => $("#detailsModal").classList.add("hidden");

  // settings
  $("#settingsForm").onsubmit = async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const body = {
      downloadDir: fd.get("downloadDir"),
      listenAddr: fd.get("listenAddr"),
      btListenPort: parseInt(fd.get("btListenPort") || "0", 10),
      downloadLimit: (parseInt(fd.get("downloadLimit") || "0", 10) || 0) * 1024,
      uploadLimit: (parseInt(fd.get("uploadLimit") || "0", 10) || 0) * 1024,
      nzbMaxConnections: parseInt(fd.get("nzbMaxConnections") || "12", 10),
      nzbMissingAbortPct: (parseFloat(fd.get("nzbMissingAbortPct") || "20") || 20) / 100,
      enableDHT: fd.get("enableDHT") === "on",
      enableUpnp: fd.get("enableUpnp") === "on",
      enablePex: fd.get("enablePex") === "on",
    };
    const tok = fd.get("uiToken");
    if (tok) body.uiToken = tok; // empty = keep existing token
    body.disableWebUI = fd.get("webUI") !== "on";
    body.watchDir = fd.get("watchDir") || "";
    try {
      const r = await postJSON("/api/settings", body);
      $("#settingsMsg").textContent = r.restartRequired ? "saved — restart needed for: " + r.restartRequired.join(", ") : "saved";
      state.settingsLoaded = false;
    } catch (err) { toast(err.message); }
  };
}

function openServerModal(id) {
  state.editingServer = id;
  const f = $("#serverForm"); f.reset();
  if (id) {
    const s = (state.data?.servers || []).find(x => x.id === id);
    if (s) {
      f.name.value = s.name || ""; f.host.value = s.host || "";
      f.port.value = s.port || ""; f.tls.checked = !!s.tls;
      f.username.value = s.username || ""; f.connections.value = s.connections || 12;
      f.password.placeholder = s.hasPass ? "•••• (unchanged)" : "";
    }
  }
  $("#serverModal").classList.remove("hidden");
}

// ---------- speed graph ----------
function pushSpeed(g) {
  state.speedHist.push({ d: g.downloadSpeed || 0, u: g.uploadSpeed || 0 });
  if (state.speedHist.length > 160) state.speedHist.shift();
}
function drawSpeedGraph() {
  const c = $("#speedGraph"); if (!c) return;
  const x = c.getContext("2d"), W = c.width, H = c.height;
  x.clearRect(0, 0, W, H);
  const h = state.speedHist;
  if (h.length < 2) return;
  const max = Math.max(1, ...h.map(p => Math.max(p.d, p.u)));
  const step = W / (h.length - 1);
  x.strokeStyle = "#2c3547"; x.beginPath(); x.moveTo(0, H - 0.5); x.lineTo(W, H - 0.5); x.stroke();
  const plot = (key, color) => {
    x.strokeStyle = color; x.lineWidth = 1.1; x.beginPath();
    h.forEach((p, i) => {
      const px = i * step, py = H - 2 - (p[key] / max) * (H - 5);
      i ? x.lineTo(px, py) : x.moveTo(px, py);
    });
    x.stroke();
  };
  plot("d", "#4ade80"); plot("u", "#fbbf24");
}

// ---------- completion notifications ----------
function checkNotifications(d) {
  if (!("Notification" in window)) return;
  const doneSt = ["done", "seeding", "completed"];
  for (const [kind, list] of [["torrent", d.torrents || []], ["nzb", d.nzb || []]]) {
    for (const j of list) {
      const key = kind + ":" + j.id, st = j.status || "", was = state.prevJobs[key];
      if (doneSt.includes(st) && was && !doneSt.includes(was)) {
        try { new Notification("Stroom — " + (j.name || "download"), { body: "Download complete" }); } catch (e) {}
      }
      state.prevJobs[key] = st;
    }
  }
}

// ---------- drag & drop ----------
function wireDragDrop() {
  const overlay = $("#dropOverlay");
  let depth = 0;
  const interesting = (e) => e.dataTransfer &&
    (e.dataTransfer.types.includes("Files") || e.dataTransfer.types.includes("text/uri-list") || e.dataTransfer.types.includes("text/plain"));
  window.addEventListener("dragenter", (e) => {
    if (!interesting(e)) return;
    e.preventDefault(); depth++; overlay.classList.remove("hidden");
  });
  window.addEventListener("dragleave", (e) => {
    if ((e.clientX === 0 && e.clientY === 0) || --depth <= 0) { depth = 0; overlay.classList.add("hidden"); }
  });
  window.addEventListener("dragover", (e) => { if (interesting(e)) e.preventDefault(); });
  window.addEventListener("drop", async (e) => {
    if (!interesting(e)) return;
    e.preventDefault(); depth = 0; overlay.classList.add("hidden");
    const files = [...(e.dataTransfer.files || [])];
    const uris = (e.dataTransfer.getData("text/uri-list") || e.dataTransfer.getData("text/plain") || "")
      .split(/\r?\n/).map(s => s.trim()).filter(s => s && !s.startsWith("#"));
    let n = 0;
    for (const f of files) {
      const fd = new FormData(); fd.append("file", f);
      try { await api("/api/add", { method: "POST", body: fd }); n++; }
      catch (err) { toast("drop: " + err.message); }
    }
    for (const u of uris) {
      try { await postJSON("/api/add", { uri: u }); n++; }
      catch (err) { toast("drop: " + err.message); }
    }
    if (n) toast("added " + n + " item" + (n > 1 ? "s" : ""));
  });
  // ask notification permission on first interaction
  window.addEventListener("pointerdown", () => {
    if ("Notification" in window && Notification.permission === "default") {
      try { Notification.requestPermission(); } catch (e) {}
    }
  }, { once: true });
}

// ---------- per-file panels ----------
function fileKey(kind, id) { return kind + ":" + id; }

async function toggleFiles(kind, id) {
  const key = fileKey(kind, id);
  if (state.expanded[key]) {
    delete state.expanded[key];
    const row = document.querySelector(`tr[data-job="${CSS.escape(key)}"]`);
    const panel = row && row.nextElementSibling;
    if (panel && panel.classList.contains("files-row")) panel.remove();
    const btn = document.querySelector(`button[data-files="${CSS.escape(key)}"]`);
    if (btn) btn.textContent = "▸";
    return;
  }
  state.expanded[key] = true;
  if (!state.filesCache[key]) {
    try { state.filesCache[key] = await api(`/api/jobs/${kind}/${id}/files`); }
    catch (e) { delete state.expanded[key]; toast(e.message); return; }
  }
  if (state.expanded[key]) restoreOne(kind, id);
}

function fileRowsHTML(kind, id, files) {
  const isT = kind === "torrent";
  const prioName = { 0: "skip", 1: "normal", 2: "high" };
  return (files || []).map(f => {
    const pct = f.size > 0 ? (100 * (f.done || 0) / f.size).toFixed(0) + "%" : "—";
    const complete = f.size > 0 && f.done >= f.size;
    let prio = "";
    if (isT) {
      prio = `<select data-prio="${esc(f.path)}" data-kind="${kind}" data-id="${esc(id)}">` +
        [0, 1, 2].map(v => `<option value="${v}" ${f.priority === v ? "selected" : ""}>${prioName[v]}</option>`).join("") + `</select>`;
    }
    const fpath = (f.path || f.name || "").split("/").map(encodeURIComponent).join("/");
    const dl = complete
      ? `<a href="${withToken("/api/files/" + kind + "/" + encodeURIComponent(id) + "/" + fpath)}" download>⬇</a> ` +
        `<a href="#" data-openfile="${esc(kind)}|${esc(id)}|${esc(f.path || f.name)}" title="open with default app">▶</a>`
      : `<span class="dim">—</span>`;
    const seg = f.segments ? `<span class="dim">${f.segDone || 0}/${f.segments} seg</span> ` : "";
    return `<tr><td class="fname" title="${esc(f.path || f.name)}">${esc(f.path || f.name)}</td>
      <td>${fmtBytes(f.size)}</td><td>${fmtBytes(f.done)}</td><td>${pct}</td><td>${seg}${prio}</td><td>${dl}</td></tr>`;
  }).join("");
}

function filesPanelHTML(kind, id) {
  const data = state.filesCache[fileKey(kind, id)];
  if (!data || !data.files) return "";
  const isT = kind === "torrent";
  const rows = fileRowsHTML(kind, id, data.files);
  return `<tr class="files-row" data-ts="${Date.now()}"><td colspan="20"><table class="filetable">
    <thead><tr><th>Name</th><th>Size</th><th>Done</th><th>%</th><th>${isT ? "Priority" : "Segments"}</th><th></th></tr></thead>
    <tbody>${rows}</tbody></table></td></tr>`;
}

function restoreOne(kind, id) {
  const key = fileKey(kind, id);
  const tr = document.querySelector(`tr[data-job="${CSS.escape(key)}"]`);
  if (!tr) return;
  const html = filesPanelHTML(kind, id);
  if (!html) return;
  const tmp = document.createElement("tbody");
  tmp.innerHTML = html;
  const panel = tmp.firstElementChild;
  const old = tr.nextElementSibling;
  if (old && old.classList.contains("files-row")) {
    if (Date.now() - (+old.dataset.ts || 0) < 5000) return; // fresh enough
    old.replaceWith(panel);
  } else {
    tr.after(panel);
  }
  const btn = document.querySelector(`button[data-files="${CSS.escape(key)}"]`);
  if (btn) btn.textContent = "▾";
}

function restoreExpanded() {
  for (const key of Object.keys(state.expanded)) {
    if (!state.filesCache[key]) continue;
    const i = key.indexOf(":");
    restoreOne(key.slice(0, i), key.slice(i + 1));
  }
}

// ---------- context menu ----------
let ctxEl = null;
function hideCtx() { if (ctxEl) { ctxEl.remove(); ctxEl = null; } }

function showCtx(x, y, items) {
  hideCtx();
  ctxEl = document.createElement("div");
  ctxEl.className = "ctx-menu";
  for (const it of items) {
    if (it === "-") {
      const sep = document.createElement("div");
      sep.className = "ctx-sep"; ctxEl.appendChild(sep); continue;
    }
    const b = document.createElement("button");
    b.className = "ctx-item" + (it.danger ? " danger" : "");
    b.textContent = it.label;
    b.onclick = () => { hideCtx(); it.fn && it.fn(); };
    ctxEl.appendChild(b);
  }
  document.body.appendChild(ctxEl);
  const r = ctxEl.getBoundingClientRect();
  ctxEl.style.left = Math.min(x, innerWidth - r.width - 8) + "px";
  ctxEl.style.top = Math.min(y, innerHeight - r.height - 8) + "px";
}

function findJob(kind, id) {
  const list = kind === "torrent" ? (state.data?.torrents || []) : (state.data?.nzb || []);
  return list.find(j => j.id === id) || {};
}

function ctxItems(kind, id, job) {
  const st = job.status || "";
  const paused = st === "paused";
  const doneSt = ["done", "seeding", "completed"].includes(st);
  const items = [
    { label: "Open containing folder", fn: () => postJSON(`/api/jobs/${kind}/${id}/reveal`, {}).then(() => toast("opened on server")).catch(e => toast(e.message)) },
    "-",
    { label: paused ? "Resume" : "Pause", fn: () => postJSON(`/api/jobs/${kind}/${id}/${paused ? "resume" : "pause"}`, {}).catch(e => toast(e.message)) },
  ];
  if (kind === "torrent") {
    items.push(
      { label: "Force recheck", fn: () => postJSON(`/api/jobs/torrent/${id}/recheck`, {}).then(() => toast("recheck started")).catch(e => toast(e.message)) },
      { label: "Add trackers…", fn: () => { state.ctxJobId = id; $("#trackerList").value = ""; $("#trackerModal").classList.remove("hidden"); } },
      { label: "Rename…", fn: async () => {
          const name = prompt("New name:", job.name || "");
          if (!name) return;
          try { await postJSON(`/api/jobs/torrent/${id}/rename`, { name }); toast("renamed"); }
          catch (e) { toast(e.message); }
        } },
      { label: "Set max connections…", fn: async () => {
          const n = prompt("Max peer connections (0 = default):", "");
          if (n === null) return;
          try { await postJSON(`/api/jobs/torrent/${id}/maxconns`, { n: parseInt(n, 10) || 0 }); toast("saved"); }
          catch (e) { toast(e.message); }
        } },
      { label: "Copy magnet link", fn: async () => {
          try {
            const r = await fetch(withToken(`/api/jobs/torrent/${id}/magnet`));
            const m = await r.text();
            await navigator.clipboard.writeText(m); toast("magnet copied");
          } catch (e) { toast(e.message); }
        } },
      { label: "Copy info hash", fn: () => { navigator.clipboard.writeText(id).then(() => toast("hash copied")); } },
      { label: "Download .torrent", fn: () => window.open(withToken(`/api/jobs/torrent/${id}/torrent`), "_blank") },
      "-",
    );
  }
  items.push(
    { label: "Details…", fn: () => showDetails(kind, id, job) },
    "-",
    { label: "Remove (keep files)", fn: () => removeJob(kind, id, false) },
    { label: "Remove + delete files", danger: true, fn: () => {
        if (confirm("Delete downloaded files too? This cannot be undone.")) removeJob(kind, id, true);
      } },
  );
  return items;
}

function showRemoveModal(kind, id) {
  const job = findJob(kind, id);
  state.removeTarget = { kind, id };
  $("#removeMsg").textContent = `Remove "${job.name || id}" from the list?`;
  $("#removeDelFiles").checked = false;
  $("#removeModal").classList.remove("hidden");
}

async function removeJob(kind, id, delFiles) {
  try { await postJSON(`/api/jobs/${kind}/${id}/remove`, { deleteFiles: delFiles }); toast("removed"); }
  catch (e) { toast(e.message); }
}

// ---------- details modal ----------
let detJob = null;
const DET_SRC = { Tr: "tracker", I: "incoming", X: "pex", Hg: "dht", Ha: "dht", M: "direct", C: "utp" };
const detTabs = { info: () => $("#detInfo"), files: () => $("#detFiles"), peers: () => $("#detPeers") };

function setDetTab(t) {
  document.querySelectorAll("#detTabs [data-dtab]").forEach(b => b.classList.toggle("active", b.dataset.dtab === t));
  for (const [k, el] of Object.entries(detTabs)) el().classList.toggle("active", k === t);
}

async function showDetails(kind, id, job) {
  detJob = { kind, id };
  $("#detName").textContent = job.name || job.id || id;
  $("#dtPeersTab").style.display = kind === "torrent" ? "" : "none";
  setDetTab("info");
  $("#detInfo").innerHTML = `<div class="dim">loading…</div>`;
  $("#detFiles").innerHTML = "";
  $("#detPeers").innerHTML = "";
  $("#detailsModal").classList.remove("hidden");
  const [info, files] = await Promise.all([
    kind === "torrent"
      ? api(`/api/jobs/torrent/${id}/details`).catch(e => ({ error: e.message }))
      : Promise.resolve(null),
    api(`/api/jobs/${kind}/${id}/files`).catch(e => ({ error: e.message })),
  ]);
  if (!detJob || detJob.kind !== kind || detJob.id !== id) return; // switched jobs
  state.filesCache[fileKey(kind, id)] = files;
  renderDetInfo(kind, id, job, info);
  renderDetFiles(kind, id, files);
  if (kind === "torrent" && info && !info.error) renderDetPeers(info);
}

function detRow(k, v) { return `<dt>${esc(k)}</dt><dd>${v}</dd>`; }

function renderDetInfo(kind, id, job, info) {
  const el = $("#detInfo");
  if (kind === "nzb") {
    const seg = job.segmentsTotal ? `${job.segmentsDone || 0}/${job.segmentsTotal} segments` +
      (job.segmentsFailed ? ` (${job.segmentsFailed} failed)` : "") : "—";
    el.innerHTML = `<dl class="det-grid">` +
      detRow("Status", esc(job.status || "—")) +
      detRow("Progress", `${((job.progress || 0) * 100).toFixed(1)}%`) +
      detRow("Size", fmtBytes(job.totalBytes)) +
      detRow("Downloaded", fmtBytes(job.doneBytes)) +
      detRow("Speed", fmtSpeed(job.downSpeed)) +
      detRow("Segments", esc(seg)) +
      detRow("Server", esc(job.serverName || job.serverID || "—")) +
      detRow("Directory", esc(job.dir || "—")) +
      detRow("Added", esc(job.addedAt ? new Date(job.addedAt).toLocaleString() : "—")) +
      (job.error ? detRow("Error", `<span class="badge err">${esc(job.error)}</span>`) : "") +
      detRow("ID", `<code>${esc(id)}</code>`) +
      `</dl>`;
    return;
  }
  if (!info || info.error) {
    el.innerHTML = `<div class="dim">details unavailable: ${esc(info && info.error || "unknown")}</div>` +
      `<dl class="det-grid">` + detRow("Status", esc(job.status || "—")) + detRow("ID", `<code>${esc(id)}</code>`) + `</dl>`;
    return;
  }
  const st = info.stats || {};
  const pctTxt = info.size > 0 ? (100 * ((job.done || 0) / info.size)).toFixed(1) + "%" : "—";
  const trackers = (info.trackers || []).map(t => `<li>${esc(t)}</li>`).join("");
  el.innerHTML = `<dl class="det-grid">` +
    detRow("Status", esc(job.status || (info.seeding ? "seeding" : "downloading"))) +
    detRow("Progress", pctTxt) +
    detRow("Size", fmtBytes(info.size)) +
    detRow("Downloaded", fmtBytes(job.done || 0)) +
    detRow("Download", fmtSpeed(job.downSpeed)) +
    detRow("Upload", fmtSpeed(job.upSpeed)) +
    detRow("ETA", fmtEta(job.eta)) +
    detRow("Peers", `${st.activePeers ?? "—"} active · ${st.pendingPeers ?? 0} pending · ${st.connectedSeeds ?? 0} seeds`) +
    detRow("Swarm", `${info.peerCount ?? (info.peers || []).length} known`) +
    detRow("Pieces", `${st.piecesComplete ?? "—"}/${info.pieces ?? "—"}`) +
    detRow("Max connections", info.maxConns ? info.maxConns : "default") +
    detRow("Directory", esc(info.dir || "—")) +
    detRow("Added", esc(info.addedAt ? new Date(info.addedAt).toLocaleString() : "—")) +
    detRow("Info hash", `<code>${esc(id)}</code>`) +
    `</dl>` +
    (trackers ? `<div class="det-sec">Trackers</div><ul class="det-list">${trackers}</ul>` : "");
}

function renderDetFiles(kind, id, files) {
  const el = $("#detFiles");
  if (!files || files.error) {
    el.innerHTML = `<div class="dim">files unavailable: ${esc(files && files.error || "unknown")}</div>`;
    return;
  }
  const isT = kind === "torrent";
  const rows = fileRowsHTML(kind, id, files.files);
  el.innerHTML = `<table class="filetable"><thead><tr>
    <th>Name</th><th>Size</th><th>Done</th><th>%</th><th>${isT ? "Priority" : "Segments"}</th><th></th>
    </tr></thead><tbody>${rows}</tbody></table>`;
}

function renderDetPeers(info) {
  const el = $("#detPeers");
  const peers = (info.peers || []).slice().sort((a, b) => a.addr.localeCompare(b.addr));
  const total = info.peerCount || peers.length;
  if (!peers.length) {
    el.innerHTML = `<div class="dim">No peers known yet — the swarm list appears once trackers/DHT respond.</div>`;
    return;
  }
  const maxShow = 500;
  const shown = peers.slice(0, maxShow);
  const rows = shown.map(p => `<tr>
    <td><code>${esc(p.addr)}</code></td>
    <td class="dim" title="${esc(p.id)}"><code>${esc(p.id.slice(0, 16))}…</code></td>
    <td>${esc(DET_SRC[p.src] || p.src || "—")}</td>
    <td>${p.enc ? "yes" : "—"}</td>
  </tr>`).join("");
  el.innerHTML = `<table class="filetable"><thead><tr>
    <th>Address</th><th>Peer ID</th><th>Source</th><th>Enc</th>
    </tr></thead><tbody>${rows}</tbody></table>` +
    (total > shown.length ? `<div class="dim" style="padding:6px">…and ${total - shown.length} more</div>` : "");
}

// ---------- boot + SSE ----------
async function boot() {
  wireEvents();
  try {
    const r = await fetch("/api/state");
    state.data = await r.json();
    render();
  } catch (e) { toast("API unreachable: " + e.message); }
  const es = new EventSource(withToken("/api/events"));
  es.onmessage = (ev) => {
    try { state.data = JSON.parse(ev.data); render(); } catch {}
  };
  es.onerror = () => { /* auto-reconnect */ };
}
boot();
