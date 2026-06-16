// dash — SPA bootstrap JS for /app and /v/<owner>/<slug>.
//
// Served from ClickHouse user_files as file://dash/spa.js by the handler at
// /lib/v1/spa.js. The SPA shell references this as
// <script type="module" src="/lib/v1/spa.js">, which lets a strict CSP on
// the shell avoid inline bootstrap code AND lets us pull pure helpers from
// the sibling ./spa-helpers.js module for direct unit-test coverage.
//
// Data path: this shell calls ClickHouse HTTPS DIRECTLY (the CH_URL from
// /config.json) with the OAuth bearer in the Authorization header. The MCP
// is used only for the OAuth bootstrap (RFC 9728 protected-resource
// discovery, /oauth/authorize, /oauth/token).
//
// Deploy: see install.sh — pushes this file to every cluster replica's
// user_files via INSERT INTO FUNCTION clusterAllReplicas('{cluster}',
// '<db>', '_asset_<safe-name>') against a File-engine table.

import {
  escHtml,
  fmtBytes,
  jsonCompactRows,
  sqlStr,
  assertSafe,
  safeReturnTo,
  moduleToClassic,
} from './spa-helpers.js';
import { mount as mountTweaks, readPrefs as readTweakPrefs } from './tweaks.js';

// ---- Runtime config (loaded from /config.json before main()) ----
var CFG       = null;                    // populated by loadConfig() at boot
var MCP       = null;                    // CFG.mcp_url    — OAuth AS / RS root
var CH_URL    = null;                    // CFG.ch_url     — data-path origin
var DB        = null;                    // CFG.db         — dashboards database name
var TOK_KEY   = 'mcp_tok';
var OLD_CID_KEYS = ['mcp_cid', 'mcp_cid_v2'];
var CID_KEY   = 'mcp_cid_v3';
var CLIENT_KEY = 'mcp_oauth_client_id';
var OLD_AS_KEY = 'mcp_as_meta';
var AS_KEY    = 'mcp_as_meta_v2';
// BASE — URL path prefix this SPA is mounted under (otel install serves it
// under /b/ to coexist with the legacy /v/ dashboard prototype). All absolute
// SPA paths (routes, assets, config, OAuth) are built relative to BASE.
var BASE      = '/b';
var CIMD_PATH = BASE + '/oauth/client.json';
var VER_KEY   = 'pkce_v';
var STATE_KEY = 'pkce_state';
// SAFE: allow chars that appear in an email localpart (+) and in full-email
// owner URLs (@) so /v/<full-email>/<slug> works. owners and slugs are
// quote-escaped via sqlStr() before splicing into SQL, but we still apply
// a conservative character allowlist as defense in depth.
var FETCH_MS  = 20000;

// Storage helpers. The SPA can render inside a sandbox-without-same-origin
// frame (e.g. someone embedded /app in a chat-runtime iframe); in that case
// localStorage/sessionStorage throw on every access. We detect once at startup
// and fall back to a per-page-load in-memory store so the script doesn't crash
// — though OAuth state can't persist across loads in that environment.
var STORAGE_AVAILABLE = (function () {
  try { localStorage.getItem('__probe__'); return true; }
  catch (_) { return false; }
})();
var memStore = {};
function lsGet(k)    { try { return STORAGE_AVAILABLE ? localStorage.getItem(k) : (memStore[k] || null); } catch (_) { return memStore[k] || null; } }
function lsSet(k, v) { try { if (STORAGE_AVAILABLE) localStorage.setItem(k, v); else memStore[k] = v; } catch (_) { memStore[k] = v; } }
function lsDel(k)    { try { if (STORAGE_AVAILABLE) localStorage.removeItem(k); delete memStore[k]; } catch (_) { delete memStore[k]; } }
function ssGet(k)    { try { return STORAGE_AVAILABLE ? sessionStorage.getItem(k) : (memStore[k] || null); } catch (_) { return memStore[k] || null; } }
function ssSet(k, v) { try { if (STORAGE_AVAILABLE) sessionStorage.setItem(k, v); else memStore[k] = v; } catch (_) { memStore[k] = v; } }
function ssDel(k)    { try { if (STORAGE_AVAILABLE) sessionStorage.removeItem(k); delete memStore[k]; } catch (_) { delete memStore[k]; } }

