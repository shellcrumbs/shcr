/* Shellcrumbs dashboard.
   Everything on the page comes from the local API; nothing is fetched from
   anywhere else. Command text is inserted as text nodes rather than markup,
   because a recorded command is arbitrary input and is never to be trusted as
   HTML. */

const TOKEN = new URLSearchParams(location.search).get("token") || "";

async function api(path, options = {}) {
  const url = path + (path.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(TOKEN);
  const res = await fetch(url, options);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `request failed (${res.status})`);
  return body;
}

// ------------------------------------------------------------------ helpers

const $ = (id) => document.getElementById(id);

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

function fmtDuration(ms) {
  if (ms == null) return null;
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  if (ms < 3600000) return `${Math.floor(ms / 60000)}m ${Math.floor((ms % 60000) / 1000)}s`;
  return `${Math.floor(ms / 3600000)}h ${Math.floor((ms % 3600000) / 60000)}m`;
}

function relTime(ms) {
  const d = Date.now() - ms;
  if (d < 60000) return "just now";
  if (d < 3600000) return `${Math.floor(d / 60000)} min ago`;
  if (d < 86400000) return `${Math.floor(d / 3600000)} hr ago`;
  if (d < 172800000) return "yesterday";
  if (d < 2592000000) return `${Math.floor(d / 86400000)} days ago`;
  return new Date(ms).toLocaleDateString();
}

const clock = (ms) => new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const shortId = (s) => (s || "").slice(-4);

// A table row is one line. The stored command keeps its newlines; only the
// summary collapses, and it says so rather than running the lines together.
function firstLine(s) {
  const i = (s || "").indexOf("\n");
  return i < 0 ? s : s.slice(0, i).replace(/\s+$/, "") + " ↵";
}

function shortPath(p) {
  if (!p) return "";
  if (state.home && p === state.home) return "~";
  if (state.home && p.startsWith(state.home + "/")) return "~" + p.slice(state.home.length);
  return p;
}

let toastTimer;
function toast(message, isError) {
  const t = $("toast");
  t.textContent = message;
  t.classList.toggle("error", !!isError);
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove("show"), 2600);
}

// ------------------------------------------------------------------ state

const state = {
  commands: new Map(),
  runs: {},
  home: "",
  total: 0,
  devices: [],
  filters: { q: "", host: "", status: "" },
  // Which row the keyboard is on. -1 is "none yet", so the first press of j or
  // down lands on the newest command rather than the second one.
  cursor: -1,
  selected: null,
};

// ------------------------------------------------------------------ rows

// A row's second line says whatever is most worth knowing about it — why it
// failed, why it has no end, or that you have run it before today.
function rowMeta(c) {
  if (c.status === "failed" && c.exit_code != null) return `exit code ${c.exit_code}`;
  if (c.status === "orphaned") return "no end event recorded";
  if (c.imported) return "from a shell history file — no exit code recorded";
  const run = state.runs[c.id];
  if (run && run.total > 1) return `${ordinal(run.ordinal)} run today`;
  if (c.session_id) return `session ${shortId(c.session_id)}`;
  return "";
}

function ordinal(n) {
  const s = ["th", "st", "nd", "rd"], v = n % 100;
  return n + (s[(v - 20) % 10] || s[v] || s[0]);
}

function badge(status) {
  const b = el("span", `badge ${status}`);
  b.appendChild(el("span", "dot"));
  b.appendChild(document.createTextNode(status.charAt(0).toUpperCase() + status.slice(1)));
  return b;
}

// An imported command was never observed running, so it gets its own badge
// rather than a green "Completed" it cannot support.
function statusBadge(c) {
  if (!c.imported) return badge(c.status);
  const b = el("span", "badge imported");
  b.appendChild(el("span", "dot"));
  b.appendChild(document.createTextNode("Imported"));
  return b;
}

