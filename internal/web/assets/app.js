// Vanilla ES module console for the control API. No build step, no
// dependencies: this ships as static files embedded straight into the
// daemon binary, so it has to run as-is in a browser.

// --- API client --------------------------------------------------------

const errorBanner = document.getElementById("error-banner");

let errorTimer = null;
function showError(message) {
  errorBanner.textContent = message;
  errorBanner.hidden = false;
  clearTimeout(errorTimer);
  errorTimer = setTimeout(() => {
    errorBanner.hidden = true;
  }, 6000);
}

// Deliberately sessionStorage, not localStorage like cal.theme/cal.pollInterval
// below: this is what makes closing the tab or browser log the operator out,
// per the "session persisted only" requirement.
const TOKEN_KEY = "cal.token";

function getToken() {
  return sessionStorage.getItem(TOKEN_KEY) || "";
}
function setToken(token) {
  sessionStorage.setItem(TOKEN_KEY, token);
}
function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY);
}
function authHeaders() {
  const token = getToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

async function apiFetch(path, options) {
  const opts = options ? { ...options } : {};
  opts.headers = { ...authHeaders(), ...(opts.headers || {}) };

  let res;
  try {
    res = await fetch(path, opts);
  } catch (err) {
    showError(`Network error calling ${path}: ${err.message}`);
    throw err;
  }
  let body = null;
  const text = await res.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }
  if (res.status === 401) {
    clearToken();
    lock();
    const err = new Error("authentication required");
    err.status = res.status;
    err.body = body;
    throw err;
  }
  if (!res.ok) {
    const message = (body && body.error) || res.statusText || `HTTP ${res.status}`;
    showError(`${path}: ${message}`);
    const err = new Error(message);
    err.status = res.status;
    err.body = body;
    throw err;
  }
  return body;
}

const api = {
  get: (path) => apiFetch(path),
  post: (path, body) =>
    apiFetch(path, {
      method: "POST",
      headers: body ? { "Content-Type": "application/json" } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    }),
};

// --- Formatters ---------------------------------------------------------

const ZERO_TIME = "0001-01-01T00:00:00Z";

function fmtTime(value) {
  if (!value || value === ZERO_TIME) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString();
}

function fmtRelative(value) {
  if (!value || value === ZERO_TIME) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "—";
  const diffMs = d.getTime() - Date.now();
  const abs = Math.abs(diffMs);
  const mins = Math.round(abs / 60000);
  let text;
  if (mins < 1) text = "now";
  else if (mins < 60) text = `${mins}m`;
  else if (mins < 60 * 24) text = `${Math.round(mins / 60)}h`;
  else text = `${Math.round(mins / (60 * 24))}d`;
  if (text === "now") return "now";
  return diffMs < 0 ? `${text} ago` : `in ${text}`;
}

function fmtDuration(startISO, endISO) {
  if (!startISO || startISO === ZERO_TIME) return "—";
  const start = new Date(startISO).getTime();
  const end = endISO && endISO !== ZERO_TIME ? new Date(endISO).getTime() : Date.now();
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return "—";
  const secs = Math.round((end - start) / 1000);
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ${secs % 60}s`;
  const hrs = Math.floor(mins / 60);
  return `${hrs}h ${mins % 60}m`;
}

function fmtUSD(value) {
  if (value === undefined || value === null) return "—";
  return `$${Number(value).toFixed(4)}`;
}

function fmtTokens(value) {
  if (value === undefined || value === null) return "—";
  const n = Number(value);
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

// Splits tokens_in into fresh input vs. cache traffic: on an agentic run the
// whole conversation prefix is re-read from cache every turn, so the total
// alone makes usage look far higher than what actually drove a rate limit.
function fmtTokensBreakdown(run) {
  const total = fmtTokens(run.TokensIn);
  const cacheRead = Number(run.TokensCacheRead) || 0;
  const cacheWrite = Number(run.TokensCacheWrite) || 0;
  const fresh = Number(run.TokensIn) - cacheRead - cacheWrite;
  return `${total} in (${fmtTokens(fresh)} new · ${fmtTokens(cacheWrite)} written · ${fmtTokens(cacheRead)} cached)`;
}

const STATUS_KIND = {
  pr_open: "ok",
  addressed: "ok",
  planned: "ok",
  failed: "danger",
  canceled: "warn",
  deferred: "warn",
  abandoned: "warn",
  claimed: "neutral",
  working: "neutral",
  verifying: "neutral",
  pushed: "neutral",
};

function statusBadge(status) {
  const kind = STATUS_KIND[status] || "neutral";
  const span = document.createElement("span");
  span.className = `pill pill-${kind}`;
  span.textContent = status || "unknown";
  return span;
}

function gateBadge(blocking) {
  const span = document.createElement("span");
  span.className = `pill pill-${blocking ? "danger" : "warn"}`;
  span.textContent = blocking ? "blocking" : "cooldown";
  return span;
}

function issueURL(repo, issue) {
  if (!repo || !issue) return null;
  return `https://github.com/${repo}/issues/${issue}`;
}