OLD_CID_KEYS.forEach(function(k){ lsDel(k); });
lsDel(OLD_AS_KEY);

function setStatus(msg, isErr) {
  var el = document.getElementById('msg');
  if (el) { el.textContent = msg; el.className = isErr ? 'err' : ''; }
  var actions = document.getElementById('actions');
  if (!actions) return;
  if (isErr) {
    actions.innerHTML =
      '<a id="retry" href="#">Retry</a>' +
      '<a id="relogin" href="#">Log in again</a>';
    actions.classList.remove('hide');
    document.getElementById('retry').onclick = function(e){ e.preventDefault(); location.reload(); };
    document.getElementById('relogin').onclick = function(e){
      e.preventDefault();
      lsDel(TOK_KEY);
      lsDel(CID_KEY);
      lsDel(CLIENT_KEY);
      lsDel(AS_KEY);
      location.reload();
    };
  } else {
    actions.classList.add('hide');
    actions.innerHTML = '';
  }
}

function showFrame() {
  document.getElementById('status').classList.add('hide');
  document.getElementById('frame').classList.remove('hide');
}

function withTimeout(input, init) {
  init = init || {};
  var ctrl = new AbortController();
  var t = setTimeout(function(){ ctrl.abort(); }, FETCH_MS);
  init.signal = ctrl.signal;
  return fetch(input, init).finally(function(){ clearTimeout(t); });
}

// ---- Runtime config ----
// Loaded at startup. Server-side `Cache-Control: no-store` makes re-config
// fast: reload the page after `INSERT INTO FUNCTION file('dash/config.json', ...)`.
async function loadConfig() {
  var r = await withTimeout(BASE + '/config.json', { headers: { 'Accept': 'application/json' }, cache: 'no-store' });
  if (!r.ok) throw new Error('config.json fetch failed: HTTP ' + r.status);
  var j = await r.json();
  if (!j.mcp_url || !j.ch_url) throw new Error('config.json missing mcp_url or ch_url');
  CFG    = j;
  MCP    = j.mcp_url.replace(/\/$/, '');
  CH_URL = j.ch_url.replace(/\/$/, '');
  DB     = j.db || 'dashboards';
  if (j.brand_name) document.title = j.brand_name;
}

function parseRoute() {
  var p = location.pathname;
  if (BASE && p.indexOf(BASE) === 0) p = p.slice(BASE.length) || '/';  // strip mount prefix
  var q = new URLSearchParams(location.search);
  var m;
  if ((m = p.match(/^\/v\/([^\/]+)\/([^\/]+)\/?$/))) {
    return { kind: 'dashboard', owner: decodeURIComponent(m[1]), slug: decodeURIComponent(m[2]) };
  }
  if (p === '/app' || p === '/app/' || p === '/') {
    var d = q.get('dashboard');
    if (d) {
      var parts = d.split('/');
      if (parts.length === 2) return { kind: 'dashboard', owner: parts[0], slug: parts[1] };
    }
    // /app with no args → built-in index listing the caller's own dashboards.
    return { kind: 'index' };
  }
  return { kind: 'unknown' };
}

// ---- PKCE S256 ----
function randBytes(n) {
  var b = new Uint8Array(n); crypto.getRandomValues(b); return b;
}
function b64url(bytes) {
  var s = btoa(String.fromCharCode.apply(null, bytes));
  return s.replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
}
function randVerifier() { return b64url(randBytes(32)); }
async function challenge(verifier) {
  var h = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier));
  return b64url(new Uint8Array(h));
}