function commandRow(c) {
  const row = el("button", "row data");
  row.type = "button";

  const first = el("div");
  const cmd = el("div", "cmd", firstLine(c.command) || "not recorded");
  if (!c.command || c.command === "[REDACTED]") cmd.classList.add("redacted");
  first.appendChild(cmd);
  const meta = rowMeta(c);
  if (meta) first.appendChild(el("div", "cmd-meta", meta));

  const where = el("div", "col-dim");
  where.appendChild(el("span", "host", c.hostname || "—"));
  where.appendChild(document.createElement("br"));
  where.appendChild(el("span", "dir", shortPath(c.cwd)));

  const started = el("div", "col-dim", relTime(c.start_time));
  const dur = el("div", "col-dim",
    c.status === "orphaned" ? "unknown" : (fmtDuration(c.duration_ms) ?? "—"));

  row.append(first, where, started, dur, statusBadge(c));
  row.addEventListener("click", () => openDetail(c.id));
  return row;
}

function renderRows() {
  const container = $("rows");
  container.replaceChildren();

  const rows = [...state.commands.values()].sort((a, b) => b.start_time - a.start_time);
  if (rows.length === 0) {
    container.appendChild(el("div", "empty",
      state.filters.q || state.filters.host || state.filters.status
        ? "No commands match these filters."
        : "Nothing recorded yet. Run a command in a shell with the hooks loaded."));
  } else {
    rows.forEach((c, i) => {
      const row = commandRow(c);
      if (i === state.cursor) row.classList.add("cursor");
      container.appendChild(row);
    });
  }
  state.visible = rows;
  $("footCount").textContent = `Showing ${rows.length} of ${state.total} commands`;
}

// ---- Keyboard -----------------------------------------------------------

// The table is a list and behaves like one everywhere else; it should here too.
function moveCursor(delta) {
  const rows = state.visible || [];
  if (rows.length === 0) return;
  const next = state.cursor < 0
    ? (delta > 0 ? 0 : rows.length - 1)
    : Math.min(Math.max(state.cursor + delta, 0), rows.length - 1);
  if (next === state.cursor) return;
  state.cursor = next;

  const nodes = $("rows").querySelectorAll(".row.data");
  nodes.forEach((n, i) => n.classList.toggle("cursor", i === state.cursor));
  const node = nodes[state.cursor];
  if (node) node.scrollIntoView({ block: "nearest" });

  // Following the selection while the panel is open turns j/k into a way of
  // reading through history, rather than a way of closing and reopening it.
  if (state.selected) openDetail(rows[state.cursor].id);
}

function openCursor() {
  const rows = state.visible || [];
  if (state.cursor >= 0 && rows[state.cursor]) openDetail(rows[state.cursor].id);
}

function toggleShortcuts(show) {
  const box = $("shortcuts");
  box.classList.toggle("open", show);
  box.setAttribute("aria-hidden", show ? "false" : "true");
}

// Typing "j" in a text field must type a j. Only the keys that cannot be
// confused with input — Escape, and the arrows — work while one has focus.
function isTyping(target) {
  return target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement ||
    target instanceof HTMLSelectElement;
}

// ------------------------------------------------------------------ loading

function queryString() {
  const p = new URLSearchParams();
  if (state.filters.q) p.set("q", state.filters.q);
  if (state.filters.host) p.set("host", state.filters.host);
  if (state.filters.status) p.set("status", state.filters.status);
  p.set("limit", "200");
  return p.toString();
}

async function loadCommands() {
  const { commands, runs } = await api("/api/commands?" + queryString());
  state.runs = runs || {};
  state.commands = new Map(commands.map((c) => [c.id, c]));
  renderRows();
}

