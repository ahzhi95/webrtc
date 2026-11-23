'use strict';

// ============ helpers ============
let lang = 'zh';
try { lang = localStorage.getItem('lang') || 'zh'; } catch (e) { /* storage blocked */ }

function t(key, vars) {
  let s = (I18N[lang] && I18N[lang][key]) ?? I18N.zh[key] ?? key;
  if (vars) for (const k in vars) s = s.replace('{' + k + '}', vars[k]);
  return s;
}

// h('div', {class: 'x', onclick: fn}, child, 'text', ...) builds DOM safely (no innerHTML).
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : String(kid));
  }
  return el;
}

class ApiError extends Error {}

async function api(method, path, body) {
  const opts = { method, headers: { 'X-Requested-With': 'schoolapp' }, credentials: 'same-origin' };
  if (body instanceof FormData) opts.body = body;
  else if (body !== undefined) { opts.body = JSON.stringify(body); opts.headers['Content-Type'] = 'application/json'; }
  let res;
  try { res = await fetch(path, opts); } catch (e) { throw new ApiError(t('err:network')); }
  const data = await res.json().catch(() => ({}));
  if (res.status === 401 && path !== '/api/login') { state.me = null; render(); throw new ApiError(''); }
  if (!res.ok) {
    const msg = data.error || 'error';
    const tr = t('err:' + msg);
    throw new ApiError(tr.startsWith('err:') ? msg : tr);
  }
  return data;
}

let toastTimer;
function toast(msg) {
  if (!msg) return;
  document.querySelector('.toast')?.remove();
  const el = h('div', { class: 'toast', role: 'status' }, msg);
  document.body.append(el);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.remove(), 3000);
}

// Runs an async action, shows errors as a toast.
async function run(fn, okMsg) {
  try { await fn(); if (okMsg) toast(okMsg); return true; }
  catch (e) { toast(e.message); return false; }
}

