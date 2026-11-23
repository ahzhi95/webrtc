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
const todayStr = () => state.me.today;
const nowHHMM = () => new Date().toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: state.me.time_zone || undefined });

// Staff who are not tied to one campus see the campus in front of class names.
const allCampuses = () => state.me.user.role !== 'parent' && !state.me.user.campus_id;
const className = (c) => (allCampuses() && c.campus_name ? c.campus_name + ' · ' : '') + c.name;

function classOptions(classes, selected, allLabel) {
  return [
    allLabel != null && h('option', { value: '' }, allLabel),
    ...classes.map((c) => h('option', { value: c.id, selected: String(c.id) === String(selected) }, className(c))),
  ];
}

function campusOptions(campuses, selected, allLabel) {
  return [
    allLabel != null && h('option', { value: '' }, allLabel),
    ...campuses.map((c) => h('option', { value: c.id, selected: String(c.id) === String(selected) }, c.name)),
  ];
}

// A modal dialog. body: nodes; onOk: async () => boolean (true closes). Returns close().
function openDialog(title, body, onOk, okLabel) {
  const prevFocus = document.activeElement;
  const okBtn = h('button', { class: 'btn primary', type: 'submit' }, okLabel || t('save'));
  const close = () => { back.remove(); document.removeEventListener('keydown', onKey); prevFocus?.focus?.(); };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  const form = h('form', { class: 'dialog', role: 'dialog', 'aria-modal': 'true', 'aria-label': title, onsubmit: async (e) => {
    e.preventDefault();
    okBtn.disabled = true;
    try { if (await onOk()) close(); } finally { okBtn.disabled = false; }
  } },
  h('div', { class: 'row' }, h('h3', { class: 'grow', style: 'margin:0' }, title), h('button', { type: 'button', class: 'btn sm', 'aria-label': t('close'), onclick: close }, '✕')),
  h('div', { class: 'dialog-body' }, body),
  onOk && h('div', { class: 'row', style: 'justify-content:flex-end' }, h('button', { type: 'button', class: 'btn', onclick: close }, t('cancel')), okBtn));
  const back = h('div', { class: 'dialog-back', onclick: (e) => { if (e.target === back) close(); } }, form);
  document.addEventListener('keydown', onKey);
  document.body.append(back);
  form.querySelector('input,select,textarea')?.focus();
  return close;
}

const field = (label, input) => h('label', { class: 'f' }, label, input);

// ============ app state & shell ============
const state = { me: null, tab: null, currency: 'RM', studentId: 0, backTab: null };
const TABS = {
  parent: [['home', '🏠'], ['photos', '🖼️'], ['notices', '📢'], ['receipts', '🧾'], ['me', '👤']],
  teacher: [['scan', '📷'], ['attendance', '✅'], ['photos', '🖼️'], ['notices', '📢'], ['me', '👤']],
  admin: [['overview', '📊'], ['attendance', '✅'], ['photos', '🖼️'], ['notices', '📢'], ['receipts', '🧾'], ['manage', '⚙️'], ['me', '👤']],
};
const VIEWS = {};
let cleanup = null; // called when leaving a view (e.g. to stop the camera)

async function boot() {
  // A password reset link from email: #reset=<token>
  if (location.hash.startsWith('#reset=')) {
    state.resetToken = location.hash.slice(7);
    history.replaceState(null, '', location.pathname);
  }
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
  if (state.resetToken) { root.append(resetView()); return; }
  if (!state.me) { root.append(state.forgot ? forgotView() : loginView()); return; }
  const role = state.me.user.role;
  const tabs = TABS[role];
  if (state.tab === 'student' && !state.studentId) state.tab = null;
  // Pages reached from inside another page rather than from the tab bar.
  const extra = state.tab === 'student' || state.tab === 'messages' || (state.tab === 'scan' && role !== 'parent');
  if (!extra && !tabs.some(([k]) => k === state.tab)) state.tab = (location.hash.slice(1) && tabs.some(([k]) => k === location.hash.slice(1))) ? location.hash.slice(1) : tabs[0][0];
  const main = h('main', {});
  root.append(
    h('header', { class: 'topbar' },
      h('h1', {}, t('appName')),
      state.me.user.campus_name && h('span', { class: 'campus-tag' }, state.me.user.campus_name),
      h('span', { class: 'who' }, state.me.user.name),
      bellButton(),
      langButton(),
      h('button', { class: 'lang-btn', onclick: () => { if (confirm(t('logoutConfirm'))) logout(); } }, t('logoutShort'))),
    main,
    h('nav', { class: 'tabs' }, tabs.map(([k, ic]) =>
      h('button', { class: state.tab === k ? 'on' : null, 'aria-current': state.tab === k ? 'page' : null, onclick: () => go(k) },
        h('span', { class: 'ic', 'aria-hidden': 'true' }, ic), t('tab_' + k)))),
  );
  main.append(h('div', { class: 'empty' }, t('loading')));
  Promise.resolve(VIEWS[state.tab](main)).catch((e) => { main.replaceChildren(h('div', { class: 'empty' }, e.message)); });
}

