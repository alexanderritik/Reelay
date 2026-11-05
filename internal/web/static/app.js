// Reelay dashboard: a client of the public /videos API, nothing more.
"use strict";

const $ = (sel, el = document) => el.querySelector(sel);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
const view = $("#view");
const ORIGIN = location.origin;

const icons = {
  upload: '<svg viewBox="0 0 24 24"><path d="M12 16V4M7 9l5-5 5 5M5 20h14"/></svg>',
  back: '<svg viewBox="0 0 24 24"><path d="M15 18l-6-6 6-6"/></svg>',
  search: '<svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/></svg>',
  play: '<svg viewBox="0 0 24 24"><path d="M8 5.5v13l10-6.5z"/></svg>',
  edit: '<svg viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16v4zM14 6l4 4"/></svg>',
  x: '<svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg>',
  shield: '<svg viewBox="0 0 24 24"><path d="M12 3l8 3v6c0 4.5-3.4 8.3-8 9-4.6-.7-8-4.5-8-9V6l8-3z"/><path d="M9 12l2 2 4-4"/></svg>',
  alert: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16h.01"/></svg>',
  info: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/></svg>',
  check: '<svg viewBox="0 0 24 24"><path d="M5 12l5 5L20 7"/></svg>',
  bolt: '<svg viewBox="0 0 24 24"><path d="M13 2L4 14h7l-1 8 9-12h-7l1-8z"/></svg>',
  layers: '<svg viewBox="0 0 24 24"><path d="M12 3l9 5-9 5-9-5 9-5z"/><path d="M3 13l9 5 9-5"/></svg>',
  server: '<svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/></svg>',
  clock: '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>',
};

// ---------- formatting ----------
function fmtDuration(sec) {
  if (sec == null || !isFinite(sec)) return "";
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  const pad = (n) => String(n).padStart(2, "0");
  return h ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}
function fmtBytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toFixed(i > 1 ? 1 : 0)} ${u[i]}`;
}
function timeAgo(iso) {
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  if (s < 86400 * 7) return `${Math.round(s / 86400)} d ago`;
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
}
const pct = (p) => `${Math.floor(p)}%`;

function uuid() {
  if (crypto.randomUUID) return crypto.randomUUID(); // secure contexts only
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 15) | 64; b[8] = (b[8] & 63) | 128;
  const h = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// ---------- api ----------
class HTTPError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}
async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new HTTPError(res.status, data.error || `${res.status} ${res.statusText}`);
  return data;
}

// ---------- small UI helpers ----------
function toast(msg, kind = "") {
  const el = document.createElement("div");
  el.className = `toast ${kind}`;
  el.textContent = msg;
  $("#toasts").append(el);
  setTimeout(() => el.remove(), 3600);
}
async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text; ta.style.position = "fixed"; ta.style.opacity = "0";
    document.body.append(ta); ta.select(); document.execCommand("copy"); ta.remove();
  }
  toast("Copied to clipboard");
}
// Every .code block gets a Copy button that copies its raw text.
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  e.preventDefault();
  const src = btn.dataset.copy ? btn.dataset.copy : btn.closest(".code")?.querySelector("code")?.textContent;
  if (src) copy(src);
});
function codeBlock(text, lang = "") {
  const label = { sh: "Terminal", html: "HTML", json: "JSON" }[lang] || "";
  return `<div class="code"><div class="code-bar"><span>${label}</span><button class="copy" data-copy type="button">Copy</button></div><code>${highlight(text, lang)}</code></div>`;
}
// Just enough highlighting for shell and HTML snippets.
function highlight(text, lang) {
  let s = esc(text);
  if (lang === "sh") {
    s = s.replace(/^(#.*)$/gm, '<span class="tok-c">$1</span>')
      .replace(/(&quot;.*?&quot;|&#39;.*?&#39;)/g, '<span class="tok-s">$1</span>')
      .replace(/^(curl)\b/gm, '<span class="tok-k">$1</span>');
  } else if (lang === "html") {
    s = s.replace(/(&lt;\/?[a-z]+)/g, '<span class="tok-k">$1</span>')
      .replace(/(&quot;.*?&quot;)/g, '<span class="tok-s">$1</span>');
  } else if (lang === "json") {
    s = s.replace(/(&quot;[^&]*?&quot;)(\s*:)/g, '<span class="tok-k">$1</span>$2')
      .replace(/(:\s*)(&quot;.*?&quot;)/g, '$1<span class="tok-s">$2</span>');
  }
  return s;
}
function confirmDialog(title, text) {
  const d = $("#confirm");
  $("#confirmTitle").textContent = title;
  $("#confirmText").textContent = text;
  return new Promise((resolve) => {
    d.addEventListener("close", () => resolve(d.returnValue === "ok"), { once: true });
    d.showModal();
  });
}
function statusPill(v, cls = "") {
  if (v.status === "ready") return `<span class="pill ready ${cls}">Ready</span>`;
  if (v.status === "failed") return `<span class="pill failed ${cls}">Failed</span>`;
  const label = v.progress > 0 ? `Processing ${pct(v.progress)}` : "Preparing";
  return `<span class="pill processing ${cls}">${label}</span>`;
}

// ---------- theme ----------
$("#themeBtn").addEventListener("click", () => {
  const root = document.documentElement;
  const dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  root.dataset.theme = dark ? "light" : "dark";
  try { localStorage.setItem("reelay-theme", root.dataset.theme); } catch {}
});

// ---------- uploads ----------
// Each file gets its own Idempotency-Key, kept across retries, so a retry
// after a dropped connection can never create the same video twice.
const uploads = new Map();

function pickFiles() { $("#fileInput").click(); }
$("#uploadBtn").addEventListener("click", pickFiles);
$("#fileInput").addEventListener("change", (e) => {
  [...e.target.files].forEach((f) => startUpload(f));
  e.target.value = "";
});

function startUpload(file, key = uuid()) {
  const item = { key, file, loaded: 0, total: file.size, status: "uploading", started: performance.now(), error: "", video: null };
  uploads.set(key, item);
  const xhr = new XMLHttpRequest();
  item.xhr = xhr;
  xhr.open("POST", "/videos");
  xhr.setRequestHeader("Idempotency-Key", key);
  xhr.upload.onprogress = (e) => {
    if (!e.lengthComputable) return;
    item.loaded = e.loaded; item.total = e.total;
    if (e.loaded >= e.total) item.status = "checking";
    renderUploads();
  };
  xhr.onload = () => {
    let data = {};
    try { data = JSON.parse(xhr.responseText); } catch {}
    if (xhr.status >= 200 && xhr.status < 300) {
      item.status = "done"; item.video = data; item.loaded = item.total;
      toast(`“${data.title}” uploaded. Processing started.`);
      setTimeout(() => { uploads.delete(key); renderUploads(); }, 5000);
      refreshCurrent();
    } else {
      item.status = "error";
      item.error = data.error || `Upload failed (${xhr.status})`;
      item.permanent = xhr.status === 422 || xhr.status === 413;
    }
    renderUploads();
  };
  xhr.onerror = () => { item.status = "error"; item.error = "Connection lost. Retry to continue."; renderUploads(); };
  xhr.onabort = () => { uploads.delete(key); renderUploads(); };
  const fd = new FormData();
  fd.append("file", file);
  xhr.send(fd);
  renderUploads();
}

function renderUploads() {
  const el = $("#uploads");
  if (!uploads.size) { el.hidden = true; el.innerHTML = ""; return; }
  el.hidden = false;
  const items = [...uploads.values()];
  const active = items.filter((u) => u.status === "uploading" || u.status === "checking").length;
  el.innerHTML = `
    <div class="uploads-head">
      <span>${active ? `Uploading ${active} ${active === 1 ? "file" : "files"}` : "Uploads"}</span>
      ${active ? "" : `<button class="icon-btn sm" data-up-clear aria-label="Close">${icons.x}</button>`}
    </div>
    <div class="uploads-list">${items.map(uploadRow).join("")}</div>`;
}
function uploadRow(u) {
  const p = u.total ? (100 * u.loaded) / u.total : 0;
  const secs = (performance.now() - u.started) / 1000;
  let info = "", right = "";
  switch (u.status) {
    case "uploading":
      info = `${fmtBytes(u.loaded)} of ${fmtBytes(u.total)}`;
      right = secs > 1 ? `${fmtBytes(u.loaded / secs)}/s` : pct(p);
      break;
    case "checking": info = "Checking the video…"; break;
    case "done": info = "Uploaded. Processing started"; right = `<a href="#/videos/${esc(u.video.id)}">View</a>`; break;
    case "error": info = esc(u.error); break;
  }
  const actions = u.status === "error"
    ? `<div class="up-actions">${u.permanent ? "" : `<button class="btn sm" data-up-retry="${u.key}">Retry</button>`}<button class="btn sm" data-up-dismiss="${u.key}">Dismiss</button></div>`
    : u.status === "uploading" ? `<button class="icon-btn sm" data-up-cancel="${u.key}" aria-label="Cancel upload">${icons.x}</button>` : "";
  return `<div class="up ${u.status}">
    <div class="up-row"><span class="up-name" title="${esc(u.file.name)}">${esc(u.file.name)}</span>${u.status === "error" ? "" : actions}</div>
    <div class="progress"><i style="width:${u.status === "error" ? 100 : p}%"></i></div>
    <div class="up-info"><span>${info}</span><span>${right}</span></div>
    ${u.status === "error" ? actions : ""}
  </div>`;
}
$("#uploads").addEventListener("click", (e) => {
  const t = e.target.closest("button");
  if (!t) return;
  const { upCancel, upRetry, upDismiss } = t.dataset;
  if (upCancel) uploads.get(upCancel)?.xhr.abort();
  if (upRetry) { const u = uploads.get(upRetry); uploads.delete(upRetry); startUpload(u.file, u.key); }
  if (upDismiss) { uploads.delete(upDismiss); renderUploads(); }
  if ("upClear" in t.dataset) { uploads.clear(); renderUploads(); }
});

// Drag and drop anywhere on the page.
let dragDepth = 0;
const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes("Files");
addEventListener("dragenter", (e) => { if (!hasFiles(e)) return; e.preventDefault(); dragDepth++; $("#dropOverlay").hidden = false; });
addEventListener("dragover", (e) => { if (hasFiles(e)) e.preventDefault(); });
addEventListener("dragleave", (e) => { if (!hasFiles(e)) return; if (--dragDepth <= 0) { dragDepth = 0; $("#dropOverlay").hidden = true; } });
addEventListener("drop", (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault(); dragDepth = 0; $("#dropOverlay").hidden = true;
  [...e.dataTransfer.files].forEach((f) => startUpload(f));
});

// ---------- router ----------
let current = null;
function refreshCurrent() { current?.refresh?.(); }
function route() {
  current?.cleanup?.();
  const h = location.hash.slice(1) || "/";
  const m = h.match(/^\/videos\/([0-9a-f-]{36})$/);
  const name = m ? "videos" : h === "/api" ? "api" : "videos";
  document.querySelectorAll("[data-nav]").forEach((a) => a.classList.toggle("active", a.dataset.nav === name));
  current = m ? detailView(m[1]) : h === "/api" ? apiView() : listView();
  scrollTo(0, 0);
}
addEventListener("hashchange", route);

// poller calls fn now and again after the delay it returns, until stopped.
// Hidden tabs poll slowly.
function poller(fn) {
  let timer, alive = true;
  const run = async () => {
    clearTimeout(timer);
    let delay = 5000;
    try { delay = await fn(); } catch (err) { console.error(err); }
    if (alive) timer = setTimeout(run, document.hidden ? Math.max(delay, 15000) : delay);
  };
  run();
  return { stop() { alive = false; clearTimeout(timer); }, now: run };
}

// ---------- videos list ----------
const listState = { filter: "all", query: "", videos: null };

function listView() {
  view.innerHTML = `
    <section>
      <div class="page-head">
        <div>
          <h1>Videos</h1>
          <p class="sub">Upload a file, get a link that plays smoothly on any device.</p>
        </div>
      </div>
      <div class="stats" id="stats"></div>
      <div class="toolbar">
        <div class="seg" id="filters" role="tablist"></div>
        <label class="search">${icons.search}<input id="q" type="search" placeholder="Search by title" value="${esc(listState.query)}" autocomplete="off"></label>
      </div>
      <div class="grid" id="grid">${'<div class="skel skel-card"></div>'.repeat(4)}</div>
    </section>`;

  const cards = new Map();
  $("#q").addEventListener("input", (e) => { listState.query = e.target.value; render(); });
  $("#filters").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-f]");
    if (b) { listState.filter = b.dataset.f; render(); }
  });

  function render() {
    const all = listState.videos;
    if (!all) return;
    const count = (s) => all.filter((v) => v.status === s).length;
    const bytes = all.reduce((n, v) => n + (v.size_bytes || 0), 0);
    const secs = all.reduce((n, v) => n + (v.duration || 0), 0);
    const processing = count("processing");
    $("#stats").innerHTML = [
      ["Videos", all.length, ""],
      ["Processing", processing, processing ? "warn pulse" : "warn"],
      ["Video stored", secs ? fmtDuration(secs) : "0:00", "accent"],
      ["Storage used", fmtBytes(bytes), "ok"],
    ].map(([l, v, d]) => `<div class="stat"><div class="label">${d ? `<span class="dot ${d}"></span>` : ""}${l}</div><div class="value">${v}</div></div>`).join("");

    const f = listState.filter;
    $("#filters").innerHTML = [["all", "All", all.length], ["processing", "Processing", processing], ["ready", "Ready", count("ready")], ["failed", "Failed", count("failed")]]
      .map(([k, l, n]) => `<button data-f="${k}" class="${f === k ? "on" : ""}" role="tab" aria-selected="${f === k}">${l}<span class="count">${n}</span></button>`).join("");

    const grid = $("#grid");
    if (!all.length) {
      cards.clear();
      grid.innerHTML = emptyState();
      $(".empty-state", grid).addEventListener("click", (e) => { if (!e.target.closest(".code")) pickFiles(); });
      return;
    }
    const q = listState.query.trim().toLowerCase();
    const shown = all.filter((v) => (f === "all" || v.status === f) && (!q || v.title.toLowerCase().includes(q)));
    if (!shown.length) {
      cards.clear();
      grid.innerHTML = `<div class="no-results">No videos match${q ? ` “${esc(listState.query)}”` : ""}.</div>`;
      return;
    }
    if (grid.querySelector(".no-results, .empty-state, .skel")) grid.innerHTML = "";
    // Cards are kept by id and updated in place, so polling never reloads
    // thumbnails or makes the grid flicker.
    const keep = new Set();
    shown.forEach((v, i) => {
      let el = cards.get(v.id);
      if (!el) { el = createCard(v); cards.set(v.id, el); }
      updateCard(el, v);
      if (grid.children[i] !== el) grid.insertBefore(el, grid.children[i] || null);
      keep.add(v.id);
    });
    for (const [id, el] of cards) if (!keep.has(id)) { el.remove(); cards.delete(id); }
  }

  const poll = poller(async () => {
    const { data } = await api("GET", "/videos");
    listState.videos = data;
    render();
    const busy = data.some((v) => v.status === "processing");
    return busy ? 1500 : 8000;
  });
  if (listState.videos) render();
  return { cleanup: poll.stop, refresh: poll.now };
}

function emptyState() {
  return `<div class="empty-state" role="button" tabindex="0">
    <div class="empty-icon">${icons.upload}</div>
    <h2>Upload your first video</h2>
    <p>Drop a file anywhere on this page, or click to choose one. You get back a link that plays smoothly in any browser.</p>
    <div class="or">or use the API</div>
    ${codeBlock(`curl -F file=@video.mp4 ${ORIGIN}/videos`, "sh")}
  </div>`;
}

function createCard(v) {
  const a = document.createElement("a");
  a.className = "card";
  a.href = `#/videos/${v.id}`;
  a.innerHTML = `
    <div class="thumb">
      <img alt="" loading="lazy" decoding="async">
      <span class="pill"></span>
      <span class="dur"></span>
      <div class="play-hint">${icons.play}</div>
      <div class="bar"><i></i></div>
    </div>
    <div class="card-body">
      <div class="card-title"></div>
      <div class="card-meta"></div>
    </div>`;
  const img = $("img", a);
  img.addEventListener("error", () => a.querySelector(".thumb").classList.add("noimg"));
  img.src = v.thumbnail_url;
  return a;
}