function repoIssueLink(repo, issue) {
  const url = issueURL(repo, issue);
  if (!url) return document.createTextNode(repo || "—");
  const a = document.createElement("a");
  a.href = url;
  a.target = "_blank";
  a.rel = "noopener noreferrer";
  a.textContent = `${repo}#${issue}`;
  return a;
}

// --- DOM helpers ---------------------------------------------------------

function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v);
    }
  }
  for (const child of children || []) {
    if (child === null || child === undefined) continue;
    node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
  }
  return node;
}

function emptyState(text) {
  return el("div", { class: "empty-state", text });
}

function td(label, content) {
  const cell = el("td", { "data-label": label });
  if (content instanceof Node) cell.appendChild(content);
  else cell.textContent = content ?? "—";
  return cell;
}

// --- Theme ---------------------------------------------------------------

const THEME_KEY = "cal.theme";
function applyTheme(theme) {
  document.documentElement.setAttribute("data-theme", theme);
  document.getElementById("theme-toggle-btn").textContent = theme === "dark" ? "☀" : "☽";
}
function initTheme() {
  const stored = localStorage.getItem(THEME_KEY);
  const theme = stored || (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  applyTheme(theme);
}
document.getElementById("theme-toggle-btn").addEventListener("click", () => {
  const current = document.documentElement.getAttribute("data-theme") === "dark" ? "dark" : "light";
  const next = current === "dark" ? "light" : "dark";
  localStorage.setItem(THEME_KEY, next);
  applyTheme(next);
});
initTheme();

// --- Status pill / pause-resume / poll now -------------------------------

const statusPill = document.getElementById("status-pill");
const pauseResumeBtn = document.getElementById("pause-resume-btn");
const pollNowBtn = document.getElementById("poll-now-btn");

let lastStatus = null;

function renderStatusPill(status) {
  statusPill.className = "pill " + (status.claiming_work ? "pill-ok" : "pill-warn");
  statusPill.textContent = status.claiming_work ? "Claiming work" : "Paused";
  pauseResumeBtn.textContent = status.claiming_work ? "Pause" : "Resume";
}

async function refreshStatus() {
  try {
    const status = await api.get("/status");
    lastStatus = status;
    renderStatusPill(status);
    if (currentRoute && currentRoute.name === "dashboard") {
      renderDashboard(status);
    }
  } catch {
    statusPill.className = "pill pill-danger";
    statusPill.textContent = "Unreachable";
  }
}

pauseResumeBtn.addEventListener("click", async () => {
  pauseResumeBtn.disabled = true;
  try {
    if (lastStatus && lastStatus.claiming_work) {
      await api.post("/pause", { reason: "operator requested via web console" });
    } else {
      await api.post("/resume");
    }
    await refreshStatus();
  } finally {
    pauseResumeBtn.disabled = false;
  }
});

pollNowBtn.addEventListener("click", async () => {
  pollNowBtn.disabled = true;
  try {
    await api.post("/poll");
  } finally {
    setTimeout(() => {
      pollNowBtn.disabled = false;
    }, 1500);
  }
});

// --- Poller ----------------------------------------------------------------

const POLL_KEY = "cal.pollInterval";
const pollSelect = document.getElementById("poll-interval");
let pollTimer = null;

function schedulePoll() {
  clearInterval(pollTimer);
  const ms = Number(pollSelect.value);
  if (!ms || document.hidden) return;
  pollTimer = setInterval(tick, ms);
}

function tick() {
  refreshStatus(); // already re-renders the dashboard itself when it's the active route
  if (currentRoute && currentRoute.name !== "dashboard") {
    renderRoute(currentRoute, { silent: true });
  }
}

pollSelect.addEventListener("change", () => {
  localStorage.setItem(POLL_KEY, pollSelect.value);
  schedulePoll();
});
document.addEventListener("visibilitychange", schedulePoll);

const storedInterval = localStorage.getItem(POLL_KEY);
if (storedInterval) pollSelect.value = storedInterval;

// --- Lock screen ---------------------------------------------------------

const lockScreen = document.getElementById("lock-screen");
const lockForm = document.getElementById("lock-form");
const lockPassword = document.getElementById("lock-password");
const lockError = document.getElementById("lock-error");
const lockBtn = document.getElementById("lock-btn");

function showLockError(message) {
  lockError.textContent = message;
  lockError.hidden = false;
}

function lock() {
  clearToken();
  clearInterval(pollTimer);
  document.body.classList.add("locked");
  lockScreen.hidden = false;
  lockPassword.value = "";
  lockPassword.focus();
}

function unlock() {
  document.body.classList.remove("locked");
  lockScreen.hidden = true;
  lockError.hidden = true;
  schedulePoll();
  refreshStatus();
  currentRoute = parseHash();
  renderRoute(currentRoute);
}

lockForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  lockError.hidden = true;
  try {
    const res = await api.post("/login", { password: lockPassword.value });
    setToken(res.token);
    unlock();
  } catch (err) {
    if (err.status === 429) showLockError("Too many attempts, wait a minute.");
    else showLockError("Incorrect password.");
  }
});