// 🔔 with the number of unread messages; opens the message center.
function bellButton() {
  const n = state.me.unread || 0;
  return h('button', { class: 'bell' + (state.tab === 'messages' ? ' on' : ''), 'aria-label': t('messagesTitle') + (n ? ` (${n})` : ''),
    onclick: () => { if (state.tab !== 'messages') state.backTab = state.tab; go('messages'); } },
  '🔔', n > 0 && h('span', { class: 'bell-n' }, n > 99 ? '99+' : n));
}

async function refreshUnread() {
  if (!state.me || document.hidden) return;
  try {
    const { unread } = await api('GET', '/api/messages/unread');
    if (unread !== state.me.unread) {
      state.me.unread = unread;
      document.querySelector('.bell')?.replaceWith(bellButton());
    }
  } catch (e) { /* offline: try again later */ }
}
setInterval(refreshUnread, 60000);
document.addEventListener('visibilitychange', refreshUnread);

function go(tab) { state.tab = tab; history.replaceState(null, '', '#' + tab); render(); window.scrollTo(0, 0); }

// Opens a student's page (profile, records, photos, fees); the back button returns to where we were.
function openStudent(id) {
  state.backTab = state.tab === 'student' ? state.backTab : state.tab;
  state.studentId = id;
  go('student');
}

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
  btn,
  h('div', { style: 'text-align:center;margin-top:12px' }, h('button', { type: 'button', class: 'linkbtn', onclick: () => { state.forgot = true; render(); } }, t('forgotPw'))));
  return h('div', { class: 'login' },
    h('div', { class: 'row', style: 'justify-content:flex-end;margin-bottom:8px' }, (() => { const b = langButton(); b.style.color = 'var(--primary)'; return b; })()),
    h('img', { class: 'logo', src: 'icon.svg', alt: '' }),
    h('h1', {}, t('appName')),
    h('p', { class: 'sub muted' }, t('appSub')),
    form);
}


function authShell(title, ...content) {
  return h('div', { class: 'login' },
    h('div', { class: 'row', style: 'justify-content:flex-end;margin-bottom:8px' }, (() => { const b = langButton(); b.style.color = 'var(--primary)'; return b; })()),
    h('img', { class: 'logo', src: 'icon.svg', alt: '' }),
    h('h1', {}, title), ...content);
}

// Forgot password: by username, phone or email. The answer is the same whether or not the account exists.
function forgotView() {
  const id = h('input', { type: 'text', required: true, autocapitalize: 'none', autocomplete: 'username' });
  const contact = h('input', { type: 'text', maxlength: 100, placeholder: t('forgotContactPh') });
  const btn = h('button', { class: 'btn primary block', type: 'submit' }, t('forgotSend'));
  const back = h('button', { type: 'button', class: 'linkbtn', onclick: () => { state.forgot = false; render(); } }, '‹ ', t('backToLogin'));
  const form = h('form', { class: 'card', onsubmit: async (e) => {
    e.preventDefault();
    btn.disabled = true;
    const ok = await run(() => api('POST', '/api/password/forgot', { identifier: id.value, contact: contact.value }));
    btn.disabled = false;
    if (ok) form.replaceChildren(h('div', { class: 'result info', style: 'text-align:left' }, h('p', { style: 'margin:0' }, t('forgotDone'))), h('div', { style: 'text-align:center;margin-top:12px' }, back));
  } },
  h('p', { class: 'small muted', style: 'margin-top:0' }, t('forgotHint')),
  field(t('forgotId'), id), field(t('forgotContact'), contact), btn,
  h('div', { style: 'text-align:center;margin-top:12px' }, back));
  return authShell(t('forgotTitle'), form);
}

