'use strict';

// ============ fees & receipts ============
// Parents see each child's bills, tick the ones they paid and upload one receipt for them.
// Admins create fee items, follow who has paid, and confirm or return receipts.

const FEE_CATEGORIES = ['monthly', 'event', 'misc', 'other'];

function feeBadge(f) {
  if (f.status === 'unpaid' && f.due_date && f.due_date < todayStr()) return h('span', { class: 'badge bad' }, t('overdue'));
  const cls = { unpaid: 'warn', pending: 'info', paid: 'ok', waived: '' }[f.status];
  return h('span', { class: 'badge ' + cls }, t('fee_' + f.status));
}

async function uploadReceipt(fd, btn) {
  btn.disabled = true;
  const ok = await run(() => api('POST', '/api/receipts', fd), t('uploaded'));
  btn.disabled = false;
  return ok;
}

VIEWS.receipts = async (main) => (state.me.user.role === 'parent' ? parentFees(main) : reviewReceipts(main));

async function parentFees(main) {
  const rerender = () => parentFees(main);
  const children = state.me.children || [];
  const [fees, receipts] = await Promise.all([api('GET', '/api/fees'), api('GET', '/api/receipts')]);
  const parts = [h('h2', {}, t('feesTitle'))];
  if (!children.length) parts.push(h('div', { class: 'card empty' }, t('noChildren')));

  for (const c of children) {
    const mine = fees.filter((f) => f.student_id === c.id);
    const open = mine.filter((f) => f.status === 'unpaid' || f.status === 'pending');
    const done = mine.filter((f) => f.status === 'paid' || f.status === 'waived');
    const picked = new Set();
    const total = h('b', {}, money(0));
    const file = h('input', { type: 'file', required: true, accept: 'image/jpeg,image/png,image/webp,application/pdf' });
    const note = h('input', { type: 'text', maxlength: 200, placeholder: t('payNotePh') });
    const btn = h('button', { class: 'btn primary block', type: 'submit' }, t('uploadForSelected'));
    const payBox = h('form', { class: 'paybox', hidden: true, onsubmit: async (e) => {
      e.preventDefault();
      const fd = new FormData();
      fd.set('student_id', c.id);
      picked.forEach((id) => fd.append('fee_id', id));
      if (note.value.trim()) fd.set('description', mine.filter((f) => picked.has(f.id)).map((f) => f.name).join('、') + ' — ' + note.value.trim());
      fd.set('file', file.files[0]);
      if (await uploadReceipt(fd, btn)) rerender();
    } }, h('div', { class: 'row' }, h('span', { class: 'grow' }, t('selectedTotal')), total), field(t('file'), file), field(t('payNote'), note), btn);
    const update = () => {
      payBox.hidden = !picked.size;
      total.textContent = money(mine.filter((f) => picked.has(f.id)).reduce((s, f) => s + f.amount_cents, 0));
    };
    parts.push(h('section', { class: 'card' },
      h('h3', {}, c.name, h('span', { class: 'muted small' }, ' · ', c.campus_name)),
      open.length ? h('ul', { class: 'list' }, open.map((f) => h('li', {},
        h('label', { class: 'row fee-row' },
          f.status === 'unpaid'
            ? h('input', { type: 'checkbox', onchange: (e) => { e.target.checked ? picked.add(f.id) : picked.delete(f.id); update(); } })
            : h('span', { class: 'fee-spacer' }),
          h('span', { class: 'grow' }, f.name, h('div', { class: 'muted small' }, t('cat_' + f.category), f.due_date ? ' · ' + t('dueOn', { d: f.due_date }) : '')),
          h('b', {}, money(f.amount_cents)), feeBadge(f))))) : h('div', { class: 'muted', style: 'padding:8px 0' }, '✓ ', t('allPaid')),
      open.some((f) => f.status === 'unpaid') && h('p', { class: 'small muted', style: 'margin:8px 0 0' }, t('tickToPay')),
      payBox,
      done.length > 0 && h('details', { style: 'margin-top:10px' }, h('summary', { class: 'small muted' }, t('paidHistory', { n: done.length })),
        h('ul', { class: 'list' }, done.map((f) => h('li', { class: 'row' },
          h('span', { class: 'grow' }, f.name, h('div', { class: 'muted small' }, f.paid_at ? t('paidOn', { d: f.paid_at.slice(0, 10) }) : f.note)),
          h('span', {}, money(f.amount_cents)), feeBadge(f)))))));
  }

  // Payments not on the list (e.g. agreed with the school directly).
  if (children.length) {
    const child = h('select', { required: true }, children.map((c) => h('option', { value: c.id }, c.name)));
    const desc = h('input', { type: 'text', required: true, maxlength: 200, placeholder: t('descPlaceholder') });
    const amount = h('input', { type: 'number', required: true, min: '0.01', step: '0.01', inputmode: 'decimal' });
    const file = h('input', { type: 'file', required: true, accept: 'image/jpeg,image/png,image/webp,application/pdf' });
    const btn = h('button', { class: 'btn primary block', type: 'submit' }, t('upload'));
    parts.push(h('details', { class: 'add card' }, h('summary', {}, t('otherPayment')),
      h('form', { onsubmit: async (e) => {
        e.preventDefault();
        const fd = new FormData();
        fd.set('student_id', child.value); fd.set('description', desc.value); fd.set('amount', amount.value); fd.set('file', file.files[0]);
        if (await uploadReceipt(fd, btn)) rerender();
      } }, field(t('child'), child), field(t('description'), desc), field(t('amount') + ` (${state.currency})`, amount), field(t('file'), file), btn)));
  }

  parts.push(h('h3', { style: 'margin-top:8px' }, t('uploadHistory')));
  parts.push(receipts.length ? receipts.map((r) => receiptCard(r, rerender)) : h('div', { class: 'card' }, emptyRow()));
  main.replaceChildren(h('div', { class: 'stack' }, parts));
}