const hhmm = (iso) => (iso || '').slice(11, 16);
const dateTime = (iso) => (iso || '').slice(0, 16).replace('T', ' ');
const money = (cents) => `${state.currency} ${(cents / 100).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
const statusBadge = (st) => h('span', { class: 'badge ' + ({ pending: 'warn', approved: 'ok', rejected: 'bad' }[st]) }, t('st_' + st));
const emptyRow = () => h('div', { class: 'empty' }, t('none'));

function classOptions(classes, selected, allLabel) {
  return [
    h('option', { value: '' }, allLabel),
    ...classes.map((c) => h('option', { value: c.id, selected: String(c.id) === String(selected) }, c.name)),
  ];
}

// ============ app state & shell ============
const state = { me: null, tab: null, currency: 'RM' };
const TABS = {
  parent: [['home', '🏠'], ['notices', '📢'], ['receipts', '🧾'], ['me', '👤']],
  teacher: [['scan', '📷'], ['attendance', '✅'], ['notices', '📢'], ['me', '👤']],
  admin: [['scan', '📷'], ['attendance', '✅'], ['notices', '📢'], ['receipts', '🧾'], ['manage', '⚙️'], ['me', '👤']],
};
const VIEWS = {};
let cleanup = null; // called when leaving a view (e.g. to stop the camera)

async function boot() {
  try {
    const me = await api('GET', '/api/me');
    state.me = me;
    state.currency = me.currency || 'RM';
  } catch (e) { state.me = null; }
  render();
  if (state.me) push.refresh();
}

// ============ push notifications ============
function b64urlToBytes(s) {
  const b = atob(s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - s.length % 4) % 4));
  return Uint8Array.from(b, (c) => c.charCodeAt(0));
}

const push = {
  supported: () => window.isSecureContext && 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window,
  isIOS: () => /iphone|ipad|ipod/i.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1),
  installed: () => window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,
  // On iPhone, web push only works after "Add to Home Screen".
  needsInstall() { return this.isIOS() && !this.installed(); },
  async registration() { return navigator.serviceWorker.register('/sw.js'); },
  async current() {
    if (!this.supported()) return null;
    const reg = await navigator.serviceWorker.getRegistration();
    return reg ? reg.pushManager.getSubscription() : null;
  },
  async enable() {
    if (!this.supported()) throw new ApiError(t('pushUnsupported'));
    const perm = await Notification.requestPermission();
    if (perm !== 'granted') throw new ApiError(t('pushDenied'));
    const reg = await this.registration();
    await navigator.serviceWorker.ready;
    const { key } = await api('GET', '/api/push/key');
    let sub = await reg.pushManager.getSubscription();
    try {
      // The browser contacts its push service (Google/Apple/Mozilla) here; don't wait forever on a bad connection.
      if (!sub) {
        sub = await Promise.race([
          reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64urlToBytes(key) }),
          new Promise((resolve, reject) => setTimeout(() => reject(new Error(t('pushTimeout'))), 20000)),
        ]);
      }
    } catch (e) { throw new ApiError(t('pushFailed') + ' ' + e.message); }
    await api('POST', '/api/push/subscribe', sub.toJSON());
  },
  async disable() {
    const sub = await this.current().catch(() => null);
    if (!sub) return;
    await api('POST', '/api/push/unsubscribe', { endpoint: sub.endpoint }).catch(() => {});
    await sub.unsubscribe().catch(() => {});
  },
  // Keep the server's copy of this device's subscription up to date.
  async refresh() {
    try {
      if (!this.supported()) return;
      await this.registration();
      const sub = await this.current();
      if (sub && Notification.permission === 'granted') await api('POST', '/api/push/subscribe', sub.toJSON());
    } catch (e) { /* not critical */ }
  },
};

async function logout() {
  await push.disable(); // this device should stop getting the previous user's notifications
  await run(() => api('POST', '/api/logout'));
  state.me = null;
  render();
}

// Card that asks a parent to turn on notifications; returns null when already on.
async function pushPromptCard(onChange) {
  if (!push.supported() && !push.needsInstall()) return null;
  if (push.needsInstall()) {
    return h('section', { class: 'card' }, h('h3', {}, '🔔 ', t('pushTitle')), h('p', { class: 'small muted', style: 'margin:0' }, t('pushIOSHint')));
  }
  if (await push.current()) return null;
  if (Notification.permission === 'denied') {
    return h('section', { class: 'card' }, h('h3', {}, '🔕 ', t('pushTitle')), h('p', { class: 'small muted', style: 'margin:0' }, t('pushBlocked')));
  }
  const btn = h('button', { class: 'btn primary', onclick: async () => {
    btn.disabled = true;
    if (await run(() => push.enable(), t('pushOn'))) onChange();
    btn.disabled = false;
  } }, t('pushEnable'));
  return h('section', { class: 'card' }, h('div', { class: 'row' },
    h('div', { class: 'grow' }, h('h3', { style: 'margin:0' }, '🔔 ', t('pushTitle')), h('div', { class: 'small muted' }, t('pushWhy'))), btn));
}

function render() {
  if (cleanup) { cleanup(); cleanup = null; }
  document.documentElement.lang = lang === 'zh' ? 'zh' : 'en';
  const root = document.getElementById('app');
  root.replaceChildren();
  if (!state.me) { root.append(loginView()); return; }
  const role = state.me.user.role;
  const tabs = TABS[role];
  if (!tabs.some(([k]) => k === state.tab)) state.tab = (location.hash.slice(1) && tabs.some(([k]) => k === location.hash.slice(1))) ? location.hash.slice(1) : tabs[0][0];
  const main = h('main', {});
  root.append(
    h('header', { class: 'topbar' },
      h('h1', {}, t('appName')),
      h('span', { class: 'who' }, state.me.user.name),
      langButton()),
    main,
    h('nav', { class: 'tabs' }, tabs.map(([k, ic]) =>
      h('button', { class: state.tab === k ? 'on' : null, 'aria-current': state.tab === k ? 'page' : null, onclick: () => go(k) },
        h('span', { class: 'ic', 'aria-hidden': 'true' }, ic), t('tab_' + k)))),
  );
  main.append(h('div', { class: 'empty' }, t('loading')));
  Promise.resolve(VIEWS[state.tab](main)).catch((e) => { main.replaceChildren(h('div', { class: 'empty' }, e.message)); });
}

function go(tab) { state.tab = tab; history.replaceState(null, '', '#' + tab); render(); window.scrollTo(0, 0); }

function langButton() {
  return h('button', { class: 'lang-btn', onclick: () => {
    lang = lang === 'zh' ? 'en' : 'zh';
    try { localStorage.setItem('lang', lang); } catch (e) { /* ignore */ }
    render();
  } }, lang === 'zh' ? 'EN' : '中文');
}

// ============ login ============
function loginView() {
  const user = h('input', { type: 'text', autocomplete: 'username', autocapitalize: 'none', required: true });
  const pw = h('input', { type: 'password', autocomplete: 'current-password', required: true });
  const btn = h('button', { class: 'btn primary block', type: 'submit' }, t('login'));
  const form = h('form', { class: 'card', onsubmit: async (e) => {
    e.preventDefault();
    btn.disabled = true;
    const ok = await run(() => api('POST', '/api/login', { username: user.value, password: pw.value }));
    btn.disabled = false;
    if (ok) { state.tab = null; boot(); }
  } },
  h('label', { class: 'f' }, t('username'), user),
  h('label', { class: 'f' }, t('password'), pw),
  btn);
  return h('div', { class: 'login' },
    h('div', { class: 'row', style: 'justify-content:flex-end;margin-bottom:8px' }, (() => { const b = langButton(); b.style.color = 'var(--primary)'; return b; })()),
    h('img', { class: 'logo', src: 'icon.svg', alt: '' }),
    h('h1', {}, t('appName')),
    h('p', { class: 'sub muted' }, t('appSub')),
    form);
}

// ============ parent: home ============
VIEWS.home = async (main) => {
  const me = await api('GET', '/api/me');
  const children = me.children || [];
  const notices = await api('GET', '/api/announcements');
  const out = [];
  const pushCard = await pushPromptCard(() => VIEWS.home(main)).catch(() => null);
  if (pushCard) out.push(pushCard);
  if (!children.length) out.push(h('div', { class: 'card empty' }, t('noChildren')));
  for (const c of children) {
    const recs = await api('GET', `/api/attendance/student/${c.id}`);
    const today = recs.find((r) => r.date === me.today);
    out.push(h('section', { class: 'card' },
      h('div', { class: 'row' },
        h('div', { class: 'grow' }, h('h3', { style: 'margin:0' }, c.name), h('div', { class: 'muted small' }, c.class_name || t('noClass'), ' · ', c.student_no))),
      h('div', { class: 'result ' + (today ? (today.late ? 'warn' : 'ok') : 'info'), style: 'margin:12px 0' },
        h('div', { class: 'small' }, t('todayStatus'), ' ', me.today),
        h('div', { class: 'big' }, today ? t('arrivedAt', { t: hhmm(today.checkin_at) }) : t('notArrived')),
        today && h('div', { class: 'small' }, today.late ? t('late') : t('onTime'))),
      h('h3', {}, t('history')),
      recs.length ? h('ul', { class: 'list' }, recs.slice(0, 14).map((r) => h('li', { class: 'row' },
        h('span', { class: 'grow' }, r.date),
        h('span', {}, hhmm(r.checkin_at)),
        h('span', { class: 'badge ' + (r.late ? 'warn' : 'ok') }, r.late ? t('late') : t('onTime'))))) : emptyRow()));
  }
  if (notices.length) {
    const n = notices[0];
    out.push(h('section', { class: 'card' },
      h('div', { class: 'row' }, h('h3', { class: 'grow', style: 'margin:0' }, t('latestNotice')), h('button', { class: 'btn sm', onclick: () => go('notices') }, t('more'))),
      noticeItem(n, false)));
  }
  main.replaceChildren(h('div', { class: 'stack' }, out));
};

// ============ scan ============
VIEWS.scan = async (main) => {
  const video = h('video', { playsinline: true, muted: true });
  const msg = h('div', { class: 'msg' }, h('button', { class: 'btn primary', onclick: () => start() }, t('startCamera')));
  const box = h('div', { class: 'scanner' }, video, h('div', { class: 'frame' }), msg);
  const result = h('div', { 'aria-live': 'assertive' });
  const toggle = h('button', { class: 'btn sm', onclick: () => (stream ? stop() : start()) }, t('startCamera'));
  const canvas = document.createElement('canvas');
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  let stream = null, timer = null, busy = false, lastCode = '', lastAt = 0;

  async function start() {
    if (!navigator.mediaDevices?.getUserMedia) { msg.replaceChildren(t('cameraError')); return; }
    try {
      stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: { ideal: 'environment' } }, audio: false });
    } catch (e) { msg.replaceChildren(t('cameraError')); return; }
    video.srcObject = stream;
    await video.play().catch(() => {});
    msg.replaceChildren();
    msg.style.display = 'none';
    toggle.textContent = t('stopCamera');
    timer = setInterval(tick, 180);
  }
  function stop() {
    clearInterval(timer); timer = null;
    stream?.getTracks().forEach((tr) => tr.stop());
    stream = null;
    video.srcObject = null;
    msg.style.display = '';
    msg.replaceChildren(h('button', { class: 'btn primary', onclick: () => start() }, t('startCamera')));
    toggle.textContent = t('startCamera');
  }
  cleanup = stop;

  async function tick() {
    if (busy || !video.videoWidth) return;
    const scale = Math.min(1, 640 / video.videoWidth);
    canvas.width = Math.round(video.videoWidth * scale);
    canvas.height = Math.round(video.videoHeight * scale);
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
    const img = ctx.getImageData(0, 0, canvas.width, canvas.height);
    const code = jsQR(img.data, img.width, img.height, { inversionAttempts: 'dontInvert' });
    if (!code || !code.data) return;
    if (code.data === lastCode && Date.now() - lastAt < 4000) return; // same card still in view
    lastCode = code.data; lastAt = Date.now();
    busy = true;
    await submit({ code: code.data }, '/api/attendance/scan');
    busy = false;
  }

  async function submit(body, path) {
    try {
      const r = await api('POST', path, body);
      showResult(r);
    } catch (e) {
      beep(false);
      result.replaceChildren(h('div', { class: 'result bad' }, h('div', { class: 'big' }, e.message)));
    }
  }

  function showResult(r) {
    const kind = r.already ? 'info' : (r.late ? 'warn' : 'ok');
    beep(true);
    if (navigator.vibrate) navigator.vibrate(r.already ? [60, 60, 60] : 120);
    result.replaceChildren(h('div', { class: 'result ' + kind },
      h('div', { class: 'small' }, r.already ? t('alreadyIn') : t('checkedIn')),
      h('div', { class: 'name' }, (r.already ? '' : '✓ ') + r.student.name),
      h('div', {}, r.student.class_name, ' · ', r.student.student_no),
      h('div', { class: 'big' }, hhmm(r.checkin_at), ' · ', r.late ? t('late') : t('onTime'))));
  }

  // Manual check-in for students without their card.
  const students = await api('GET', '/api/students');
  const search = h('input', { type: 'search', placeholder: t('manualSearch'), oninput: () => renderMatches() });
  const matches = h('ul', { class: 'list' });
  function renderMatches() {
    const q = search.value.trim().toLowerCase();
    matches.replaceChildren(...(!q ? [] : students
      .filter((s) => s.name.toLowerCase().includes(q) || s.student_no.toLowerCase().includes(q))
      .slice(0, 8)
      .map((s) => h('li', { class: 'row' },
        h('div', { class: 'grow' }, s.name, h('div', { class: 'muted small' }, s.class_name, ' · ', s.student_no)),
        h('button', { class: 'btn sm primary', onclick: () => { submit({ student_id: s.id }, '/api/attendance/manual'); search.value = ''; renderMatches(); window.scrollTo(0, 0); } }, t('manualBtn'))))));
  }

  main.replaceChildren(h('div', { class: 'stack' },
    h('div', { class: 'row' }, h('h2', { class: 'grow', style: 'margin:0' }, t('scanTitle')), toggle),
    h('p', { class: 'muted small', style: 'margin:0' }, t('cameraHint'), ' · ', t('lateAfter', { t: state.me.late_after })),
    box, result,
    h('section', { class: 'card' }, h('h3', {}, t('manualTitle')), search, matches)));
  start();
};

let audioCtx;
function beep(ok) {
  try {
    audioCtx = audioCtx || new (window.AudioContext || window.webkitAudioContext)();
    const o = audioCtx.createOscillator(), g = audioCtx.createGain();
    o.frequency.value = ok ? 880 : 220;
    g.gain.value = 0.15;
    o.connect(g); g.connect(audioCtx.destination);
    o.start(); o.stop(audioCtx.currentTime + (ok ? 0.15 : 0.4));
  } catch (e) { /* no audio */ }
}

// ============ attendance ============
const attFilter = { date: '', classId: '', onlyAbsent: false };
VIEWS.attendance = async (main) => {
  const classes = await api('GET', '/api/classes');
  if (!attFilter.classId && state.me.user.role === 'teacher') {
    const mine = classes.find((c) => c.teacher_id === state.me.user.id);
    if (mine) attFilter.classId = String(mine.id);
  }
  const q = new URLSearchParams();
  if (attFilter.date) q.set('date', attFilter.date);
  if (attFilter.classId) q.set('class_id', attFilter.classId);
  const data = await api('GET', '/api/attendance?' + q);
  attFilter.date = data.date;
  const rows = data.rows;
  const present = rows.filter((r) => r.present).length;
  const late = rows.filter((r) => r.late).length;
  const shown = attFilter.onlyAbsent ? rows.filter((r) => !r.present) : rows;

  const rerender = () => VIEWS.attendance(main);
  main.replaceChildren(h('div', { class: 'stack' },
    h('h2', {}, t('attendanceTitle')),
    h('div', { class: 'row' },
      h('input', { type: 'date', class: 'grow', value: data.date, onchange: (e) => { attFilter.date = e.target.value; rerender(); } }),
      h('select', { class: 'grow', onchange: (e) => { attFilter.classId = e.target.value; rerender(); } }, classOptions(classes, attFilter.classId, t('allClasses')))),
    h('div', { class: 'stats' },
      h('div', { class: 'stat' }, h('b', {}, present, h('span', {}, ' / ', rows.length)), h('span', {}, t('present'))),
      h('div', { class: 'stat' }, h('b', { style: 'color:var(--bad)' }, rows.length - present), h('span', {}, t('absent'))),
      h('div', { class: 'stat' }, h('b', { style: 'color:var(--warn)' }, late), h('span', {}, t('late')))),
    h('div', { class: 'row' },
      h('label', { class: 'row grow small' }, h('input', { type: 'checkbox', checked: attFilter.onlyAbsent, onchange: (e) => { attFilter.onlyAbsent = e.target.checked; rerender(); } }), t('onlyAbsent')),
      h('button', { class: 'btn sm', onclick: () => exportCsv(data.date, rows) }, t('exportCsv'))),
    h('section', { class: 'card' }, shown.length ? h('ul', { class: 'list' }, shown.map((r) => h('li', { class: 'row' },
      h('div', { class: 'grow' }, r.name, h('div', { class: 'muted small' }, r.class_name || t('noClass'), ' · ', r.student_no)),
      r.present
        ? [h('span', {}, hhmm(r.checkin_at), r.method === 'manual' ? h('span', { class: 'muted small' }, ' (' + t('manual') + ')') : null),
          h('span', { class: 'badge ' + (r.late ? 'warn' : 'ok') }, r.late ? t('late') : t('onTime'))]
        : h('span', { class: 'badge bad' }, t('absent'))))) : emptyRow())));
};

function exportCsv(date, rows) {
  const esc = (v) => '"' + String(v ?? '').replace(/"/g, '""') + '"';
  const lines = [[t('studentNo'), t('studentName'), t('class'), t('checkinTime'), t('present') + '/' + t('absent'), t('late')].map(esc).join(',')];
  for (const r of rows) {
    lines.push([r.student_no, r.name, r.class_name, hhmm(r.checkin_at), r.present ? t('present') : t('absent'), r.late ? t('late') : ''].map(esc).join(','));
  }
  const blob = new Blob(['﻿' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' });
  const a = h('a', { href: URL.createObjectURL(blob), download: `attendance-${date}.csv` });
  document.body.append(a); a.click(); a.remove();
}

// ============ notices ============
function noticeItem(n, canDelete, onDelete) {
  return h('article', {},
    h('div', { class: 'row' },
      h('h3', { class: 'grow', style: 'margin:0' }, n.title),
      h('span', { class: 'badge info' }, n.class_name || t('wholeSchool'))),
    h('div', { class: 'muted small' }, dateTime(n.created_at), ' · ', n.author_name),
    h('p', { class: 'pre', style: 'margin:8px 0 0' }, n.body),
    canDelete && h('div', { class: 'row', style: 'justify-content:flex-end' },
      h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/announcements/${n.id}`), t('deleted'))) onDelete(); } }, t('delete'))));
}

