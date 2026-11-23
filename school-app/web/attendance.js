'use strict';

// ============ scan ============
// Arrival before noon, departure after, unless the teacher picks otherwise.
let scanMode = null;
const modeLabel = (m) => t(m === 'out' ? 'modeOut' : 'modeIn');

VIEWS.scan = async (main) => {
  if (!scanMode) scanMode = Number(nowHHMM().slice(0, 2)) < 12 ? 'in' : 'out';
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
    await submit('/api/attendance/scan', { code: code.data, mode: scanMode });
    busy = false;
  }

  async function submit(path, body) {
    try {
      let r = await api('POST', path, body);
      if (r.results) r = r.results[0];
      showResult(r);
    } catch (e) {
      beep(false);
      result.replaceChildren(h('div', { class: 'result bad' }, h('div', { class: 'big' }, e.message)));
    }
  }

  function showResult(r) {
    const out = r.mode === 'out';
    const kind = r.already ? 'info' : out ? 'ok' : (r.late ? 'warn' : 'ok');
    beep(true);
    if (navigator.vibrate) navigator.vibrate(r.already ? [60, 60, 60] : 120);
    result.replaceChildren(h('div', { class: 'result ' + kind },
      h('div', { class: 'small' }, modeLabel(r.mode), ' · ', r.already ? t('alreadyDone') : t('recordedOk')),
      h('div', { class: 'name' }, (r.already ? '' : '✓ ') + r.student.name),
      h('div', {}, r.student.class_name, ' · ', r.student.student_no),
      h('div', { class: 'big' }, out ? hhmm(r.checkout_at) : [hhmm(r.checkin_at), ' · ', r.late ? t('late') : t('onTime')])));
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
        h('button', { class: 'btn sm primary', onclick: () => { submit('/api/attendance/mark', { mode: scanMode, entries: [{ student_id: s.id }] }); search.value = ''; renderMatches(); window.scrollTo(0, 0); } }, modeLabel(scanMode))))));
  }

  main.replaceChildren(h('div', { class: 'stack' },
    h('div', { class: 'row' }, h('h2', { class: 'grow', style: 'margin:0' }, t('scanTitle')), toggle),
    h('div', { class: 'seg mode-seg' }, ['in', 'out'].map((m) => h('button', { class: scanMode === m ? 'on' : null, onclick: () => {
      scanMode = m; lastCode = '';
      main.querySelectorAll('.mode-seg button').forEach((b, i) => b.classList.toggle('on', ['in', 'out'][i] === m));
      result.replaceChildren();
    } }, m === 'in' ? '🌅 ' : '🏠 ', modeLabel(m)))),
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


// ============ attendance: roll call (点名) ============
const attFilter = { date: '', campusId: '', classId: '', mode: 'in' };

VIEWS.attendance = async (main) => {
  const rerender = () => VIEWS.attendance(main);
  const [classes, campuses] = await Promise.all([api('GET', '/api/classes'), allCampuses() ? api('GET', '/api/campuses') : []]);
  if (!attFilter.classId && state.me.user.role === 'teacher') {
    const mine = classes.find((c) => c.teacher_id === state.me.user.id);
    if (mine) attFilter.classId = String(mine.id);
  }
  if (!attFilter.date && Number(nowHHMM().slice(0, 2)) >= 12) attFilter.mode = 'out';
  const visibleClasses = classes.filter((c) => !attFilter.campusId || String(c.campus_id) === attFilter.campusId);
  const q = new URLSearchParams();
  if (attFilter.date) q.set('date', attFilter.date);
  if (attFilter.classId) q.set('class_id', attFilter.classId);
  else if (attFilter.campusId) q.set('campus_id', attFilter.campusId);
  const data = await api('GET', '/api/attendance?' + q);
  attFilter.date = data.date;
  const isToday = data.date === data.today;
  const rows = data.rows;
  const mode = attFilter.mode;
  const key = mode === 'in' ? 'checkin_at' : 'checkout_at';
  const present = rows.filter((r) => r.checkin_at).length;
  const late = rows.filter((r) => r.checkin_at && r.late).length;
  const left = rows.filter((r) => r.checkout_at).length;

  // Roll call: tick students, time defaults to now and can be changed before submitting.
  const picked = new Map(); // student id -> "HH:MM"
  const submitBtn = h('button', { class: 'btn primary block', disabled: true, onclick: async () => {
    if (!picked.size) return;
    if (!confirm(t('confirmRollCall', { n: picked.size, mode: modeLabel(mode) }))) return;
    submitBtn.disabled = true;
    const entries = [...picked].map(([id, time]) => ({ student_id: id, time }));
    const ok = await run(async () => {
      const r = await api('POST', '/api/attendance/mark', { mode, entries });
      toast(t('rollCallDone', { n: r.recorded }));
    });
    submitBtn.disabled = false;
    if (ok) rerender();
  } });
  const updateSubmit = () => {
    submitBtn.disabled = !picked.size;
    submitBtn.textContent = t('submitRollCall', { mode: modeLabel(mode), n: picked.size });
  };
  updateSubmit();

  const rowFor = (r) => {
    const done = r[key];
    let right;
    if (done) {
      right = h('div', { class: 'att-done' },
        h('span', { class: 'att-time' }, hhmm(done)),
        mode === 'in' && h('span', { class: 'badge ' + (r.late ? 'warn' : 'ok') }, r.late ? t('late') : t('onTime')),
        r.edited && h('span', { class: 'badge', title: t('edited') }, '✎'),
        h('button', { class: 'btn sm', onclick: () => editAttendance(r, data.date, rerender) }, t('edit')));
    } else if (isToday) {
      const time = h('input', { type: 'time', class: 'att-clock', value: nowHHMM(), disabled: true, 'aria-label': t('time'),
        onchange: (e) => { if (picked.has(r.id)) picked.set(r.id, e.target.value); } });
      const box = h('input', { type: 'checkbox', class: 'att-check', 'aria-label': r.name, onchange: (e) => {
        if (e.target.checked) { if (!time.value) time.value = nowHHMM(); picked.set(r.id, time.value); } else picked.delete(r.id);
        time.disabled = !e.target.checked;
        e.target.closest('li').classList.toggle('picked', e.target.checked);
        updateSubmit();
      } });
      right = h('div', { class: 'att-pick' }, time, box);
    } else {
      right = h('div', { class: 'att-done' },
        h('span', { class: 'badge ' + (mode === 'in' ? 'bad' : '') }, mode === 'in' ? t('absent') : '—'),
        h('button', { class: 'btn sm', onclick: () => editAttendance(r, data.date, rerender) }, t('edit')));
    }
    const sub = mode === 'out' && !r.checkin_at ? t('noArrival') : (r[mode === 'in' ? 'in_by' : 'out_by'] || '');
    return h('li', { class: 'att-row' },
      h('button', { class: 'linkish grow', onclick: () => openStudent(r.id) },
        h('span', { class: 'att-name' }, r.name),
        h('span', { class: 'muted small' }, (attFilter.classId ? '' : className({ campus_name: r.campus_name, name: r.class_name }) + ' · '), sub)),
      right);
  };

  const selectAll = isToday && h('button', { class: 'btn sm', onclick: () => {
    main.querySelectorAll('.att-check').forEach((b) => { if (!b.checked) { b.checked = true; b.dispatchEvent(new Event('change')); } });
  } }, t('selectAll'));

  main.replaceChildren(h('div', { class: 'stack' },
    h('h2', {}, t('attendanceTitle')),
    h('div', { class: 'row' },
      h('input', { type: 'date', class: 'grow', value: data.date, max: data.today, onchange: (e) => { attFilter.date = e.target.value; rerender(); } }),
      allCampuses() && campuses.length > 1 && h('select', { class: 'grow', onchange: (e) => { attFilter.campusId = e.target.value; attFilter.classId = ''; rerender(); } }, campusOptions(campuses, attFilter.campusId, t('allSchools'))),
      h('select', { class: 'grow', onchange: (e) => { attFilter.classId = e.target.value; rerender(); } }, classOptions(visibleClasses, attFilter.classId, t('allClasses')))),
    h('div', { class: 'seg' }, ['in', 'out'].map((m) => h('button', { class: mode === m ? 'on' : null, onclick: () => { attFilter.mode = m; rerender(); } }, m === 'in' ? '🌅 ' : '🏠 ', t(m === 'in' ? 'rollIn' : 'rollOut')))),
    h('div', { class: 'stats stats4' },
      h('div', { class: 'stat' }, h('b', {}, present, h('span', {}, ' / ', rows.length)), h('span', {}, t('present'))),
      h('div', { class: 'stat' }, h('b', { class: 'bad-text' }, rows.length - present), h('span', {}, t('absent'))),
      h('div', { class: 'stat' }, h('b', { class: 'warn-text' }, late), h('span', {}, t('late'))),
      h('div', { class: 'stat' }, h('b', {}, left), h('span', {}, t('leftCount')))),
    h('div', { class: 'row' },
      h('span', { class: 'muted small grow' }, isToday ? t('rollHint') : t('pastDayHint')),
      selectAll,
      h('button', { class: 'btn sm', onclick: () => exportCsv(data.date, rows) }, t('exportCsv'))),
    h('section', { class: 'card' }, rows.length ? h('ul', { class: 'list' }, rows.map(rowFor)) : emptyRow()),
    isToday && h('div', { class: 'sticky-submit' }, submitBtn)));
};

// Correct a submitted time. A reason is required; the change is kept in the student's history.
function editAttendance(r, date, done) {
  const inT = h('input', { type: 'time', value: hhmm(r.checkin_at) });
  const outT = h('input', { type: 'time', value: hhmm(r.checkout_at) });
  const reason = h('input', { type: 'text', required: true, maxlength: 200, placeholder: t('reasonPh') });
  openDialog(t('editTimes', { name: r.name, date }), [
    field(t('arrivalTime'), inT), field(t('departureTime'), outT),
    h('p', { class: 'small muted', style: 'margin-top:-4px' }, t('clearHint')),
    field(t('editReason'), reason),
  ], async () => run(async () => {
    await api('PUT', `/api/attendance/${r.id}/${date}`, { checkin: inT.value, checkout: outT.value, reason: reason.value });
    done();
  }, t('saved')));
}

function exportCsv(date, rows) {
  const esc = (v) => '"' + String(v ?? '').replace(/"/g, '""') + '"';
  const lines = [[t('studentNo'), t('studentName'), t('campus'), t('class'), t('arrivalTime'), t('departureTime'), t('late'), t('recordedBy')].map(esc).join(',')];
  for (const r of rows) {
    lines.push([r.student_no, r.name, r.campus_name, r.class_name, hhmm(r.checkin_at), hhmm(r.checkout_at), r.late ? t('late') : '', [r.in_by, r.out_by].filter(Boolean).join(' / ')].map(esc).join(','));
  }
  const blob = new Blob(['﻿' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' });
  const a = h('a', { href: URL.createObjectURL(blob), download: `attendance-${date}.csv` });
  document.body.append(a); a.click(); a.remove();
}

// ============ student page (学生详情) ============
let studentMonth = '';
VIEWS.student = async (main) => {
  const u = state.me.user;
  const id = state.studentId;
  const rerender = () => VIEWS.student(main);
  if (!studentMonth) studentMonth = todayStr().slice(0, 7);
  const [detail, att, photos, fees] = await Promise.all([
    api('GET', `/api/students/${id}`),
    api('GET', `/api/attendance/student/${id}?month=${studentMonth}`),
    api('GET', `/api/photos?student_id=${id}&limit=9`),
    u.role !== 'teacher' ? api('GET', `/api/fees?student_id=${id}`) : [],
  ]);
  const s = detail.student, p = detail.profile;
  const info = (label, value, cls) => value && h('div', { class: 'kv' }, h('span', { class: 'muted' }, label), h('span', { class: cls || '' }, value));
  const tel = (phone) => phone && h('a', { href: 'tel:' + phone.replace(/[^\d+]/g, '') }, phone);
  const back = h('button', { class: 'btn sm', onclick: () => { state.studentId = 0; go(state.backTab || TABS[u.role][0][0]); } }, '‹ ', t('back'));

  const months = [];
  for (let i = 0; i < 12; i++) {
    const d = new Date(todayStr().slice(0, 7) + '-01T00:00:00');
    d.setMonth(d.getMonth() - i);
    months.push(d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0'));
  }
  const fieldName = (f) => t(f === 'checkin' ? 'arrivalTime' : 'departureTime');

  main.replaceChildren(h('div', { class: 'stack' },
    h('div', { class: 'row' }, back),
    h('section', { class: 'card' },
      h('h2', { style: 'margin:0' }, s.name),
      h('div', { class: 'muted' }, s.campus_name, ' · ', s.class_name, ' · ', s.student_no),
      detail.class_teacher && h('div', { class: 'muted small' }, t('classTeacher'), ': ', detail.class_teacher)),
    h('section', { class: 'card' },
      h('h3', {}, t('profile')),
      p.health_notes && h('div', { class: 'alert' }, '⚠️ ', p.health_notes),
      info(t('birthDate'), p.birth_date),
      info(t('gender'), p.gender && t('gender_' + p.gender)),
      info(t('emergencyContact'), p.emergency_name && [p.emergency_name, ' ', tel(p.emergency_phone)]),
      info(t('notes'), p.notes),
      !p.birth_date && !p.gender && !p.health_notes && !p.emergency_name && !p.notes && h('div', { class: 'muted small' }, t('noProfile'))),
    u.role !== 'parent' && h('section', { class: 'card' },
      h('h3', {}, t('parents')),
      (s.parents || []).length ? h('ul', { class: 'list' }, s.parents.map((x) => h('li', {},
        h('b', {}, x.name), h('div', { class: 'small' }, tel(x.phone), x.email ? [' · ', h('a', { href: 'mailto:' + x.email }, x.email)] : '')))) : emptyRow()),
    h('section', { class: 'card' },
      h('div', { class: 'row', style: 'margin-bottom:8px' }, h('h3', { class: 'grow', style: 'margin:0' }, t('attendanceRecords')),
        h('select', { style: 'width:auto', onchange: (e) => { studentMonth = e.target.value; rerender(); } }, months.map((m) => h('option', { value: m, selected: m === studentMonth }, m)))),
      att.records.length ? h('ul', { class: 'list' }, att.records.map((r) => h('li', {},
        h('div', { class: 'row' },
          h('b', { class: 'grow' }, r.date),
          h('span', {}, '🌅 ', hhmm(r.checkin_at) || '—'), r.checkin_at && r.late && h('span', { class: 'badge warn' }, t('late')),
          h('span', {}, '🏠 ', hhmm(r.checkout_at) || '—')),
        (r.in_by || r.out_by) && h('div', { class: 'muted small' }, t('recordedBy'), ': ', [r.in_by, r.out_by].filter((v, i, a) => v && a.indexOf(v) === i).join(' / ')),
        r.edits.map((e) => h('div', { class: 'edit-note small' }, '✎ ', fieldName(e.field), ' ', hhmm(e.old) || '—', ' → ', hhmm(e.new) || '—',
          ' · ', e.reason, ' · ', e.edited_by, ' ', dateTime(e.edited_at)))))) : emptyRow()),
    h('section', { class: 'card' },
      h('h3', {}, t('tab_photos')),
      photos.length ? photoGrid(photos, rerender) : emptyRow()),
    u.role !== 'teacher' && h('section', { class: 'card' },
      h('h3', {}, t('fees')),
      fees.length ? h('ul', { class: 'list' }, fees.map((f) => h('li', { class: 'row' },
        h('div', { class: 'grow' }, f.name, h('div', { class: 'muted small' }, f.due_date && t('dueOn', { d: f.due_date }))),
        h('b', {}, money(f.amount_cents)), feeBadge(f)))) : emptyRow())));
};