// Discover the OAuth AS via RFC 9728 protected-resource metadata against the
// MCP, then fetch AS-level metadata so we know where to /authorize and /token.
async function discoverAS() {
  var rs = await withTimeout(MCP + '/.well-known/oauth-protected-resource', { headers: { 'Accept': 'application/json' } });
  if (!rs.ok) throw new Error('protected-resource discovery failed: HTTP ' + rs.status);
  var pr = await rs.json();
  var asUrl = (pr.authorization_servers || [])[0];
  if (!asUrl) throw new Error('No authorization_servers in protected-resource metadata');

  var cached = null;
  try { cached = JSON.parse(lsGet(AS_KEY) || 'null'); } catch (_) {}
  if (cached && cached.issuer === asUrl && cached.authorization_endpoint && cached.token_endpoint && Object.prototype.hasOwnProperty.call(cached, 'client_id_metadata_document_supported')) {
    return cached;
  }

  var base = asUrl.replace(/\/$/, '');
  var meta = null;
  for (var path of ['/.well-known/oauth-authorization-server', '/.well-known/openid-configuration']) {
    var r = await withTimeout(base + path, { headers: { 'Accept': 'application/json' } });
    if (r.ok) { meta = await r.json(); break; }
  }
  if (!meta) throw new Error('AS metadata discovery failed for ' + asUrl);
  meta.issuer = meta.issuer || asUrl;

  // Mode-flip cleanup: if the AS issuer changed since last use, drop the
  // cached client_id (the old client state is meaningless against the new AS).
  if (cached && cached.issuer !== meta.issuer) {
    lsDel(CID_KEY);
    lsDel(CLIENT_KEY);
  }
  lsSet(AS_KEY, JSON.stringify(meta));
  return meta;
}

// Static client: the SPA's client_id is a fixed OAuth client registered with
// the AS (Auth0), served in config.json as `oauth_client_id`. CIMD is reserved
// for the MCP's dynamic clients; a SPA we control gets a real registered app.
function oauthClientId() {
  var cid = CFG && CFG.oauth_client_id;
  if (!cid) throw new Error('config.json missing oauth_client_id');
  lsSet(CLIENT_KEY, cid);
  return cid;
}

async function startAuth(returnTo) {
  var meta = await discoverAS();
  var cid = oauthClientId();
  var v = randVerifier();
  ssSet(VER_KEY, v);
  var state = b64url(randBytes(16));
  ssSet(STATE_KEY, JSON.stringify({
    state: state,
    return_to: safeReturnTo(returnTo),
    ts: Date.now()
  }));
  var c = await challenge(v);
  var u = new URL(meta.authorization_endpoint);
  u.searchParams.set('client_id', cid);
  u.searchParams.set('redirect_uri', location.origin + BASE + '/oauth/callback');
  u.searchParams.set('response_type', 'code');
  u.searchParams.set('code_challenge', c);
  u.searchParams.set('code_challenge_method', 'S256');
  u.searchParams.set('state', state);
  u.searchParams.set('scope', 'openid email offline_access');
  // Auth0: `audience` selects the API whose identifier becomes the JWT `aud`,
  // which CH's <token_processor expected_audience> byte-matches. Auth0 uses
  // `audience` (not the RFC 8707 `resource` param) to mint API access tokens.
  var aud = (CFG && CFG.oauth_audience) || (MCP + '/');
  u.searchParams.set('audience', aud);
  // Auth0 Organizations: scope the login to this tenant's org. Membership is
  // the hard allowlist (non-members are rejected at login); org-scoped roles
  // assigned in the Auth0 UI drive CH access via the roles claim.
  if (CFG && CFG.oauth_organization) u.searchParams.set('organization', CFG.oauth_organization);
  location.replace(u.toString());
}

function clearTokenAndRestart() {
  lsDel(TOK_KEY);
  location.reload();
}

// ---- Content fetch via ClickHouse HTTP, direct ----
// URL segments are whitelisted via assertSafe / sqlStr from ./spa-helpers.js.