VIEWS.notices = async (main) => {
  const role = state.me.user.role;
  const list = await api('GET', '/api/announcements');
  const rerender = () => VIEWS.notices(main);
  let form = null;
  if (role !== 'parent') {
    const classes = await api('GET', '/api/classes');
    const title = h('input', { type: 'text', required: true, maxlength: 120 });
    const body = h('textarea', { required: true });
    const cls = h('select', {}, classOptions(classes, '', t('wholeSchool')));
    form = h('details', { class: 'add card' }, h('summary', {}, t('newNotice')),
      h('form', { onsubmit: async (e) => {
        e.preventDefault();
        if (await run(() => api('POST', '/api/announcements', { title: title.value, body: body.value, class_id: Number(cls.value) || 0 }), t('published'))) rerender();
      } },
      h('label', { class: 'f' }, t('audience'), cls),
      h('label', { class: 'f' }, t('title'), title),
      h('label', { class: 'f' }, t('content'), body),
      h('button', { class: 'btn primary block', type: 'submit' }, t('publish'))));
  }
  main.replaceChildren(h('div', { class: 'stack' },
    h('h2', {}, t('noticesTitle')),
    form,
    list.length ? list.map((n) => h('section', { class: 'card' },
      noticeItem(n, role === 'admin' || n.author_id === state.me.user.id, rerender))) : h('div', { class: 'card' }, emptyRow())));
};