function receiptCard(r, rerender) {
  const admin = state.me.user.role === 'admin';
  return h('section', { class: 'card' },
    h('div', { class: 'row' },
      h('div', { class: 'grow' }, h('h3', { style: 'margin:0' }, r.student_name), h('div', { class: 'muted small' }, admin && allCampuses() ? r.campus_name + ' · ' : '', r.class_name)),
      statusBadge(r.status)),
    h('div', { class: 'row', style: 'margin-top:8px' }, h('div', { class: 'grow' }, r.description), h('b', {}, money(r.amount_cents))),
    r.fees.length > 0 && h('div', {}, r.fees.map((f) => h('span', { class: 'chip', style: 'padding-right:10px' }, f.name, ' ', money(f.amount_cents)))),
    h('div', { class: 'muted small' }, t('uploadedAt'), ': ', dateTime(r.created_at),
      admin ? [' · ', t('parent'), ': ', r.parent_name, r.parent_phone ? ` (${r.parent_phone})` : ''] : null),
    r.review_note && h('div', { class: 'small', style: 'margin-top:6px' }, h('b', {}, t('reviewNote'), ': '), r.review_note),
    admin && r.reviewed_by && h('div', { class: 'muted small' }, t('reviewedBy'), ': ', r.reviewed_by, ' · ', dateTime(r.reviewed_at)),
    h('div', { class: 'row', style: 'margin-top:10px' },
      h('a', { class: 'btn sm', href: `/api/receipts/${r.id}/file`, target: '_blank', rel: 'noopener' }, t('viewFile')),
      h('span', { class: 'spacer' }),
      admin && r.status !== 'approved' && h('button', { class: 'btn sm ok', onclick: async () => {
        if (await run(() => api('POST', `/api/receipts/${r.id}/review`, { status: 'approved', note: '' }), t('saved'))) rerender();
      } }, t('approve')),
      admin && r.status !== 'rejected' && h('button', { class: 'btn sm bad', onclick: () => {
        const note = h('textarea', { required: true, maxlength: 300, style: 'min-height:80px' });
        openDialog(t('reject'), [field(t('rejectReason'), note)], () => run(async () => {
          await api('POST', `/api/receipts/${r.id}/review`, { status: 'rejected', note: note.value });
          rerender();
        }, t('saved')), t('reject'));
      } }, t('reject'))));
}

const receiptFilter = { status: 'pending', campusId: '' };
async function reviewReceipts(main) {
  const rerender = () => reviewReceipts(main);
  const q = new URLSearchParams();
  if (receiptFilter.status !== 'all') q.set('status', receiptFilter.status);
  if (receiptFilter.campusId) q.set('campus_id', receiptFilter.campusId);
  const [list, campuses] = await Promise.all([api('GET', '/api/receipts?' + q), allCampuses() ? api('GET', '/api/campuses') : []]);
  main.replaceChildren(h('div', { class: 'stack' },
    h('h2', {}, t('receiptsTitle')),
    campuses.length > 1 && h('select', { onchange: (e) => { receiptFilter.campusId = e.target.value; rerender(); } }, campusOptions(campuses, receiptFilter.campusId, t('allSchools'))),
    h('div', { class: 'seg' }, ['pending', 'approved', 'rejected', 'all'].map((s) =>
      h('button', { class: receiptFilter.status === s ? 'on' : null, onclick: () => { receiptFilter.status = s; rerender(); } }, t('st_' + s)))),
    list.length ? list.map((r) => receiptCard(r, rerender)) : h('div', { class: 'card' }, emptyRow())));
}