async function chQuery(sql) {
  var tok = lsGet(TOK_KEY);
  if (!tok) throw new Error('Auth expired');
  // Direct CH HTTP. CH's <token_processor> resolves the JWT and runs the
  // query under currentUser() = the OAuth subject's email. No MCP hop.
  var u = new URL(CH_URL + '/');
  u.searchParams.set('query', sql);
  u.searchParams.set('default_format', 'JSONCompact');
  var r = await withTimeout(u.toString(), { headers: { 'Authorization': 'Bearer ' + tok } });
  if (r.status === 401 || r.status === 403) throw new Error('Auth expired');
  if (!r.ok) {
    var body = (await r.text()).slice(0, 400);
    // CH surfaces JWT validation failures as HTTP 500 with
    // jwt::error::token_verification_exception ("token expired",
    // signature mismatch, issuer/audience drift). Route through the
    // 'Auth expired' path so the SPA re-runs OAuth instead of showing
    // the raw CH exception text.
    if (/token_verification_exception|token expired/i.test(body)) {
      throw new Error('Auth expired');
    }
    throw new Error('HTTP ' + r.status + ': ' + body);
  }
  return await r.json();
}

async function fetchDashboard(owner, slug) {
  assertSafe('owner', owner); assertSafe('slug', slug);
  // URL owner segment is the URL-encoded full email (e.g.
  // foo%40altinity.com); parseRoute() has already decoded it. DB stores
  // full email (the MV computes owner = currentUser()), so match directly.
  // v1 schema: structured columns. We read them as JSON strings via
  // toJSONString() so the SPA can rebuild the spec object client-side
  // without depending on the wire format of nested JSON columns.
  var sql = 'SELECT title, subtitle, concurrent, spec_version,'
          + ' toJSONString(params) AS params_json,'
          + ' toJSONString(panels) AS panels_json,'
          + ' toJSONString(meta) AS meta_json'
          + ' FROM ' + DB + '.dashboards FINAL'
          + ' WHERE owner = ' + sqlStr(owner)
          + ' AND slug = ' + sqlStr(slug)
          + ' LIMIT 1';
  var d = await chQuery(sql);
  var rows = d.data || d.rows || [];
  if (!rows.length) throw new Error('Dashboard not found: ' + owner + '/' + slug);
  var r = rows[0];
  var spec = {
    title:        r[0] || '',
    subtitle:     r[1] || '',
    concurrent:   !!r[2],
    spec_version: Number(r[3]) || 1,
  };
  try { spec.params = JSON.parse(r[4] || '[]'); } catch (_) { spec.params = []; }
  try { spec.panels = JSON.parse(r[5] || '[]'); } catch (_) { spec.panels = []; }
  try { spec.meta   = JSON.parse(r[6] || '{}'); } catch (_) { spec.meta = {}; }
  return synthesizeSpecWrapper(spec);
}

// Wrap a spec into a minimal HTML doc that loads /lib/v<N>/dash.js as
// an ES module and invokes renderSpec into #dash-root.
//
// spec_version selects the runtime version. Multiple runtime versions
// coexist; old dashboards keep rendering with their original runtime.
//
// Spec JSON is inlined into an inline <script>; we escape `</` to
// defuse any `</script>` breakout payload (well-known XSS guard for
// inlined JSON-in-script). The sanitize_panel MV scrubbed the panels
// server-side; this layer is defense-in-depth.

