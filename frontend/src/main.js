import './style.css';
import {
  GetState, GetConfig, SaveConfig, TestLogin,
  ReconnectNow, CheckNow, GetLog, ListProbePresets,
  HideWindow, Quit,
} from '../wailsjs/go/main/App';
import {EventsOn} from '../wailsjs/runtime/runtime';

// ---------------------------------------------------------------- templates

const TEMPLATE = `
<header class="status" id="status">
  <span class="dot" id="dot"></span>
  <div class="status-text">
    <strong id="state-label">Starting up…</strong>
    <span class="detail" id="state-detail"></span>
  </div>
  <button class="ghost" id="btn-hide" title="Hide to the notification area">Hide</button>
</header>

<div class="actions">
  <button id="btn-test" class="primary">Test login</button>
  <button id="btn-reconnect">Reconnect now</button>
  <button id="btn-check">Check now</button>
</div>
<p class="hint" id="action-hint">"Test login" tries the settings currently on screen — you do not need to save first.</p>

<div id="banner"></div>

<section>
  <h2>Account</h2>
  <label>Username <input id="username" type="text" autocomplete="off" spellcheck="false"></label>
  <label>Password
    <span class="row">
      <input id="password" type="password" autocomplete="off">
      <button class="ghost small" id="btn-reveal" type="button">Show</button>
    </span>
  </label>
  <label>Service outlet
    <select id="carrier"></select>
  </label>
  <p class="hint">
    Enter just your account number. The outlet is appended automatically —
    <code id="account-preview" class="account-preview"></code>
  </p>
  <p class="hint">Everything is stored unencrypted in the settings file, and is only ever sent to the campus portal.</p>
</section>

<section>
  <h2>Portal</h2>
  <p class="hint">
    The campus portal and the Dr.COM login request are built in — there is nothing to
    configure. Gatehouse sends:
  </p>
  <pre class="request-preview" id="request-preview"></pre>
  <p class="hint">
    Your password is escaped into that query, so a password containing <code>&amp;</code>
    or <code>=</code> is still sent correctly.
  </p>
</section>

<section>
  <h2>Connectivity check</h2>
  <p class="hint">
    Gatehouse only signs in when this check fails <em>and</em> the login host answers, so a
    hotel or airport portal never sees your campus password.
  </p>
  <div class="presets" id="presets"></div>
  <label>Check URL <input id="probe-url" type="text" spellcheck="false"></label>
  <div class="grid">
    <label>Expected status <input id="probe-status" type="number" min="100" max="599"></label>
    <label>Expected text <input id="probe-body" type="text" spellcheck="false"></label>
  </div>
  <p class="hint">Leave "Expected text" empty for a 204-style endpoint that returns no body.</p>

  <details>
    <summary>Advanced</summary>
    <label>Check every (seconds) <input id="poll" type="number" min="5" max="3600"></label>
    <label class="checkline">
      <input id="autostart" type="checkbox"> Start Gatehouse when I sign in
    </label>
  </details>
</section>

<div class="actions">
  <button id="btn-save" class="primary">Save</button>
  <button id="btn-quit" class="ghost danger">Quit Gatehouse</button>
</div>

<details class="history">
  <summary>Recent attempts</summary>
  <div id="history"></div>
</details>

<details class="help">
  <summary>How does Gatehouse decide whether to sign in?</summary>
  <ol>
    <li>It checks whether the internet actually works.</li>
    <li>If it does, nothing happens at all.</li>
    <li>If it does not, it checks whether the campus portal answers. If the portal is
      not there — a hotel, an airport, a cafe — nothing is sent, so your password
      never leaves for an unknown network.</li>
    <li>Only when the internet is down <em>and</em> the portal answers does it send
      the login request, then re-check the internet to confirm it worked.</li>
  </ol>
  <p class="hint">
    If a login fails, <strong>Recent attempts</strong> below shows the portal's own
    reply, which is the quickest way to see whether the password was wrong or the
    portal was simply not ready.
  </p>
</details>

<details class="log">
  <summary>Log</summary>
  <pre id="log"></pre>
</details>
`;

// ------------------------------------------------------------------- helpers