// ============ receipts ============
let receiptFilter = 'pending';
VIEWS.receipts = async (main) => {
  const role = state.me.user.role;
  const rerender = () => VIEWS.receipts(main);
  const q = role === 'admin' && receiptFilter !== 'all' ? '?status=' + receiptFilter : '';
  const list = await api('GET', '/api/receipts' + q);
  const parts = [h('h2', {}, t('receiptsTitle'))];

  if (role === 'parent') {
    const children = state.me.children || [];
    const child = h('select', { required: true }, children.map((c) => h('option', { value: c.id }, c.name)));
    const desc = h('input', { type: 'text', required: true, maxlength: 200, placeholder: t('descPlaceholder') });
    const amount = h('input', { type: 'number', required: true, min: '0.01', step: '0.01', inputmode: 'decimal' });
    const file = h('input', { type: 'file', required: true, accept: 'image/jpeg,image/png,image/webp,application/pdf' });
    const btn = h('button', { class: 'btn primary block', type: 'submit' }, t('upload'));
    parts.push(children.length ? h('details', { class: 'add card', open: list.length === 0 }, h('summary', {}, t('uploadReceipt')),
      h('form', { onsubmit: async (e) => {
        e.preventDefault();
        const fd = new FormData();
        fd.set('student_id', child.value); fd.set('description', desc.value);
        fd.set('amount', amount.value); fd.set('file', file.files[0]);
        btn.disabled = true;
        const ok = await run(() => api('POST', '/api/receipts', fd), t('uploaded'));
        btn.disabled = false;
        if (ok) rerender();
      } },
      h('label', { class: 'f' }, t('child'), child),
      h('label', { class: 'f' }, t('description'), desc),
      h('label', { class: 'f' }, t('amount') + ` (${state.currency})`, amount),
      h('label', { class: 'f' }, t('file'), file),
      btn)) : h('div', { class: 'card empty' }, t('noChildren')));
  } else {
    parts.push(h('div', { class: 'seg' }, ['pending', 'approved', 'rejected', 'all'].map((s) =>
      h('button', { class: receiptFilter === s ? 'on' : null, onclick: () => { receiptFilter = s; rerender(); } }, t('st_' + s)))));
  }

  parts.push(list.length ? list.map((r) => h('section', { class: 'card' },
    h('div', { class: 'row' },
      h('div', { class: 'grow' }, h('h3', { style: 'margin:0' }, r.student_name), h('div', { class: 'muted small' }, r.class_name)),
      statusBadge(r.status)),
    h('div', { class: 'row', style: 'margin-top:8px' },
      h('div', { class: 'grow' }, r.description),
      h('b', {}, money(r.amount_cents))),
    h('div', { class: 'muted small' }, t('uploadedAt'), ': ', dateTime(r.created_at),
      role === 'admin' ? [' · ', t('parent'), ': ', r.parent_name, r.parent_phone ? ` (${r.parent_phone})` : ''] : null),
    r.review_note && h('div', { class: 'small', style: 'margin-top:6px' }, h('b', {}, t('reviewNote'), ': '), r.review_note),
    role === 'admin' && r.reviewed_by && h('div', { class: 'muted small' }, t('reviewedBy'), ': ', r.reviewed_by, ' · ', dateTime(r.reviewed_at)),
    h('div', { class: 'row', style: 'margin-top:10px' },
      h('a', { class: 'btn sm', href: `/api/receipts/${r.id}/file`, target: '_blank', rel: 'noopener' }, t('viewFile')),
      h('span', { class: 'spacer' }),
      role === 'admin' && r.status !== 'approved' && h('button', { class: 'btn sm ok', onclick: async () => {
        if (await run(() => api('POST', `/api/receipts/${r.id}/review`, { status: 'approved', note: '' }), t('saved'))) rerender();
      } }, t('approve')),
      role === 'admin' && r.status !== 'rejected' && h('button', { class: 'btn sm bad', onclick: async () => {
        const note = prompt(t('rejectReason'));
        if (note === null) return;
        if (await run(() => api('POST', `/api/receipts/${r.id}/review`, { status: 'rejected', note }), t('saved'))) rerender();
      } }, t('reject'))))) : h('div', { class: 'card' }, emptyRow()));

  main.replaceChildren(h('div', { class: 'stack' }, parts));
};