async function loadStats() {
  const s = await api("/api/stats");
  state.total = s.total;
  // Colour carries meaning, so a zero stays neutral. A red 0 next to "Failed
  // today" reads as an alarm when it is the best possible news.
  setStat("statToday", s.today, null);
  setStat("statRunning", s.running, "c-accent");
  setStat("statFailed", s.failed, "c-danger");
  setStat("statOrphaned", s.orphaned, "c-warning");
  $("rhythmCount").textContent = `${s.today} command${s.today === 1 ? "" : "s"} today`;

  const bars = $("bars");
  bars.replaceChildren();
  const peak = Math.max(1, ...s.hourly.map((h) => h.count));
  s.hourly.forEach((h, i) => {
    // A silent hour gets a flat rule, not a short bar. Giving zero the same
    // minimum height as one command made an empty database look like steady
    // activity all day, which is the opposite of what a rhythm chart is for.
    const silent = h.count === 0;
    const bar = el("div", "bar" + (silent ? " silent" : "") + (i === s.hourly.length - 1 ? " now" : ""));
    // Set through the CSSOM rather than a style attribute, so the page needs no
    // inline-style allowance in its CSP.
    bar.style.height = silent ? "2px" : Math.round((h.count / peak) * 100) + "%";
    bar.title = `${h.count} command${h.count === 1 ? "" : "s"} at ${clock(h.hour)}`;
    bars.appendChild(bar);
  });

  const labels = $("barLabels");
  labels.replaceChildren();
  [0, 6, 12, 18, 23].forEach((i) => {
    if (s.hourly[i]) labels.appendChild(el("span", null, clock(s.hourly[i].hour)));
  });
}

function setStat(id, value, colorClass) {
  const node = $(id);
  node.textContent = value;
  node.className = "stat-value" + (value > 0 && colorClass ? " " + colorClass : "");
}

async function loadDevices() {
  const { devices, home } = await api("/api/devices");
  state.devices = devices;
  state.home = home || "";

  const side = $("sidebarDevices");
  side.replaceChildren();
  devices.forEach((d) => {
    const row = el("div", "device-row");
    // "Live" here means this machine or a peer seen within the hour; anything
    // older is shown as idle rather than implied to be online.
    const fresh = d.is_this_device || (d.last_synced_at && Date.now() - d.last_synced_at < 3600000);
    row.appendChild(el("span", "dot-sm" + (fresh ? " live" : "")));
    row.appendChild(document.createTextNode(
      (d.hostname || d.device_id.slice(0, 8)) + (d.is_this_device ? "" : idleSuffix(d))));
    side.appendChild(row);
  });

  $("footDevices").textContent =
    `${devices.length} machine${devices.length === 1 ? "" : "s"} · end-to-end encrypted`;

  const list = $("deviceList");
  list.replaceChildren();
  devices.forEach((d) => list.appendChild(deviceCard(d)));
}

function idleSuffix(d) {
  if (!d.last_synced_at) return " · never synced";
  const h = Math.floor((Date.now() - d.last_synced_at) / 3600000);
  return h >= 1 ? ` · idle ${h}h` : "";
}

function deviceCard(d) {
  const card = el("div", "device-card");

  const icon = el("div", "device-icon");
  icon.innerHTML = d.is_this_device
    ? '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M2 20h20"/></svg>'
    : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="5" y="2" width="14" height="20" rx="2"/><path d="M9 18h6"/></svg>';

  const info = el("div", "device-info");
  const name = el("div", "device-name");
  name.appendChild(document.createTextNode(d.hostname || d.device_id.slice(0, 12)));
  if (d.is_this_device) {
    const tag = el("span", "badge completed");
    tag.appendChild(el("span", "dot"));
    tag.appendChild(document.createTextNode("This device"));
    name.appendChild(tag);
  }
  info.appendChild(name);
  info.appendChild(el("div", "device-sub",
    `${d.commands.toLocaleString()} commands · ` +
    (d.is_this_device ? d.device_id
      : d.last_synced_at ? `last synced ${relTime(d.last_synced_at)}` : "never synced")));

  card.append(icon, info);
  return card;
}

