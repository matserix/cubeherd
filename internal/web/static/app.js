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
  document.addEventListener('input', e => {
    const f = e.target.closest('.member-filter');
    if (!f) return;
    const q = f.value.trim().toLowerCase();
    f.closest('.members').querySelectorAll('.memberlist .pick').forEach(l => { l.hidden = q !== '' && !l.dataset.name.includes(q); });
  });
  document.addEventListener('click', e => {
    if (e.target.closest('[data-close]') || e.target.id === 'modal') closeModal();
    const um = $('.usermenu[open]');
    if (um && !um.contains(e.target)) um.removeAttribute('open');
  });
  document.addEventListener('keydown', e => { if (e.key === 'Escape') { $('.usermenu[open]')?.removeAttribute('open'); if (modal() && !modal().hidden) closeModal(); } });

  /* ---------------- Confirm dialog ----------------
     Replaces window.confirm. Every element with hx-confirm gets it automatically; options come from
     data-dlg-* attributes (title, ok, tone="danger", note, icon, detail). "{}" in the hx-confirm text
     is replaced by the detail and shown as code. cubeConfirm({...}) returns a Promise<boolean>. */
  const SVG = d => `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${d}</svg>`;
  const ICONS = {
    question: SVG('<circle cx="8" cy="8" r="6.2"/><path d="M6.2 6.3a1.9 1.9 0 1 1 2.6 1.7c-.5.3-.8.7-.8 1.3M8 11.4v.1"/>'),
    upload: SVG('<path d="M8 10.5V2.5M5 5.5l3-3 3 3M2.5 10v3h11v-3"/>'),
    trash: SVG('<path d="M2.5 4h11M6 4V2.5h4V4M4 4l.6 9h6.8L12 4M6.5 6.5v4M9.5 6.5v4"/>'),
    shield: SVG('<path d="M8 1.8 3 3.6v4c0 3 2.1 5.2 5 6.6 2.9-1.4 5-3.6 5-6.6v-4z"/><path d="m5.8 8 1.6 1.6L10.4 6.6"/>'),
  };
  let confirmOpen = null;
  function cubeConfirm(o) {
    if (confirmOpen) confirmOpen(false);
    return new Promise(resolve => {
      const danger = o.tone === 'danger', prev = document.activeElement;
      const host = document.createElement('div');
      host.className = 'modal confirm' + (danger ? ' danger' : '');
      host.innerHTML = `<div class="modal-card" role="alertdialog" aria-modal="true" aria-labelledby="cf-title" aria-describedby="cf-msg">
        <div class="confirm-body"><div class="confirm-ico">${ICONS[o.icon] || ICONS.question}</div>
          <div class="confirm-text"><h2 id="cf-title"></h2><p id="cf-msg"></p></div></div>
        <div class="modal-foot"><button type="button" class="btn ghost" data-act="no"></button><button type="button" class="btn ${danger ? 'danger-solid' : 'primary'}" data-act="yes"></button></div></div>`;
      $('#cf-title', host).textContent = o.title || o.message || '';
      const msg = $('#cf-msg', host);
      if (o.title && o.message) {
        const [a, b] = o.detail && o.message.includes('{}') ? o.message.split('{}') : [o.message, null];
        msg.append(a);
        if (b !== null) { const c = document.createElement('span'); c.className = 'mono'; c.textContent = o.detail; msg.append(c, b); }
      } else msg.remove();
      if (o.note) {
        const n = document.createElement('div'); n.className = 'confirm-note';
        n.innerHTML = ICONS.shield; n.append(o.note); $('.confirm-text', host).append(n);
      }
      const no = $('[data-act=no]', host), yes = $('[data-act=yes]', host);
      no.textContent = o.cancel || document.body.dataset.tCancel;
      yes.textContent = o.ok || document.body.dataset.tOk;
      const done = ok => {
        confirmOpen = null; host.remove();
        document.removeEventListener('keydown', onKey, true);
        if (prev && prev.focus) prev.focus();
        resolve(ok);
      };
      const onKey = e => {
        if (e.key === 'Escape') { e.preventDefault(); e.stopImmediatePropagation(); done(false); }
        else if (e.key === 'Tab') {
          e.preventDefault();
          (document.activeElement === no ? yes : no).focus();
        }
      };
      host.addEventListener('click', e => {
        if (e.target === host) done(false);
        else if (e.target.closest('[data-act=no]')) done(false);
        else if (e.target.closest('[data-act=yes]')) done(true);
      });
      document.addEventListener('keydown', onKey, true);
      confirmOpen = done;
      document.body.append(host);
      (danger ? no : yes).focus();
    });
  }
  window.cubeConfirm = cubeConfirm;
  document.body.addEventListener('htmx:confirm', e => {
    const q = e.detail.question;
    if (!q) return;
    e.preventDefault();
    const el = e.detail.elt, d = el.dataset || {};
    const file = el.querySelector && el.querySelector('input[type=file]');
    const f = file && file.files[0];
    const detail = d.dlgDetail || (f ? f.name + ' (' + fmtBytes(f.size) + ')' : '');
    cubeConfirm({ message: q, detail, title: d.dlgTitle, ok: d.dlgOk, tone: d.dlgTone, note: d.dlgNote, icon: d.dlgIcon }).then(ok => {
      if (ok) e.detail.issueRequest(true);
      else if (el.tagName === 'FORM') el.reset();
    });
  });

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
  // A disabled file input also keeps its label from opening the file picker again.
  const upLock = (f, on) => { f.querySelector('input[type=file]').disabled = on; f.querySelector('label').setAttribute('aria-disabled', on); };
  const upEnd = () => {
    up = null;
    const e = upEls(); if (e.box) e.box.hidden = true;
    const f = $('#world-form'); if (f) { f.reset(); upLock(f, false); }
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
    upLock(e.target, true);
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

// White-label editor: the color pickers and the name restyle the two preview boxes while you type.
(function () {
  const soft = { accent: 1, ok: 1, warn: 1, crit: 1, info: 1 };
  const box = side => document.querySelector('.bpv[data-side="' + side + '"]');
  function paint(inp) {
    const b = box(inp.dataset.side), k = inp.dataset.var; if (!b) return;
    b.style.setProperty('--' + k, inp.value);
    if (soft[k]) b.style.setProperty('--' + k + '-soft', 'color-mix(in srgb,' + inp.value + ' ' + (inp.dataset.side === 'light' ? 10 : 14) + '%,transparent)');
  }
  document.addEventListener('input', e => {
    const t = e.target;
    if (t.matches && t.matches('input[type=color][data-var]')) paint(t);
    else if (t.matches && t.matches('[data-brand-name]')) document.querySelectorAll('[data-brand-name-pv]').forEach(n => { n.textContent = t.value.trim() || 'Cubeherd'; });
  });
  document.addEventListener('change', e => {
    const t = e.target;
    if (!(t.matches && t.matches('[data-brand-logo]')) || !t.files[0]) return;
    const rd = new FileReader();
    rd.onload = () => document.querySelectorAll('[data-brand-logo-pv]').forEach(i => { i.src = rd.result; i.hidden = false; const s = i.parentNode.querySelector('[data-brand-logo-pv-svg]'); if (s) s.hidden = true; });
    rd.readAsDataURL(t.files[0]);
  });
  document.addEventListener('click', e => {
    const b = e.target.closest && e.target.closest('[data-reset]'); if (!b) return;
    const inp = b.parentNode.querySelector('input[type=color]');
    inp.value = inp.dataset.def;
    inp.dispatchEvent(new Event('input', { bubbles: true })); // paint() above, and the color picker's own swatch below
  });
})();

/* ---------------- Custom Color Picker ----------------
   Enhances each input[type=color][data-var]. The native input stays in the form as the value holder (its
   value, and the input/change events the white-label preview and the reset button above already rely on),
   hidden behind a swatch button that opens a saturation/value area plus a hue slider and a hex field. */
(function () {
  const clamp = (v, a, b) => Math.max(a, Math.min(b, v));
  function hexToHsv(hex) {
    const n = /^#?([0-9a-f]{6})$/i.test(hex || '') ? parseInt(hex.replace('#', ''), 16) : 0;
    const r = (n >> 16 & 255) / 255, g = (n >> 8 & 255) / 255, b = (n & 255) / 255;
    const max = Math.max(r, g, b), min = Math.min(r, g, b), d = max - min;
    let h = 0;
    if (d) {
      if (max === r) h = ((g - b) / d) % 6;
      else if (max === g) h = (b - r) / d + 2;
      else h = (r - g) / d + 4;
      h *= 60; if (h < 0) h += 360;
    }
    return { h, s: max === 0 ? 0 : d / max, v: max };
  }
  function hsvToHex(h, s, v) {
    const c = v * s, x = c * (1 - Math.abs((h / 60) % 2 - 1)), m = v - c;
    const [r, g, b] = h < 60 ? [c, x, 0] : h < 120 ? [x, c, 0] : h < 180 ? [0, c, x] : h < 240 ? [0, x, c] : h < 300 ? [x, 0, c] : [c, 0, x];
    const to255 = n => Math.round((n + m) * 255).toString(16).padStart(2, '0');
    return '#' + to255(r) + to255(g) + to255(b);
  }

  let cp = null; // { inp, btn, pop, h, s, v }

  function swatch(btn, hex) { const sw = btn.querySelector('.cp-sw'); if (sw) sw.style.background = hex; }

  function enhanceColor(inp) {
    inp.dataset.cp = '1';
    const wrap = document.createElement('span'); wrap.className = 'cpick';
    const btn = document.createElement('button');
    btn.type = 'button'; btn.className = 'cp-btn'; btn.setAttribute('aria-haspopup', 'dialog'); btn.setAttribute('aria-expanded', 'false');
    btn.setAttribute('aria-label', inp.getAttribute('aria-label') || 'Farbe');
    btn.innerHTML = '<span class="cp-sw"></span>';
    swatch(btn, inp.value);
    inp.before(wrap); wrap.append(btn, inp);
    inp.tabIndex = -1; inp.setAttribute('aria-hidden', 'true');
  }

  function place() {
    if (!cp) return;
    const r = cp.btn.getBoundingClientRect();
    cp.pop.style.left = Math.max(8, Math.min(r.left, innerWidth - cp.pop.offsetWidth - 8)) + 'px';
    const below = innerHeight - r.bottom - 8;
    if (below < cp.pop.offsetHeight && r.top - 8 > below) { cp.pop.style.top = ''; cp.pop.style.bottom = (innerHeight - r.top + 6) + 'px'; }
    else { cp.pop.style.bottom = ''; cp.pop.style.top = (r.bottom + 6) + 'px'; }
  }

  function render() {
    const { pop, h, s, v } = cp;
    pop.querySelector('.cp-area').style.setProperty('--cp-h', h);
    pop.querySelector('.cp-cursor').style.left = (s * 100) + '%';
    pop.querySelector('.cp-cursor').style.top = ((1 - v) * 100) + '%';
    pop.querySelector('.cp-hue-cursor').style.left = (h / 360 * 100) + '%';
    const hex = hsvToHex(h, s, v);
    pop.querySelector('.cp-hex').value = hex.slice(1);
    pop.querySelector('.cp-pv').style.background = hex;
  }

  function commit() {
    const hex = hsvToHex(cp.h, cp.s, cp.v);
    if (cp.inp.value !== hex) {
      cp.inp.value = hex;
      cp.inp.dispatchEvent(new Event('input', { bubbles: true }));
      cp.inp.dispatchEvent(new Event('change', { bubbles: true }));
    }
    swatch(cp.btn, hex);
  }

  function open(btn) {
    if (cp && cp.btn === btn) return close();
    close();
    const inp = btn.nextElementSibling;
    const { h, s, v } = hexToHsv(inp.value);
    const pop = document.createElement('div');
    pop.className = 'cp-pop'; pop.setAttribute('role', 'dialog');
    pop.innerHTML = '<div class="cp-area"><div class="cp-cursor"></div></div>' +
      '<div class="cp-hue"><div class="cp-hue-cursor"></div></div>' +
      '<div class="cp-hex-row"><span class="cp-pv"></span><span class="cp-hash">#</span>' +
      '<input class="cp-hex" maxlength="7" spellcheck="false" autocomplete="off" aria-label="Hex"></div>';
    document.body.appendChild(pop);
    btn.setAttribute('aria-expanded', 'true');
    cp = { inp, btn, pop, h, s, v };
    render(); place();
    pop.querySelector('.cp-hex').addEventListener('input', e => {
      const t = e.target.value.replace(/^#/, '');
      if (!/^[0-9a-f]{6}$/i.test(t)) return;
      Object.assign(cp, hexToHsv('#' + t));
      render(); commit();
    });
    const drag = (track, onMove) => {
      track.addEventListener('pointerdown', e => {
        e.preventDefault();
        track.setPointerCapture(e.pointerId);
        const move = ev => { onMove(ev, track.getBoundingClientRect()); render(); commit(); };
        move(e);
        const up = () => { track.removeEventListener('pointermove', move); track.removeEventListener('pointerup', up); };
        track.addEventListener('pointermove', move);
        track.addEventListener('pointerup', up, { once: true });
      });
    };
    drag(pop.querySelector('.cp-area'), (e, r) => {
      cp.s = clamp((e.clientX - r.left) / r.width, 0, 1);
      cp.v = clamp(1 - (e.clientY - r.top) / r.height, 0, 1);
    });
    drag(pop.querySelector('.cp-hue'), (e, r) => { cp.h = clamp((e.clientX - r.left) / r.width, 0, 1) * 360; });
    btn.focus();
  }
  function close() { if (cp) { cp.pop.remove(); cp.btn.setAttribute('aria-expanded', 'false'); cp = null; } }

  document.addEventListener('click', e => {
    const btn = e.target.closest('.cp-btn');
    if (btn) return open(btn);
    if (cp && !e.target.closest('.cp-pop')) close();
  });
  document.addEventListener('keydown', e => { if (e.key === 'Escape' && cp) { close(); } });
  window.addEventListener('scroll', () => place(), true);
  window.addEventListener('resize', () => place());
  // the reset button and a hand-edited .value both go through a real input event (see above), so re-reading
  // the value here keeps the swatch (and an open popover) in sync without the picker needing to know why it changed.
  document.addEventListener('input', e => {
    const t = e.target;
    if (!(t.matches && t.matches('input[type=color][data-cp]'))) return;
    const btn = t.previousElementSibling;
    if (btn && btn.classList.contains('cp-btn')) swatch(btn, t.value);
    if (cp && cp.inp === t) { Object.assign(cp, hexToHsv(t.value)); render(); }
  });
  new MutationObserver(() => {
    document.querySelectorAll('input[type=color][data-var]:not([data-cp])').forEach(enhanceColor);
    if (cp && !cp.btn.isConnected) close();
  }).observe(document.body, { childList: true, subtree: true });
  document.querySelectorAll('input[type=color][data-var]').forEach(enhanceColor);
})();

// Custom file picker: the native <input type=file class=input> stays in the form as the value holder (submit, htmx,
// validation), what people see is a drop zone with the chosen file. Dropping a file sets it on the input.
(function () {
  const T = k => document.body.dataset['t' + k[0].toUpperCase() + k.slice(1)] || '';
  const size = n => n < 1024 ? n + ' B' : n < 1048576 ? (n / 1024).toFixed(0) + ' KB' : (n / 1048576).toFixed(1) + ' MB';
  const ICON = '<svg viewBox="0 0 20 20" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><path d="M10 13V4m0 0L6.5 7.5M10 4l3.5 3.5M4 13v2.5A1.5 1.5 0 0 0 5.5 17h9a1.5 1.5 0 0 0 1.5-1.5V13"/></svg>';
  const accepts = (inp, f) => {
    const a = (inp.accept || '').split(',').map(s => s.trim().toLowerCase()).filter(Boolean);
    if (!a.length) return true;
    const n = f.name.toLowerCase(), t = (f.type || '').toLowerCase();
    return a.some(p => p.startsWith('.') ? n.endsWith(p) : p.endsWith('/*') ? t.startsWith(p.slice(0, -1)) : t === p);
  };
  function enhance(inp) {
    if (inp.dataset.fp) return;
    inp.dataset.fp = '1';
    const wrap = document.createElement('label');
    wrap.className = 'filepick';
    inp.parentNode.insertBefore(wrap, inp);
    wrap.appendChild(inp);
    inp.classList.add('fp-native');
    inp.classList.remove('input');
    const zone = document.createElement('span');
    zone.className = 'fp-zone';
    zone.innerHTML = '<span class="fp-ico">' + ICON + '</span><span class="fp-text"><b class="fp-main"></b><span class="fp-sub"></span></span>';
    const clear = document.createElement('button');
    clear.type = 'button'; clear.className = 'fp-clear'; clear.hidden = true; clear.textContent = '×';
    clear.title = clear.ariaLabel = T('fpclear');
    zone.appendChild(clear);
    wrap.appendChild(zone);
    const main = zone.querySelector('.fp-main'), sub = zone.querySelector('.fp-sub');
    const show = () => {
      const fs = [...inp.files];
      wrap.classList.toggle('has-file', fs.length > 0);
      clear.hidden = !fs.length;
      if (!fs.length) { main.textContent = T(inp.multiple ? 'fpchoosemany' : 'fpchoose'); sub.textContent = T('fpdrop'); return; }
      main.textContent = fs.length === 1 ? fs[0].name : fs.length + ' ' + T('fpfiles');
      sub.textContent = size(fs.reduce((s, f) => s + f.size, 0));
    };
    inp.addEventListener('change', show);
    inp.addEventListener('focus', () => wrap.classList.add('focus'));
    inp.addEventListener('blur', () => wrap.classList.remove('focus'));
    clear.addEventListener('click', e => { e.preventDefault(); e.stopPropagation(); inp.value = ''; inp.dispatchEvent(new Event('change', { bubbles: true })); });
    let depth = 0;
    wrap.addEventListener('dragenter', e => { e.preventDefault(); depth++; wrap.classList.add('drag'); });
    wrap.addEventListener('dragleave', () => { if (--depth <= 0) { depth = 0; wrap.classList.remove('drag'); } });
    wrap.addEventListener('dragover', e => { e.preventDefault(); });
    wrap.addEventListener('drop', e => {
      e.preventDefault(); depth = 0; wrap.classList.remove('drag');
      if (inp.disabled) return;
      let fs = [...e.dataTransfer.files].filter(f => accepts(inp, f));
      if (!inp.multiple) fs = fs.slice(0, 1);
      if (!fs.length) return;
      const dt = new DataTransfer(); fs.forEach(f => dt.items.add(f));
      inp.files = dt.files;
      inp.dispatchEvent(new Event('change', { bubbles: true }));
    });
    if (inp.form) inp.form.addEventListener('reset', () => setTimeout(show));
    show();
  }
  const run = () => document.querySelectorAll('input[type=file].input:not([data-fp])').forEach(enhance);
  new MutationObserver(run).observe(document.body, { childList: true, subtree: true });
  run();
})();
