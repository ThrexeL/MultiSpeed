import './style.css';
import {
  ChooseProfile,
  Connect,
  Disconnect,
  GetLogs,
  GetStatus,
  LoadProfile,
} from '../wailsjs/go/main/App';

const $ = (id) => document.getElementById(id);

const stateLabels = {
  disconnected: 'Disconnected',
  connecting: 'Connecting…',
  connected: 'Connected',
  reconnecting: 'Reconnecting…',
  disconnecting: 'Disconnecting…',
};

let lastSeq = 0;
let lastStatus = null;
let busy = false;
let pollInFlight = false;
let transientError = '';

function setText(id, text) {
  $(id).textContent = text;
}

function showError(message) {
  if (!message) {
    $('error-banner').hidden = true;
    return;
  }
  $('error-banner').hidden = false;
  setText('error-text', message);
}

function updateControls() {
  const state = lastStatus?.state ?? 'disconnected';
  const hasProfile = Boolean(lastStatus?.profile?.path);
  $('connect-btn').disabled = busy || state !== 'disconnected' || !hasProfile;
  $('disconnect-btn').disabled = busy || (state !== 'connected' && state !== 'reconnecting');
  $('browse-btn').disabled = busy || state !== 'disconnected';
  $('reload-btn').disabled = busy || state !== 'disconnected' || !hasProfile;
}

function renderProfile(profile) {
  const path = profile?.path ?? '';
  const pathEl = $('profile-path');
  pathEl.textContent = path || 'No profile selected';
  pathEl.title = path;
  pathEl.classList.toggle('empty', !path);
  setText('profile-server', profile?.server || '—');
  const paths = profile?.paths ?? [];
  setText('profile-paths', paths.length ? paths.join(', ') : '—');
  setText('profile-reorder', profile?.reorder_delay_ms ? `${profile.reorder_delay_ms} ms` : '—');
  $('profile-paths').title = paths.join('\n');
}

function renderSession(st) {
  const connected = st.state === 'connected' || st.state === 'reconnecting';
  setText('tunnel-ip', connected ? st.tunnel_ip : '—');
  setText('session-mtu', connected && st.mtu ? String(st.mtu) : '—');
  setText('delivered', connected ? String(st.reorder?.delivered ?? 0) : '—');
  setText('pending', connected ? String(st.reorder?.pending ?? 0) : '—');
  setText('session-uptime', connected ? (st.connected_for || '') : '');
}

function formatRate(bitsPerSecond) {
  const value = Number(bitsPerSecond || 0);
  if (!value) return '—';
  if (value >= 1e9) return `${(value / 1e9).toFixed(1)} Gbps`;
  if (value >= 1e6) return `${(value / 1e6).toFixed(1)} Mbps`;
  if (value >= 1e3) return `${(value / 1e3).toFixed(1)} Kbps`;
  return `${value} bps`;
}

function formatMs(value) {
  const n = Number(value || 0);
  return n > 0 ? `${n.toFixed(1)} ms` : '—';
}

function formatLoss(value) {
  const n = Number(value || 0);
  return n > 0 ? `${(n * 100).toFixed(1)}%` : '0.0%';
}

function cell(text, className) {
  const td = document.createElement('td');
  td.textContent = text;
  if (className) td.className = className;
  return td;
}

