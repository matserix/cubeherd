// Cubeherd – kleines Client-Skript: Custom Selects, Wizard-Schritte, Modals, Toasts, Konsole.
(() => {
  'use strict';
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

  /* ---------------- Toasts (Server setzt HX-Trigger: {"fc-toast": {...}}) ---------------- */
  function toast(msg, kind = '') {
    const host = $('#toasts'); if (!host) return;
    const t = document.createElement('div'); t.className = 'toast ' + kind; t.textContent = msg;
    host.appendChild(t); setTimeout(() => t.remove(), 4200);
  }
  document.body.addEventListener('fc-toast', e => toast(e.detail.msg, e.detail.kind || ''));
  // Translated texts and number format come from data attributes on <body>.
  const msg = k => document.body.dataset['t' + k] || k;
  const decimalSep = document.body.dataset.decimal || '.';
  document.body.addEventListener('htmx:responseError', () => toast(msg('Reqfail'), 'crit'));
  document.body.addEventListener('htmx:sendError', () => toast(msg('Noconn'), 'crit'));

  /* ---------------- Modal ---------------- */
  const modal = () => $('#modal');
  function closeModal() { const m = modal(); if (m) { m.hidden = true; $('#modal-card').innerHTML = ''; } }
  const swapTarget = e => (e.detail && e.detail.target) || e.target;
  document.body.addEventListener('htmx:afterSwap', e => {
    if (swapTarget(e).id === 'modal-card') {
      modal().hidden = false;
      const f = $('#modal-card [autofocus], #modal-card input:not([type=radio]):not([type=checkbox]), #modal-card .cs-btn');
      if (f) f.focus();
    }
  });
  document.body.addEventListener('fc-close-modal', closeModal);
  document.addEventListener('click', e => {
    if (e.target.closest('[data-close]') || e.target.id === 'modal') closeModal();
  });
  document.addEventListener('keydown', e => { if (e.key === 'Escape' && modal() && !modal().hidden) closeModal(); });

  /* ---------------- Formular-Helfer ---------------- */
  // Same formula as server.HeapMB: the container limit minus headroom for non-heap memory.
  const heapGB = (gb, modded) => {
    const total = gb * 1024;
    const overhead = modded ? Math.max(2048, Math.floor(total * 3 / 10)) : Math.max(1024, Math.floor(total / 5));
    return Math.max(512, Math.floor((total - overhead) / 256) * 256) / 1024;
  };
  const wizModded = () => {
    const f = document.getElementById('wizard'); if (!f) return false;
    const k = (f.querySelector('input[name=kind]:checked') || {}).value;
    const t = (f.querySelector('input[name=type]:checked') || {}).value;
    return k !== 'software' || !['VANILLA', 'PAPER', 'PURPUR', 'FOLIA'].includes(t);
  };
  const ramLabel = el => {
    if (!el.dataset.heap) return el.value + ' GB';
    const modded = el.dataset.heap === 'auto' ? wizModded() : el.dataset.heap === '1';
    return el.value + ' GB · Heap ' + heapGB(+el.value, modded).toFixed(1).replace('.', decimalSep) + ' GB';
  };
  document.addEventListener('change', e => {
    if (e.target.name === 'kind' || e.target.name === 'type') { const r = document.getElementById('w-ram'); if (r) document.getElementById(r.dataset.out).textContent = ramLabel(r); }
  });
  window.addEventListener('load', () => { const r = document.getElementById('w-ram'); if (r) document.getElementById(r.dataset.out).textContent = ramLabel(r); });
  document.addEventListener('input', e => {
    const t = e.target;
    if (t.dataset.out) { const o = document.getElementById(t.dataset.out); if (o) o.textContent = ramLabel(t); }
    if (t.dataset.confirm) { const b = document.getElementById(t.dataset.confirmBtn); if (b) b.disabled = t.value.trim() !== t.dataset.confirm; }
  });

  /* ---------------- Konsole: automatisch mitscrollen ---------------- */
  const nearEnd = el => el.scrollHeight - el.scrollTop - el.clientHeight < 60;
  let logStick = true;
  document.addEventListener('scroll', e => { if (e.target.id === 'log') logStick = nearEnd(e.target); }, true);
  const stickLog = () => { const lg = $('#log'); if (lg && logStick) lg.scrollTop = lg.scrollHeight; };
  document.body.addEventListener('htmx:sseMessage', e => {
    if (e.detail.type === 'log') {
      const lg = $('#log');
      if (lg && lg.childElementCount > 1500) for (let i = 0; i < 200; i++) lg.firstElementChild.remove();
      stickLog();
    }
  });
  document.body.addEventListener('htmx:afterSwap', e => { if (swapTarget(e).id === 'log') { logStick = true; stickLog(); } });
  document.body.addEventListener('htmx:afterRequest', e => {
    const f = e.detail.elt;
    if (f && f.id === 'cmd-form' && e.detail.successful) { f.reset(); $('#cmd-input', f).focus(); }
  });
  window.addEventListener('load', stickLog);

  /* ---------------- Sidebar follows status changes ---------------- */
  document.body.addEventListener('htmx:afterSwap', e => { if (e.target && e.target.id === 'srv-head') htmx.trigger(document.body, 'rack-refresh'); });
  document.body.addEventListener('htmx:afterRequest', e => {
    const p = e.detail && e.detail.pathInfo && e.detail.pathInfo.requestPath;
    if (p === '/partials/overview' && e.detail.successful) htmx.trigger(document.body, 'rack-refresh');
  });

  /* ---------------- Update dialog: downgrade warning only for older targets ---------------- */
  document.addEventListener('change', e => {
    const sel = e.target.closest && e.target.closest('select[name=version]');
    if (!sel || !sel.closest('#modal')) return;
    const older = !!(sel.selectedOptions[0] && sel.selectedOptions[0].dataset.older);
    $$('[data-older-only]', sel.closest('form')).forEach(el => { el.hidden = !older; });
  });

  /* ---------------- Backup plan: remote-only needs the upload switch ---------------- */
  document.addEventListener('change', e => {
    if (!e.target.matches || !e.target.matches('input[name=remote]')) return;
    const ro = e.target.form && e.target.form.querySelector('input[name=remoteOnly]');
    if (!ro) return;
    ro.disabled = !e.target.checked;
    if (!e.target.checked) ro.checked = false;
  });

  /* ---------------- World upload: progress, speed and remaining time ---------------- */
  const fmtBytes = n => {
    const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i < 2 ? Math.round(n) : n.toFixed(1).replace('.', decimalSep)) + ' ' + u[i];
  };
  const fmtTime = sec => {
    sec = Math.max(0, Math.round(sec));
    const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
    return h ? `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}` : `${m}:${String(s).padStart(2, '0')}`;
  };
  let up = null;
  const upEls = () => ({ box: $('#world-up'), name: $('#wu-name'), pct: $('#wu-pct'), bar: $('#wu-bar'), info: $('#wu-info'), cancel: $('#wu-cancel'), phase: $('#wu-phase') });
  const upEnd = () => {
    up = null;
    const e = upEls(); if (e.box) e.box.hidden = true;
    const f = $('#world-form'); if (f) f.reset();
  };
  document.body.addEventListener('htmx:beforeRequest', e => {
    if (!e.target || e.target.id !== 'world-form') return;
    const file = e.target.querySelector('input[type=file]').files[0], el = upEls();
    if (!file || !el.box) return;
    up = { start: performance.now(), total: file.size, samples: [] };
    el.name.textContent = file.name;
    el.pct.textContent = '0 %'; el.bar.style.width = '0';
    el.info.textContent = '0 B / ' + fmtBytes(file.size);
    el.cancel.hidden = false; el.phase.hidden = true; el.box.hidden = false;
  });
  document.body.addEventListener('htmx:xhr:progress', e => {
    if (!up || !e.target || e.target.id !== 'world-form') return;
    const el = upEls(), now = performance.now(), loaded = e.detail.loaded;
    const total = e.detail.lengthComputable ? e.detail.total : up.total;
    // Speed over the last ~5 s, so a short stall does not distort the remaining time.
    up.samples.push([now, loaded]);
    while (up.samples.length > 2 && now - up.samples[0][0] > 5000) up.samples.shift();
    const [t0, b0] = up.samples[0], dt = (now - t0) / 1000;
    const speed = dt > 0.3 ? (loaded - b0) / dt : 0;
    const pct = Math.min(100, Math.floor(loaded / total * 100));
    el.bar.style.width = (loaded / total * 100) + '%';
    el.pct.textContent = pct + ' %';
    let info = fmtBytes(loaded) + ' / ' + fmtBytes(total);
    if (loaded >= total) {
      up.done = true;
      el.phase.hidden = false; el.cancel.hidden = true;
    } else if (speed > 0) {
      info += ' · ' + fmtBytes(speed) + '/s · ' + msg('Uprest').replace('%s', fmtTime((total - loaded) / speed));
    }
    el.info.textContent = info;
  });
  document.body.addEventListener('htmx:afterRequest', e => { if (e.target && e.target.id === 'world-form') upEnd(); });
  document.addEventListener('click', e => {
    if (e.target.closest('#wu-cancel')) { const f = $('#world-form'); if (f) htmx.trigger(f, 'htmx:abort'); }
  });
  window.addEventListener('beforeunload', e => { if (up && !up.done) { e.preventDefault(); e.returnValue = msg('Upleave'); } });

  /* ---------------- Wizard ---------------- */
  const wiz = $('#wizard');
  if (wiz) {
    let step = 1;
    const steps = $$('.wiz-step', wiz);
    const kind = () => (($('input[name=kind]:checked', wiz) || {}).value) || 'software';
    const valid = () => {
      if (step === 2 && kind() !== 'software') return !!$(`select[name=pack_version_${kind()}]`, wiz);
      if (step === 3) return $('#w-name', wiz).value.trim() !== '';
      if (step === 5) return $('#w-eula', wiz).checked;
      return true;
    };
    const show = () => {
      steps.forEach(s => { s.hidden = +s.dataset.step !== step; });
      $$('#stepper li').forEach((li, i) => { li.className = i + 1 < step ? 'done' : i + 1 === step ? 'cur' : ''; });
      $$('[data-kind-only]', wiz).forEach(el => { el.hidden = el.dataset.kindOnly !== kind(); });
      $('#wiz-prev').style.visibility = step === 1 ? 'hidden' : 'visible';
      $('#wiz-next').hidden = step === 5;
      $('#wiz-create').hidden = step !== 5;
      update();
      if (step === 5) htmx.trigger('#w-summary', 'fc-preview');
      if (step === 3 && !$('#w-name', wiz).value) {
        const sug = kind() === 'software'
          ? ($('input[name=type]:checked', wiz).dataset.name + ' ' + $('#w-version', wiz).value)
          : (($(`input[name=pack_name_${kind()}]`, wiz) || {}).value || '');
        $('#w-name', wiz).value = sug;
        update();
      }
    };
    const update = () => { $('#wiz-next').disabled = !valid(); $('#wiz-create').disabled = !valid(); };
    wiz.addEventListener('input', update);
    wiz.addEventListener('change', update);
    document.body.addEventListener('htmx:afterSwap', update);
    $('#wiz-next').addEventListener('click', () => { if (valid() && step < 5) { step++; show(); } });
    $('#wiz-prev').addEventListener('click', () => { if (step > 1) { step--; show(); } });
    show();
  }

  /* ---------------- Custom Select ----------------
     Erweitert jedes select.input automatisch. Das native Element bleibt im DOM und
     erhält Wert + input/change-Events, damit Formulare und htmx unverändert funktionieren. */
  const CHEV = '<svg class="cs-chev" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m4 6 4 4 4-4"/></svg>';
  let csSeq = 0, csOpen = null;
  function csLabel(sel, btn) { const o = sel.options[sel.selectedIndex]; btn.innerHTML = `<span class="cs-val">${esc(o ? o.text : '')}</span>${CHEV}`; }
  function enhanceSelect(sel) {
    sel.dataset.cs = '1';
    const wrap = document.createElement('div'); wrap.className = 'cselect';
    const btn = document.createElement('button');
    btn.type = 'button'; btn.className = 'input cs-btn'; btn.disabled = sel.disabled;
    btn.setAttribute('role', 'combobox'); btn.setAttribute('aria-haspopup', 'listbox'); btn.setAttribute('aria-expanded', 'false');
    sel.parentNode.insertBefore(wrap, sel); wrap.append(btn, sel);
    sel.tabIndex = -1; sel.setAttribute('aria-hidden', 'true');
    csLabel(sel, btn);
    sel.addEventListener('change', () => csLabel(sel, btn));
  }
  function csShow(btn) {
    csClose();
    const sel = btn.nextElementSibling;
    const list = document.createElement('ul');
    list.className = 'cs-list'; list.setAttribute('role', 'listbox'); list.id = 'cs-list-' + (++csSeq);
    list.innerHTML = [...sel.options].map((o, i) => `<li class="cs-opt" role="option" id="${list.id}-${i}" data-i="${i}" aria-selected="${i === sel.selectedIndex}">${esc(o.text)}</li>`).join('');
    document.body.appendChild(list);
    btn.setAttribute('aria-expanded', 'true'); btn.setAttribute('aria-controls', list.id);
    csOpen = { sel, btn, list, idx: sel.selectedIndex };
    csPlace(); csActive(Math.max(0, sel.selectedIndex));
  }
  function csPlace() {
    if (!csOpen) return;
    const { btn, list } = csOpen;
    if (!btn.isConnected) return csClose();
    const r = btn.getBoundingClientRect(), below = innerHeight - r.bottom - 8, above = r.top - 8;
    list.style.minWidth = r.width + 'px';
    list.style.left = Math.max(8, Math.min(r.left, innerWidth - list.offsetWidth - 8)) + 'px';
    if (below < Math.min(list.scrollHeight, 280) && above > below) {
      list.style.top = ''; list.style.bottom = (innerHeight - r.top + 4) + 'px'; list.style.maxHeight = Math.min(280, above) + 'px';
    } else {
      list.style.bottom = ''; list.style.top = (r.bottom + 4) + 'px'; list.style.maxHeight = Math.min(280, below) + 'px';
    }
  }
  function csActive(i) {
    const { list, btn } = csOpen, opts = list.children;
    if (!opts.length) return;
    i = Math.max(0, Math.min(opts.length - 1, i)); csOpen.idx = i;
    [...opts].forEach((o, k) => o.classList.toggle('active', k === i));
    btn.setAttribute('aria-activedescendant', opts[i].id);
    opts[i].scrollIntoView({ block: 'nearest' });
  }
  function csPick(i) {
    const { sel, btn } = csOpen;
    csClose(); btn.focus();
    if (sel.selectedIndex === i) return;
    sel.selectedIndex = i;
    sel.dispatchEvent(new Event('input', { bubbles: true }));
    sel.dispatchEvent(new Event('change', { bubbles: true }));
  }
  function csClose() {
    if (!csOpen) return;
    csOpen.list.remove(); csOpen.btn.setAttribute('aria-expanded', 'false'); csOpen.btn.removeAttribute('aria-activedescendant');
    csOpen = null;
  }
  document.addEventListener('click', e => {
    const opt = e.target.closest('.cs-opt');
    if (opt && csOpen) return csPick(+opt.dataset.i);
    const btn = e.target.closest('.cs-btn');
    if (btn) return csOpen && csOpen.btn === btn ? csClose() : csShow(btn);
    if (csOpen && !e.target.closest('.cs-list')) csClose();
  }, true);
  document.addEventListener('mouseover', e => { const o = e.target.closest('.cs-opt'); if (o && csOpen) csActive(+o.dataset.i); });
  window.addEventListener('keydown', e => {
    const btn = e.target.closest && e.target.closest('.cs-btn'); if (!btn) return;
    const open = csOpen && csOpen.btn === btn;
    if (!open) { if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) { e.preventDefault(); csShow(btn); } return; }
    const n = csOpen.list.children.length;
    if (e.key === 'ArrowDown') { e.preventDefault(); csActive(csOpen.idx + 1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); csActive(csOpen.idx - 1); }
    else if (e.key === 'Home') { e.preventDefault(); csActive(0); }
    else if (e.key === 'End') { e.preventDefault(); csActive(n - 1); }
    else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); csPick(csOpen.idx); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); csClose(); }
    else if (e.key === 'Tab') csClose();
    else if (e.key.length === 1) {
      const opts = [...csOpen.list.children], ch = e.key.toLowerCase();
      for (let k = 1; k <= n; k++) { const j = (csOpen.idx + k) % n; if (opts[j].textContent.toLowerCase().startsWith(ch)) { csActive(j); break; } }
    }
  }, true);
  window.addEventListener('scroll', e => { if (csOpen && !csOpen.list.contains(e.target)) csPlace(); }, true);
  window.addEventListener('resize', csPlace);
  new MutationObserver(() => {
    $$('select.input:not([data-cs])').forEach(enhanceSelect);
    if (csOpen && !csOpen.btn.isConnected) csClose();
  }).observe(document.body, { childList: true, subtree: true });
  $$('select.input').forEach(enhanceSelect);
})();