async function loadSettings() {
  const s = await api("/api/settings");
  const rows = $("syncRows");
  rows.replaceChildren();

  rows.appendChild(settingRow("Storage backend",
    s.sync_backend ? `${s.sync_backend} · ${s.sync_path}` : "not configured",
    null));

  rows.appendChild(settingRow("Sync enabled",
    s.sync_backend ? "uploads and downloads in the background" : "configure a backend first",
    toggle(s.sync_enabled, !s.sync_backend, async (on) => {
      await api("/api/settings", {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ sync_enabled: on }),
      });
      toast(on ? "Sync enabled" : "Sync disabled");
      loadSettings();
    })));

  rows.appendChild(settingRow("Share hostname",
    "puts a machine name in the manifest, which the storage provider can read",
    toggle(s.share_hostname, false, async (on) => {
      await api("/api/settings", {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ share_hostname: on }),
      });
      toast(on ? "Hostname will be shared" : "Hostname will not be shared");
      loadSettings();
    })));

  const syncBtn = el("button", "key-reveal", "Sync now");
  syncBtn.type = "button";
  syncBtn.disabled = !s.sync_available;
  syncBtn.addEventListener("click", async () => {
    syncBtn.disabled = true;
    syncBtn.textContent = "Syncing…";
    try {
      const r = await api("/api/sync", { method: "POST" });
      toast(`Pushed ${r.pushed}, pulled ${r.pulled}`);
      await Promise.all([loadCommands(), loadStats(), loadDevices()]);
    } catch (e) {
      toast(e.message, true);
    } finally {
      syncBtn.disabled = !s.sync_available;
      syncBtn.textContent = "Sync now";
    }
  });
  rows.appendChild(settingRow("Manual sync",
    s.sync_available ? "run a round now" : "unavailable until sync is configured", syncBtn));
}

function settingRow(label, sub, control) {
  const row = el("div", "setting-row");
  const left = el("div");
  left.appendChild(el("div", "label", label));
  if (sub) left.appendChild(el("div", "sub", sub));
  row.appendChild(left);
  if (control) row.appendChild(control);
  return row;
}

function toggle(on, disabled, onChange) {
  const t = el("button", "toggle" + (on ? " on" : ""));
  t.type = "button";
  t.disabled = !!disabled;
  t.setAttribute("aria-pressed", String(!!on));
  t.addEventListener("click", async () => {
    const next = !t.classList.contains("on");
    t.classList.toggle("on", next);
    t.setAttribute("aria-pressed", String(next));
    try {
      await onChange(next);
    } catch (e) {
      t.classList.toggle("on", !next); // put it back; the server said no
      toast(e.message, true);
    }
  });
  return t;
}

// ------------------------------------------------------------------ detail

async function openDetail(id) {
  let data;
  try {
    data = await api("/api/commands/" + encodeURIComponent(id));
  } catch (e) {
    toast(e.message, true);
    return;
  }
  state.selected = data.command;
  const c = data.command;

  $("detailCmd").textContent = c.command || "not recorded";

  const badgeRow = $("detailBadge");
  badgeRow.replaceChildren();
  const b = statusBadge(c);
  if (c.exit_code != null && c.exit_code !== 0) {
    b.appendChild(document.createTextNode(` · exit ${c.exit_code}`));
  }
  badgeRow.appendChild(b);

  const meta = $("detailMeta");
  meta.replaceChildren();
  const items = [
    ["Host", c.hostname || "—", false],
    ["Directory", shortPath(c.cwd) || "—", true],
    ["Git branch", c.git_branch || "—", true],
    ["Session", `${shortId(c.session_id)} · ${clock(c.start_time)}`, true],
    ["Started", `${relTime(c.start_time)}, ${new Date(c.start_time).toLocaleTimeString()}`, false],
    ["Duration", c.status === "orphaned" ? "unknown" : (fmtDuration(c.duration_ms) ?? "—"), true],
    // The hook can only ever report the shell's own pid, so it is labelled as
    // that rather than as the command's.
    ["Shell PID", c.pgid ? String(c.pgid) : "—", true],
    ["Shell", c.shell || "—", true],
  ];
  if (c.exit_code != null) items.splice(6, 0, ["Exit code", String(c.exit_code), true, c.exit_code !== 0]);
  // The table lists executions, so without this a command run fifty times
  // looks exactly like one run once.
  if (data.usage && data.usage.runs > 1) items.unshift(["Runs", data.usage.summary, false]);

  items.forEach(([k, v, mono, danger]) => {
    const item = el("div", "meta-item");
    item.appendChild(el("div", "k", k));
    item.appendChild(el("div", "v" + (mono ? " mono" : "") + (danger ? " danger" : ""), v));
    meta.appendChild(item);
  });

  const timeline = $("detailTimeline");
  timeline.replaceChildren();
  const around = [...(data.session_before || []).slice().reverse(), c, ...(data.session_after || [])];
  around.forEach((n) => {
    const item = el("button", "tl-item" + (n.id === c.id ? " current" : ""));
    item.type = "button";
    item.appendChild(el("span", "tl-rail"));
    item.appendChild(el("span", "tl-cmd", firstLine(n.command) || "not recorded"));
    item.appendChild(el("span", "tl-time", clock(n.start_time)));
    if (n.id !== c.id) item.addEventListener("click", () => openDetail(n.id));
    timeline.appendChild(item);
  });
  $("detailTimelineSection").style.display = around.length > 1 ? "" : "none";

  $("detail").classList.add("open");
  $("detail").setAttribute("aria-hidden", "false");
  $("scrim").classList.add("open");
  $("closeBtn").focus();
}