// The iframe is sandboxed without `allow-same-origin`, so an ES-module
// import from the null-origin srcdoc would fail CORS. We fetch the
// runtime + its sibling chart primitives, concatenate, strip the ESM
// surface, and inline the result as a classic <script>. Order matters:
// charts.js's exports must be defined before dash.js's renderers refer
// to them — and after moduleToClassic, both files are just top-level
// const/function declarations sharing one script scope.
async function synthesizeSpecWrapper(spec) {
  // Asset path is always /lib/v1/* — runtime versioning is on hold
  // until a spec contract actually breaks (Phase 2 is when new panel
  // types arrive and the bump to spec_version=2 makes sense). Any
  // existing row with spec_version != 1 still gets rendered by this
  // runtime; the field is preserved on the spec for forward use.
  var specJson = JSON.stringify(spec).replace(/<\/(?=script)/gi, '<\\/');
  var origin = location.origin;
  // Capture current tweaks prefs once so the iframe initializes with
  // the same look as the parent shell. Live updates come in via
  // postMessage `{type:"tokens", data:{…}}` once the iframe loads.
  var initialPrefs = readTweakPrefs();
  var prefsJson = JSON.stringify(initialPrefs).replace(/<\/(?=script)/gi, '<\\/');
  async function fetchRuntime(path) {
    // `cache: 'no-cache'` forces conditional revalidation. The static
    // handler doesn't set ETag/Last-Modified, so every fetch returns
    // a fresh body — one small round-trip per /v/ load. Default cache
    // mode (and the prior `force-cache`) pinned stale runtime bytes
    // for the full max-age after a deploy.
    var r = await withTimeout(BASE + '/lib/v1' + path,
      { headers: { 'Accept': 'application/javascript' }, cache: 'no-cache' });
    if (!r.ok) throw new Error('runtime fetch failed: ' + path + ' HTTP ' + r.status);
    return r.text();
  }
  var parts = await Promise.all([
    fetchRuntime('/charts.js'),
    fetchRuntime('/dash.js'),
  ]);
  var runtimeJs = moduleToClassic(parts.join('\n'));
  // Iframe-side tweaks listener — toggles data-accent/data-theme/
  // data-density on the iframe's own <html> in response to
  // `{type:"tokens", data:{…}}` messages from the parent.
  var tweaksBoot =
    '(function(){\n'
    + '  function apply(p){\n'
    + '    var r=document.documentElement;\n'
    + '    if (p && p.accent && p.accent !== "teal") r.setAttribute("data-accent", p.accent); else r.removeAttribute("data-accent");\n'
    + '    if (p && p.theme === "light") r.setAttribute("data-theme", "light"); else r.removeAttribute("data-theme");\n'
    + '    if (p && p.density === "dense") r.setAttribute("data-density", "dense"); else r.removeAttribute("data-density");\n'
    + '    var hide = (p && p.narrative === "off");\n'
    + '    document.querySelectorAll("[data-narrative]").forEach(function(el){\n'
    + '      if (hide) el.setAttribute("data-hidden-by-tweak", ""); else el.removeAttribute("data-hidden-by-tweak");\n'
    + '    });\n'
    + '  }\n'
    + '  apply(' + prefsJson + ');\n'
    + '  window.addEventListener("message", function(ev){\n'
    + '    if (ev.source !== window.parent) return;\n'
    + '    var m = ev.data;\n'
    + '    if (m && m.type === "tokens" && m.data) apply(m.data);\n'
    + '  });\n'
    + '})();';
  return ''
    + '<!doctype html><html lang="en"><head>'
    + '<meta charset="utf-8">'
    + '<link rel="stylesheet" href="' + origin + BASE + '/lib/v1/dash-theme.css">'
    + '</head><body>'
    + '<div id="dash-root"></div>'
    + '<script>\n' + tweaksBoot + '\n<\/script>'
    + '<script>\n' + runtimeJs.replace(/<\/(?=script)/gi, '<\\/') + '\n<\/script>'
    + '<script>(async () => {\n'
    + '  try { await window.DASH.renderSpec(' + specJson + ', document.getElementById("dash-root")); }\n'
    + '  catch (e) {\n'
    + '    document.getElementById("dash-root").innerHTML =\n'
    + '      "<pre style=\\"color:#ff6b6b;white-space:pre-wrap;font:12px ui-monospace\\">renderSpec error:\\n" +\n'
    + '      (e && e.message ? e.message : e) + "</pre>";\n'
    + '  }\n'
    + '})();<\/script>'
    + '</body></html>';
}

