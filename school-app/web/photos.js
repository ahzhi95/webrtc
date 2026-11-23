'use strict';

// ============ photos ============
// Shrink a photo in the browser before upload: faster on mobile data, and drawing it on a canvas
// applies the phone's rotation and drops metadata such as GPS location. The server re-encodes too.
async function shrinkImage(file, maxSide = 1920) {
  try {
    const bmp = await createImageBitmap(file, { imageOrientation: 'from-image' });
    const scale = Math.min(1, maxSide / Math.max(bmp.width, bmp.height));
    const c = document.createElement('canvas');
    c.width = Math.round(bmp.width * scale); c.height = Math.round(bmp.height * scale);
    c.getContext('2d').drawImage(bmp, 0, 0, c.width, c.height);
    bmp.close && bmp.close();
    const blob = await new Promise((res) => c.toBlob(res, 'image/jpeg', 0.88));
    return blob || file;
  } catch (e) { return file; } // e.g. a format this browser can't read: let the server try
}

const photoSrc = (p, size) => `/api/photos/${p.id}/image${size ? '?size=' + size : ''}`;

// For a parent with several children: which of them are in the photo, as small letters in the corner.
function photoKids(p) {
  const kids = state.me.user.role === 'parent' ? state.me.children || [] : [];
  if (kids.length < 2) return null;
  const marks = kids.map((c, i) => [c, i]).filter(([c]) => p.students.some((s) => s.id === c.id));
  return marks.length > 0 && h('span', { class: 'p-kids', 'aria-hidden': 'true' }, marks.map(([c, i]) => h('i', { class: 'kid-tag c' + (i % 4) }, givenInitial(c.name))));
}

function photoGrid(list, onChange) {
  return h('div', { class: 'pgrid' }, list.map((p, i) => h('button', { class: 'pthumb', 'aria-label': [p.caption || t('tab_photos'), ...p.students.map((s) => s.name)].join(' · '), onclick: () => openLightbox(list, i, onChange) },
    h('img', { src: photoSrc(p, 'thumb'), alt: '', loading: 'lazy' }), photoKids(p))));
}

// Short name for a chip: the Chinese given name without the surname (林小红 → 小红), else the first word.
function shortName(name) {
  const zh = (name || '').match(/[\u3400-\u9fff]+/);
  if (zh && zh[0].length >= 2) return zh[0].slice(1);
  return (name || '').trim().split(/\s+/)[0];
}

// Parent's filter: "all" plus a small avatar chip for each child.
function kidChips(selected, onPick) {
  const kids = state.me.children || [];
  return h('div', { class: 'kid-chips', role: 'group' },
    h('button', { class: 'kid-chip' + (!selected ? ' on' : ''), 'aria-pressed': String(!selected), onclick: () => onPick('') }, t('all')),
    kids.map((c, i) => h('button', { class: 'kid-chip' + (String(c.id) === selected ? ' on' : ''), 'aria-pressed': String(String(c.id) === selected), title: c.name, onclick: () => onPick(String(c.id)) },
      h('span', { class: 'kid-tag c' + (i % 4), 'aria-hidden': 'true' }, givenInitial(c.name)), shortName(c.name))));
}

function openLightbox(list, index, onChange) {
  let i = index;
  const img = h('img', { alt: '' });
  const info = h('div', { class: 'lb-info' });
  const box = h('div', { class: 'lightbox', role: 'dialog', 'aria-modal': 'true' });
  const close = () => { box.remove(); document.removeEventListener('keydown', onKey); };
  const show = () => {
    const p = list[i];
    img.src = photoSrc(p);
    const canDelete = state.me.user.role === 'admin' || (state.me.user.role === 'teacher' && p.uploader_id === state.me.user.id);
    info.replaceChildren(
      h('div', { class: 'row', style: 'margin-bottom:6px' }, p.tag && h('span', { class: 'chip', style: 'padding-right:10px' }, tagLabel(p.tag)), h('b', {}, p.taken_on)),
      p.caption && h('div', { class: 'lb-cap' }, p.caption),
      h('div', {}, p.students.map((s) => h('span', { class: 'chip', style: 'padding-right:10px' }, s.name))),
      h('div', { class: 'small', style: 'opacity:.75' }, dateTime(p.created_at), ' · ', p.uploaded_by),
      h('div', { class: 'row', style: 'margin-top:8px' },
        h('a', { class: 'btn sm', href: photoSrc(p) + (photoSrc(p).includes('?') ? '&' : '?') + 'download=1' }, '⬇ ', t('download')),
        h('span', { class: 'spacer' }),
        canDelete && h('button', { class: 'btn sm bad', onclick: async () => {
          if (confirm(t('confirmDelete')) && await run(() => api('DELETE', `/api/photos/${p.id}`), t('deleted'))) { close(); onChange && onChange(); }
        } }, t('delete'))));
    prev.disabled = i === 0; next.disabled = i === list.length - 1;
  };
  const prev = h('button', { class: 'lb-nav lb-prev', 'aria-label': '‹', onclick: () => { if (i > 0) { i--; show(); } } }, '‹');
  const next = h('button', { class: 'lb-nav lb-next', 'aria-label': '›', onclick: () => { if (i < list.length - 1) { i++; show(); } } }, '›');
  const onKey = (e) => { if (e.key === 'Escape') close(); if (e.key === 'ArrowLeft') prev.click(); if (e.key === 'ArrowRight') next.click(); };
  box.append(h('button', { class: 'lb-close', 'aria-label': t('close'), onclick: close }, '✕'), h('div', { class: 'lb-img' }, img, prev, next), info);
  box.addEventListener('click', (e) => { if (e.target === box) close(); });
  document.addEventListener('keydown', onKey);
  document.body.append(box);
  show();
}