// Set a new password from an emailed link.
function resetView() {
  const pw = h('input', { type: 'password', required: true, minlength: 6, autocomplete: 'new-password' });
  const pw2 = h('input', { type: 'password', required: true, minlength: 6, autocomplete: 'new-password' });
  const btn = h('button', { class: 'btn primary block', type: 'submit' }, t('save'));
  const toLogin = () => { state.resetToken = null; state.forgot = false; state.me = null; render(); };
  const form = h('form', { class: 'card', onsubmit: async (e) => {
    e.preventDefault();
    if (pw.value !== pw2.value) { toast(t('pwMismatch')); return; }
    btn.disabled = true;
    const ok = await run(() => api('POST', '/api/password/reset', { token: state.resetToken, password: pw.value }));
    btn.disabled = false;
    if (ok) form.replaceChildren(h('div', { class: 'result ok' }, h('div', { class: 'big' }, '✓ ', t('resetDone'))),
      h('button', { type: 'button', class: 'btn primary block', style: 'margin-top:12px', onclick: toLogin }, t('login')));
  } }, field(t('newPw'), pw), field(t('newPwAgain'), pw2), btn,
  h('div', { style: 'text-align:center;margin-top:12px' }, h('button', { type: 'button', class: 'linkbtn', onclick: toLogin }, '‹ ', t('backToLogin'))));
  return authShell(t('resetTitle'), form);
}

// Password reset requests from people without an email address (shown on the admin overview).
async function resetRequestsCard(rerender) {
  const list = await api('GET', '/api/password/requests');
  if (!list.length) return null;
  return h('section', { class: 'card', style: 'border-color:var(--warn)' },
    h('h3', {}, '🔑 ', t('resetRequests', { n: list.length })),
    h('ul', { class: 'list' }, list.map((x) => h('li', {},
      h('div', { class: 'row' },
        h('div', { class: 'grow' }, h('b', {}, x.name), ' ', h('span', { class: 'badge' }, t('role_' + x.role)),
          h('div', { class: 'muted small' }, x.username, x.phone ? ' · ' + x.phone : '', x.children ? ' · ' + x.children : ''),
          x.contact && h('div', { class: 'small' }, t('forgotContact'), ': ', x.contact),
          h('div', { class: 'muted small' }, dateTime(x.created_at)))),
      h('div', { class: 'row', style: 'justify-content:flex-end;margin-top:6px' },
        h('button', { class: 'btn sm', onclick: async () => { if (await run(() => api('POST', `/api/password/requests/${x.id}`, { action: 'dismiss' }))) rerender(); } }, t('dismiss')),
        h('button', { class: 'btn sm primary', onclick: async () => {
          if (!confirm(t('resetConfirm', { n: x.name }))) return;
          let r;
          if (!(await run(async () => { r = await api('POST', `/api/password/requests/${x.id}`, { action: 'reset' }); }))) return;
          openDialog(t('tempPwTitle'), [
            h('p', {}, t('tempPwFor', { n: x.name, u: x.username })),
            h('div', { class: 'temp-pw' }, r.password),
            h('p', { class: 'small muted' }, t('tempPwHint')),
          ], null);
          rerender();
        } }, t('resetPw')))))));
}

// ============ message center (消息) ============
VIEWS.messages = async (main) => {
  const role = state.me.user.role;
  let list = [];
  let more = true;
  const ul = h('ul', { class: 'list' });
  const moreBtn = h('button', { class: 'btn block', onclick: () => load() }, t('loadMore'));
  const open = async (m) => {
    if (!m.read) {
      const r = await api('POST', '/api/messages/read', { id: m.id }).catch(() => null);
      if (r) state.me.unread = r.unread;
    }
    const tab = (m.url || '').split('#')[1];
    if (tab && (TABS[role].some(([k]) => k === tab) || tab === 'overview')) go(tab); else render();
  };
  const draw = () => {
    ul.replaceChildren(...list.map((m) => h('li', { class: 'msg' + (m.read ? '' : ' unread') },
      h('button', { class: 'linkish', onclick: () => open(m) },
        h('div', { class: 'row' }, !m.read && h('span', { class: 'dot', 'aria-label': t('unread') }), h('b', { class: 'grow' }, m.title), h('span', { class: 'muted small' }, dateTime(m.created_at))),
        h('div', { class: 'pre small' }, m.body)))));
    moreBtn.hidden = !more;
  };
  const load = async () => {
    const before = list.length ? list[list.length - 1].id : 0;
    const r = await api('GET', '/api/messages' + (before ? '?before=' + before : ''));
    list = list.concat(r.messages);
    more = r.messages.length === 50;
    state.me.unread = r.unread;
    draw();
  };
  await load();
  const back = h('button', { class: 'btn sm', onclick: () => go(state.backTab || TABS[role][0][0]) }, '‹ ', t('back'));
  const readAll = h('button', { class: 'btn sm', onclick: async () => {
    if (await run(async () => { const r = await api('POST', '/api/messages/read', { all: true }); state.me.unread = r.unread; })) { list.forEach((m) => { m.read = true; }); draw(); document.querySelector('.bell')?.replaceWith(bellButton()); }
  } }, t('markAllRead'));
  main.replaceChildren(h('div', { class: 'stack' },
    h('div', { class: 'row' }, back, h('h2', { class: 'grow', style: 'margin:0' }, t('messagesTitle')), list.some((m) => !m.read) && readAll),
    list.length ? h('section', { class: 'card' }, ul) : h('div', { class: 'card empty' }, t('noMessages')),
    moreBtn));
};

