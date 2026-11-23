'use strict';

// ============ manage (admin) ============
// Students (with their file), classes, accounts, fees, QR cards, campuses (top-level admin) and the alerts log.

let manageTab = 'students';
const manageFilter = { classId: '', role: '' };

VIEWS.manage = async (main) => {
  const rerender = () => VIEWS.manage(main);
  const top = !state.me.user.campus_id;
  const tabs = ['students', 'classes', 'users', 'fees', 'feedback', 'cards', 'campuses', 'notify', 'security'];
  if (!tabs.includes(manageTab)) manageTab = 'students';
  const body = [];
  await MANAGE[manageTab](body, rerender);
  main.replaceChildren(h('div', { class: 'stack' },
    h('div', { class: 'seg no-print' }, tabs.map((k) =>
      h('button', { class: manageTab === k ? 'on' : null, onclick: () => { manageTab = k; rerender(); } }, t('m_' + k)))),
    body));
};

const MANAGE = {};

// ---------- students ----------
function studentDialog(s, classes, done) {
  const p = s ? s.profile : {};
  const st = s ? s.student : {};
  const v = (x) => x || '';
  const name = h('input', { type: 'text', required: true, value: v(st.name) });
  const no = h('input', { type: 'text', required: true, value: v(st.student_no) });
  const cls = h('select', { required: true }, h('option', { value: '' }, '—'), classOptions(classes, st.class_id || manageFilter.classId));
  const birth = h('input', { type: 'date', value: v(p.birth_date) });
  const gender = h('select', {}, ['', 'M', 'F'].map((g) => h('option', { value: g, selected: g === v(p.gender) }, g ? t('gender_' + g) : '—')));
  const health = h('textarea', { style: 'min-height:70px', placeholder: t('healthPh') }, v(p.health_notes));
  const eName = h('input', { type: 'text', value: v(p.emergency_name) });
  const ePhone = h('input', { type: 'tel', value: v(p.emergency_phone) });
  const notes = h('textarea', { style: 'min-height:70px' }, v(p.notes));
  openDialog(s ? t('editStudent') : t('newStudent'), [
    field(t('studentName'), name),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('studentNo'), no)), h('div', { class: 'grow' }, field(t('class'), cls))),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('birthDate'), birth)), h('div', { class: 'grow' }, field(t('gender'), gender))),
    field(t('healthNotes'), health),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('emergencyName'), eName)), h('div', { class: 'grow' }, field(t('emergencyPhone'), ePhone))),
    field(t('notes'), notes),
  ], () => run(async () => {
    const req = { name: name.value, student_no: no.value, class_id: Number(cls.value) || 0, birth_date: birth.value, gender: gender.value,
      health_notes: health.value, emergency_name: eName.value, emergency_phone: ePhone.value, notes: notes.value };
    await api(s ? 'PUT' : 'POST', s ? `/api/students/${st.id}` : '/api/students', req);
    done();
  }, t('saved')));
}