// Mirrors config.Carriers; the portal's own dropdown offers the same three.
const CARRIERS = [
  {suffix: '@aust', name: '学生电信出口'},
  {suffix: '@unicom', name: '学生联通出口'},
  {suffix: '@cmcc', name: '学生移动出口'},
];
const DEFAULT_CARRIER = '@cmcc';

const $ = (id) => document.getElementById(id);

const STATE_CLASS = {
  online: 'ok',
  logging_in: 'busy',
  login_failed: 'warn',
  auth_rejected: 'bad',
  suspended: 'bad',
  not_on_campus: 'idle',
  misconfigured: 'warn',
  needs_config: 'warn',
  unknown: 'idle',
};

const STATE_LABEL = {
  online: 'Online',
  logging_in: 'Signing in…',
  login_failed: 'Sign-in failed — retrying',
  auth_rejected: 'Credentials rejected',
  suspended: 'Paused after repeated failures',
  not_on_campus: 'Not on the campus network',
  misconfigured: 'Check the connectivity settings',
  needs_config: 'Not configured yet',
  unknown: 'Starting up…',
};

function banner(kind, message) {
  const el = $('banner');
  el.className = kind ? `banner ${kind}` : '';
  el.textContent = message || '';
  el.hidden = !message;
}

// ----------------------------------------------------------------- form I/O

function fillForm(cfg) {
  $('username').value = cfg.username || '';
  $('password').value = cfg.password || '';

  const carrier = $('carrier');
  carrier.innerHTML = '';
  for (const c of CARRIERS) {
    const opt = document.createElement('option');
    opt.value = c.suffix;
    opt.textContent = `${c.name} (${c.suffix})`;
    carrier.appendChild(opt);
  }
  carrier.value = cfg.carrier || DEFAULT_CARRIER;
  if (!carrier.value) carrier.selectedIndex = 0;

  const probe = cfg.internet_probe || {};
  $('probe-url').value = probe.url || '';
  $('probe-status').value = probe.expect_status || 200;
  $('probe-body').value = probe.expect_body ?? '';

  $('poll').value = cfg.poll_interval_seconds || 15;
  $('autostart').checked = !!cfg.autostart;

  renderRequestPreview();
}

function readForm() {
  return {
    username: $('username').value,
    password: $('password').value,
    carrier: $('carrier').value,
    internet_probe: {
      url: $('probe-url').value.trim(),
      expect_status: Number($('probe-status').value) || 200,
      expect_body: $('probe-body').value,
    },
    poll_interval_seconds: Number($('poll').value) || 15,
    autostart: $('autostart').checked,
  };
}

// The portal address and the request are both fixed in Go; this mirrors the
// shape so the user can see exactly what will be sent. It never contains the
// real password.
function renderRequestPreview() {
  const account = accountName();

  const preview = $('account-preview');
  if (preview) {
    preview.textContent = account || '(your account)';
  }

  const el = $('request-preview');
  if (!el) return;

  el.textContent =
    `GET http://10.255.0.19/drcom/login\n` +
    `  ?callback=dr1003\n` +
    `  &DDDDD=${encodeURIComponent(account || '{username}')}\n` +
    `  &upass={password}\n` +
    `  &0MKKey=123456&R1=0&R3=0&R6=0&para=00&v6ip=&v={random}`;
}

// accountName mirrors Config.AccountName in Go: the outlet suffix is appended
// unless the username already carries one.
function accountName() {
  const user = ($('username').value || '').trim();
  const suffix = $('carrier').value || '';
  if (!user) return '';
  if (!suffix || user.includes('@')) return user;
  return user + suffix;
}

// ---------------------------------------------------------------- rendering

function renderState(s) {
  const state = s.state || 'unknown';
  $('status').className = `status ${STATE_CLASS[state] || 'idle'}`;
  $('state-label').textContent = STATE_LABEL[state] || state;

  const bits = [];
  if (s.detail) bits.push(s.detail);
  if (s.last_check_at) bits.push('checked ' + new Date(s.last_check_at).toLocaleTimeString());
  $('state-detail').textContent = bits.join(' · ');

  renderHistory(s.history || []);
}