// ============ parent: home ============
function todayBox(rec) {
  const inT = rec && hhmm(rec.checkin_at), outT = rec && hhmm(rec.checkout_at);
  const kind = outT ? 'info' : inT ? (rec.late ? 'warn' : 'ok') : 'info';
  return h('div', { class: 'result ' + kind, style: 'margin:12px 0;text-align:left' },
    h('div', { class: 'small' }, t('todayStatus'), ' ', todayStr()),
    h('div', { class: 'big' }, inT ? t('arrivedAt', { t: inT }) : t('notArrived'), inT && rec.late ? ' · ' + t('late') : ''),
    outT && h('div', { class: 'big' }, t('leftAt', { t: outT })));
}

VIEWS.home = async (main) => {
  const me = await api('GET', '/api/me');
  state.me.children = me.children;
  const children = me.children || [];
  const [notices, fees, photos] = await Promise.all([
    api('GET', '/api/announcements'),
    children.length ? api('GET', '/api/fees') : [],
    children.length ? api('GET', '/api/photos?limit=6') : [],
  ]);
  const out = [];
  const pushCard = await pushPromptCard(() => VIEWS.home(main)).catch(() => null);
  if (pushCard) out.push(pushCard);
  if (!children.length) out.push(h('div', { class: 'card empty' }, t('noChildren')));
  for (const c of children) {
    const data = await api('GET', `/api/attendance/student/${c.id}`);
    const today = data.records.find((r) => r.date === todayStr());
    out.push(h('section', { class: 'card' },
      h('div', { class: 'row' },
        h('div', { class: 'grow' }, h('h3', { style: 'margin:0' }, c.name), h('div', { class: 'muted small' }, c.campus_name, ' · ', c.class_name || t('noClass'))),
        h('button', { class: 'btn sm', onclick: () => openStudent(c.id) }, t('profileAndRecords'))),
      todayBox(today)));
  }
  const unpaid = fees.filter((f) => f.status === 'unpaid');
  if (unpaid.length) {
    const total = unpaid.reduce((s, f) => s + f.amount_cents, 0);
    const overdue = unpaid.some((f) => f.due_date && f.due_date < todayStr());
    out.push(h('section', { class: 'card' }, h('div', { class: 'row' },
      h('div', { class: 'grow' }, h('h3', { style: 'margin:0' }, '🧾 ', t('feesDue', { n: unpaid.length })),
        h('div', { class: 'small ' + (overdue ? 'bad-text' : 'muted') }, money(total), overdue ? ' · ' + t('overdue') : '')),
      h('button', { class: 'btn sm primary', onclick: () => go('receipts') }, t('payNow')))));
  }
  if (photos.length) {
    out.push(h('section', { class: 'card' },
      h('div', { class: 'row', style: 'margin-bottom:10px' }, h('h3', { class: 'grow', style: 'margin:0' }, t('latestPhotos')), h('button', { class: 'btn sm', onclick: () => go('photos') }, t('more'))),
      photoGrid(photos, () => VIEWS.home(main))));
  }
  if (notices.length) {
    out.push(h('section', { class: 'card' },
      h('div', { class: 'row' }, h('h3', { class: 'grow', style: 'margin:0' }, t('latestNotice')), h('button', { class: 'btn sm', onclick: () => go('notices') }, t('more'))),
      noticeItem(notices[0], false)));
  }
  main.replaceChildren(h('div', { class: 'stack' }, out));
};