// ---- Render ----
//
// Iframe runs under sandbox="allow-scripts" — scripts execute but the iframe
// has a null origin with no access to parent's localStorage, cookies, or the
// Bearer token. CH_FETCH inside the iframe is a postMessage RPC to this
// parent, which holds the bearer and performs the actual query.
//
// Benefit: a malicious dashboard (stored XSS) cannot exfiltrate the Bearer.
// It can still drive queries as the viewer while the tab is open, but can't
// walk away with a reusable credential.
function bootScript() {
  return '<script>\n' +
    '(function(){\n' +
    '  window.CH_ENDPOINT = ' + JSON.stringify(CH_URL) + ';\n' +
    '  var pending = {}, nextId = 0;\n' +
    '  window.addEventListener("message", function(ev){\n' +
    '    if (ev.source !== window.parent) return;\n' +
    '    var m = ev.data;\n' +
    '    if (!m || typeof m.id !== "number" || !pending[m.id]) return;\n' +
    '    var p = pending[m.id]; delete pending[m.id];\n' +
    '    if (m.type === "ch-result") p.resolve({cols: m.cols, rows: m.rows, count: m.count});\n' +
    '    else if (m.type === "ch-error") p.reject(new Error(m.message || "Query failed"));\n' +
    '  });\n' +
    '  window.CH_FETCH = function(sql){\n' +
    '    var id = ++nextId;\n' +
    '    return new Promise(function(resolve, reject){\n' +
    '      pending[id] = {resolve: resolve, reject: reject};\n' +
    '      setTimeout(function(){ if(pending[id]){ delete pending[id]; reject(new Error("Query timed out")); } }, 30000);\n' +
    '      window.parent.postMessage({type: "ch-query", id: id, sql: sql}, ' + JSON.stringify(location.origin) + ');\n' +
    '    });\n' +
    '  };\n' +
    '})();\n' +
    '<\/script>\n';
}

function renderIntoFrame(html) {
  var frame = document.getElementById('frame');
  frame.addEventListener('load', function onload(){
    frame.removeEventListener('load', onload);
    showFrame();
    setupTweaksForwarding(frame);
  });
  frame.srcdoc = bootScript() + html;
}

// Mount the floating tweaks panel and wire its onChange to forward
// the new prefs into the dashboard iframe via postMessage.
function setupTweaksForwarding(frame) {
  if (document.querySelector('.tweaks')) return;   // already mounted
  mountTweaks({
    document: document,
    onChange: function (prefs) {
      try {
        if (frame && frame.contentWindow) {
          frame.contentWindow.postMessage({ type: 'tokens', data: prefs }, '*');
        }
      } catch (_) { /* sandboxed iframe rejected the post; harmless */ }
    },
  });
}

// ---- postMessage handler for iframe's CH_FETCH RPC ----
function handleIframeMessage(ev) {
  var frame = document.getElementById('frame');
  if (!frame || ev.source !== frame.contentWindow) return;
  if (ev.origin !== 'null') return;
  var m = ev.data;
  if (!m || typeof m !== 'object' || m.type !== 'ch-query') return;
  var id = m.id, sql = m.sql;
  if (typeof id !== 'number' || typeof sql !== 'string') return;
  chQuery(sql).then(function(j){
    var cols = j.columns || (j.meta ? j.meta.map(function(c){return c.name;}) : []);
    var rows = jsonCompactRows(j);
    var count = (typeof j.count === 'number') ? j.count : rows.length;
    // targetOrigin "*": sandboxed iframe origin is "null", which
    // postMessage rejects as a literal string. Source check above is
    // the defense — and the iframe's own listener filters on
    // ev.source === window.parent.
    frame.contentWindow.postMessage(
      { type: 'ch-result', id: id, cols: cols, rows: rows, count: count },
      '*'
    );
  }).catch(function(e){
    if (e.message === 'Auth expired') { clearTokenAndRestart(); return; }
    frame.contentWindow.postMessage(
      { type: 'ch-error', id: id, message: String(e.message || e).slice(0, 400) },
      '*'
    );
  });
}

window.addEventListener('message', handleIframeMessage);

// ---- built-in /app index (signed-in dashboards listing) ----
// Renders at top-level (no iframe) — the listing has no user-supplied
// content so the bearer-exfil sandbox /v/ relies on isn't needed here.
// /sql is the design reference for header + login card; the OAuth flow
// itself is unchanged (CIMD + PKCE via discoverAS/startAuth).

function hideShellChrome() {
  var st = document.getElementById('status'); if (st) st.classList.add('hide');
  var fr = document.getElementById('frame');  if (fr) fr.classList.add('hide');
}