function updateCard(el, v) {
  el.className = `card s-${v.status}${v.playable ? " playable" : ""}`;
  const pill = $(".pill", el);
  const tmp = document.createElement("div");
  tmp.innerHTML = statusPill(v);
  pill.className = tmp.firstChild.className;
  pill.textContent = tmp.firstChild.textContent;
  $(".dur", el).textContent = fmtDuration(v.duration);
  $(".bar i", el).style.width = `${v.progress}%`;
  $(".card-title", el).textContent = v.title;
  $(".card-title", el).title = v.title;
  const meta = [timeAgo(v.created_at), fmtBytes(v.size_bytes)];
  if (v.source_height) meta.push(`${v.source_height}p`);
  if (v.status === "processing" && v.playable) meta.push("Watch now");
  $(".card-meta", el).innerHTML = meta.map((m) => `<span>${esc(m)}</span>`).join("");
}

// ---------- video detail ----------
function detailView(id) {
  view.innerHTML = `
    <section>
      <a class="back" href="#/">${icons.back} All videos</a>
      <div class="detail-head">
        <div class="title-wrap" id="titleWrap"><div class="skel" style="height:32px;width:280px;border-radius:8px"></div></div>
        <span id="statusPill"></span>
      </div>
      <div class="detail-grid">
        <div class="col-main">
          <div class="player">
            <video id="video" controls playsinline preload="metadata"></video>
            <div class="player-overlay" id="overlay"></div>
          </div>
          <div class="panel" id="pipeline"></div>
        </div>
        <aside class="col-side">
          <div class="panel" id="share"></div>
          <div class="panel" id="details"></div>
          <div class="panel danger-zone" id="danger"></div>
        </aside>
      </div>
    </section>`;

  const video = $("#video");
  let hls = null, attached = "", editing = false, tab = "embed", shareKey = "", last = null;

  function attach(v) {
    if (!v.playable || attached === v.play_url) return;
    attached = v.play_url;
    video.poster = v.thumbnail_url;
    if (window.Hls && Hls.isSupported()) {
      // While processing the playlist grows, which hls.js treats as live and
      // would start at the end; start at the beginning instead.
      hls = new Hls({ startPosition: 0 });
      hls.on(Hls.Events.ERROR, (_, d) => { if (d.fatal) console.warn("player", d.type, d.details); });
      hls.loadSource(v.play_url);
      hls.attachMedia(video);
    } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
      video.src = v.play_url; // Safari plays HLS natively
    }
  }

  function renderTitle(v) {
    if (editing) return;
    $("#titleWrap").innerHTML = `<h1 title="${esc(v.title)}">${esc(v.title)}</h1>
      <button class="icon-btn sm" id="editTitle" aria-label="Rename video" title="Rename">${icons.edit}</button>`;
    $("#editTitle").onclick = () => {
      editing = true;
      $("#titleWrap").innerHTML = `<input class="title-input" id="titleInput" maxlength="200" value="${esc(v.title)}" aria-label="Video title">`;
      const input = $("#titleInput");
      input.focus(); input.select();
      let done = false;
      const finish = async (save) => {
        if (done) return;
        done = true; editing = false;
        const title = input.value.trim();
        if (save && title && title !== v.title) {
          try { last = await api("PATCH", `/videos/${id}`, { title }); toast("Renamed"); } catch (err) { toast(err.message, "bad"); }
        }
        renderTitle(last);
      };
      input.addEventListener("keydown", (e) => { if (e.key === "Enter") finish(true); if (e.key === "Escape") finish(false); });
      input.addEventListener("blur", () => finish(true));
    };
  }

  function renderOverlay(v) {
    const o = $("#overlay");
    if (v.playable) { o.hidden = true; return; }
    o.hidden = false;
    o.style.backgroundImage = `url("${v.thumbnail_url}")`;
    const r = 28, c = 2 * Math.PI * r;
    const p = v.status === "failed" ? 100 : v.progress;
    const ring = `<div class="ring ${v.status === "failed" ? "failed" : ""}"><svg viewBox="0 0 64 64"><circle class="track" cx="32" cy="32" r="${r}"/><circle class="fill" cx="32" cy="32" r="${r}" stroke-dasharray="${c}" stroke-dashoffset="${c * (1 - p / 100)}"/></svg></div>`;
    o.innerHTML = v.status === "failed"
      ? `${ring}<strong>Processing failed</strong><span>See the error below. Your original upload is unchanged.</span>`
      : `${ring}<strong>${v.progress > 0 ? `Processing ${pct(v.progress)}` : "Preparing"}</strong>
         <span>${v.progress > 0 ? "Long videos start playing as soon as the first chunk is encoded." : "Encoding starts as soon as a worker is free."}</span>`;
  }

  function renderPipeline(v) {
    const encodeDone = v.status === "ready" || v.progress >= 99;
    const stage = v.status === "ready" ? 4 : encodeDone ? 2 : 1;
    const failedAt = v.status === "failed" ? stage : -1;
    const steps = [["Uploaded", 100], ["Encoding", encodeDone ? 100 : v.progress], ["Verifying", stage > 2 ? 100 : stage === 2 ? 60 : 0], ["Ready", v.status === "ready" ? 100 : 0]];
    const stepHTML = steps.map(([name, p], i) => {
      const cls = i === failedAt ? "failed" : (i < stage || v.status === "ready") ? "done" : i === stage ? "active" : "";
      const label = i === 1 && cls === "active" && p > 0 ? pct(p) : "";
      return `<div class="step ${cls}"><div class="line"><i style="width:${cls === "active" ? p : ""}%"></i></div><div class="name"><span>${name}</span><span>${label}</span></div></div>`;
    }).join("");

    let body, summary = "";
    const parts = v.parts || [];
    if (parts.length) {
      const chunks = parts.filter((p) => p.kind === "chunk");
      const done = parts.filter((p) => p.state === "done").length;
      const retried = parts.filter((p) => p.attempts > 1).length;
      const cells = parts.map((p) => {
        const label = p.kind === "audio" ? "Audio" : p.index;
        const span = p.kind === "audio" ? "" : ` · ${fmtDuration(p.start)}–${fmtDuration(p.end)}`;
        const title = `${p.kind === "audio" ? "Audio track" : `Chunk ${p.index}`}${span} · ${p.state}${p.state === "running" ? ` ${pct(p.progress)}` : ""}${p.attempts > 1 ? ` · attempt ${p.attempts}` : ""}`;
        return `<div class="cell s-${p.state} ${p.kind}" style="--p:${p.state === "running" ? p.progress : 0}%" title="${esc(title)}"><span>${label}</span>${p.attempts > 1 ? '<b class="retry">↻</b>' : ""}</div>`;
      }).join("");
      body = `
        <div class="chunks">${cells}</div>
        <div class="legend">
          <span><i></i>Queued</span><span><i class="l-run"></i>Encoding</span><span><i class="l-val"></i>Checking</span>
          <span><i class="l-done"></i>Done</span><span><i class="l-fail"></i>Failed</span><span>↻ Retried</span>
        </div>
        ${retried ? `<div class="note">${icons.shield}<div><strong>${retried} ${retried === 1 ? "part was" : "parts were"} retried automatically</strong> after a worker stopped or an output failed its checks. Finished parts were kept and nothing was lost.</div></div>` : ""}`;
      summary = `${done} of ${parts.length} parts done · ${chunks.length} chunks${parts.length > chunks.length ? " + audio" : ""}`;
    } else {
      body = `<div class="single">
        <div class="progress"><i style="width:${v.status === "ready" ? 100 : v.progress}%"></i></div>
        <span class="muted">${v.status === "ready" ? "Encoded in one pass and checked frame by frame." : "Short videos encode in one pass. Long ones are split into chunks that encode in parallel."}</span>
      </div>`;
    }
    const err = v.status === "failed" && v.error
      ? `<div class="note bad">${icons.alert}<div><strong>Why it failed</strong><br><code>${esc(v.error)}</code></div></div>` : "";
    $("#pipeline").innerHTML = `
      <div class="panel-head"><h2>Processing</h2><span class="muted">${summary}</span></div>
      <div class="steps">${stepHTML}</div>
      ${body}${err}`;
  }

  function snippets(v) {
    const iframe = `<iframe src="${v.embed_url}" width="640" height="360"\n  frameborder="0" allow="autoplay; fullscreen" allowfullscreen></iframe>`;
    const html = `<video id="player" controls playsinline\n  poster="${v.thumbnail_url}"></video>\n<script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.13"></script>\n<script>\n  const video = document.getElementById("player");\n  const src = "${v.play_url}";\n  if (video.canPlayType("application/vnd.apple.mpegurl")) video.src = src;\n  else { const hls = new Hls(); hls.loadSource(src); hls.attachMedia(video); }\n</script>`;
    const sh = `curl ${ORIGIN}/videos/${v.id}`;
    return { embed: [iframe, "html"], html: [html, "html"], api: [sh, "sh"] };
  }

  function renderShare(v) {
    const key = `${v.play_url}|${tab}`;
    if (key === shareKey) return;
    shareKey = key;
    const s = snippets(v);
    $("#share").innerHTML = `
      <div class="panel-head"><h2>Play it anywhere</h2></div>
      ${v.play_url ? `<div class="field"><input readonly value="${esc(v.play_url)}" aria-label="Play URL" onfocus="this.select()"><button class="btn" data-copy="${esc(v.play_url)}">Copy</button></div>`
        : `<div class="field"><input readonly value="Available once encoding starts" aria-label="Play URL"></div>`}
      <div class="tabs" role="tablist">
        ${[["embed", "Embed"], ["html", "HTML"], ["api", "API"]].map(([k, l]) => `<button data-tab="${k}" class="${tab === k ? "on" : ""}" role="tab">${l}</button>`).join("")}
      </div>
      ${tab === "api" || v.play_url ? codeBlock(...s[tab]) : `<div class="waiting">Snippets appear once the video has a play URL.</div>`}`;
    $("#share .tabs").onclick = (e) => {
      const b = e.target.closest("button[data-tab]");
      if (b) { tab = b.dataset.tab; renderShare(v); }
    };
  }

  function renderDetails(v) {
    const rows = [
      ["Duration", fmtDuration(v.duration) || "—"],
      ["Source", [v.source_height ? `${v.source_height}p` : "", fmtBytes(v.size_bytes)].filter(Boolean).join(" · ")],
      ["File", v.filename || "—"],
      ["Playback", v.format === "hls" ? "HLS (fMP4)" : "—"],
      ["Uploaded", new Date(v.created_at).toLocaleString()],
      ["Video ID", `<span class="mono" title="${esc(v.id)}">${esc(v.id)}</span>`, true],
    ];
    $("#details").innerHTML = `
      <div class="panel-head"><h2>Details</h2><button class="btn sm" data-copy="${esc(v.id)}">Copy ID</button></div>
      <dl class="dl">${rows.map(([k, val, raw]) => `<dt>${k}</dt><dd ${raw ? "" : `title="${esc(val)}"`}>${raw ? val : esc(val)}</dd>`).join("")}</dl>`;
  }

  function renderDanger(v) {
    const busy = v.status === "processing";
    $("#danger").innerHTML = `
      <h2>Delete video</h2>
      <p>${busy ? "You can delete it once processing finishes." : "Removes the original upload and every encoded file. This cannot be undone."}</p>
      <button class="btn danger" id="del" ${busy ? "disabled" : ""}>Delete video</button>`;
    $("#del").onclick = async () => {
      if (!(await confirmDialog("Delete this video?", `“${v.title}” and all of its files will be removed for good.`))) return;
      try {
        await api("DELETE", `/videos/${id}`);
        listState.videos = listState.videos?.filter((x) => x.id !== id) ?? null;
        toast("Video deleted");
        location.hash = "#/";
      } catch (err) { toast(err.message, "bad"); }
    };
  }

  function render(v) {
    last = v;
    document.title = `${v.title} · Reelay`;
    renderTitle(v);
    $("#statusPill").innerHTML = statusPill(v, "lg");
    attach(v);
    renderOverlay(v);
    renderPipeline(v);
    renderShare(v);
    renderDetails(v);
    if (!$("#danger").dataset.status || $("#danger").dataset.status !== v.status) {
      renderDanger(v);
      $("#danger").dataset.status = v.status;
    }
  }

  const poll = poller(async () => {
    try {
      const v = await api("GET", `/videos/${id}`);
      render(v);
      return v.status === "processing" ? 1000 : 15000;
    } catch (err) {
      if (err.status === 404) {
        view.innerHTML = `<div class="empty-state" style="cursor:default"><div class="empty-icon">${icons.alert}</div><h2>Video not found</h2><p>It may have been deleted.</p><a class="btn" href="#/" style="margin-top:12px">Back to videos</a></div>`;
        poll.stop();
      }
      throw err;
    }
  });
  return {
    cleanup() { poll.stop(); hls?.destroy(); document.title = "Reelay"; },
    refresh: poll.now,
  };
}