function closeDetail() {
  $("detail").classList.remove("open");
  $("detail").setAttribute("aria-hidden", "true");
  $("scrim").classList.remove("open");
  state.selected = null;
}

// ------------------------------------------------------------------ live

function connectStream() {
  const es = new EventSource("/api/events?token=" + encodeURIComponent(TOKEN));

  es.addEventListener("commands", (ev) => {
    const { commands } = JSON.parse(ev.data);
    let touched = false;
    commands.forEach((c) => {
      // Respect the active filters: a row that does not match should not
      // appear just because it changed.
      if (state.commands.has(c.id)) {
        state.commands.set(c.id, c);
        touched = true;
      } else if (!state.filters.q && !state.filters.host && !state.filters.status) {
        state.commands.set(c.id, c);
        touched = true;
      }
      if (state.selected && state.selected.id === c.id) openDetail(c.id);
    });
    if (touched) renderRows();
    loadStats().catch(() => {});
  });

  es.onopen = () => {
    $("liveDot").classList.remove("stale");
    $("liveText").textContent = "live";
  };
  es.onerror = () => {
    $("liveDot").classList.add("stale");
    $("liveText").textContent = "reconnecting…";
  };
}

// ------------------------------------------------------------------ wiring

function showView(which) {
  document.querySelectorAll(".view").forEach((v) => v.classList.remove("active"));
  document.querySelectorAll(".nav-item").forEach((n) => n.classList.remove("active"));
  $("view-" + which).classList.add("active");
  $(which === "dashboard" ? "navDashboard" : "navSettings").classList.add("active");
  if (which === "settings") loadSettings().catch((e) => toast(e.message, true));
}