function renderLoginCard(returnTo) {
  hideShellChrome();
  var prev = document.getElementById('app-root'); if (prev) prev.remove();
  var root = document.createElement('div');
  root.id = 'app-root';
  root.className = 'login-screen';
  root.innerHTML =
    '<div class="login-card">' +
      '<span class="login-logo">A</span>' +
      '<h2>Altinity Dashboards</h2>' +
      '<p class="muted" id="login-msg">Sign in to continue</p>' +
      '<button id="signin" class="btn-primary" type="button">Sign in</button>' +
      '<p class="muted" style="margin-top:14px;font-size:11px">OAuth · ' + escHtml(location.host) + '</p>' +
    '</div>';
  document.body.appendChild(root);
  document.getElementById('signin').addEventListener('click', async function() {
    var btn = document.getElementById('signin');
    btn.textContent = 'Redirecting…';
    btn.disabled = true;
    try { await startAuth(returnTo); }
    catch (e) {
      btn.textContent = 'Sign in';
      btn.disabled = false;
      var m = document.getElementById('login-msg');
      if (m) { m.textContent = 'Error: ' + (e.message || e); m.style.color = '#ff6b6b'; }
    }
  });
}

async function renderIndex() {
  // Identity + listing through chQuery (top-level fetch — no iframe RPC).
  var ownerResp = await chQuery('SELECT owner FROM ' + DB + '.dashboards_prefix');
  var me = jsonCompactRows(ownerResp)[0] || { owner: '?' };

  var listResp = await chQuery(
    'SELECT slug, title, owner,' +
    ' length(toJSONString(panels)) AS content_size,' +
    ' updated_at,' +
    ' toJSONString(tags) AS tags_json' +
    ' FROM ' + DB + '.dashboards FINAL' +
    ' WHERE owner = currentUser()' +
    ' ORDER BY updated_at DESC LIMIT 200'
  );
  var rows = jsonCompactRows(listResp).map(function(r) {
    try { r.tags = JSON.parse(r.tags_json || '[]'); } catch (_) { r.tags = []; }
    return r;
  });

  hideShellChrome();
  var prev = document.getElementById('app-root'); if (prev) prev.remove();
  var root = document.createElement('div');
  root.id = 'app-root';
  root.innerHTML =
    '<header class="app-header">' +
      '<span class="logo-mark">A</span>' +
      '<span class="logo-name">Altinity Dashboards</span>' +
      '<span class="env-chip">' + escHtml(location.host) + '</span>' +
      '<span class="spacer"></span>' +
      '<span class="user-email" title="' + escHtml(me.owner) + '">' + escHtml(me.owner) + '</span>' +
      '<button class="hd-btn" id="logout" type="button">Sign out</button>' +
    '</header>' +
    '<main class="app-main">' +
      '<h1>My dashboards</h1>' +
      '<div class="toolbar">' +
        '<input id="filter" type="search" placeholder="Filter by title or slug…">' +
        '<span id="dash-status" class="muted"></span>' +
      '</div>' +
      '<table class="dash-list">' +
        '<thead><tr><th>Title</th><th>Slug</th><th class="right">Size</th><th>Updated</th><th>Tags</th></tr></thead>' +
        '<tbody id="rows"></tbody></table>' +
    '</main>';
  document.body.appendChild(root);

  var rowsEl    = document.getElementById('rows');
  var statusEl  = document.getElementById('dash-status');
  var filterEl  = document.getElementById('filter');

  function renderRows(rs) {
    if (!rs.length) {
      rowsEl.innerHTML = '<tr><td colspan="5" class="empty">No dashboards yet. Save one via your MCP connector to see it here.</td></tr>';
      return;
    }
    rowsEl.innerHTML = rs.map(function(r) {
      var href = BASE + '/v/' + encodeURIComponent(r.owner || '') + '/' + encodeURIComponent(r.slug || '');
      var tags = (r.tags || []).map(function(t){ return '<span class="tag">' + escHtml(t) + '</span>'; }).join('');
      var updated = String(r.updated_at || '').slice(0, 16).replace('T', ' ');
      return '<tr>' +
        '<td class="title"><a href="' + escHtml(href) + '" target="_blank" rel="noopener">' + escHtml(r.title || r.slug) + '</a></td>' +
        '<td class="slug">' + escHtml(r.slug) + '</td>' +
        '<td class="right">' + fmtBytes(r.content_size) + '</td>' +
        '<td>' + escHtml(updated) + '</td>' +
        '<td>' + tags + '</td>' +
      '</tr>';
    }).join('');
  }

  function applyFilter() {
    var q = filterEl.value.trim().toLowerCase();
    if (!q) return renderRows(rows);
    renderRows(rows.filter(function(r) {
      return String(r.title || '').toLowerCase().indexOf(q) >= 0
          || String(r.slug  || '').toLowerCase().indexOf(q) >= 0;
    }));
  }

  filterEl.addEventListener('input', applyFilter);
  document.getElementById('logout').addEventListener('click', function() {
    if (confirm('Sign out and return to the login screen?')) clearTokenAndRestart();
  });

  statusEl.textContent = rows.length + ' dashboard' + (rows.length === 1 ? '' : 's');
  renderRows(rows);
}