lockBtn.addEventListener("click", async () => {
  try {
    await api.post("/logout");
  } catch {
    // fall through to locking locally regardless
  }
  lock();
});

// --- Router ------------------------------------------------------------

const views = {
  dashboard: document.getElementById("view-dashboard"),
  runs: document.getElementById("view-runs"),
  runDetail: document.getElementById("view-run-detail"),
  sessions: document.getElementById("view-sessions"),
  config: document.getElementById("view-config"),
};

let currentRoute = null;

function parseHash() {
  const hash = location.hash.replace(/^#/, "") || "/";
  const runMatch = hash.match(/^\/runs\/([^/?]+)/);
  if (runMatch) return { name: "runDetail", id: decodeURIComponent(runMatch[1]) };
  const base = hash.split("?")[0];
  switch (base) {
    case "/runs":
      return { name: "runs" };
    case "/sessions":
      return { name: "sessions" };
    case "/config":
      return { name: "config" };
    default:
      return { name: "dashboard" };
  }
}

function setActiveTab(name) {
  const routeForTab = { dashboard: "/", runs: "/runs", runDetail: "/runs", sessions: "/sessions", config: "/config" };
  document.querySelectorAll(".tabs a").forEach((a) => {
    a.classList.toggle("active", a.dataset.route === routeForTab[name]);
  });
}

function showView(name) {
  for (const [key, node] of Object.entries(views)) {
    node.hidden = key !== name;
  }
}

async function renderRoute(route, opts) {
  setActiveTab(route.name);
  showView(route.name);
  const silent = opts && opts.silent;
  try {
    if (route.name === "dashboard") {
      await renderDashboard(lastStatus);
    } else if (route.name === "runs") {
      await renderRuns(silent);
    } else if (route.name === "runDetail") {
      await renderRunDetail(route.id, silent);
    } else if (route.name === "sessions") {
      await renderSessions(silent);
    } else if (route.name === "config") {
      await renderConfig(silent);
    }
  } catch (err) {
    if (!silent) {
      views[route.name].innerHTML = "";
      views[route.name].appendChild(emptyState(`Could not load this view: ${err.message}`));
    }
  }
}

window.addEventListener("hashchange", () => {
  currentRoute = parseHash();
  renderRoute(currentRoute);
});

// --- Dashboard -----------------------------------------------------------

async function renderDashboard(status) {
  const container = views.dashboard;
  if (!status) {
    status = await api.get("/status");
    lastStatus = status;
  }

  let runs = { runs: [] };
  try {
    runs = await api.get("/runs?limit=200");
  } catch {
    // status still renders without the 24h spend tile
  }
  const dayAgo = Date.now() - 24 * 60 * 60 * 1000;
  const spend24h = (runs.runs || [])
    .filter((r) => r.StartedAt && new Date(r.StartedAt).getTime() >= dayAgo)
    .reduce((sum, r) => sum + (r.CostUSD || 0), 0);

  container.innerHTML = "";

  const stats = el("div", { class: "stat-grid" }, [
    statTile("In-flight runs", String((status.in_flight || []).length)),
    statTile("Active repos", String((status.active_repos || []).length)),
    statTile("Usage", status.usage && status.usage.percent && status.usage.available ? `${status.usage.percent.toFixed(0)}%` : "—"),
    statTile("Spend (24h)", fmtUSD(spend24h)),
  ]);
  container.appendChild(stats);

  container.appendChild(el("h2", { text: "Gates" }));
  const gates = status.gates || [];
  if (gates.length === 0) {
    container.appendChild(emptyState("No active gates."));
  } else {
    container.appendChild(
      el(
        "div",
        { class: "card-grid" },
        gates.map((g) =>
          el("div", { class: "card" }, [
            el("div", { class: "title" }, [gateBadge(g.blocking), document.createTextNode(" " + g.kind)]),
            el("div", { class: "muted", text: g.reason || "" }),
            el("div", { class: "muted", text: `until ${fmtTime(g.blocked_until)} (${fmtRelative(g.blocked_until)})` }),
          ])
        )
      )
    );
  }

  container.appendChild(el("h2", { text: "Active claims" }));
  const claims = status.claims || [];
  if (claims.length === 0) {
    container.appendChild(emptyState("No active claims."));
  } else {
    container.appendChild(claimsTable(claims));
  }

  container.appendChild(el("h2", { text: "In-flight runs" }));
  const inFlight = status.in_flight || [];
  if (inFlight.length === 0) {
    container.appendChild(emptyState("Nothing in flight."));
  } else {
    container.appendChild(inFlightTable(inFlight));
  }
}

function statTile(label, value) {
  return el("div", { class: "stat-tile" }, [el("div", { class: "label", text: label }), el("div", { class: "value", text: value })]);
}

function claimsTable(claims) {
  const table = el("table", {}, [
    el("thead", {}, [el("tr", {}, [el("th", { text: "Repo" }), el("th", { text: "Worker" }), el("th", { text: "Leased until" })])]),
  ]);
  const tbody = el("tbody");
  for (const c of claims) {
    tbody.appendChild(
      el("tr", {}, [td("Repo", repoIssueLink(c.Repo, c.Issue)), td("Worker", c.Worker), td("Leased until", fmtTime(c.LeasedUntil))])
    );
  }
  table.appendChild(tbody);
  return table;
}

function inFlightTable(runs) {
  const table = el("table", {}, [
    el("thead", {}, [
      el("tr", {}, [
        el("th", { text: "Repo" }),
        el("th", { text: "Status" }),
        el("th", { text: "Model" }),
        el("th", { text: "Started" }),
        el("th", { text: "" }),
      ]),
    ]),
  ]);
  const tbody = el("tbody");
  for (const r of runs) {
    const cancelBtn = el("button", { class: "btn btn-danger", type: "button" }, ["Cancel"]);
    cancelBtn.addEventListener("click", async () => {
      cancelBtn.disabled = true;
      try {
        await api.post(`/runs/${encodeURIComponent(r.ID)}/cancel`);
        await refreshStatus();
      } finally {
        cancelBtn.disabled = false;
      }
    });
    const link = el("a", { href: `#/runs/${encodeURIComponent(r.ID)}`, text: `${r.Repo}#${r.Issue}` });
    tbody.appendChild(
      el("tr", {}, [
        td("Repo", link),
        td("Status", statusBadge(r.Status)),
        td("Model", r.ModelID || "—"),
        td("Started", fmtTime(r.StartedAt)),
        td("", cancelBtn),
      ])
    );
  }
  table.appendChild(tbody);
  return table;
}

// --- Runs list -------------------------------------------------------------

let runsFilters = { repo: "", limit: 50, status: "", kind: "" };

async function renderRuns(silent) {
  const container = views.runs;
  const data = await api.get(`/runs?limit=${encodeURIComponent(runsFilters.limit)}&repo=${encodeURIComponent(runsFilters.repo)}`);
  let runs = data.runs || [];
  if (runsFilters.status) runs = runs.filter((r) => r.Status === runsFilters.status);
  if (runsFilters.kind) runs = runs.filter((r) => r.Kind === runsFilters.kind);

  if (silent && container.dataset.rendered === "1") {
    const tbody = container.querySelector("tbody");
    if (tbody) {
      tbody.replaceWith(runsTableBody(runs));
      return;
    }
  }

  container.innerHTML = "";
  container.dataset.rendered = "1";
  container.appendChild(el("h1", { text: "Runs" }));

  const repoInput = el("input", { type: "text", placeholder: "owner/repo", value: runsFilters.repo });
  const limitInput = el("input", { type: "number", min: "1", max: "500", value: String(runsFilters.limit) });
  const statusSelect = el(
    "select",
    {},
    ["", "claimed", "working", "verifying", "pushed", "pr_open", "failed", "abandoned", "canceled", "deferred", "planned", "addressed"].map(
      (s) => el("option", { value: s, text: s || "All statuses" })
    )
  );
  statusSelect.value = runsFilters.status;
  const kindSelect = el(
    "select",
    {},
    ["", "issue", "pr_comment"].map((k) => el("option", { value: k, text: k || "All kinds" }))
  );
  kindSelect.value = runsFilters.kind;
  const applyBtn = el("button", { class: "btn", type: "button", text: "Apply" });
  applyBtn.addEventListener("click", () => {
    runsFilters = {
      repo: repoInput.value.trim(),
      limit: Number(limitInput.value) || 50,
      status: statusSelect.value,
      kind: kindSelect.value,
    };
    renderRuns(false);
  });

  container.appendChild(el("div", { class: "filters" }, [repoInput, limitInput, statusSelect, kindSelect, applyBtn]));

  if (runs.length === 0) {
    container.appendChild(emptyState("No runs match these filters."));
    return;
  }

  const table = el("table", {}, [
    el("thead", {}, [
      el(
        "tr",
        {},
        ["Repo", "Status", "Kind", "Model", "Attempt", "Cost", "Tokens", "Verify", "Duration", "PR"].map((h) => el("th", { text: h }))
      ),
    ]),
  ]);
  table.appendChild(runsTableBody(runs));
  container.appendChild(table);
}

function runsTableBody(runs) {
  const tbody = el("tbody");
  for (const r of runs) {
    const link = el("a", { href: `#/runs/${encodeURIComponent(r.ID)}`, text: `${r.Repo}#${r.Issue}` });
    const prCell = r.PRURL ? el("a", { href: r.PRURL, target: "_blank", rel: "noopener noreferrer", text: "PR" }) : "—";
    tbody.appendChild(
      el("tr", {}, [
        td("Repo", link),
        td("Status", statusBadge(r.Status)),
        td("Kind", r.Kind || "issue"),
        td("Model", r.ModelID || "—"),
        td("Attempt", String(r.Attempt ?? "—")),
        td("Cost", fmtUSD(r.CostUSD)),
        td("Tokens", `${fmtTokensBreakdown(r)} / ${fmtTokens(r.TokensOut)} out`),
        td("Verify", r.VerifyStatus || "—"),
        td("Duration", fmtDuration(r.StartedAt, r.EndedAt)),
        td("PR", prCell),
      ])
    );
  }
  return tbody;
}

// --- Run detail --------------------------------------------------------

async function renderRunDetail(id, silent) {
  const container = views.runDetail;
  // A silent poll tick must not blow away a loaded transcript or an
  // operator's expanded event details; the dashboard already covers
  // "is anything still in flight" for the polling use case.
  if (silent) return;
  container.innerHTML = "";
  const data = await api.get(`/runs/${encodeURIComponent(id)}`);
  const run = data.run;
  const events = data.events || [];

  container.appendChild(el("a", { class: "back-link", href: "#/runs", text: "← Back to runs" }));
  container.appendChild(
    el("h1", {}, [document.createTextNode(`${run.Repo}#${run.Issue} `), statusBadge(run.Status)])
  );

  const fields = [
    ["Run ID", run.ID],
    ["Kind", run.Kind || "issue"],
    ["Attempt", run.Attempt],
    ["Model", run.ModelID || "—"],
    ["Branch", run.Branch || "—"],
    ["Session", run.SessionID || "—"],
    ["Cost", fmtUSD(run.CostUSD)],
    ["Tokens in", fmtTokensBreakdown(run)],
    ["Tokens out", fmtTokens(run.TokensOut)],
    ["Turns", run.NumTurns],
    ["Verify", run.VerifyStatus || "—"],
    ["Started", fmtTime(run.StartedAt)],
    ["Ended", fmtTime(run.EndedAt)],
    ["Duration", fmtDuration(run.StartedAt, run.EndedAt)],
  ];
  container.appendChild(
    el(
      "div",
      { class: "detail-grid" },
      fields.map(([label, value]) =>
        el("div", { class: "field" }, [el("div", { class: "label", text: label }), el("div", { class: "value", text: String(value ?? "—") })])
      )
    )
  );

  if (run.PRURL) {
    container.appendChild(el("p", {}, [el("a", { href: run.PRURL, target: "_blank", rel: "noopener noreferrer", text: "Open pull request →" })]));
  }

  if (run.Error) {
    container.appendChild(el("h2", { text: "Error" }));
    container.appendChild(el("div", { class: "error-block", text: run.Error }));
  }

  container.appendChild(el("h2", { text: "Event timeline" }));
  if (events.length === 0) {
    container.appendChild(emptyState("No events recorded."));
  } else {
    container.appendChild(
      el(
        "ul",
        { class: "timeline" },
        events.map((e) =>
          el("li", {}, [
            el("span", { class: "kind", text: e.Kind }),
            el("span", { text: e.Detail || "" }),
            el("span", { class: "at", text: fmtTime(e.At) }),
          ])
        )
      )
    );
  }

  container.appendChild(el("h2", { text: "Transcript" }));
  const transcriptHolder = el("div");
  container.appendChild(transcriptHolder);
  if (!run.LogPath) {
    transcriptHolder.appendChild(emptyState("This run has no transcript."));
  } else {
    const loadBtn = el("button", { class: "btn", type: "button", text: "Load transcript" });
    transcriptHolder.appendChild(loadBtn);
    loadBtn.addEventListener("click", async () => {
      loadBtn.disabled = true;
      loadBtn.textContent = "Loading…";
      try {
        const res = await fetch(`/runs/${encodeURIComponent(id)}/log`, { headers: authHeaders() });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const text = await res.text();
        transcriptHolder.innerHTML = "";
        transcriptHolder.appendChild(renderTranscript(text));
      } catch (err) {
        showError(`Could not load transcript: ${err.message}`);
        loadBtn.disabled = false;
        loadBtn.textContent = "Load transcript";
      }
    });
  }
}

function renderTranscript(text) {
  const wrap = el("div");
  const lines = text.split("\n").filter((l) => l.trim() !== "");
  if (lines.length === 0) {
    wrap.appendChild(emptyState("Transcript is empty."));
    return wrap;
  }
  for (const line of lines) {
    let record;
    try {
      record = JSON.parse(line);
    } catch {
      record = null;
    }
    const summaryText = record ? record.type || record.subtype || "record" : "unparsed line";
    const details = el("pre", { text: record ? JSON.stringify(record, null, 2) : line });
    wrap.appendChild(el("details", { class: "transcript-record" }, [el("summary", { text: summaryText }), details]));
  }
  return wrap;
}

// --- Sessions ------------------------------------------------------------

let sessionsFilters = { repo: "", issue: "", limit: 50 };

async function renderSessions(silent) {
  const container = views.sessions;
  const qs = new URLSearchParams();
  qs.set("limit", String(sessionsFilters.limit));
  if (sessionsFilters.repo) qs.set("repo", sessionsFilters.repo);
  if (sessionsFilters.issue) qs.set("issue", sessionsFilters.issue);
  const data = await api.get(`/sessions?${qs.toString()}`);
  const sessions = data.sessions || [];

  if (silent && container.dataset.rendered === "1") {
    const tbody = container.querySelector("tbody");
    if (tbody) {
      tbody.replaceWith(sessionsTableBody(sessions));
      return;
    }
  }

  container.innerHTML = "";
  container.dataset.rendered = "1";
  container.appendChild(el("h1", { text: "Sessions" }));

  const repoInput = el("input", { type: "text", placeholder: "owner/repo", value: sessionsFilters.repo });
  const issueInput = el("input", { type: "number", placeholder: "issue #", value: sessionsFilters.issue });
  const applyBtn = el("button", { class: "btn", type: "button", text: "Apply" });
  applyBtn.addEventListener("click", () => {
    sessionsFilters = { repo: repoInput.value.trim(), issue: issueInput.value.trim(), limit: 50 };
    renderSessions(false);
  });
  container.appendChild(el("div", { class: "filters" }, [repoInput, issueInput, applyBtn]));

  if (sessions.length === 0) {
    container.appendChild(emptyState("No sessions recorded."));
    return;
  }

  const table = el("table", {}, [
    el("thead", {}, [el("tr", {}, ["Session", "Repo", "Model", "Run", "Created"].map((h) => el("th", { text: h })))]),
  ]);
  table.appendChild(sessionsTableBody(sessions));
  container.appendChild(table);
}

function sessionsTableBody(sessions) {
  const tbody = el("tbody");
  for (const s of sessions) {
    const copyBtn = el("button", { class: "btn btn-icon mono", type: "button", text: s.SessionID });
    copyBtn.title = "Copy session ID";
    copyBtn.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(s.SessionID);
        copyBtn.textContent = "Copied!";
        setTimeout(() => (copyBtn.textContent = s.SessionID), 1000);
      } catch {
        showError("Clipboard access was denied by the browser.");
      }
    });
    tbody.appendChild(
      el("tr", {}, [
        td("Session", copyBtn),
        td("Repo", repoIssueLink(s.Repo, s.Issue)),
        td("Model", s.ModelID || "—"),
        td("Run", el("a", { href: `#/runs/${encodeURIComponent(s.RunID)}`, text: s.RunID })),
        td("Created", fmtTime(s.CreatedAt)),
      ])
    );
  }
  return tbody;
}