MANAGE.students = async (body, rerender) => {
  const [classes, users] = await Promise.all([api('GET', '/api/classes'), api('GET', '/api/users?role=parent')]);
  const students = await api('GET', '/api/students' + (manageFilter.classId ? '?class_id=' + manageFilter.classId : ''));
  body.push(h('div', { class: 'row' },
    h('select', { class: 'grow', onchange: (e) => { manageFilter.classId = e.target.value; rerender(); } }, classOptions(classes, manageFilter.classId, t('allClasses'))),
    h('button', { class: 'btn', onclick: () => importDialog(rerender) }, '📥 ', t('importExcel')),
    h('button', { class: 'btn primary', onclick: () => studentDialog(null, classes, rerender) }, t('newStudent'))));
  body.push(h('section', { class: 'card' }, students.length ? h('ul', { class: 'list' }, students.map((s) => h('li', {},
    h('div', { class: 'row' },
      h('button', { class: 'linkish grow', onclick: () => openStudent(s.id) }, h('b', {}, s.name),
        h('span', { class: 'muted small' }, className(s), ' · ', s.student_no)),
      h('button', { class: 'btn sm', onclick: async () => studentDialog(await api('GET', `/api/students/${s.id}`), classes, rerender) }, t('edit')),
      h('button', { class: 'btn sm', onclick: async () => { if (confirm(t('resetQRConfirm')) && await run(() => api('POST', `/api/students/${s.id}/qr/reset`), t('saved'))) rerender(); } }, t('resetQR')),
      h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/students/${s.id}`), t('deleted'))) rerender(); } }, t('delete'))),
    h('div', { class: 'small', style: 'margin-top:6px' },
      h('span', { class: 'muted' }, t('parents'), ': '),
      (s.parents || []).map((p) => h('span', { class: 'chip' }, p.name,
        h('button', { 'aria-label': t('unlink'), onclick: async () => { if (confirm(t('unlinkConfirm', { p: p.name, s: s.name })) && await run(() => api('DELETE', `/api/students/${s.id}/parents/${p.id}`))) rerender(); } }, '×'))),
      h('select', { class: 'inline-select', onchange: async (e) => {
        if (e.target.value && await run(() => api('POST', `/api/students/${s.id}/parents`, { parent_id: Number(e.target.value) }), t('saved'))) rerender();
      } }, h('option', { value: '' }, t('linkParent')),
      users.filter((p) => !(s.parents || []).some((x) => x.id === p.id)).map((p) => h('option', { value: p.id }, p.name + (p.phone ? ` (${p.phone})` : '')))))))) : emptyRow()));
};

// ---------- bulk import from Excel ----------
// Step 1: upload → preview (nothing saved). Step 2: confirm → import all rows, then download the new parents' first passwords.
function importDialog(done) {
  const file = h('input', { type: 'file', accept: '.xlsx,.csv,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet,text/csv' });
  const out = h('div', { 'aria-live': 'polite' });
  const actions = h('div', { class: 'row' });
  let imported = false;
  const send = async (dryRun) => {
    if (!file.files[0]) { toast(t('err:please choose a file')); return null; }
    const fd = new FormData();
    fd.set('file', file.files[0]);
    out.replaceChildren(h('div', { class: 'muted' }, t('loading')));
    try { return await api('POST', '/api/import/students' + (dryRun ? '?dry_run=1' : ''), fd); }
    catch (e) { out.replaceChildren(h('div', { class: 'alert' }, e.message)); return null; }
  };
  const summary = (sm) => h('div', { class: 'import-sum' },
    [['new', 'ok'], ['update', 'info'], ['error', 'bad'], ['new_parents', ''], ['linked_parents', ''], ['new_classes', '']].map(([k, cls]) =>
      h('span', { class: 'badge ' + cls }, t('imp_' + k), ' ', sm[k] || 0)));
  const rowList = (rows) => h('ul', { class: 'list import-rows' }, rows.map((r) => h('li', { class: r.action === 'error' ? 'imp-err' : null },
    h('div', { class: 'row' },
      h('span', { class: 'muted small' }, '#', r.row),
      h('b', { class: 'grow' }, r.student_no, ' ', r.name),
      h('span', { class: 'badge ' + ({ new: 'ok', update: 'info', error: 'bad' }[r.action]) }, t('imp_' + r.action))),
    h('div', { class: 'small muted' }, r.campus && allCampuses() ? r.campus + ' · ' : '', r.class, r.new_class ? ' (' + t('imp_newClassTag') + ')' : '',
      r.parents.length ? ' · ' + r.parents.map((p) => `${p.name || p.phone} (${t(p.action === 'new' ? 'imp_parentNew' : 'imp_parentLink')})`).join('、') : ''),
    r.errors.map((e) => h('div', { class: 'small bad-text' }, '⚠ ', e)))));
  const preview = async () => {
    const r = await send(true);
    if (!r) return;
    const errors = r.summary.error || 0;
    const count = (r.summary.new || 0) + (r.summary.update || 0);
    out.replaceChildren(summary(r.summary),
      errors ? h('div', { class: 'alert' }, t('imp_fixErrors', { n: errors })) : count ? h('p', { class: 'small' }, t('imp_ready', { n: count })) : h('p', { class: 'muted' }, t('none')),
      rowList(r.rows));
    actions.replaceChildren(...(!errors && count > 0 ? [h('button', { type: 'button', class: 'btn primary block', onclick: confirmImport }, t('imp_confirm', { n: count }))] : []));
  };
  const confirmImport = async () => {
    actions.querySelector('button').disabled = true;
    const r = await send(false);
    if (!r) return;
    if (!r.imported) { preview(); return; }
    imported = true;
    const saveAccounts = () => {
      const bin = atob(r.accounts_xlsx);
      const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
      saveBlob(new Blob([bytes], { type: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' }), `parent-accounts-${todayStr()}.xlsx`);
    };
    out.replaceChildren(h('div', { class: 'result ok' }, h('div', { class: 'big' }, '✓ ', t('imp_done', { n: (r.summary.new || 0) + (r.summary.update || 0) }))),
      summary(r.summary),
      ...(r.new_accounts > 0 ? [h('div', { class: 'alert warn-alert' }, t('imp_accountsWarn', { n: r.new_accounts }))] : []));
    actions.replaceChildren(...(r.new_accounts > 0 ? [h('button', { type: 'button', class: 'btn primary block', onclick: saveAccounts }, '⬇ ', t('imp_downloadAccounts'))] : []));
    if (r.new_accounts > 0) saveAccounts();
  };
  file.addEventListener('change', preview);
  const close = openDialog(t('importExcel'), [
    h('ol', { class: 'small', style: 'margin:0 0 12px;padding-left:20px' },
      h('li', {}, t('imp_step1'), ' ', h('a', { href: '/api/import/template.xlsx', download: 'student-import-template.xlsx' }, '⬇ ', t('imp_template'))),
      h('li', {}, t('imp_step2')),
      h('li', {}, t('imp_step3'))),
    field(t('imp_file'), file),
    out, actions,
  ], null);
  // Refresh the student list once the dialog closes after an import.
  const back = document.querySelector('.dialog-back');
  new MutationObserver((_, obs) => { if (!document.body.contains(back)) { obs.disconnect(); if (imported) done(); } }).observe(document.body, { childList: true });
  return close;
}

// ---------- classes ----------
function classDialog(c, campuses, teachers, done) {
  const name = h('input', { type: 'text', required: true, value: c ? c.name : '' });
  const campus = h('select', { required: true }, campusOptions(campuses, c ? c.campus_id : (campuses.length === 1 ? campuses[0].id : '')));
  const teacher = h('select', {}, h('option', { value: '' }, '—'), teachers.map((u) => h('option', { value: u.id, selected: c && c.teacher_id === u.id }, u.name + (u.campus_name ? ` · ${u.campus_name}` : ''))));
  openDialog(c ? t('editClass') : t('newClass'), [
    field(t('className'), name),
    allCampuses() && field(t('campus'), campus),
    field(t('classTeacher'), teacher),
  ], () => run(async () => {
    await api(c ? 'PUT' : 'POST', c ? `/api/classes/${c.id}` : '/api/classes', { name: name.value, campus_id: Number(campus.value) || 0, teacher_id: Number(teacher.value) || 0 });
    done();
  }, t('saved')));
}

MANAGE.classes = async (body, rerender) => {
  const [classes, campuses, users] = await Promise.all([api('GET', '/api/classes'), api('GET', '/api/campuses'), api('GET', '/api/users?role=teacher')]);
  body.push(h('div', { class: 'row', style: 'justify-content:flex-end' }, h('button', { class: 'btn primary', onclick: () => classDialog(null, campuses, users, rerender) }, t('newClass'))));
  body.push(h('section', { class: 'card' }, classes.length ? h('ul', { class: 'list' }, classes.map((c) => h('li', { class: 'row' },
    h('div', { class: 'grow' }, h('b', {}, className(c)), h('div', { class: 'muted small' }, t('classTeacher'), ': ', c.teacher_name || '—', ' · ', t('nStudents', { n: c.students }))),
    h('button', { class: 'btn sm', onclick: () => classDialog(c, campuses, users, rerender) }, t('edit')),
    h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/classes/${c.id}`), t('deleted'))) rerender(); } }, t('delete'))))) : emptyRow()));
};