// ============ manage (admin) ============
let manageTab = 'students';
let manageClass = '';
VIEWS.manage = async (main) => {
  const rerender = () => VIEWS.manage(main);
  const [classes, users] = await Promise.all([api('GET', '/api/classes'), api('GET', '/api/users')]);
  const teachers = users.filter((u) => u.role !== 'parent');
  const parentsList = users.filter((u) => u.role === 'parent');
  const body = [];

  if (manageTab === 'classes') {
    const name = h('input', { type: 'text', required: true });
    const teacher = h('select', {}, h('option', { value: '' }, '—'), teachers.map((u) => h('option', { value: u.id }, u.name)));
    body.push(h('details', { class: 'add card' }, h('summary', {}, t('newClass')),
      h('form', { onsubmit: async (e) => {
        e.preventDefault();
        if (await run(() => api('POST', '/api/classes', { name: name.value, teacher_id: Number(teacher.value) || 0 }), t('saved'))) rerender();
      } }, h('label', { class: 'f' }, t('className'), name), h('label', { class: 'f' }, t('classTeacher'), teacher),
      h('button', { class: 'btn primary block' }, t('add')))));
    body.push(h('section', { class: 'card' }, classes.length ? h('ul', { class: 'list' }, classes.map((c) => h('li', { class: 'row' },
      h('div', { class: 'grow' }, h('b', {}, c.name), h('div', { class: 'muted small' }, t('classTeacher'), ': ', c.teacher_name || '—', ' · ', c.students, ' ', t('students'))),
      h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/classes/${c.id}`), t('deleted'))) rerender(); } }, t('delete'))))) : emptyRow()));
  }

  if (manageTab === 'students' || manageTab === 'cards') {
    body.push(h('select', { onchange: (e) => { manageClass = e.target.value; rerender(); } }, classOptions(classes, manageClass, t('allClasses'))));
  }

  if (manageTab === 'students') {
    const students = await api('GET', '/api/students' + (manageClass ? '?class_id=' + manageClass : ''));
    const name = h('input', { type: 'text', required: true });
    const no = h('input', { type: 'text', required: true });
    const cls = h('select', {}, classOptions(classes, manageClass, t('noClass')));
    body.push(h('details', { class: 'add card' }, h('summary', {}, t('newStudent')),
      h('form', { onsubmit: async (e) => {
        e.preventDefault();
        if (await run(() => api('POST', '/api/students', { name: name.value, student_no: no.value, class_id: Number(cls.value) || 0 }), t('saved'))) rerender();
      } }, h('label', { class: 'f' }, t('studentName'), name), h('label', { class: 'f' }, t('studentNo'), no), h('label', { class: 'f' }, t('class'), cls),
      h('button', { class: 'btn primary block' }, t('add')))));
    body.push(h('section', { class: 'card' }, students.length ? h('ul', { class: 'list' }, students.map((s) => h('li', {},
      h('div', { class: 'row' },
        h('div', { class: 'grow' }, h('b', {}, s.name), h('div', { class: 'muted small' }, s.class_name || t('noClass'), ' · ', s.student_no)),
        h('button', { class: 'btn sm', onclick: async () => { if (confirm(t('resetQRConfirm')) && await run(() => api('POST', `/api/students/${s.id}/qr/reset`), t('saved'))) rerender(); } }, t('resetQR')),
        h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/students/${s.id}`), t('deleted'))) rerender(); } }, t('delete'))),
      h('div', { class: 'small', style: 'margin-top:6px' },
        h('span', { class: 'muted' }, t('parents'), ': '),
        (s.parents || []).map((p) => h('span', { class: 'chip' }, p.name,
          h('button', { 'aria-label': t('delete'), onclick: async () => { if (await run(() => api('DELETE', `/api/students/${s.id}/parents/${p.id}`))) rerender(); } }, '×'))),
        h('select', { style: 'width:auto;min-height:32px;padding:2px 8px;margin-top:4px', onchange: async (e) => {
          if (e.target.value && await run(() => api('POST', `/api/students/${s.id}/parents`, { parent_id: Number(e.target.value) }), t('saved'))) rerender();
        } }, h('option', { value: '' }, t('linkParent')),
        parentsList.filter((p) => !(s.parents || []).some((x) => x.id === p.id)).map((p) => h('option', { value: p.id }, p.name + (p.phone ? ` (${p.phone})` : ''))))))))
      : emptyRow()));
  }

  if (manageTab === 'users') {
    const role = h('select', {}, ['parent', 'teacher', 'admin'].map((r) => h('option', { value: r }, t('role_' + r))));
    const name = h('input', { type: 'text', required: true });
    const username = h('input', { type: 'text', required: true, autocapitalize: 'none', pattern: '[a-zA-Z0-9._\\-]{3,32}' });
    const phone = h('input', { type: 'tel' });
    const pw = h('input', { type: 'text', required: true, minlength: 6, autocomplete: 'new-password' });
    body.push(h('details', { class: 'add card' }, h('summary', {}, t('newUser')),
      h('form', { onsubmit: async (e) => {
        e.preventDefault();
        if (await run(() => api('POST', '/api/users', { role: role.value, name: name.value, username: username.value, phone: phone.value, password: pw.value }), t('saved'))) rerender();
      } }, h('label', { class: 'f' }, t('role'), role), h('label', { class: 'f' }, t('name'), name), h('label', { class: 'f' }, t('username'), username),
      h('label', { class: 'f' }, t('phone'), phone), h('label', { class: 'f' }, t('password'), pw),
      h('button', { class: 'btn primary block' }, t('add')))));
    body.push(h('section', { class: 'card' }, h('ul', { class: 'list' }, users.map((u) => h('li', { class: 'row' },
      h('div', { class: 'grow' }, h('b', {}, u.name), ' ', h('span', { class: 'badge' }, t('role_' + u.role)),
        h('div', { class: 'muted small' }, u.username, u.phone ? ' · ' + u.phone : '')),
      h('button', { class: 'btn sm', onclick: async () => {
        const p = prompt(t('newPwPrompt'));
        if (p) await run(() => api('POST', `/api/users/${u.id}/password`, { password: p }), t('saved'));
      } }, t('resetPw')),
      u.id !== state.me.user.id && h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/users/${u.id}`), t('deleted'))) rerender(); } }, t('delete')))))));
  }

  if (manageTab === 'notify') {
    const data = await api('GET', '/api/notifications/log');
    const phone = h('input', { type: 'tel', required: true, placeholder: '012-3456789' });
    body.push(h('section', { class: 'card' },
      h('h3', {}, 'WhatsApp ', data.whatsapp_enabled ? h('span', { class: 'badge ok' }, t('waEnabled')) : h('span', { class: 'badge warn' }, t('waDisabled'))),
      data.whatsapp_enabled
        ? h('form', { class: 'row', onsubmit: async (e) => { e.preventDefault(); if (await run(() => api('POST', '/api/notifications/whatsapp-test', { phone: phone.value }), t('waTestSent'))) rerender(); } },
          h('div', { class: 'grow' }, phone), h('button', { class: 'btn primary' }, t('waTest')))
        : h('p', { class: 'small muted', style: 'margin:0' }, t('waSetupHint'))));
    body.push(h('section', { class: 'card' }, h('h3', {}, t('notifyLog')),
      data.entries.length ? h('ul', { class: 'list' }, data.entries.map((e) => h('li', {},
        h('div', { class: 'row' },
          h('span', { class: 'badge ' + (e.ok ? 'ok' : 'bad') }, e.ok ? '✓' : '✗'),
          h('span', { class: 'badge info' }, e.channel === 'push' ? 'App' : 'WhatsApp'),
          h('span', { class: 'grow' }, e.user, e.student ? ' ← ' + e.student : ''),
          h('span', { class: 'muted small' }, dateTime(e.created_at))),
        h('div', { class: 'muted small', style: 'word-break:break-all' }, e.detail)))) : emptyRow()));
  }

  if (manageTab === 'cards') {
    const students = await api('GET', '/api/students' + (manageClass ? '?class_id=' + manageClass : ''));
    body.push(h('div', { class: 'row no-print' }, h('span', { class: 'muted small grow' }, t('cardsHint')), h('button', { class: 'btn primary', onclick: () => window.print() }, t('print'))));
    body.push(students.length ? h('div', { class: 'cards' }, students.map((s) => h('div', { class: 'qrcard' },
      h('img', { src: `/api/students/${s.id}/qr.png`, alt: s.name, loading: 'lazy' }),
      h('div', { class: 'n' }, s.name),
      h('div', { class: 'c' }, s.class_name, ' · ', s.student_no)))) : emptyRow());
  }

  main.replaceChildren(h('div', { class: 'stack' },
    h('div', { class: 'seg no-print' }, ['students', 'classes', 'users', 'cards', 'notify'].map((k) =>
      h('button', { class: manageTab === k ? 'on' : null, onclick: () => { manageTab = k; rerender(); } }, t('m_' + k)))),
    body));
};

// ============ me ============
VIEWS.me = async (main) => {
  const u = state.me.user;
  const me = await api('GET', '/api/me');
  const notify = me.notify || {};
  const rerender = () => VIEWS.me(main);
  const sub = await push.current().catch(() => null);
  let pushPart;
  if (push.needsInstall()) pushPart = h('p', { class: 'small muted' }, t('pushIOSHint'));
  else if (!push.supported()) pushPart = h('p', { class: 'small muted' }, t('pushUnsupported'));
  else if (sub) {
    pushPart = h('div', { class: 'row' },
      h('span', { class: 'badge ok' }, '✓ ', t('pushOnDevice')), h('span', { class: 'spacer' }),
      h('button', { class: 'btn sm', onclick: () => run(() => api('POST', '/api/push/test'), t('pushTestSent')) }, t('pushTest')),
      h('button', { class: 'btn sm bad', onclick: async () => { await push.disable(); rerender(); } }, t('pushDisable')));
  } else if (Notification.permission === 'denied') pushPart = h('p', { class: 'small muted' }, t('pushBlocked'));
  else pushPart = h('button', { class: 'btn primary', onclick: async () => { if (await run(() => push.enable(), t('pushOn'))) rerender(); } }, t('pushEnable'));

  let waPart = null;
  if (u.role === 'parent') {
    if (!notify.whatsapp_available) waPart = h('p', { class: 'small muted', style: 'margin:0' }, t('waNotAvailable'));
    else if (!u.phone) waPart = h('p', { class: 'small muted', style: 'margin:0' }, t('waNoPhone'));
    else {
      waPart = h('label', { class: 'row' },
        h('input', { type: 'checkbox', checked: notify.whatsapp, onchange: (e) => run(() => api('POST', '/api/me/notify', { push: true, whatsapp: e.target.checked }), t('saved')) }),
        h('span', { class: 'grow' }, t('waToggle', { p: u.phone }), h('div', { class: 'small muted' }, t('waPhoneWrong'))));
    }
  }

  const oldPw = h('input', { type: 'password', required: true, autocomplete: 'current-password' });
  const newPw = h('input', { type: 'password', required: true, minlength: 6, autocomplete: 'new-password' });
  main.replaceChildren(h('div', { class: 'stack' },
    h('section', { class: 'card' },
      h('h2', { style: 'margin:0' }, u.name),
      h('div', { class: 'muted' }, t('role_' + u.role), ' · ', u.username)),
    h('section', { class: 'card stack' },
      h('h3', {}, '🔔 ', t('notifications')),
      h('div', {}, h('div', { class: 'small muted', style: 'margin-bottom:6px' }, t('pushTitle')), pushPart),
      waPart && h('div', {}, h('div', { class: 'small muted', style: 'margin-bottom:6px' }, 'WhatsApp'), waPart)),
    h('section', { class: 'card' },
      h('h3', {}, t('language')),
      h('div', { class: 'seg' }, [['zh', '中文'], ['en', 'English']].map(([k, label]) =>
        h('button', { class: lang === k ? 'on' : null, onclick: () => { lang = k; try { localStorage.setItem('lang', k); } catch (e) { /* ignore */ } render(); } }, label)))),
    h('form', { class: 'card', onsubmit: async (e) => {
      e.preventDefault();
      if (await run(() => api('POST', '/api/me/password', { old: oldPw.value, new: newPw.value }), t('pwChanged'))) { oldPw.value = ''; newPw.value = ''; }
    } },
    h('h3', {}, t('changePw')),
    h('label', { class: 'f' }, t('oldPw'), oldPw),
    h('label', { class: 'f' }, t('newPw'), newPw),
    h('button', { class: 'btn primary block' }, t('save'))),
    h('button', { class: 'btn bad block', onclick: logout }, t('logout'))));
};

boot();