// --- Config ----------------------------------------------------------------

async function renderConfig(silent) {
  const container = views.config;
  if (silent && container.dataset.rendered === "1") return;

  const [cfgResp, models] = await Promise.all([api.get("/config"), api.get("/models")]);

  container.innerHTML = "";
  container.dataset.rendered = "1";
  container.appendChild(el("h1", { text: "Configuration" }));
  container.appendChild(
    el("p", {
      class: "muted",
      text: `Read-only. Discord webhook URL is redacted (${cfgResp.discord_webhook_set ? "currently set" : "not set"}).`,
    })
  );

  container.appendChild(el("h2", { text: "Config" }));
  container.appendChild(el("pre", { class: "mono config-dump" }, [document.createTextNode(JSON.stringify(cfgResp.config, null, 2))]));

  const cooled = new Set(models.cooled_down || []);
  const allModels = (models.models || []).slice().sort((a, b) => a.priority - b.priority);

  container.appendChild(el("h2", { text: "Plan ladder" }));
  container.appendChild(ladderTable(allModels.filter((m) => roleServed(m, "plan")), cooled));

  container.appendChild(el("h2", { text: "Implement ladder" }));
  container.appendChild(ladderTable(allModels.filter((m) => roleServed(m, "implement")), cooled));
}

function roleServed(model, role) {
  return !model.roles || model.roles.length === 0 || model.roles.includes(role);
}

function ladderTable(ladder, cooled) {
  if (ladder.length === 0) return emptyState("No models on this ladder.");
  const table = el("table", {}, [el("thead", {}, [el("tr", {}, ["Priority", "ID", "Alias"].map((h) => el("th", { text: h })))])]);
  const tbody = el("tbody");
  for (const m of ladder) {
    const isCooled = cooled.has(m.id);
    const idCell = el("span", { class: isCooled ? "strike" : "", text: m.id + (isCooled ? " (cooling down)" : "") });
    tbody.appendChild(el("tr", {}, [td("Priority", String(m.priority)), td("ID", idCell), td("Alias", m.alias || "—")]));
  }
  table.appendChild(tbody);
  return table;
}

// --- Boot --------------------------------------------------------------

async function boot() {
  let auth = { required: false, authenticated: true };
  try {
    auth = await api.get("/auth");
  } catch {
    // treat an unreachable daemon like no auth requirement; refreshStatus
    // below will report "Unreachable" on the status pill either way
  }

  lockBtn.hidden = !auth.required;

  if (auth.required && !auth.authenticated) {
    lock();
    return;
  }

  schedulePoll();
  refreshStatus();
  currentRoute = parseHash();
  renderRoute(currentRoute);
}

boot();
