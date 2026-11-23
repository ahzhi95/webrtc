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
          h('span', {}, money(f.amount_cents)), f.receipt_id ? receiptButton(f.receipt_id, t('receiptShort')) : feeBadge(f)))))));
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

  const official = children.length ? await api('GET', '/api/official-receipts') : [];
  if (official.length) {
    parts.push(h('section', { class: 'card' }, h('h3', {}, '📄 ', t('myReceipts')),
      h('ul', { class: 'list' }, official.map((o) => h('li', { class: 'row' },
        h('span', { class: 'grow' }, h('b', {}, o.number), o.voided_at ? h('span', { class: 'badge bad', style: 'margin-left:6px' }, t('void')) : null,
          h('div', { class: 'muted small' }, o.student_name, ' · ', o.issued_at.slice(0, 10), ' · ', money(o.total_cents))),
        receiptButton(o.id, t('open')))))));
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
      receiptButton(r.official_id, r.official_no),
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
    h('div', { class: 'row' }, h('h2', { class: 'grow', style: 'margin:0' }, t('receiptsTitle')),
      h('button', { class: 'btn sm', onclick: () => exportDialog() }, '📤 ', t('export'))),
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
          receiptButton(f.receipt_id, f.receipt_no),
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

// ---------- official receipts (正式收据) ----------
// Drawn on the device (which has Chinese fonts) from the server's record, then saved as PDF or image.

const RECEIPT_FONT = '"PingFang SC","Hiragino Sans GB","Microsoft YaHei","Noto Sans SC","Noto Sans CJK SC","WenQuanYi Zen Hei",sans-serif';
const METHOD_LABEL = { transfer: ['网上转账 / 银行转账', 'Online / bank transfer'], cash: ['现金', 'Cash'], other: ['其他', 'Other'] };

function drawReceipt(r) {
  const W = 1240, P = 80;
  const c = document.createElement('canvas');
  c.width = W; c.height = 3200;
  const g = c.getContext('2d');
  g.fillStyle = '#fff'; g.fillRect(0, 0, W, c.height);
  g.textBaseline = 'top';
  let y = P;
  const font = (px, bold) => { g.font = `${bold ? '700 ' : ''}${px}px ${RECEIPT_FONT}`; };
  // Wrapped text; returns the height used.
  const text = (s, x, maxW, px, opts = {}) => {
    font(px, opts.bold);
    g.fillStyle = opts.color || '#111';
    g.textAlign = opts.align || 'left';
    const lines = [];
    let line = '';
    for (const ch of String(s)) {
      if (g.measureText(line + ch).width > maxW && line) { lines.push(line); line = ''; }
      line += ch;
    }
    lines.push(line);
    lines.forEach((l, i) => g.fillText(l, x, y + i * px * 1.35));
    return lines.length * px * 1.35;
  };
  const rule = (w = 2, color = '#222') => { g.fillStyle = color; g.fillRect(P, y, W - 2 * P, w); };
  const s = r.school;

  y += text(s.name || '—', W / 2, W - 2 * P, 50, { bold: true, align: 'center' }) + 6;
  if (s.reg_no) y += text(s.reg_no, W / 2, W - 2 * P, 24, { align: 'center', color: '#444' });
  if (s.address) y += text(s.address, W / 2, W - 2 * P, 24, { align: 'center', color: '#444' });
  const contact = [s.phone && 'Tel: ' + s.phone, s.email].filter(Boolean).join('   ');
  if (contact) y += text(contact, W / 2, W - 2 * P, 24, { align: 'center', color: '#444' });
  y += 24; rule(3); y += 36;
  y += text('正式收据  OFFICIAL RECEIPT', W / 2, W - 2 * P, 42, { bold: true, align: 'center' }) + 30;

  const kv = (k, v) => {
    const h1 = text(k, P, 330, 26, { color: '#555' });
    const h2 = text(v, P + 340, W - 2 * P - 340, 26, { bold: true });
    y += Math.max(h1, h2) + 10;
  };
  kv('收据编号 Receipt No.', r.number);
  kv('日期 Date', r.issued_at.slice(0, 10));
  kv('收款自 Received from', r.payer_name || `${r.student_name} 的家长 / Parent`);
  kv('学生 Student', `${r.student_name}  (${[r.class_name, r.student_no].filter(Boolean).join(' · ')})`);
  y += 20;

  // Items table
  g.fillStyle = '#eef3f1'; g.fillRect(P, y, W - 2 * P, 56);
  const amtX = W - P - 20;
  const hy = y; y += 14;
  text('项目 Description', P + 20, 700, 26, { bold: true });
  text(`金额 Amount (${state.currency})`, amtX, 400, 26, { bold: true, align: 'right' });
  y = hy + 56 + 16;
  for (const it of r.items) {
    const hgt = text(it.description, P + 20, W - 2 * P - 360, 28);
    text(centsToStr(it.amount_cents), amtX, 300, 28, { align: 'right' });
    y += hgt + 14;
    rule(1, '#ccc'); y += 14;
  }
  y += 6;
  text('合计 Total', P + 20, 600, 32, { bold: true });
  text(`${state.currency} ${centsToStr(r.total_cents)}`, amtX, 500, 32, { bold: true, align: 'right' });
  y += 52; rule(3); y += 30;
  const m = METHOD_LABEL[r.method] || [r.method, r.method];
  kv('付款方式 Payment', `${m[0]}  ${m[1]}`);
  kv('经办人 Issued by', r.issued_by || '—');
  y += 30;
  y += text('此收据由系统生成，无需签名。This is a computer-generated receipt; no signature is required.', P, W - 2 * P, 22, { color: '#666' });
  y += text(`${t('issuedAtLabel')} ${dateTime(r.issued_at)}`, P, W - 2 * P, 22, { color: '#666' });
  y += P;

  const out = document.createElement('canvas');
  out.width = W; out.height = Math.ceil(y);
  const og = out.getContext('2d');
  og.drawImage(c, 0, 0);
  if (r.voided_at) { // clearly unusable once voided
    og.save();
    og.translate(W / 2, out.height / 2);
    og.rotate(-Math.PI / 7);
    og.font = `700 150px ${RECEIPT_FONT}`;
    og.fillStyle = 'rgba(200,0,0,.28)';
    og.textAlign = 'center'; og.textBaseline = 'middle';
    og.fillText('已作废 VOID', 0, 0);
    og.restore();
    og.font = `700 26px ${RECEIPT_FONT}`;
    og.fillStyle = '#c00'; og.textAlign = 'left'; og.textBaseline = 'top';
    og.fillText(`已作废 VOID ${r.voided_at.slice(0, 10)} · ${r.void_reason}`, P, 28);
  }
  return out;
}

function centsToStr(c) { return (c / 100).toLocaleString('en-MY', { minimumFractionDigits: 2, maximumFractionDigits: 2 }); }

let jsPDFLoading;
function loadJsPDF() {
  if (window.jspdf) return Promise.resolve();
  jsPDFLoading = jsPDFLoading || new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = 'vendor/jspdf.umd.min.js';
    s.onload = resolve; s.onerror = () => { jsPDFLoading = null; reject(new Error(t('err:network'))); };
    document.head.append(s);
  });
  return jsPDFLoading;
}