// ============ admin: school overview (学校概况) ============
let overviewCampus = '';
VIEWS.overview = async (main) => {
  const rerender = () => VIEWS.overview(main);
  const [data, campuses] = await Promise.all([
    api('GET', '/api/overview' + (overviewCampus ? '?campus_id=' + overviewCampus : '')),
    allCampuses() ? api('GET', '/api/campuses') : [],
  ]);
  const pct = (a, b) => (b ? Math.round(100 * a / b) : 0);
  const stat = (n, label, cls) => h('div', { class: 'stat' }, h('b', { class: cls || '' }, n), h('span', {}, label));
  const reqCard = await resetRequestsCard(rerender).catch(() => null);
  const parts = [
    h('div', { class: 'row' }, h('h2', { class: 'grow', style: 'margin:0' }, t('overviewTitle')), h('span', { class: 'muted small' }, data.date)),
    reqCard,
    campuses.length > 1 && h('select', { onchange: (e) => { overviewCampus = e.target.value; rerender(); } }, campusOptions(campuses, overviewCampus, t('allSchools'))),
  ];
  for (const c of data.campuses) {
    const absent = c.students - c.present;
    parts.push(h('section', { class: 'card stack' },
      h('h3', { style: 'margin:0' }, c.name),
      h('div', {},
        h('div', { class: 'row small' }, h('b', { class: 'grow' }, '🌅 ', t('todayArrival')), h('span', { class: 'muted' }, `${pct(c.present, c.students)}%`)),
        h('div', { class: 'progress' }, h('div', { style: `width:${pct(c.present, c.students)}%` })),
        h('div', { class: 'stats stats4' },
          stat(h('span', {}, c.present, h('span', { class: 'muted small' }, ' / ', c.students)), t('present')),
          stat(absent, t('absent'), absent ? 'bad-text' : ''), stat(c.late, t('late'), c.late ? 'warn-text' : ''), stat(c.left, t('leftCount')))),
      c.classes.length > 0 && h('table', { class: 'mini-table' },
        h('thead', {}, h('tr', {}, h('th', {}, t('class')), h('th', {}, t('present')), h('th', {}, t('absent')), h('th', {}, t('late')), h('th', {}, t('leftCount')))),
        h('tbody', {}, c.classes.map((k) => h('tr', { class: 'clickable', onclick: () => { attFilter.classId = String(k.id); attFilter.campusId = ''; attFilter.date = ''; go('attendance'); } },
          h('td', {}, k.name, h('div', { class: 'muted small' }, k.teacher || '—')),
          h('td', {}, k.present, '/', k.students),
          h('td', { class: k.students - k.present ? 'bad-text' : '' }, k.students - k.present),
          h('td', { class: k.late ? 'warn-text' : '' }, k.late),
          h('td', {}, k.left))))),
      h('div', {},
        h('b', { class: 'small' }, '🧾 ', t('feesStatus')),
        h('ul', { class: 'list' },
          h('li', { class: 'row' }, h('span', { class: 'grow' }, t('pendingReceipts')),
            h('b', { class: c.pending_receipts ? 'warn-text' : '' }, c.pending_receipts),
            c.pending_receipts > 0 && h('button', { class: 'btn sm primary', onclick: () => { receiptFilter.status = 'pending'; receiptFilter.campusId = allCampuses() ? String(c.id) : ''; go('receipts'); } }, t('review'))),
          h('li', { class: 'row' }, h('span', { class: 'grow' }, t('overdueFees', { n: c.overdue_count })), h('b', { class: c.overdue_cents ? 'bad-text' : '' }, money(c.overdue_cents)),
            c.overdue_count > 0 && h('button', { class: 'btn sm', onclick: () => { manageTab = 'fees'; feeFilter.campusId = allCampuses() ? String(c.id) : ''; go('manage'); } }, '🔔 ', t('remind'))),
          h('li', { class: 'row' }, h('span', { class: 'grow' }, t('unpaidFees', { n: c.unpaid_count })), h('b', {}, money(c.unpaid_cents))),
          h('li', { class: 'row' }, h('span', { class: 'grow' }, t('collectedMonth')), h('b', { class: 'ok-text' }, money(c.collected_month_cents)))))));
  }
  if (!data.campuses.length) parts.push(h('div', { class: 'card' }, emptyRow()));
  parts.push(h('div', { class: 'row' },
    h('button', { class: 'btn grow', onclick: () => go('notices') }, '📢 ', t('newNotice')),
    h('button', { class: 'btn grow', onclick: () => { manageTab = 'fees'; go('manage'); } }, '🧾 ', t('newFee'))));
  main.replaceChildren(h('div', { class: 'stack' }, parts));
};

// ============ notices ============
function audienceLabel(n) {
  if (n.class_name) return (allCampuses() || state.me.user.role === 'parent' ? n.campus_name + ' · ' : '') + n.class_name;
  return n.campus_name || t('allSchools');
}

