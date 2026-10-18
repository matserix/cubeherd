// Forgecraft – kleines Client-Skript: Custom Selects, Wizard-Schritte, Modals, Toasts, Konsole.
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
  document.body.addEventListener('htmx:responseError', () => toast('Anfrage fehlgeschlagen. Ist Forgecraft erreichbar?', 'crit'));
  document.body.addEventListener('htmx:sendError', () => toast('Keine Verbindung zu Forgecraft.', 'crit'));

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
    return el.value + ' GB · Heap ' + heapGB(+el.value, modded).toFixed(1).replace('.', ',') + ' GB';
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