// ---------- fee items (管理 → 收费) ----------
const feeFilter = { campusId: '' };

async function manageFees(body, rerender) {
  const [items, campuses, classes, students] = await Promise.all([
    api('GET', '/api/fee-items' + (feeFilter.campusId ? '?campus_id=' + feeFilter.campusId : '')),
    api('GET', '/api/campuses'), api('GET', '/api/classes'), api('GET', '/api/students'),
  ]);
  body.push(h('div', { class: 'row' },
    allCampuses() && campuses.length > 1 && h('select', { class: 'grow', onchange: (e) => { feeFilter.campusId = e.target.value; rerender(); } }, campusOptions(campuses, feeFilter.campusId, t('allSchools'))),
    h('button', { class: 'btn primary', onclick: () => feeItemDialog(null, { campuses, classes, students }, rerender) }, t('newFee'))));
  if (!items.length) body.push(h('div', { class: 'card' }, emptyRow()));
  for (const f of items) {
    const pct = f.total ? Math.round(100 * (f.paid + f.waived) / f.total) : 0;
    body.push(h('section', { class: 'card' },
      h('div', { class: 'row' },
        h('div', { class: 'grow' }, h('h3', { style: 'margin:0' }, f.name),
          h('div', { class: 'muted small' }, t('cat_' + f.category), ' · ', f.class_name ? className({ campus_name: f.campus_name, name: f.class_name }) : (f.campus_name || '—'),
            f.due_date ? ' · ' + t('dueOn', { d: f.due_date }) : '')),
        h('b', {}, money(f.amount_cents))),
      h('div', { class: 'progress', role: 'img', 'aria-label': `${pct}%` }, h('div', { style: `width:${pct}%` })),
      h('div', { class: 'row small' },
        h('span', { class: 'grow' }, t('feeProgress', { paid: f.paid, total: f.total }), f.pending ? ' · ' + t('feePendingN', { n: f.pending }) : ''),
        h('span', { class: 'muted' }, t('collected'), ' ', money(f.paid_cents), ' · ', t('outstanding'), ' ', money(f.due_cents))),
      h('div', { class: 'row', style: 'margin-top:8px' },
        h('button', { class: 'btn sm primary', onclick: () => feeItemStudents(f, rerender) }, t('viewStudents')),
        h('button', { class: 'btn sm', onclick: () => feeItemDialog(f, { campuses, classes, students }, rerender) }, t('copy')),
        h('button', { class: 'btn sm', onclick: () => {
          const name = h('input', { type: 'text', required: true, value: f.name });
          const due = h('input', { type: 'date', value: f.due_date });
          openDialog(t('edit'), [field(t('feeName'), name), field(t('dueDate'), due)], () => run(async () => {
            await api('PUT', `/api/fee-items/${f.id}`, { name: name.value, due_date: due.value });
            rerender();
          }, t('saved')));
        } }, t('edit')),
        h('span', { class: 'spacer' }),
        h('button', { class: 'btn sm bad', onclick: async () => { if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/fee-items/${f.id}`), t('deleted'))) rerender(); } }, t('delete')))));
  }
}

// New fee item (or a copy of an existing one, e.g. next month's tuition).
function feeItemDialog(copyFrom, { campuses, classes, students }, done) {
  const name = h('input', { type: 'text', required: true, maxlength: 200, value: copyFrom ? copyFrom.name : '', placeholder: t('feeNamePh') });
  const cat = h('select', {}, FEE_CATEGORIES.map((c) => h('option', { value: c, selected: copyFrom ? copyFrom.category === c : c === 'monthly' }, t('cat_' + c))));
  const amount = h('input', { type: 'number', required: true, min: '0.01', step: '0.01', inputmode: 'decimal', value: copyFrom ? (copyFrom.amount_cents / 100).toFixed(2) : '' });
  const due = h('input', { type: 'date' });
  const kind = h('select', {}, ['campus', 'class', 'students'].map((k) => h('option', { value: k, selected: copyFrom ? (copyFrom.class_id ? k === 'class' : k === 'campus') : k === 'campus' }, t('target_' + k))));
  // No default: billing the wrong campus by accident is easy to miss. Show how many students each covers.
  const campus = h('select', { required: true }, h('option', { value: '' }, t('pleaseChoose')),
    campuses.map((c) => h('option', { value: c.id, selected: copyFrom && copyFrom.campus_id === c.id && !copyFrom.class_id }, c.name, ' (', t('nStudents', { n: c.students }), ')')));
  const cls = h('select', { required: true }, h('option', { value: '' }, t('pleaseChoose')),
    classes.map((c) => h('option', { value: c.id, selected: copyFrom && copyFrom.class_id === c.id }, className(c), ' (', t('nStudents', { n: c.students }), ')')));
  const picked = new Set();
  const filter = h('input', { type: 'search', placeholder: t('manualSearch'), oninput: () => drawPick() });
  const pick = h('div', { class: 'picklist' });
  const drawPick = () => {
    const q = filter.value.trim().toLowerCase();
    pick.replaceChildren(...students.filter((s) => !q || s.name.toLowerCase().includes(q) || s.class_name.toLowerCase().includes(q)).map((s) =>
      h('label', { class: 'pick' + (picked.has(s.id) ? ' on' : '') },
        h('input', { type: 'checkbox', checked: picked.has(s.id), onchange: (e) => { e.target.checked ? picked.add(s.id) : picked.delete(s.id); drawPick(); } }),
        s.name, h('span', { class: 'muted small' }, ' ', s.class_name))));
  };
  drawPick();
  const targetBox = h('div', {});
  const drawTarget = () => {
    campus.required = kind.value === 'campus';
    cls.required = kind.value === 'class';
    targetBox.replaceChildren(kind.value === 'campus' ? field(t('campus'), campus) : kind.value === 'class' ? field(t('class'), cls) : h('div', { class: 'f' }, filter, pick));
  };
  kind.addEventListener('change', drawTarget);
  drawTarget();
  openDialog(copyFrom ? t('copyFee') : t('newFee'), [
    field(t('feeName'), name), field(t('feeCategory'), cat),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('amount') + ` (${state.currency})`, amount)), h('div', { class: 'grow' }, field(t('dueDate'), due))),
    field(t('billTo'), kind), targetBox,
    h('p', { class: 'small muted' }, t('feeNotifyHint')),
  ], () => run(async () => {
    const req = { name: name.value, category: cat.value, amount: amount.value, due_date: due.value, campus_id: 0, class_id: 0, student_ids: [] };
    if (kind.value === 'campus') req.campus_id = Number(campus.value);
    if (kind.value === 'class') req.class_id = Number(cls.value);
    if (kind.value === 'students') req.student_ids = [...picked];
    const r = await api('POST', '/api/fee-items', req);
    toast(t('feeCreated', { n: r.students }));
    done();
  }), t('create'));
}

// Who has paid one fee item; record cash payments, waive, or change one student's amount.
async function feeItemStudents(item, done) {
  const list = h('ul', { class: 'list' });
  let changed = false;
  const load = async () => {
    const rows = await api('GET', `/api/fee-items/${item.id}/students`);
    list.replaceChildren(...rows.map((f) => {
      const act = async (body, msg) => { if (await run(() => api('PUT', `/api/student-fees/${f.id}`, body), msg)) { changed = true; load(); } };
      return h('li', {},
        h('div', { class: 'row' },
          h('span', { class: 'grow' }, f.student_name, h('div', { class: 'muted small' }, f.class_name, f.note ? ' · ' + f.note : '')),
          h('b', {}, money(f.amount_cents)), feeBadge(f)),
        f.status !== 'pending' && h('div', { class: 'row', style: 'margin-top:6px;justify-content:flex-end' },
          f.status !== 'paid' && h('button', { type: 'button', class: 'btn sm ok', onclick: () => act({ status: 'paid', note: t('cashNote') }, t('saved')) }, t('markCash')),
          f.status === 'unpaid' && h('button', { type: 'button', class: 'btn sm', onclick: () => {
            const reason = prompt(t('waiveReason'));
            if (reason !== null) act({ status: 'waived', note: reason }, t('saved'));
          } }, t('waive')),
          f.status === 'unpaid' && h('button', { type: 'button', class: 'btn sm', onclick: () => {
            const v = prompt(t('newAmount'), (f.amount_cents / 100).toFixed(2));
            if (v) act({ amount: v }, t('saved'));
          } }, t('changeAmount')),
          f.status !== 'unpaid' && h('button', { type: 'button', class: 'btn sm', onclick: () => act({ status: 'unpaid', note: '' }, t('saved')) }, t('markUnpaid'))));
    }));
  };
  await load();
  const close = openDialog(item.name, [list], null);
  // Refresh the summary when the dialog closes.
  const back = document.querySelector('.dialog-back');
  new MutationObserver((_, obs) => { if (!document.body.contains(back)) { obs.disconnect(); if (changed) done(); } }).observe(document.body, { childList: true });
  return close;
}