function noticeFiles(n) {
  if (!n.files || !n.files.length) return null;
  const src = (f) => `/api/announcements/${n.id}/files/${f.id}`;
  const imgs = n.files.filter((f) => f.type.startsWith('image/'));
  const docs = n.files.filter((f) => !f.type.startsWith('image/'));
  return h('div', { class: 'notice-files' },
    imgs.length > 0 && h('div', { class: 'pgrid' }, imgs.map((f) => h('a', { class: 'pthumb', href: src(f), target: '_blank', rel: 'noopener' }, h('img', { src: src(f), alt: f.name, loading: 'lazy' })))),
    docs.map((f) => h('a', { class: 'file-chip', href: src(f), target: '_blank', rel: 'noopener' }, '📄 ', f.name, h('span', { class: 'muted small' }, ` ${Math.max(1, Math.round(f.size / 1024))} KB`))));
}

function noticeItem(n, canManage, onChange) {
  const scheduled = !n.published;
  return h('article', { class: n.retracted_at ? 'is-retracted' : null },
    h('div', { class: 'row' },
      h('h3', { class: 'grow', style: 'margin:0' }, !n.read && h('span', { class: 'badge bad', style: 'margin-right:6px' }, t('newBadge')), n.title),
      h('span', { class: 'badge info' }, audienceLabel(n))),
    h('div', { class: 'muted small' },
      scheduled ? h('span', { class: 'badge warn' }, '⏰ ', t('scheduledFor', { t: dateTime(n.publish_at) })) : dateTime(n.created_at),
      ' · ', n.author_name,
      n.edited_at && h('span', {}, ' · ', t('editedOn', { t: dateTime(n.edited_at) })),
      n.retracted_at && h('span', { class: 'badge bad', style: 'margin-left:6px' }, t('retracted'))),
    h('p', { class: 'pre', style: 'margin:8px 0' }, n.body),
    noticeFiles(n),
    canManage && h('div', { class: 'row', style: 'margin-top:8px' },
      n.published && !n.retracted_at && h('button', { class: 'btn sm', onclick: () => noticeReads(n) }, '👁 ', t('readStats', { r: n.read_count, n: n.recipients })),
      h('span', { class: 'spacer' }),
      !n.retracted_at && h('button', { class: 'btn sm', onclick: () => noticeDialog(n, onChange) }, t('edit')),
      !n.retracted_at && h('button', { class: 'btn sm', onclick: async () => {
        if (confirm(t(scheduled ? 'cancelScheduledConfirm' : 'retractConfirm')) && await run(() => api('POST', `/api/announcements/${n.id}/retract`), t('saved'))) onChange();
      } }, scheduled ? t('cancelSchedule') : t('retract')),
      h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/announcements/${n.id}`), t('deleted'))) onChange(); } }, t('delete'))));
}

async function noticeReads(n) {
  const list = await api('GET', `/api/announcements/${n.id}/reads`);
  const unread = list.filter((x) => !x.read_at);
  openDialog(t('readStats', { r: list.length - unread.length, n: list.length }), [
    h('p', { class: 'small muted', style: 'margin-top:0' }, n.title),
    list.length ? h('ul', { class: 'list' }, list.map((x) => h('li', { class: 'row' },
      h('div', { class: 'grow' }, h('b', {}, x.name), h('div', { class: 'muted small' }, x.children, x.phone ? ' · ' + x.phone : '')),
      x.read_at ? h('span', { class: 'badge ok' }, '✓ ', dateTime(x.read_at)) : h('span', { class: 'badge warn' }, t('unread'))))) : emptyRow(),
  ], null);
}

// New notice or edit. Audience and time can change only before it is published.
async function noticeDialog(n, done) {
  const u = state.me.user;
  const [classes, campuses] = await Promise.all([api('GET', '/api/classes'), api('GET', '/api/campuses')]);
  const fixed = n && n.published;
  const title = h('input', { type: 'text', required: true, maxlength: 120, value: n ? n.title : '' });
  const body = h('textarea', { required: true }, n ? n.body : '');
  const cur = n ? (n.class_id ? 'class:' + n.class_id : n.campus_id ? 'campus:' + n.campus_id : '') : '';
  const aud = h('select', { disabled: fixed },
    !u.campus_id && h('option', { value: '', selected: cur === '' }, t('allSchools')),
    campuses.map((c) => h('option', { value: 'campus:' + c.id, selected: cur === 'campus:' + c.id }, c.name + ' · ' + t('wholeCampus'))),
    classes.map((c) => h('option', { value: 'class:' + c.id, selected: cur === 'class:' + c.id }, className(c))));
  const when = h('input', { type: 'datetime-local', value: n && !n.published ? n.publish_at.slice(0, 16) : '', min: todayStr() + 'T00:00' });
  const later = h('input', { type: 'checkbox', checked: !!(n && !n.published), onchange: () => { whenBox.hidden = !later.checked; } });
  const whenBox = h('div', { hidden: !(n && !n.published) }, field(t('publishAt'), when));
  const files = h('input', { type: 'file', multiple: true, accept: 'image/*,application/pdf' });
  const remove = new Set();
  const existing = n && n.files.length ? h('div', { class: 'f' }, h('div', { class: 'small muted' }, t('attachments')),
    n.files.map((f) => h('label', { class: 'row small' }, h('input', { type: 'checkbox', onchange: (e) => { e.target.checked ? remove.add(f.id) : remove.delete(f.id); } }), t('removeFile'), ': ', f.name))) : null;
  openDialog(n ? t('editNotice') : t('newNotice'), [
    field(t('audience'), aud), fixed && h('p', { class: 'small muted', style: 'margin-top:-6px' }, t('audienceFixed')),
    field(t('title'), title), field(t('content'), body),
    existing, field(t('addAttachments'), files),
    !fixed && h('label', { class: 'row small', style: 'margin-bottom:10px' }, later, t('publishLater')), !fixed && whenBox,
  ], () => run(async () => {
    const fd = new FormData();
    const [kind, id] = aud.value.split(':');
    fd.set('title', title.value); fd.set('body', body.value);
    fd.set('campus_id', kind === 'campus' ? id : '0'); fd.set('class_id', kind === 'class' ? id : '0');
    if (!fixed && later.checked) {
      if (!when.value) throw new ApiError(t('pickTime'));
      fd.set('publish_at', when.value);
    }
    remove.forEach((fid) => fd.append('remove_file', fid));
    for (const f of files.files) fd.append('file', f.type.startsWith('image/') ? new File([await shrinkImage(f)], f.name, { type: 'image/jpeg' }) : f);
    const r = await api(n ? 'PUT' : 'POST', n ? `/api/announcements/${n.id}` : '/api/announcements', fd);
    toast(n ? t('saved') : r.scheduled ? t('scheduledOk') : t('published'));
    done();
  }), n ? t('save') : t('publish'));
}

VIEWS.notices = async (main) => {
  const u = state.me.user;
  const list = await api('GET', '/api/announcements');
  const rerender = () => VIEWS.notices(main);
  main.replaceChildren(h('div', { class: 'stack' },
    h('div', { class: 'row' }, h('h2', { class: 'grow', style: 'margin:0' }, t('noticesTitle')),
      u.role === 'admin' && h('button', { class: 'btn primary', onclick: () => noticeDialog(null, rerender) }, t('newNotice'))),
    list.length ? list.map((n) => h('section', { class: 'card' + (n.read ? '' : ' unread-card') }, noticeItem(n, u.role === 'admin', rerender))) : h('div', { class: 'card' }, emptyRow())));
  // Opening the list counts as reading the notices on it.
  const unread = list.filter((n) => !n.read).map((n) => n.id);
  if (u.role === 'parent' && unread.length) {
    const r = await api('POST', '/api/announcements/read', { ids: unread }).catch(() => null);
    if (r) { state.me.unread = r.unread; document.querySelector('.bell')?.replaceWith(bellButton()); }
  }
};

// ============ suggestions & feedback (建议反馈) ============
const FEEDBACK_CATEGORIES = ['suggestion', 'problem', 'complaint', 'other'];
const feedbackStatusBadge = (s) => h('span', { class: 'badge ' + ({ new: 'warn', in_progress: 'info', resolved: 'ok' }[s]) }, t('fb_' + s));
const feedbackImages = (f) => f.images.length > 0 && h('div', { class: 'pgrid', style: 'margin-top:6px' }, f.images.map((iid) =>
  h('a', { class: 'pthumb', href: `/api/feedback/${f.id}/images/${iid}`, target: '_blank', rel: 'noopener' }, h('img', { src: `/api/feedback/${f.id}/images/${iid}`, alt: '', loading: 'lazy' }))));

async function myFeedbackCard(rerender) {
  const list = await api('GET', '/api/feedback').catch(() => []);
  return h('section', { class: 'card' },
    h('div', { class: 'row' }, h('h3', { class: 'grow', style: 'margin:0' }, '💬 ', t('feedbackTitle')),
      h('button', { class: 'btn sm primary', onclick: () => feedbackDialog(rerender) }, t('feedbackNew'))),
    list.length ? h('ul', { class: 'list' }, list.map((f) => h('li', {},
      h('div', { class: 'row' }, h('span', { class: 'badge' }, t('fbc_' + f.category)), h('span', { class: 'grow muted small' }, f.campus_name, ' · ', dateTime(f.created_at)), feedbackStatusBadge(f.status)),
      h('div', { class: 'pre small', style: 'margin-top:4px' }, f.body),
      feedbackImages(f),
      f.reply && h('div', { class: 'reply' }, h('b', {}, t('schoolReply'), '：'), h('span', { class: 'pre' }, f.reply), h('div', { class: 'muted small' }, f.handled_by, ' · ', dateTime(f.handled_at)))))) :
      h('p', { class: 'small muted', style: 'margin:8px 0 0' }, t('feedbackHint')));
}

function feedbackDialog(done) {
  const children = state.me.children || [];
  const campuses = [...new Map(children.map((c) => [c.campus_id, c.campus_name])).entries()];
  const cat = h('select', {}, FEEDBACK_CATEGORIES.map((c) => h('option', { value: c }, t('fbc_' + c))));
  const school = campuses.length > 1 && h('select', {}, campuses.map(([id, name]) => h('option', { value: id }, name)));
  const body = h('textarea', { required: true, maxlength: 2000, placeholder: t('feedbackBodyPh') });
  const images = h('input', { type: 'file', accept: 'image/*', multiple: true });
  const contact = h('input', { type: 'text', maxlength: 100, value: state.me.user.phone || '', placeholder: t('forgotContactPh') });
  openDialog(t('feedbackNew'), [
    field(t('fbCategory'), cat), school && field(t('fbSchool'), school),
    field(t('fbBody'), body), field(t('fbImages'), images), field(t('fbContact'), contact),
  ], () => run(async () => {
    if (images.files.length > 3) throw new ApiError(t('err:at most 3 photos'));
    const fd = new FormData();
    fd.set('category', cat.value); fd.set('body', body.value); fd.set('contact', contact.value);
    if (school) fd.set('campus_id', school.value);
    for (const f of images.files) fd.append('image', await shrinkImage(f), 'photo.jpg');
    await api('POST', '/api/feedback', fd);
    done();
  }, t('feedbackSent')), t('forgotSend'));
}

// ============ me ============
VIEWS.me = async (main) => {
  const u = state.me.user;
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

  const myName = h('input', { type: 'text', required: true, value: u.name });
  const myPhone = h('input', { type: 'tel', value: u.phone });
  const myEmail = h('input', { type: 'email', value: u.email || '' });
  const detailsForm = h('form', { class: 'card', onsubmit: async (e) => {
    e.preventDefault();
    if (await run(() => api('PUT', '/api/me', { name: myName.value, phone: myPhone.value, email: myEmail.value }), t('saved'))) {
      Object.assign(state.me.user, { name: myName.value.trim(), phone: myPhone.value.trim(), email: myEmail.value.trim() });
      render();
    }
  } }, h('h3', {}, t('myDetails')), field(t('name'), myName),
  h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('phone'), myPhone)), h('div', { class: 'grow' }, field(t('email'), myEmail))),
  h('p', { class: 'small muted', style: 'margin-top:0' }, t('emailHint')),
  h('button', { class: 'btn primary block' }, t('save')));
  const oldPw = h('input', { type: 'password', required: true, autocomplete: 'current-password' });
  const newPw = h('input', { type: 'password', required: true, minlength: 6, autocomplete: 'new-password' });
  main.replaceChildren(h('div', { class: 'stack' },
    h('section', { class: 'card' },
      h('h2', { style: 'margin:0' }, u.name),
      h('div', { class: 'muted' }, t('role_' + u.role), ' · ', u.username),
      h('div', { class: 'muted small' }, u.role === 'parent' ? '' : (u.campus_name || t('allSchools')))),
    h('section', { class: 'card stack' },
      h('h3', {}, '🔔 ', t('notifications')),
      h('div', {}, h('div', { class: 'small muted', style: 'margin-bottom:6px' }, t('pushTitle')), pushPart)),
    h('section', { class: 'card' },
      h('h3', {}, t('language')),
      h('div', { class: 'seg' }, [['zh', '中文'], ['en', 'English']].map(([k, label]) =>
        h('button', { class: lang === k ? 'on' : null, onclick: () => { lang = k; try { localStorage.setItem('lang', k); } catch (e) { /* ignore */ } render(); } }, label)))),
    u.role !== 'admin' && await myFeedbackCard(rerender),
    detailsForm,
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