const PHOTO_TAGS = ['class', 'outdoor', 'art', 'music', 'meal', 'festival', 'birthday', 'trip', 'other'];
const TAG_ICONS = { class: '📚', outdoor: '🌳', art: '🎨', music: '🎵', meal: '🍱', festival: '🏮', birthday: '🎂', trip: '🚌', other: '📷' };
const tagLabel = (k) => (TAG_ICONS[k] || '') + ' ' + t('tag_' + k);

// Photos grouped under the day they were taken.
function photoDays(list, onChange) {
  const days = [];
  for (const p of list) {
    const last = days[days.length - 1];
    if (last && last.day === p.taken_on) last.items.push(p); else days.push({ day: p.taken_on, items: [p] });
  }
  return h('div', { class: 'stack' }, days.map((d) => h('div', {},
    h('div', { class: 'day-head' }, d.day, ' ', h('span', { class: 'muted small' }, [...new Set(d.items.map((p) => p.tag).filter(Boolean))].map(tagLabel).join(' · '))),
    photoGrid(d.items, onChange))));
}

const photoFilter = { classId: '', studentId: '', tag: '', day: '' };
VIEWS.photos = async (main) => {
  const staff = state.me.user.role !== 'parent';
  const rerender = () => VIEWS.photos(main);
  const parts = [h('h2', {}, t('photosTitle'))];
  let classes = [], students = [];
  if (staff) {
    [classes, students] = await Promise.all([api('GET', '/api/classes'), api('GET', '/api/students')]);
    if (!photoFilter.classId && state.me.user.role === 'teacher') {
      const mine = classes.find((c) => c.teacher_id === state.me.user.id);
      if (mine) photoFilter.classId = String(mine.id);
    }
    parts.push(photoUploadCard(classes, students, rerender));
    parts.push(h('div', { class: 'row' },
      h('select', { class: 'grow', onchange: (e) => { photoFilter.classId = e.target.value; photoFilter.studentId = ''; rerender(); } }, classOptions(classes, photoFilter.classId, t('allClasses'))),
      h('select', { class: 'grow', onchange: (e) => { photoFilter.studentId = e.target.value; rerender(); } },
        h('option', { value: '' }, t('allStudents')),
        students.filter((s) => !photoFilter.classId || String(s.class_id) === photoFilter.classId)
          .map((s) => h('option', { value: s.id, selected: String(s.id) === photoFilter.studentId }, s.name)))));
  } else {
    const children = state.me.children || [];
    if (children.length > 1) parts.push(kidChips(photoFilter.studentId, (id) => { photoFilter.studentId = id; rerender(); }));
  }
  parts.push(h('div', { class: 'row' },
    h('select', { class: 'grow', 'aria-label': t('photoTag'), onchange: (e) => { photoFilter.tag = e.target.value; rerender(); } },
      h('option', { value: '' }, t('allTags')), PHOTO_TAGS.map((k) => h('option', { value: k, selected: photoFilter.tag === k }, tagLabel(k)))),
    h('input', { type: 'date', class: 'grow', 'aria-label': t('photoDay'), value: photoFilter.day, max: todayStr(), onchange: (e) => { photoFilter.day = e.target.value; rerender(); } }),
    (photoFilter.tag || photoFilter.day) && h('button', { class: 'btn sm', onclick: () => { photoFilter.tag = ''; photoFilter.day = ''; rerender(); } }, t('clearSel'))));
  const q = new URLSearchParams();
  if (photoFilter.tag) q.set('tag', photoFilter.tag);
  if (photoFilter.day) { q.set('from', photoFilter.day); q.set('to', photoFilter.day); }
  if (photoFilter.studentId) q.set('student_id', photoFilter.studentId);
  else if (staff && photoFilter.classId) q.set('class_id', photoFilter.classId);
  const list = await api('GET', '/api/photos?' + q);
  parts.push(list.length ? photoDays(list, rerender) : h('div', { class: 'card empty' }, t('noPhotos')));
  if (!staff) parts.push(h('p', { class: 'small muted', style: 'text-align:center' }, '🔒 ', t('photoPrivacy')));
  main.replaceChildren(h('div', { class: 'stack' }, parts));
};