async function receiptPDF(canvas) {
  await loadJsPDF();
  const wMM = 210, hMM = wMM * canvas.height / canvas.width;
  const doc = new window.jspdf.jsPDF({ unit: 'mm', format: [wMM, hMM], compress: true });
  doc.addImage(canvas.toDataURL('image/jpeg', 0.92), 'JPEG', 0, 0, wMM, hMM);
  return doc.output('blob');
}

function saveBlob(blob, name) {
  const a = h('a', { href: URL.createObjectURL(blob), download: name });
  document.body.append(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 60000);
}

async function openOfficialReceipt(id) {
  let r;
  try { r = await api('GET', `/api/official-receipts/${id}`); } catch (e) { toast(e.message); return; }
  const canvas = drawReceipt(r);
  const img = h('img', { class: 'receipt-preview', src: canvas.toDataURL('image/png'), alt: r.number });
  const base = 'receipt-' + r.number;
  const pdfFile = async () => new File([await receiptPDF(canvas)], base + '.pdf', { type: 'application/pdf' });
  const btns = h('div', { class: 'row' },
    h('button', { type: 'button', class: 'btn primary grow', onclick: () => run(async () => saveBlob(await pdfFile(), base + '.pdf')) }, '⬇ ', t('downloadPdf')),
    h('button', { type: 'button', class: 'btn grow', onclick: () => canvas.toBlob((b) => saveBlob(b, base + '.png'), 'image/png') }, t('downloadImage')),
    navigator.canShare && h('button', { type: 'button', class: 'btn grow', onclick: () => run(async () => {
      const f = await pdfFile();
      if (navigator.canShare({ files: [f] })) await navigator.share({ files: [f], title: r.number });
      else saveBlob(f, base + '.pdf');
    }) }, t('share')));
  openDialog(`${t('officialReceipt')} ${r.number}`, [
    r.voided_at && h('div', { class: 'alert' }, t('receiptVoided', { reason: r.void_reason })),
    img, btns,
  ], null);
}