function renderPaths(st) {
  const body = $('paths-body');
  const paths = st.paths ?? [];
  body.replaceChildren();

  if (!paths.length) {
    const row = document.createElement('tr');
    const td = document.createElement('td');
    td.colSpan = 7;
    td.className = 'empty';
    td.textContent = st.state === 'connected' || st.state === 'reconnecting'
      ? 'Waiting for the relay’s first path probes…'
      : 'Connect to see path telemetry.';
    row.appendChild(td);
    body.appendChild(row);
  } else {
    for (const path of paths) {
      const row = document.createElement('tr');
      row.appendChild(cell(path.name || `path ${path.id}`, 'path-name'));

      const kindCell = document.createElement('td');
      const kind = document.createElement('span');
      kind.className = 'kind';
      kind.textContent = path.kind || 'unknown';
      kindCell.appendChild(kind);
      row.appendChild(kindCell);

      const stateCell = document.createElement('td');
      const state = document.createElement('span');
      state.className = path.healthy ? 'state-up' : 'state-down';
      state.textContent = path.healthy ? 'up' : 'down';
      stateCell.appendChild(state);
      row.appendChild(stateCell);

      row.appendChild(cell(formatMs(path.srtt_ms), 'num'));
      row.appendChild(cell(formatMs(path.jitter_ms), 'num'));
      row.appendChild(cell(formatLoss(path.loss), 'num'));
      row.appendChild(cell(formatRate(path.rx_bps), 'num'));
      body.appendChild(row);
    }
  }

  if ((st.state === 'connected' || st.state === 'reconnecting') && st.reorder) {
    const r = st.reorder;
    setText('reorder-line',
      `delivered ${r.delivered ?? 0} · duplicate ${r.duplicates ?? 0} · late ${r.late ?? 0} · ` +
      `skipped ${r.skipped ?? 0} · pending ${r.pending ?? 0} · data frames ${st.data_seq ?? 0}`);
  } else {
    setText('reorder-line', '');
  }
}

function render(st) {
  lastStatus = st;
  const state = st.state ?? 'disconnected';
  const pill = $('state-pill');
  let label = stateLabels[state] ?? state;
  if (state === 'reconnecting' && st.retry_attempt) {
    label = `Reconnecting… (attempt ${st.retry_attempt})`;
  }
  pill.textContent = label;
  pill.className = `pill ${state}`;

  renderProfile(st.profile);
  renderSession(st);
  renderPaths(st);

  const message = st.last_error || transientError;
  showError(state === 'disconnected' || state === 'disconnecting' ? message : '');
  setText('version', st.version ? `v${st.version}` : '');
  updateControls();
}

function appendLogs(lines) {
  const log = $('log');
  const nearBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 80;
  for (const line of lines) {
    const row = document.createElement('span');
    row.className = `log-line log-${line.level}`;

    const time = document.createElement('span');
    time.className = 'log-time';
    time.textContent = line.time;

    const level = document.createElement('span');
    level.className = 'log-level';
    level.textContent = line.level;

    const message = document.createElement('span');
    message.className = 'log-message';
    message.textContent = line.message;

    row.append(time, level, message);
    log.appendChild(row);
  }
  while (log.childElementCount > 600) {
    log.firstElementChild.remove();
  }
  if (nearBottom) {
    log.scrollTop = log.scrollHeight;
  }
}

async function refresh() {
  if (pollInFlight) return;
  pollInFlight = true;
  try {
    const st = await GetStatus();
    render(st);
    const lines = await GetLogs(lastSeq);
    if (lines?.length) {
      appendLogs(lines);
      lastSeq = lines[lines.length - 1].seq;
    }
  } catch (error) {
    console.error('status refresh failed', error);
  } finally {
    pollInFlight = false;
  }
}

async function runAction(action) {
  if (busy) return;
  busy = true;
  updateControls();
  try {
    await action();
  } catch (error) {
    transientError = String(error);
    showError(transientError);
  } finally {
    busy = false;
    await refresh();
  }
}

$('connect-btn').addEventListener('click', () => {
  transientError = '';
  runAction(() => Connect());
});

$('disconnect-btn').addEventListener('click', () => {
  runAction(() => Disconnect());
});

$('browse-btn').addEventListener('click', () => {
  runAction(async () => {
    transientError = '';
    const profile = await ChooseProfile();
    if (profile?.path) {
      lastStatus = { ...(lastStatus ?? {}), profile, state: 'disconnected' };
      renderProfile(profile);
      updateControls();
    }
  });
});

$('reload-btn').addEventListener('click', () => {
  runAction(async () => {
    const path = lastStatus?.profile?.path;
    if (!path) return;
    transientError = '';
    const profile = await LoadProfile(path);
    lastStatus = { ...(lastStatus ?? {}), profile };
    renderProfile(profile);
    updateControls();
  });
});

$('clear-log').addEventListener('click', () => {
  $('log').replaceChildren();
});

window.addEventListener('beforeunload', () => {
  // Best-effort; Go's OnShutdown also restores networking.
  Disconnect().catch(() => {});
});

refresh();
setInterval(refresh, 1000);