function photoUploadCard(classes, students, done) {
  const picked = new Set();
  let cls = photoFilter.classId;
  const listBox = h('div', { class: 'picklist' });
  const count = h('span', { class: 'muted small' });
  const renderList = () => {
    const visible = students.filter((s) => !cls || String(s.class_id) === cls);
    listBox.replaceChildren(...visible.map((s) => h('label', { class: 'pick' + (picked.has(s.id) ? ' on' : '') },
      h('input', { type: 'checkbox', checked: picked.has(s.id), onchange: (e) => { e.target.checked ? picked.add(s.id) : picked.delete(s.id); renderList(); } }),
      s.name)));
    count.textContent = t('selectedN', { n: picked.size });
  };
  const visibleIds = () => students.filter((s) => !cls || String(s.class_id) === cls).map((s) => s.id);
  const files = h('input', { type: 'file', accept: 'image/*', multiple: true, required: true });
  const caption = h('input', { type: 'text', maxlength: 200, placeholder: t('captionPh') });
  const tag = h('select', {}, PHOTO_TAGS.map((k) => h('option', { value: k }, tagLabel(k))));
  const day = h('input', { type: 'date', value: todayStr(), max: todayStr() });
  const status = h('div', { class: 'small muted', 'aria-live': 'polite' });
  const btn = h('button', { class: 'btn primary block', type: 'submit' }, t('upload'));
  renderList();
  return h('details', { class: 'add card' }, h('summary', {}, t('uploadPhotos')),
    h('form', { onsubmit: async (e) => {
      e.preventDefault();
      if (!picked.size) { toast(t('pickStudentsFirst')); return; }
      const chosen = [...files.files];
      if (!chosen.length) return;
      btn.disabled = true;
      const fd = new FormData();
      picked.forEach((id) => fd.append('student_id', id));
      fd.set('caption', caption.value);
      fd.set('tag', tag.value);
      fd.set('taken_on', day.value);
      for (let k = 0; k < chosen.length; k++) {
        status.textContent = t('processing', { n: k + 1, total: chosen.length });
        fd.append('photo', await shrinkImage(chosen[k]), `photo${k}.jpg`);
      }
      status.textContent = t('sending');
      const ok = await run(async () => { const r = await api('POST', '/api/photos', fd); toast(t('photosUploaded', { n: r.uploaded })); });
      status.textContent = '';
      btn.disabled = false;
      if (ok) done();
    } },
    h('label', { class: 'f' }, t('choosePhotos'), files),
    h('div', { class: 'f' },
      h('div', { class: 'row', style: 'margin-bottom:6px' }, h('span', { class: 'grow' }, t('whoInPhoto')), count),
      h('div', { class: 'row', style: 'margin-bottom:8px' },
        h('select', { class: 'grow', onchange: (e) => { cls = e.target.value; renderList(); } }, classOptions(classes, cls, t('allClasses'))),
        h('button', { type: 'button', class: 'btn sm', onclick: () => { visibleIds().forEach((id) => picked.add(id)); renderList(); } }, t('selectAll')),
        h('button', { type: 'button', class: 'btn sm', onclick: () => { picked.clear(); renderList(); } }, t('clearSel'))),
      listBox),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, field(t('photoTag'), tag)), h('div', { class: 'grow' }, field(t('photoDay'), day))),
    h('label', { class: 'f' }, t('caption'), caption),
    h('p', { class: 'small muted' }, '🔒 ', t('photoPrivacyStaff')),
    status, btn));
}