// ---------- API guide ----------
function apiView() {
  const sample = {
    id: "3f0c1e5a-…",
    title: "launch-demo",
    status: "processing",
    progress: 42.5,
    playable: true,
    format: "hls",
    play_url: `${ORIGIN}/videos/3f0c1e5a-…/hls/master.m3u8`,
    thumbnail_url: `${ORIGIN}/videos/3f0c1e5a-…/thumbnail.jpg`,
    embed_url: `${ORIGIN}/embed/3f0c1e5a-…`,
    duration: 184.2,
    size_bytes: 104857600,
  };
  const endpoints = [
    ["post", "/videos", "Upload a video (multipart field file, optional title). Send an Idempotency-Key header so a retried upload never creates a duplicate."],
    ["get", "/videos", "List videos, newest first."],
    ["get", "/videos/{id}", "Status, progress, play URL and the per-chunk map."],
    ["patch", "/videos/{id}", "Rename: {\"title\": \"…\"}"],
    ["delete", "/videos/{id}", "Delete a ready or failed video and all of its files."],
    ["get", "/videos/{id}/hls/master.m3u8", "HLS playback. For long videos this works while processing is still running."],
    ["get", "/videos/{id}/thumbnail.jpg", "Poster frame, available right after upload."],
    ["get", "/embed/{id}", "A ready-made player page for an iframe."],
  ];
  view.innerHTML = `
    <section>
      <div class="hero">
        <h1>Add video to your app in an afternoon.</h1>
        <p>Upload a file and get back a link that plays smoothly on any device. It runs on your own servers, and a video keeps processing even if a worker crashes halfway through.</p>
        <div class="chips">
          <span class="chip">${icons.shield}Crash-safe processing</span>
          <span class="chip">${icons.bolt}Plays while encoding</span>
          <span class="chip">${icons.server}Your own servers</span>
          <span class="chip soon">${icons.layers}360p–1080p ladder · soon</span>
          <span class="chip soon">${icons.clock}Resumable uploads · soon</span>
        </div>
      </div>

      <div class="steps-3">
        <div class="panel step-card"><h3><span class="num">1</span>Upload</h3><p>Send the file. You get an id straight away.</p>
          ${codeBlock(`curl -F file=@video.mp4 \\\n  -H "Idempotency-Key: $(uuidgen)" \\\n  ${ORIGIN}/videos`, "sh")}</div>
        <div class="panel step-card"><h3><span class="num">2</span>Check status</h3><p>Poll until <code>status</code> is <code>ready</code>, or start playing as soon as <code>playable</code> is true.</p>
          ${codeBlock(`curl ${ORIGIN}/videos/<id>`, "sh")}</div>
        <div class="panel step-card"><h3><span class="num">3</span>Play</h3><p>Drop the player into any page, or use <code>play_url</code> with your own player.</p>
          ${codeBlock(`<iframe src="${ORIGIN}/embed/<id>"\n  allowfullscreen></iframe>`, "html")}</div>
      </div>

      <div class="two">
        <div class="panel">
          <h2 class="section-title">Endpoints</h2>
          <table class="endpoints">
            <thead><tr><th>Endpoint</th><th>What it does</th></tr></thead>
            <tbody>${endpoints.map(([m, p, d]) => `<tr><td><span class="method m-${m}">${m.toUpperCase()}</span><code>${esc(p)}</code></td><td class="muted">${esc(d)}</td></tr>`).join("")}</tbody>
          </table>
        </div>
        <div class="panel">
          <h2 class="section-title">A video, as JSON</h2>
          ${codeBlock(JSON.stringify(sample, null, 2), "json")}
          <div class="note">${icons.info}<div><code>status</code> is <code>processing</code>, <code>ready</code> or <code>failed</code>. A failed video carries an <code>error</code> that says why.</div></div>
        </div>
      </div>
    </section>`;
  return {};
}

route();
