import './style.css';
import {
  ChooseProfile,
  Connect,
  Disconnect,
  GetLogs,
  GetSetup,
  GetStatus,
  InstallWintun,
  LoadProfile,
  OpenWintunPage,
  PreviewJoin,
  RefreshAdapters,
  RestartElevated,
  SaveSetup,
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
let setup = null;
let firstCheckDone = false;

const wizard = {
  open: false,
  step: 1,
  code: '',
  preview: null,
  adapters: [],
  selected: new Set(),
  saved: false,
  error: '',
};

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
  const runtimeReady = lastStatus?.elevated !== false && lastStatus?.tun_ready !== false;
  $('connect-btn').disabled = busy || state !== 'disconnected' || !hasProfile || !runtimeReady;
  $('disconnect-btn').disabled = busy || (state !== 'connected' && state !== 'reconnecting');
  $('browse-btn').disabled = busy || state !== 'disconnected';
  $('reload-btn').disabled = busy || state !== 'disconnected' || !hasProfile;
  $('setup-btn').disabled = busy || state !== 'disconnected';

  const needsAdmin = lastStatus?.elevated === false;
  const needsTun = lastStatus?.tun_ready === false;
  $('admin-banner').hidden = !(needsAdmin || needsTun);
  $('admin-restart').hidden = !needsAdmin;
  $('notice-install').hidden = !needsTun;
  $('notice-install').disabled = busy;
  if (needsTun) {
    setText('notice-title', 'wintun.dll is missing');
    setText('notice-text', 'Install wintun.dll from Setup before connecting.');
  } else if (needsAdmin) {
    setText('notice-title', 'Administrator rights needed');
    setText('notice-text', 'Windows only lets an elevated app create the tunnel and change routes.');
  }
  $('admin-restart').disabled = busy;

  updateWizardControls();
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
    if (!firstCheckDone) {
      firstCheckDone = true;
      if (!st.profile?.path) {
        openWizard(1);
      }
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

/* ---------- first-run wizard ---------- */

function setWizardError(message) {
  wizard.error = message || '';
  $('wizard-error').hidden = !wizard.error;
  $('wizard-error').textContent = wizard.error;
}

function setJoinHint(message, bad = false, good = false) {
  const hint = $('join-hint');
  hint.textContent = message;
  hint.classList.toggle('bad', bad);
  hint.classList.toggle('good', good);
}

function setAdapterHint(message, bad = false) {
  const hint = $('adapter-hint');
  hint.textContent = message;
  hint.classList.toggle('bad', bad);
}

function updateWizardControls() {
  const adapterCount = wizard.adapters.length;
  const selectedCount = wizard.selected.size;
  $('join-next').disabled = busy || !$('join-code').value.trim();
  $('adapter-next').disabled = busy || selectedCount === 0;
  $('select-all').checked = adapterCount > 0 && selectedCount === adapterCount;
  $('select-all').disabled = busy || adapterCount === 0;
  $('rescan-btn').disabled = busy;
  $('adapter-back').disabled = busy;
  $('ready-back').disabled = busy;
  $('wizard-close').disabled = busy;
  $('elevate-btn').disabled = busy;
  $('wintun-btn').disabled = busy;

  const ready = wizard.saved
    && setup?.elevated !== false
    && setup?.tun?.ready !== false;
  $('finish-btn').disabled = busy || !ready;
}

function setWizardStep(step) {
  wizard.step = step;
  document.querySelectorAll('.wizard-panel').forEach((panel) => {
    panel.hidden = Number(panel.dataset.panel) !== step;
  });
  document.querySelectorAll('.wstep').forEach((item) => {
    const n = Number(item.dataset.step);
    item.classList.toggle('active', n === step);
    item.classList.toggle('done', n < step);
  });
  updateWizardControls();
}

async function refreshSetup() {
  try {
    setup = await GetSetup();
  } catch (error) {
    setup = null;
    return;
  }
  $('wizard-close').hidden = !setup.has_profile;
  if (setup.warning) {
    setAdapterHint(setup.warning, true);
  }
  renderReady();
}

function adapterCheckbox(adapter) {
  const label = document.createElement('label');
  label.className = 'adapter';

  const input = document.createElement('input');
  input.type = 'checkbox';
  input.checked = wizard.selected.has(adapter.interface);
  input.addEventListener('change', () => {
    if (input.checked) {
      wizard.selected.add(adapter.interface);
    } else {
      wizard.selected.delete(adapter.interface);
    }
    updateWizardControls();
    updateAdapterHint();
  });

  const name = document.createElement('div');
  name.className = 'adapter-name';
  const strong = document.createElement('span');
  strong.textContent = adapter.interface;
  const kind = document.createElement('span');
  kind.className = 'kind';
  kind.textContent = adapter.kind || 'unknown';
  name.append(strong, kind);

  const addr = document.createElement('span');
  addr.className = 'adapter-addr';
  addr.textContent = adapter.address || '—';

  const weight = document.createElement('span');
  weight.className = 'adapter-weight';
  weight.textContent = `weight ${adapter.weight ?? 0}`;

  label.append(input, name, addr, weight);
  return label;
}

function updateAdapterHint() {
  const count = wizard.selected.size;
  if (!wizard.adapters.length) {
    setAdapterHint('No usable adapters found. Connect Ethernet, Wi-Fi, or a USB-tethered phone and press Rescan.', true);
    return;
  }
  if (count === 0) {
    setAdapterHint('Select at least one adapter.', true);
    return;
  }
  setAdapterHint(count === 1
    ? 'One adapter means no bonding benefit. Add another link when you can.'
    : `${count} adapters will be bonded.`);
}

function renderAdapters() {
  const list = $('adapter-list');
  list.replaceChildren();
  for (const adapter of wizard.adapters) {
    list.appendChild(adapterCheckbox(adapter));
  }
  updateAdapterHint();
  updateWizardControls();
}

function renderReady() {
  if (!wizard.preview) {
    updateWizardControls();
    return;
  }
  const names = wizard.adapters
    .filter((adapter) => wizard.selected.has(adapter.interface))
    .map((adapter) => adapter.interface);
  setText('ready-summary',
    `${wizard.preview.server} · ${names.length} adapter${names.length === 1 ? '' : 's'}: ${names.join(', ')}`);

  const saved = setup?.has_profile && setup?.profile?.path;
  const profileRow = $('check-profile');
  profileRow.classList.toggle('ok', Boolean(saved));
  profileRow.classList.toggle('warn', !saved);
  setText('check-profile-text', saved
    ? `Profile saved to ${setup.profile.path}`
    : 'The profile is saved when you continue from the adapter step.');

  const elevated = setup?.elevated !== false;
  const adminRow = $('check-admin');
  adminRow.classList.toggle('ok', elevated);
  adminRow.classList.toggle('warn', !elevated);
  setText('check-admin-text', elevated
    ? 'Running with the permissions the tunnel needs'
    : 'Restart MultiSpeed as administrator to create the tunnel');
  $('elevate-btn').hidden = elevated || setup === null;

  const tun = setup?.tun;
  const tunRow = $('check-wintun');
  const tunReady = tun ? (tun.ready || !tun.required) : false;
  tunRow.classList.toggle('ok', tunReady);
  tunRow.classList.toggle('warn', !tunReady);
  if (!tun) {
    setText('check-wintun-text', 'Checking the tunnel driver…');
  } else if (!tun.required) {
    setText('check-wintun-text', 'No extra driver is needed on this platform');
  } else if (tun.ready) {
    setText('check-wintun-text', tun.detail || 'wintun.dll is installed');
  } else {
    setText('check-wintun-text', `wintun.dll is missing (${tun.location || 'next to MultiSpeed.exe'})`);
  }
  $('wintun-btn').hidden = !tun?.required || tun.ready;
  $('wintun-page-btn').hidden = !tun?.required || tun.ready;

  updateWizardControls();
}

function openWizard(step) {
  wizard.open = true;
  $('wizard').hidden = false;
  setWizardError('');
  if (!wizard.preview && step > 1) step = 1;
  if (step === 1) {
    $('join-code').value = wizard.code;
    if (wizard.preview) {
      setJoinHint('This will replace the current profile.', false);
    } else {
      setJoinHint('Waiting for a join code.');
    }
  }
  if (step === 2) renderAdapters();
  if (step === 3) renderReady();
  setWizardStep(step);
  refreshSetup();
}

function closeWizard() {
  wizard.open = false;
  $('wizard').hidden = true;
  setWizardError('');
}

async function applyJoinCode() {
  const code = $('join-code').value.trim();
  if (!code) return;
  setWizardError('');
  $('join-next').disabled = true;
  try {
    const preview = await PreviewJoin(code);
    wizard.code = code;
    wizard.preview = preview;
    wizard.adapters = preview.adapters ?? [];
    wizard.selected = new Set(wizard.adapters.map((adapter) => adapter.interface));
    wizard.saved = false;
    renderAdapters();
    setWizardStep(2);
    if (preview.client_name) {
      setJoinHint(`Relay ${preview.server} · client “${preview.client_name}”`, false, true);
    } else {
      setJoinHint(`Relay ${preview.server}`, false, true);
    }
  } catch (error) {
    setJoinHint(String(error), true);
  } finally {
    updateWizardControls();
  }
}

async function saveAndContinue() {
  if (!wizard.preview || wizard.selected.size === 0) return;
  setWizardError('');
  setAdapterHint('Saving the profile…');
  $('adapter-next').disabled = true;
  try {
    await SaveSetup(wizard.code, Array.from(wizard.selected));
    wizard.saved = true;
    await refreshSetup();
    renderReady();
    setWizardStep(3);
  } catch (error) {
    setAdapterHint('Could not save the profile.', true);
    setWizardError(String(error));
    await refreshSetup();
  } finally {
    updateWizardControls();
  }
}

async function finishWizard() {
  setWizardError('');
  try {
    await Connect();
    closeWizard();
  } catch (error) {
    setWizardError(String(error));
  } finally {
    await refresh();
  }
}

async function withWizardBusy(action) {
  if (busy) return;
  busy = true;
  updateControls();
  try {
    await action();
  } catch (error) {
    setWizardError(String(error));
  } finally {
    busy = false;
    await refresh();
    await refreshSetup();
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

$('setup-btn').addEventListener('click', () => {
  openWizard(1);
});

$('admin-restart').addEventListener('click', () => {
  runAction(() => RestartElevated());
});

$('notice-install').addEventListener('click', () => {
  runAction(() => InstallWintun());
});

$('clear-log').addEventListener('click', () => {
  $('log').replaceChildren();
});

$('join-code').addEventListener('input', () => {
  updateWizardControls();
});

$('join-next').addEventListener('click', () => {
  withWizardBusy(applyJoinCode);
});

$('use-file-btn').addEventListener('click', () => {
  withWizardBusy(async () => {
    const profile = await ChooseProfile();
    if (profile?.path) {
      closeWizard();
    }
  });
});

$('adapter-back').addEventListener('click', () => {
  setWizardStep(1);
});

$('adapter-next').addEventListener('click', () => {
  withWizardBusy(saveAndContinue);
});

$('ready-back').addEventListener('click', () => {
  renderAdapters();
  setWizardStep(2);
});

$('select-all').addEventListener('change', () => {
  const all = $('select-all').checked;
  wizard.selected = new Set(all ? wizard.adapters.map((adapter) => adapter.interface) : []);
  renderAdapters();
});

$('rescan-btn').addEventListener('click', () => {
  withWizardBusy(async () => {
    const adapters = await RefreshAdapters();
    wizard.adapters = adapters ?? [];
    wizard.selected = new Set(wizard.adapters.map((adapter) => adapter.interface));
    renderAdapters();
  });
});

$('elevate-btn').addEventListener('click', () => {
  withWizardBusy(() => RestartElevated());
});

$('wintun-btn').addEventListener('click', () => {
  withWizardBusy(async () => {
    await InstallWintun();
  });
});

$('wintun-page-btn').addEventListener('click', () => {
  withWizardBusy(() => OpenWintunPage());
});

$('finish-btn').addEventListener('click', () => {
  withWizardBusy(finishWizard);
});

$('wizard-close').addEventListener('click', () => {
  closeWizard();
});

window.addEventListener('beforeunload', () => {
  // Best-effort; Go's OnShutdown also restores networking.
  Disconnect().catch(() => {});
});

refresh();
setInterval(refresh, 1000);