const receiptButton = (id, number) => id ? h('button', { type: 'button', class: 'btn sm', onclick: () => openOfficialReceipt(id) }, '📄 ', number || t('officialReceipt')) : null;

// ---------- export (凭证导出) ----------
async function exportDialog() {
  const [campuses, classes, logs] = await Promise.all([
    allCampuses() ? api('GET', '/api/campuses') : [], api('GET', '/api/classes'), api('GET', '/api/export/logs'),
  ]);
  const campus = h('select', {}, campusOptions(campuses, '', t('allSchools')));
  const cls = h('select', {}, classOptions(classes, '', t('allClasses')));
  campus.addEventListener('change', () => cls.replaceChildren(...classOptions(classes.filter((c) => !campus.value || String(c.campus_id) === campus.value), '', t('allClasses'))));
  const month = todayStr().slice(0, 7);
  const from = h('input', { type: 'date', value: month + '-01' });
  const to = h('input', { type: 'date', value: todayStr() });
  const status = h('select', {}, ['', 'approved', 'pending', 'rejected'].map((s) => h('option', { value: s }, s ? t('st_' + s) : t('st_all'))));
  const url = (format) => {
    const q = new URLSearchParams({ format });
    if (campus.value) q.set('campus_id', campus.value);
    if (cls.value) q.set('class_id', cls.value);
    if (from.value) q.set('from', from.value);
    if (to.value) q.set('to', to.value);
    if (status.value) q.set('status', status.value);
    return '/api/export/receipts?' + q;
  };
  const dl = (format) => { const a = h('a', { href: url(format), download: '' }); document.body.append(a); a.click(); a.remove(); };
  const kindLabel = { 'receipts-xlsx': 'Excel', 'receipts-zip': 'ZIP' };
  openDialog(t('exportTitle'), [
    allCampuses() && campuses.length > 1 && field(t('campus'), campus),
    field(t('class'), cls),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('dateFrom'), from)), h('div', { class: 'grow' }, field(t('dateTo'), to))),
    field(t('status'), status),
    h('div', { class: 'row' },
      h('button', { type: 'button', class: 'btn primary grow', onclick: () => dl('xlsx') }, '📊 ', t('exportXlsx')),
      h('button', { type: 'button', class: 'btn grow', onclick: () => dl('zip') }, '🗂 ', t('exportZip'))),
    h('p', { class: 'small muted' }, t('exportHint')),
    h('details', {}, h('summary', { class: 'small' }, t('exportLog')),
      logs.length ? h('ul', { class: 'list small' }, logs.slice(0, 20).map((e) => {
        const f = JSON.parse(e.filters || '{}');
        return h('li', {}, h('div', { class: 'row' }, h('b', { class: 'grow' }, e.user), h('span', { class: 'muted' }, dateTime(e.created_at))),
          h('div', { class: 'muted' }, kindLabel[e.kind] || e.kind, ' · ', t('nRecords', { n: e.count }), ' · ',
            [f.From || '…', '~', f.To || '…'].join(' '), f.Status ? ' · ' + t('st_' + f.Status) : ''));
      })) : emptyRow()),
  ], null);
}