// ---- main ----
(async function main(){
  // If the SPA is loaded inside a sandboxed-no-same-origin iframe, OAuth
  // can't persist a token across redirects. Show a clear escape hatch
  // instead of silently looping through the login flow.
  if (!STORAGE_AVAILABLE) {
    var el = document.getElementById('msg');
    if (el) {
      el.className = 'err';
      el.innerHTML = 'This page is embedded in a sandboxed frame and can’t access browser storage, so the OAuth login can’t persist a session. <br><br>Open it in a top-level browser tab: <a id="popout" href="' + location.href + '" target="_top" rel="noopener" style="color:#00d4aa">' + location.href + '</a>';
    }
    return;
  }

  try { await loadConfig(); }
  catch (e) { setStatus('Config error: ' + e.message, true); return; }

  // Apply persisted tweaks prefs to the parent <html> so the index,
  // login card, and shell chrome match the user's preferred theme
  // immediately. The floating tweaks UI itself mounts later, once
  // there's a dashboard iframe to forward token changes into.
  try {
    var rootPrefs = readTweakPrefs();
    var htmlEl = document.documentElement;
    if (rootPrefs.accent && rootPrefs.accent !== 'teal') htmlEl.setAttribute('data-accent', rootPrefs.accent);
    if (rootPrefs.theme === 'light') htmlEl.setAttribute('data-theme', 'light');
    if (rootPrefs.density === 'dense') htmlEl.setAttribute('data-density', 'dense');
  } catch (_) {}

  var route = parseRoute();
  if (route.kind === 'unknown') {
    setStatus('No dashboard or page in the URL.\nTry /v/<owner>/<slug> or /p/<name>.', true);
    return;
  }
  if (!lsGet(TOK_KEY)) {
    // For the index, show a login card and let the user click to OAuth.
    // For deep links to /v/ or /p/, jump straight into OAuth so the
    // bookmark/share-link flow stays one redirect.
    if (route.kind === 'index') {
      renderLoginCard(location.pathname + location.search);
      return;
    }
    setStatus('Redirecting to login…');
    try { await startAuth(location.pathname + location.search); }
    catch (e) { setStatus('Login error: ' + e.message, true); }
    return;
  }
  try {
    var label = route.kind === 'dashboard' ? (route.owner + '/' + route.slug) : 'index';
    document.title = label + (CFG.brand_name ? ' — ' + CFG.brand_name : '');
    setStatus('Loading ' + label + '…');
    if (route.kind === 'index') {
      await renderIndex();
    } else {
      var html = await fetchDashboard(route.owner, route.slug);
      renderIntoFrame(html);
    }
  } catch (e) {
    if (e.message === 'Auth expired') { clearTokenAndRestart(); return; }
    setStatus(e.message || 'Failed to load', true);
  }
})();