// ---------- accounts ----------
function userDialog(u, campuses, done) {
  const top = allCampuses();
  const roles = top ? ['parent', 'teacher', 'admin'] : ['parent', 'teacher']; // only the super admin adds admins
  const role = h('select', {}, roles.map((r) => h('option', { value: r, selected: u ? u.role === r : r === 'parent' }, t('role_' + r))));
  const name = h('input', { type: 'text', required: true, value: u ? u.name : '' });
  const username = h('input', { type: 'text', required: true, autocapitalize: 'none', pattern: '[a-zA-Z0-9._\\-]{3,32}' });
  const phone = h('input', { type: 'tel', value: u ? u.phone : '' });
  const email = h('input', { type: 'email', value: u ? u.email : '' });
  const pw = h('input', { type: 'text', required: true, minlength: 8, autocomplete: 'new-password' });
  const campus = h('select', {}, h('option', { value: '' }, t('noCampus')), campusOptions(campuses, u ? u.campus_id : ''));
  const campusHint = h('p', { class: 'small muted', style: 'margin-top:-4px' });
  const syncHint = () => { campusHint.textContent = role.value === 'admin' ? t('adminCampusHint') : role.value === 'teacher' ? t('teacherCampusHint') : ''; };
  role.addEventListener('change', syncHint);
  syncHint();
  openDialog(u ? t('editUser') : t('newUser'), [
    !u && field(t('role'), role),
    field(t('name'), name),
    !u && field(t('username'), username),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('phone'), phone)), h('div', { class: 'grow' }, field(t('email'), email))),
    top && field(t('campus'), campus), top && campusHint,
    !u && field(t('password'), pw),
  ], () => run(async () => {
    const req = { role: role.value, name: name.value, username: username.value, phone: phone.value, email: email.value, password: pw.value, campus_id: Number(campus.value) || 0 };
    if (u) await api('PUT', `/api/users/${u.id}`, { ...req, campus_id: top ? req.campus_id : u.campus_id });
    else await api('POST', '/api/users', req);
    done();
  }, t('saved')));
}