let searchTimer;
function wire() {
  $("navDashboard").addEventListener("click", () => showView("dashboard"));
  $("navSettings").addEventListener("click", () => showView("settings"));
  $("closeBtn").addEventListener("click", closeDetail);
  $("scrim").addEventListener("click", closeDetail);
  $("shortcuts").addEventListener("click", () => toggleShortcuts(false));

  document.addEventListener("keydown", (e) => {
    const typing = isTyping(e.target);

    if (e.key === "Escape") {
      // In the order they are in the way: the panel covers the page, the
      // search hides most of the history, and clearing either is what Escape
      // is for.
      if ($("shortcuts").classList.contains("open")) toggleShortcuts(false);
      else if (state.selected) closeDetail();
      else if ($("search").value) {
        $("search").value = "";
        $("search").dispatchEvent(new Event("input"));
      }
      return;
    }
    if (e.key === "ArrowDown" || (e.key === "j" && !typing)) {
      e.preventDefault(); moveCursor(1); return;
    }
    if (e.key === "ArrowUp" || (e.key === "k" && !typing)) {
      e.preventDefault(); moveCursor(-1); return;
    }
    if (e.key === "Enter" && !state.selected) {
      // Rows are buttons: if one holds focus the browser will click it, and
      // opening it here as well would fire twice.
      if (!(e.target instanceof HTMLButtonElement)) openCursor();
      return;
    }
    if (typing) return;

    if (e.key === "/") { e.preventDefault(); $("search").focus(); }
    if (e.key === "?") { e.preventDefault(); toggleShortcuts(true); }
  });

  $("search").addEventListener("input", (e) => {
    state.filters.q = e.target.value.trim();
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => loadCommands().catch((err) => toast(err.message, true)), 160);
  });
  $("hostFilter").addEventListener("change", (e) => {
    state.filters.host = e.target.value;
    loadCommands().catch((err) => toast(err.message, true));
  });
  $("statusFilter").addEventListener("change", (e) => {
    state.filters.status = e.target.value;
    loadCommands().catch((err) => toast(err.message, true));
  });

  $("copyBtn").addEventListener("click", async () => {
    if (!state.selected) return;
    try {
      await navigator.clipboard.writeText(state.selected.command);
      toast("Copied");
    } catch {
      toast("Clipboard unavailable", true);
    }
  });

  $("redactBtn").addEventListener("click", async () => {
    if (!state.selected) return;
    if (!confirm("Replace this command's text with a tombstone on every machine?\n\nThe metadata stays; the text does not. This cannot be undone.")) return;
    try {
      const { command } = await api(`/api/commands/${encodeURIComponent(state.selected.id)}/redact`, { method: "POST" });
      state.commands.set(command.id, command);
      state.selected = command;
      renderRows();
      openDetail(command.id);
      toast("Redacted — it will reach your other machines on the next sync");
    } catch (e) {
      toast(e.message, true);
    }
  });

  // Deliberately not a real reveal. The key never leaves the machine it was
  // made on, and that includes not travelling over a loopback socket into a
  // browser tab that could be left open on a shared screen.
  $("keyReveal").addEventListener("click", () => {
    $("keyHint").innerHTML =
      "The recovery phrase is not served to this page, by design. Run " +
      "<code>shcr key show --reveal</code> in a terminal to see it. It is the only " +
      "way to add a new device or recover your history, and nobody can recover it for you.";
    $("keyReveal").disabled = true;
    $("keyReveal").textContent = "See terminal";
  });
}

// ---- Theme toggle -------------------------------------------------------
// Three states rather than two, because "follow the system" is a real answer
// and the only one that stays right when the machine switches at sunset.
const THEMES = ["system", "light", "dark"];

const THEME_ICONS = {
  system: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8"/></svg>',
  light: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>',
  dark: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>',
};

function currentTheme() {
  const set = document.documentElement.getAttribute("data-theme");
  return set === "light" || set === "dark" ? set : "system";
}

function applyTheme(theme) {
  if (theme === "system") {
    document.documentElement.removeAttribute("data-theme");
    try { localStorage.removeItem("shcr-theme"); } catch (e) {}
  } else {
    document.documentElement.setAttribute("data-theme", theme);
    try { localStorage.setItem("shcr-theme", theme); } catch (e) {}
  }
  const label = theme.charAt(0).toUpperCase() + theme.slice(1);
  $("themeLabel").textContent = label;
  // Static markup, one of three constants above — no interpolation reaches it.
  $("themeIcon").innerHTML = THEME_ICONS[theme];
  $("themeToggle").setAttribute("aria-label", `Theme: ${label}. Click to change.`);
}

function initTheme() {
  applyTheme(currentTheme());
  $("themeToggle").addEventListener("click", () => {
    const next = THEMES[(THEMES.indexOf(currentTheme()) + 1) % THEMES.length];
    applyTheme(next);
  });
}

async function boot() {
  initTheme();
  wire();
  try {
    // Devices first, and not in parallel with the rest: it carries the home
    // directory that path shortening depends on, and rows rendered before it
    // arrives would show full paths until something re-rendered them.
    const [{ hosts }] = await Promise.all([api("/api/hosts"), loadDevices()]);
    const sel = $("hostFilter");
    hosts.forEach((h) => {
      const o = document.createElement("option");
      o.value = h;
      o.textContent = h;
      sel.appendChild(o);
    });
    await Promise.all([loadCommands(), loadStats()]);
  } catch (e) {
    toast(e.message, true);
    $("liveText").textContent = "error";
    $("liveDot").classList.add("stale");
    return;
  }
  connectStream();
}

boot();