function renderHistory(items) {
  const el = $('history');
  if (!items.length) {
    el.innerHTML = '<p class="hint">No sign-in attempts yet.</p>';
    return;
  }
  el.innerHTML = items.map((a) => {
    const when = new Date(a.at).toLocaleTimeString();
    const cls = a.ok ? 'ok' : 'bad';
    const summary = (a.summary || '').replace(/[<>&]/g, (c) => ({'<': '&lt;', '>': '&gt;', '&': '&amp;'}[c]));
    return `<div class="attempt ${cls}">
      <span class="when">${when}</span>
      <span class="verdict">${a.verdict}</span>
      <span class="summary">${summary}</span>
    </div>`;
  }).join('');
}

async function refreshLog() {
  try {
    $('log').textContent = await GetLog();
  } catch (err) {
    console.error(err);
  }
}

async function loadPresets() {
  try {
    const presets = await ListProbePresets();
    $('presets').innerHTML = presets.map((p, i) =>
      `<button type="button" class="ghost small" data-preset="${i}" title="${p.note}">${p.name}</button>`
    ).join('');
    $('presets').querySelectorAll('button').forEach((btn) => {
      btn.addEventListener('click', () => {
        const p = presets[Number(btn.dataset.preset)];
        $('probe-url').value = p.url;
        $('probe-status').value = p.status;
        $('probe-body').value = p.body;
        banner('', '');
      });
    });
  } catch (err) {
    console.error(err);
  }
}

// ------------------------------------------------------------------ actions

function busy(on, label) {
  for (const id of ['btn-test', 'btn-reconnect', 'btn-check', 'btn-save']) {
    $(id).disabled = on;
  }
  if (on) {
    $('action-hint').textContent = label;
  } else {
    $('action-hint').textContent =
      '"Test login" tries the settings currently on screen — you do not need to save first.';
  }
}

async function doSave() {
  banner('', '');
  try {
    const res = await SaveConfig(readForm());
    if (!res.ok) {
      banner('bad', (res.errors || []).join(' '));
      return;
    }
    const warns = res.warnings || [];
    banner(warns.length ? 'warn' : 'good',
      warns.length ? `Saved, with notes: ${warns.join(' ')}` : 'Saved.');
  } catch (err) {
    banner('bad', String(err));
  }
}

async function doTest() {
  banner('', '');
  busy(true, 'Testing the login request…');
  try {
    const res = await TestLogin(readForm());
    const parts = [res.message];
    if (res.probe_detail) parts.push(`Check: ${res.probe_detail}.`);
    if (res.portal_address) parts.push(`Login host: ${res.portal_address}.`);
    banner(res.ok ? 'good' : 'bad', parts.filter(Boolean).join(' '));
  } catch (err) {
    banner('bad', String(err));
  } finally {
    busy(false);
    refreshLog();
  }
}

function wire() {
  $('btn-save').addEventListener('click', doSave);
  $('btn-test').addEventListener('click', doTest);
  $('btn-reconnect').addEventListener('click', () => ReconnectNow());
  $('btn-check').addEventListener('click', () => CheckNow());
  $('btn-hide').addEventListener('click', () => HideWindow());
  $('btn-quit').addEventListener('click', () => Quit());

  $('btn-reveal').addEventListener('click', () => {
    const field = $('password');
    const hidden = field.type === 'password';
    field.type = hidden ? 'text' : 'password';
    $('btn-reveal').textContent = hidden ? 'Hide' : 'Show';
  });

  // Keep the request preview in step with the username.
  $('username').addEventListener('input', renderRequestPreview);
  $('carrier').addEventListener('change', renderRequestPreview);

  for (const el of document.querySelectorAll('.history, .log')) {
    el.addEventListener('toggle', () => {
      if (el.open && el.classList.contains('log')) refreshLog();
    });
  }
}

// --------------------------------------------------------------------- init

async function init() {
  $('app').innerHTML = TEMPLATE;
  wire();

  // WebView2 throttles a hidden window, so treat pushed events as best-effort
  // and re-sync whenever the window becomes visible again.
  EventsOn('state', renderState);
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refreshState();
  });

  try {
    fillForm(await GetConfig());
  } catch (err) {
    banner('bad', 'Could not read the settings: ' + err);
  }

  await loadPresets();
  await refreshState();
  refreshLog();
}

async function refreshState() {
  try {
    renderState(await GetState());
  } catch (err) {
    console.error(err);
  }
}

init();