MANAGE.users = async (body, rerender) => {
  const [users, campuses] = await Promise.all([api('GET', '/api/users' + (manageFilter.role ? '?role=' + manageFilter.role : '')), api('GET', '/api/campuses')]);
  const me = state.me.user;
  body.push(h('div', { class: 'row' },
    h('select', { class: 'grow', onchange: (e) => { manageFilter.role = e.target.value; rerender(); } },
      h('option', { value: '' }, t('allRoles')), ['parent', 'teacher', 'admin'].map((r) => h('option', { value: r, selected: manageFilter.role === r }, t('role_' + r)))),
    h('button', { class: 'btn primary', onclick: () => userDialog(null, campuses, rerender) }, t('newUser'))));
  body.push(h('section', { class: 'card' }, users.length ? h('ul', { class: 'list' }, users.map((u) => h('li', { class: u.disabled ? 'is-disabled' : null },
    h('div', { class: 'row' },
      h('div', { class: 'grow' },
        h('b', {}, u.name), ' ', h('span', { class: 'badge' }, t('role_' + u.role)),
        u.role !== 'parent' && h('span', { class: 'badge info', style: 'margin-left:4px' }, u.campus_name || t('allSchools')),
        u.disabled && h('span', { class: 'badge bad', style: 'margin-left:4px' }, t('disabled')),
        u.two_factor && h('span', { class: 'badge ok', style: 'margin-left:4px', title: t('twoStepTitle') }, '🔐'),
        h('div', { class: 'muted small' }, u.username, u.phone ? ' · ' + u.phone : '', u.email ? ' · ' + u.email : ''),
        u.children && h('div', { class: 'muted small' }, t('children'), ': ', u.children.map((c) => c.name).join('、')))),
    (allCampuses() || u.role !== 'admin') && h('div', { class: 'row', style: 'margin-top:6px;justify-content:flex-end' },
      h('button', { class: 'btn sm', onclick: () => userDialog(u, campuses, rerender) }, t('edit')),
      h('button', { class: 'btn sm', onclick: async () => {
        const p = prompt(t('newPwPrompt'));
        if (p) await run(() => api('POST', `/api/users/${u.id}/password`, { password: p }), t('saved'));
      } }, t('resetPw')),
      u.two_factor && u.id !== me.id && h('button', { class: 'btn sm', onclick: async () => {
        if (confirm(t('tfResetConfirm', { n: u.name })) && await run(() => api('POST', `/api/users/${u.id}/2fa/reset`), t('saved'))) rerender();
      } }, t('tfReset')),
      u.id !== me.id && h('button', { class: 'btn sm', onclick: async () => {
        if (!u.disabled && !confirm(t('disableConfirm', { n: u.name }))) return;
        if (await run(() => api('POST', `/api/users/${u.id}/disabled`, { disabled: !u.disabled }), t('saved'))) rerender();
      } }, u.disabled ? t('enable') : t('disable')),
      u.id !== me.id && h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/users/${u.id}`), t('deleted'))) rerender(); } }, t('delete')))))) : emptyRow()));
};

// ---------- fees ----------
MANAGE.fees = (body, rerender) => manageFees(body, rerender);

// ---------- feedback ----------
let feedbackFilter = '';
MANAGE.feedback = async (body, rerender) => {
  const list = await api('GET', '/api/feedback' + (feedbackFilter ? '?status=' + feedbackFilter : ''));
  body.push(h('div', { class: 'seg' }, ['', 'new', 'in_progress', 'resolved'].map((s) =>
    h('button', { class: feedbackFilter === s ? 'on' : null, onclick: () => { feedbackFilter = s; rerender(); } }, s ? t('fb_' + s) : t('st_all')))));
  if (!list.length) body.push(h('div', { class: 'card' }, emptyRow()));
  for (const f of list) {
    body.push(h('section', { class: 'card' },
      h('div', { class: 'row' }, h('span', { class: 'badge' }, t('fbc_' + f.category)), h('b', { class: 'grow' }, f.user_name, ' ', h('span', { class: 'muted small' }, t('role_' + f.user_role))), feedbackStatusBadge(f.status)),
      h('div', { class: 'muted small' }, allCampuses() ? f.campus_name + ' · ' : '', f.children && f.children + ' · ', dateTime(f.created_at)),
      h('p', { class: 'pre', style: 'margin:8px 0' }, f.body),
      feedbackImages(f),
      (f.contact || f.user_phone) && h('div', { class: 'small' }, t('fbContact'), ': ', f.contact || f.user_phone),
      f.reply && h('div', { class: 'reply' }, h('b', {}, t('schoolReply'), '：'), h('span', { class: 'pre' }, f.reply), h('div', { class: 'muted small' }, f.handled_by, ' · ', dateTime(f.handled_at))),
      h('div', { class: 'row', style: 'justify-content:flex-end;margin-top:8px' },
        h('button', { class: 'btn sm primary', onclick: () => {
          const reply = h('textarea', { maxlength: 2000 }, f.reply);
          const status = h('select', {}, ['new', 'in_progress', 'resolved'].map((s) => h('option', { value: s, selected: s === (f.status === 'new' ? 'resolved' : f.status) }, t('fb_' + s))));
          openDialog(t('replyTitle'), [h('p', { class: 'small muted pre', style: 'margin-top:0' }, truncateText(f.body, 200)), field(t('fbReply'), reply), field(t('status'), status),
            h('p', { class: 'small muted' }, t('replyHint'))],
          () => run(async () => { await api('POST', `/api/feedback/${f.id}/reply`, { status: status.value, reply: reply.value }); rerender(); }, t('saved')));
        } }, t('fbHandle')))));
  }
};

const truncateText = (s, n) => (s.length > n ? s.slice(0, n) + '…' : s);

// ---------- QR cards ----------
MANAGE.cards = async (body, rerender) => {
  const classes = await api('GET', '/api/classes');
  const students = await api('GET', '/api/students' + (manageFilter.classId ? '?class_id=' + manageFilter.classId : ''));
  body.push(h('div', { class: 'row no-print' },
    h('select', { class: 'grow', onchange: (e) => { manageFilter.classId = e.target.value; rerender(); } }, classOptions(classes, manageFilter.classId, t('allClasses'))),
    h('button', { class: 'btn primary', onclick: () => window.print() }, t('print'))));
  body.push(h('p', { class: 'muted small no-print', style: 'margin:0' }, t('cardsHint')));
  body.push(students.length ? h('div', { class: 'cards' }, students.map((s) => h('div', { class: 'qrcard' },
    h('img', { src: `/api/students/${s.id}/qr.png`, alt: s.name, loading: 'lazy' }),
    h('div', { class: 'n' }, s.name),
    h('div', { class: 'c' }, s.campus_name),
    h('div', { class: 'c' }, s.class_name, ' · ', s.student_no)))) : emptyRow());
};

// ---------- campuses (top-level admin) ----------
MANAGE.campuses = async (body, rerender) => {
  const top = allCampuses();
  const campuses = await api('GET', '/api/campuses');
  const edit = (c) => {
    const v = (k) => (c ? c[k] : '');
    const name = h('input', { type: 'text', required: true, value: v('name'), readonly: !top });
    const prefix = h('input', { type: 'text', maxlength: 8, value: v('receipt_prefix'), placeholder: 'TTM', style: 'text-transform:uppercase' });
    const reg = h('input', { type: 'text', value: v('reg_no'), placeholder: t('regNoPh') });
    const addr = h('textarea', { style: 'min-height:70px' }, v('address'));
    const phone = h('input', { type: 'tel', value: v('phone') });
    const email = h('input', { type: 'email', value: v('email') });
    const about = h('textarea', { style: 'min-height:80px', placeholder: t('aboutPh') }, v('about'));
    const whatsapp = h('input', { type: 'tel', value: v('whatsapp'), placeholder: '012-3456789' });
    const hours = h('textarea', { style: 'min-height:60px', placeholder: t('hoursPh') }, v('hours'));
    openDialog(c ? t('editCampus') : t('newCampus'), [
      field(t('campusName'), name),
      h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('receiptPrefix'), prefix)), h('div', { class: 'grow' }, field(t('regNo'), reg))),
      field(t('address'), addr),
      h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('phone'), phone)), h('div', { class: 'grow' }, field(t('email'), email))),
      h('p', { class: 'small muted' }, t('campusReceiptHint')),
      field('WhatsApp', whatsapp), field(t('hours'), hours), field(t('schoolAbout'), about),
      h('p', { class: 'small muted' }, t('campusInfoHint')),
    ], () => run(async () => {
      await api(c ? 'PUT' : 'POST', c ? `/api/campuses/${c.id}` : '/api/campuses',
        { name: name.value, receipt_prefix: prefix.value, reg_no: reg.value, address: addr.value, phone: phone.value, email: email.value,
          about: about.value, whatsapp: whatsapp.value, hours: hours.value });
      rerender();
    }, t('saved')));
  };
  if (top) body.push(h('div', { class: 'row', style: 'justify-content:flex-end' }, h('button', { class: 'btn primary', onclick: () => edit(null) }, t('newCampus'))));
  body.push(h('p', { class: 'small muted', style: 'margin:0' }, top ? t('campusHint') : t('campusHintOwn')));
  body.push(h('section', { class: 'card' }, campuses.length ? h('ul', { class: 'list' }, campuses.map((c) => h('li', { class: 'row' },
    h('div', { class: 'grow' }, h('b', {}, c.name), ' ', h('span', { class: 'badge' }, c.receipt_prefix),
      h('div', { class: 'muted small' }, t('nClasses', { n: c.classes }), ' · ', t('nStudents', { n: c.students })),
      c.address && h('div', { class: 'muted small' }, c.address)),
    h('button', { class: 'btn sm', onclick: () => edit(c) }, t('edit')),
    top && h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/campuses/${c.id}`), t('deleted'))) rerender(); } }, t('delete'))))) : emptyRow()));
};

