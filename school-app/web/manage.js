'use strict';

// ============ manage (admin) ============
// Students (with their file), classes, accounts, fees, QR cards, campuses (top-level admin) and the alerts log.

let manageTab = 'students';
const manageFilter = { classId: '', role: '' };

VIEWS.manage = async (main) => {
  const rerender = () => VIEWS.manage(main);
  const top = !state.me.user.campus_id;
  const tabs = ['students', 'classes', 'users', 'fees', 'cards', 'campuses', 'notify'];
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
  const role = h('select', {}, ['parent', 'teacher', 'admin'].map((r) => h('option', { value: r, selected: u ? u.role === r : r === 'parent' }, t('role_' + r))));
  const name = h('input', { type: 'text', required: true, value: u ? u.name : '' });
  const username = h('input', { type: 'text', required: true, autocapitalize: 'none', pattern: '[a-zA-Z0-9._\\-]{3,32}' });
  const phone = h('input', { type: 'tel', value: u ? u.phone : '' });
  const email = h('input', { type: 'email', value: u ? u.email : '' });
  const pw = h('input', { type: 'text', required: true, minlength: 6, autocomplete: 'new-password' });
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
        h('div', { class: 'muted small' }, u.username, u.phone ? ' · ' + u.phone : '', u.email ? ' · ' + u.email : ''),
        u.children && h('div', { class: 'muted small' }, t('children'), ': ', u.children.map((c) => c.name).join('、')))),
    h('div', { class: 'row', style: 'margin-top:6px;justify-content:flex-end' },
      h('button', { class: 'btn sm', onclick: () => userDialog(u, campuses, rerender) }, t('edit')),
      h('button', { class: 'btn sm', onclick: async () => {
        const p = prompt(t('newPwPrompt'));
        if (p) await run(() => api('POST', `/api/users/${u.id}/password`, { password: p }), t('saved'));
      } }, t('resetPw')),
      u.id !== me.id && h('button', { class: 'btn sm', onclick: async () => {
        if (!u.disabled && !confirm(t('disableConfirm', { n: u.name }))) return;
        if (await run(() => api('POST', `/api/users/${u.id}/disabled`, { disabled: !u.disabled }), t('saved'))) rerender();
      } }, u.disabled ? t('enable') : t('disable')),
      u.id !== me.id && h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/users/${u.id}`), t('deleted'))) rerender(); } }, t('delete')))))) : emptyRow()));
};

// ---------- fees ----------
MANAGE.fees = (body, rerender) => manageFees(body, rerender);

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
    openDialog(c ? t('editCampus') : t('newCampus'), [
      field(t('campusName'), name),
      h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('receiptPrefix'), prefix)), h('div', { class: 'grow' }, field(t('regNo'), reg))),
      field(t('address'), addr),
      h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('phone'), phone)), h('div', { class: 'grow' }, field(t('email'), email))),
      h('p', { class: 'small muted' }, t('campusReceiptHint')),
    ], () => run(async () => {
      await api(c ? 'PUT' : 'POST', c ? `/api/campuses/${c.id}` : '/api/campuses',
        { name: name.value, receipt_prefix: prefix.value, reg_no: reg.value, address: addr.value, phone: phone.value, email: email.value });
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