// ---------- security log (安全记录) ----------
const EVENT_ICONS = { login_ok: '✅', login_fail: '⛔', login_blocked: '🚫', login_2fa_fail: '⛔', password_change: '🔑', password_reset: '🔑',
  '2fa_on': '🔐', '2fa_off': '🔓', '2fa_reset': '🔓', account_created: '➕', account_deleted: '🗑️', account_disabled: '⏸️', account_enabled: '▶️' };
MANAGE.security = async (body) => {
  const list = await api('GET', '/api/security/events');
  const fails = list.filter((e) => e.event === 'login_fail' || e.event === 'login_blocked' || e.event === 'login_2fa_fail').length;
  body.push(h('p', { class: 'small muted', style: 'margin:0' }, t('securityHint')));
  if (fails) body.push(h('div', { class: 'card nag' }, '⚠️ ', t('failedLogins', { n: fails })));
  body.push(h('section', { class: 'card' }, list.length ? h('ul', { class: 'list' }, list.map((e) => h('li', {},
    h('div', { class: 'row' }, h('span', {}, EVENT_ICONS[e.event] || '•'), h('b', { class: 'grow' }, t('ev_' + e.event)), h('span', { class: 'muted small' }, dateTime(e.created_at))),
    h('div', { class: 'small' }, e.name || e.username || '—', e.detail ? ' · ' + e.detail : ''),
    h('div', { class: 'muted small', style: 'word-break:break-all' }, e.ip, ' · ', e.user_agent.slice(0, 80))))) : emptyRow()));
};

// ---------- push log ----------
MANAGE.notify = async (body) => {
  const data = await api('GET', '/api/notifications/log');
  body.push(h('section', { class: 'card' }, h('p', { class: 'small muted', style: 'margin:0' }, t('pushUsers', { n: data.users_with_push }))));
  body.push(h('section', { class: 'card' }, h('h3', {}, t('notifyLog')),
    data.entries.length ? h('ul', { class: 'list' }, data.entries.map((e) => h('li', {},
      h('div', { class: 'row' },
        h('span', { class: 'badge ' + (e.ok ? 'ok' : 'bad') }, e.ok ? '✓' : '✗'),
        h('span', { class: 'grow' }, e.user, e.student ? ' ← ' + e.student : ''),
        h('span', { class: 'muted small' }, dateTime(e.created_at))),
      h('div', { class: 'muted small', style: 'word-break:break-all' }, e.detail)))) : emptyRow()));
};
