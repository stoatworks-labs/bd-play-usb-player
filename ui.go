package main

// The browser-side halves: the script injected into birdUI, and bdplay's own
// standalone control page.

// injectJS runs inside birdUI's videoset page. It must be defensive: it shares
// a document with BirdDog's own jQuery and its form-submitting select handlers,
// and it must leave the page working normally when bdplay is not running.
const injectJS = `
(function () {
  'use strict';
  if (window.__bdplayLoaded) return;
  window.__bdplayLoaded = true;

  var API = location.protocol + '//' + location.hostname + ':` + defaultPort + `';
  var POLL_MS = 2000;
  var sel = null, panel = null, timer = null, lastLib = null;

  function api(path, opts) {
    return fetch(API + path, opts || {}).then(function (r) {
      if (!r.ok) return r.json().catch(function(){return {error:'HTTP '+r.status};})
                              .then(function(j){ throw new Error(j.error || ('HTTP '+r.status)); });
      return r.json();
    });
  }
  function post(path, body) {
    return api(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {})
    });
  }

  function findSelect() {
    return document.getElementById('decode_SourceSelection') ||
           document.querySelector('select[name="decode_SourceSelection"]');
  }

  // The stock select submits its form on change. We intercept in the capture
  // phase so our handler runs before BirdDog's inline onchange, and stop
  // propagation for USB only — every other value must behave exactly as it
  // did before we existed.
  function hookSelect(s) {
    s.addEventListener('change', function (ev) {
      if (s.value !== 'USB') { hidePanel(); return; }
      ev.stopImmediatePropagation();
      ev.preventDefault();
      showPanel();
    }, true);
  }

  function css() {
    if (document.getElementById('bdplay-css')) return;
    var st = document.createElement('style');
    st.id = 'bdplay-css';
    st.textContent = [
      '.bdplay-panel{border:1px solid #444;border-radius:6px;padding:12px;margin:10px 0;background:rgba(0,0,0,.15)}',
      '.bdplay-row{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin:6px 0}',
      '.bdplay-row label{margin:0 6px 0 0}',
      '.bdplay-list{max-height:260px;overflow:auto;border:1px solid #555;border-radius:4px;padding:4px;font-family:monospace;font-size:12px}',
      '.bdplay-list div{padding:3px 6px;cursor:pointer;border-radius:3px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}',
      '.bdplay-list div:hover{background:rgba(128,128,128,.25)}',
      '.bdplay-list div.sel{background:#0a7;color:#fff}',
      '.bdplay-list div.skip{opacity:.5;cursor:not-allowed}',
      '.bdplay-status{font-size:12px;opacity:.85;margin-top:8px}',
      '.bdplay-err{color:#e55;font-size:12px;margin-top:6px}',
      '.bdplay-btn{padding:4px 12px;border-radius:4px;border:1px solid #666;background:#333;color:#eee;cursor:pointer}',
      '.bdplay-btn:hover{background:#444}',
      '.bdplay-btn[disabled]{opacity:.4;cursor:default}'
    ].join('\n');
    document.head.appendChild(st);
  }

  function el(tag, attrs, kids) {
    var e = document.createElement(tag);
    for (var k in (attrs || {})) {
      if (k === 'class') e.className = attrs[k];
      else if (k === 'text') e.textContent = attrs[k];
      else if (k.slice(0, 2) === 'on') e.addEventListener(k.slice(2), attrs[k]);
      else e.setAttribute(k, attrs[k]);
    }
    (kids || []).forEach(function (c) { if (c) e.appendChild(c); });
    return e;
  }

  function buildPanel() {
    css();
    var p = el('div', { class: 'bdplay-panel', id: 'bdplay-panel' });

    var head = el('div', { class: 'bdplay-row' }, [
      el('strong', { text: 'USB Media Player' }),
      el('span', { id: 'bdplay-vol', class: 'bdplay-status' })
    ]);

    var list = el('div', { class: 'bdplay-list', id: 'bdplay-list' });

    var orderSel = el('select', { id: 'bdplay-order' }, [
      el('option', { value: 'order', text: 'Play in order' }),
      el('option', { value: 'random', text: 'Random' })
    ]);
    var loopBox = el('input', { type: 'checkbox', id: 'bdplay-loop' });
    loopBox.checked = true;
    var dwell = el('input', { type: 'number', id: 'bdplay-dwell', min: '1', max: '600', value: '10', style: 'width:70px' });
    var recurse = el('input', { type: 'checkbox', id: 'bdplay-recurse' });
    recurse.checked = true;

    var opts = el('div', { class: 'bdplay-row' }, [
      orderSel,
      el('label', {}, [loopBox, document.createTextNode(' Loop')]),
      el('label', {}, [recurse, document.createTextNode(' Include subfolders')]),
      el('label', { text: 'Stills (s):' }), dwell
    ]);

    var buttons = el('div', { class: 'bdplay-row' }, [
      el('button', { class: 'bdplay-btn', id: 'bdplay-play', text: 'PLAY', onclick: doPlay }),
      el('button', { class: 'bdplay-btn', id: 'bdplay-stop', text: 'STOP', onclick: function () { post('/api/stop').then(refresh).catch(showErr); } }),
      el('button', { class: 'bdplay-btn', text: '‹ PREV', onclick: function () { post('/api/prev').then(refresh).catch(showErr); } }),
      el('button', { class: 'bdplay-btn', text: 'NEXT ›', onclick: function () { post('/api/next').then(refresh).catch(showErr); } }),
      el('button', { class: 'bdplay-btn', text: 'RESCAN', onclick: function () { post('/api/rescan').then(loadLibrary).catch(showErr); } }),
      el('button', { class: 'bdplay-btn', text: 'EJECT', onclick: function () { post('/api/eject').then(loadLibrary).catch(showErr); } })
    ]);

    p.appendChild(head);
    p.appendChild(list);
    p.appendChild(opts);
    p.appendChild(buttons);
    p.appendChild(el('div', { class: 'bdplay-status', id: 'bdplay-status' }));
    p.appendChild(el('div', { class: 'bdplay-err', id: 'bdplay-err' }));
    return p;
  }

  var selected = null;      // {rel, isFolder}

  function loadLibrary() {
    return api('/api/library').then(function (lib) {
      lastLib = lib;
      var list = document.getElementById('bdplay-list');
      if (!list) return;
      list.innerHTML = '';

      list.appendChild(row('▶ Everything on the stick', '', true, false));
      (lib.folders || []).forEach(function (f) {
        if (f.rel === '') return;
        list.appendChild(row('📁 ' + f.rel + '  (' + f.total + ')', f.rel, true, false));
      });
      (lib.items || []).forEach(function (it) {
        var icon = it.kind === 'video' ? '🎬' : (it.kind === 'pdf' ? '📄' : '🖼');
        list.appendChild(row(icon + ' ' + it.rel, it.rel, false, !!it.skipped, it.skipped));
      });
      if (!(lib.items || []).length) {
        list.appendChild(el('div', { class: 'skip', text: lib.error ? ('Error: ' + lib.error) : 'No playable files found.' }));
      }
    }).catch(showErr);
  }

  function row(label, rel, isFolder, skipped, why) {
    var d = el('div', { text: label, title: why || rel });
    if (skipped) { d.className = 'skip'; return d; }
    d.addEventListener('click', function () {
      selected = { rel: rel, isFolder: isFolder };
      Array.prototype.forEach.call(d.parentNode.children, function (c) { c.classList.remove('sel'); });
      d.classList.add('sel');
    });
    if (selected && selected.rel === rel && selected.isFolder === isFolder) d.classList.add('sel');
    return d;
  }

  function doPlay() {
    var s = selected || { rel: '', isFolder: true };
    showErr(null);
    post('/api/play', {
      path: s.rel,
      recurse: document.getElementById('bdplay-recurse').checked,
      order: document.getElementById('bdplay-order').value,
      loop: document.getElementById('bdplay-loop').checked,
      dwell_sec: parseInt(document.getElementById('bdplay-dwell').value, 10) || 10
    }).then(refresh).catch(showErr);
  }

  function showErr(e) {
    var box = document.getElementById('bdplay-err');
    if (!box) return;
    box.textContent = e ? ('' + (e.message || e)) : '';
  }

  function refresh() {
    return api('/api/status').then(function (st) {
      var vol = document.getElementById('bdplay-vol');
      var stat = document.getElementById('bdplay-status');
      if (vol) {
        vol.textContent = st.mounted
          ? ('— ' + (st.label || st.mount_point) + ' (' + st.fs_type + '), ' + st.item_count + ' file(s)')
          : '— no USB drive detected';
      }
      if (stat) {
        var p = st.player;
        var bits = [];
        bits.push('State: ' + p.state);
        if (p.state === 'playing' && p.current && p.current.name) {
          bits.push('Now: ' + p.current.name);
          bits.push('Item ' + p.position + '/' + p.total);
          if (p.cycles) bits.push('Loop ' + (p.cycles + 1));
        }
        bits.push('Output: ' + p.width + 'x' + p.height);
        if (!st.pdf_ready) bits.push('PDF: not installed');
        if (!st.exfat_ready) bits.push('exFAT: unsupported');
        stat.textContent = bits.join(' · ');
        if (p.error) showErr(new Error(p.error));
      }
      // Keep the dropdown showing USB while we own the display, so the page
      // reflects reality after a reload.
      if (sel && st.player.state === 'playing' && sel.value !== 'USB') sel.value = 'USB';
    }).catch(function (e) {
      var stat = document.getElementById('bdplay-status');
      if (stat) stat.textContent = 'bdplay unreachable — is the bd-play service running?';
    });
  }

  function showPanel() {
    if (!panel) {
      panel = buildPanel();
      // Drop the panel just after the row containing the select, which keeps
      // it inside birdUI's own column layout.
      var host = sel.closest ? sel.closest('.row') : null;
      if (host && host.parentNode) host.parentNode.insertBefore(panel, host.nextSibling);
      else sel.parentNode.appendChild(panel);
    }
    panel.style.display = '';
    loadLibrary().then(refresh);
    if (!timer) timer = setInterval(refresh, POLL_MS);
  }

  function hidePanel() {
    if (panel) panel.style.display = 'none';
    if (timer) { clearInterval(timer); timer = null; }
  }

  function init() {
    sel = findSelect();
    if (!sel) return;
    if (!sel.querySelector('option[value="USB"]')) {
      sel.appendChild(el('option', { value: 'USB', text: 'USB' }));
    }
    hookSelect(sel);
    // If we are already playing, restore the USB view on load.
    api('/api/status').then(function (st) {
      if (st.player && st.player.state === 'playing') { sel.value = 'USB'; showPanel(); }
    }).catch(function () {});
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
`

// defaultPort must match the default -addr flag; it is baked into injectJS.
const defaultPort = "8091"

// controlPageHTML is bdplay's own page, served at http://<unit>:8091/. It is
// the fallback when birdUI cannot be patched, and the place to look when
// something is wrong — it shows the log, which the injected panel does not.
const controlPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>BirdDog PLAY — USB Media Player</title>
<style>
  :root { color-scheme: dark; --bg:#16181c; --fg:#e8e8ea; --mut:#9aa0a6; --line:#33373d; --acc:#00a878; }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg); font:14px/1.5 -apple-system,Segoe UI,Roboto,sans-serif; }
  header { padding:14px 20px; border-bottom:1px solid var(--line); display:flex; gap:12px; align-items:baseline; flex-wrap:wrap; }
  h1 { font-size:16px; margin:0; font-weight:600; }
  .mut { color:var(--mut); font-size:12px; }
  main { padding:20px; max-width:900px; }
  .card { border:1px solid var(--line); border-radius:8px; padding:14px; margin-bottom:16px; }
  .row { display:flex; gap:10px; align-items:center; flex-wrap:wrap; margin:8px 0; }
  button { padding:6px 14px; border-radius:5px; border:1px solid var(--line); background:#24272c; color:var(--fg); cursor:pointer; font:inherit; }
  button:hover { background:#2c3037; }
  button.primary { background:var(--acc); border-color:var(--acc); color:#04120d; font-weight:600; }
  select, input[type=number] { background:#24272c; color:var(--fg); border:1px solid var(--line); border-radius:5px; padding:5px 8px; font:inherit; }
  #list { max-height:340px; overflow:auto; border:1px solid var(--line); border-radius:6px; font-family:ui-monospace,Menlo,monospace; font-size:12px; }
  #list div { padding:5px 10px; cursor:pointer; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  #list div:hover { background:#22252a; }
  #list div.sel { background:var(--acc); color:#04120d; }
  #list div.skip { opacity:.45; cursor:not-allowed; }
  #err { color:#ff6b6b; min-height:1.2em; }
  #log { max-height:220px; overflow:auto; font-family:ui-monospace,Menlo,monospace; font-size:11px; color:var(--mut); white-space:pre-wrap; }
  table { border-collapse:collapse; width:100%; font-size:13px; }
  td { padding:3px 0; }
  td:first-child { color:var(--mut); width:150px; }
</style>
</head>
<body>
<header>
  <h1>USB Media Player</h1>
  <span class="mut" id="host"></span>
  <span class="mut" id="ver"></span>
</header>
<main>
  <div class="card">
    <div class="row"><strong>Drive</strong><span class="mut" id="vol">checking…</span></div>
    <div id="list"></div>
    <div class="row">
      <select id="order"><option value="order">Play in order</option><option value="random">Random</option></select>
      <label><input type="checkbox" id="loop" checked> Loop</label>
      <label><input type="checkbox" id="recurse" checked> Include subfolders</label>
      <label>Stills (s): <input type="number" id="dwell" value="10" min="1" max="600" style="width:72px"></label>
    </div>
    <div class="row">
      <button class="primary" id="play">PLAY</button>
      <button id="stop">STOP</button>
      <button id="prev">‹ PREV</button>
      <button id="next">NEXT ›</button>
      <button id="rescan">RESCAN</button>
      <button id="eject">EJECT</button>
    </div>
    <div id="err"></div>
  </div>

  <div class="card">
    <strong>Status</strong>
    <table id="status"></table>
  </div>

  <div class="card">
    <strong>Log</strong>
    <div id="log"></div>
  </div>
</main>
<script>
var selected = null;
function api(p, o) {
  return fetch(p, o || {}).then(function (r) {
    if (!r.ok) return r.json().catch(function(){return {error:'HTTP '+r.status};}).then(function(j){ throw new Error(j.error||('HTTP '+r.status)); });
    return r.json();
  });
}
function post(p, b) { return api(p, { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(b||{}) }); }
function err(e) { document.getElementById('err').textContent = e ? (e.message || e) : ''; }

function loadLibrary() {
  return api('/api/library').then(function (lib) {
    var list = document.getElementById('list');
    list.innerHTML = '';
    list.appendChild(row('▶ Everything on the stick', '', true, false));
    (lib.folders||[]).forEach(function (f) { if (f.rel) list.appendChild(row('📁 ' + f.rel + '  (' + f.total + ')', f.rel, true, false)); });
    (lib.items||[]).forEach(function (it) {
      var icon = it.kind === 'video' ? '🎬' : (it.kind === 'pdf' ? '📄' : '🖼');
      list.appendChild(row(icon + ' ' + it.rel, it.rel, false, !!it.skipped, it.skipped));
    });
    if (!(lib.items||[]).length) {
      var d = document.createElement('div'); d.className = 'skip';
      d.textContent = lib.error ? ('Error: ' + lib.error) : 'No playable files found.';
      list.appendChild(d);
    }
  }).catch(err);
}
function row(label, rel, isFolder, skipped, why) {
  var d = document.createElement('div');
  d.textContent = label; d.title = why || rel;
  if (skipped) { d.className = 'skip'; return d; }
  d.onclick = function () {
    selected = { rel: rel, isFolder: isFolder };
    Array.prototype.forEach.call(d.parentNode.children, function (c) { c.classList.remove('sel'); });
    d.classList.add('sel');
  };
  return d;
}
function refresh() {
  return api('/api/status').then(function (st) {
    document.getElementById('ver').textContent = 'bdplay ' + st.version;
    document.getElementById('vol').textContent = st.mounted
      ? ((st.label || st.mount_point) + ' · ' + st.fs_type + ' · ' + st.item_count + ' playable file(s)')
      : 'no USB drive detected';
    var p = st.player, rows = [
      ['State', p.state],
      ['Now playing', p.current && p.current.name ? p.current.name : '—'],
      ['Position', p.total ? (p.position + ' / ' + p.total + (p.cycles ? '  (loop ' + (p.cycles+1) + ')' : '')) : '—'],
      ['Order', p.order || '—'],
      ['Output', p.width + '×' + p.height],
      ['PDF rendering', st.pdf_ready ? 'available' : 'not installed'],
      ['exFAT', st.exfat_ready ? 'available' : 'not supported by this kernel']
    ];
    document.getElementById('status').innerHTML = rows.map(function (r) {
      return '<tr><td>' + r[0] + '</td><td>' + String(r[1]).replace(/[<>&]/g, '') + '</td></tr>';
    }).join('');
    if (p.error) err(new Error(p.error));
  }).catch(function () { document.getElementById('vol').textContent = 'bdplay unreachable'; });
}
function loadLog() {
  return api('/api/log').then(function (lines) {
    document.getElementById('log').textContent = (lines||[]).map(function (l) {
      return new Date(l.time).toLocaleTimeString() + '  [' + l.level + ']  ' + l.text;
    }).join('\n');
    var box = document.getElementById('log'); box.scrollTop = box.scrollHeight;
  }).catch(function(){});
}
document.getElementById('play').onclick = function () {
  var s = selected || { rel: '', isFolder: true };
  err(null);
  post('/api/play', {
    path: s.rel,
    recurse: document.getElementById('recurse').checked,
    order: document.getElementById('order').value,
    loop: document.getElementById('loop').checked,
    dwell_sec: parseInt(document.getElementById('dwell').value, 10) || 10
  }).then(refresh).catch(err);
};
document.getElementById('stop').onclick   = function () { post('/api/stop').then(refresh).catch(err); };
document.getElementById('next').onclick   = function () { post('/api/next').then(refresh).catch(err); };
document.getElementById('prev').onclick   = function () { post('/api/prev').then(refresh).catch(err); };
document.getElementById('rescan').onclick = function () { post('/api/rescan').then(loadLibrary).catch(err); };
document.getElementById('eject').onclick  = function () { post('/api/eject').then(loadLibrary).catch(err); };

document.getElementById('host').textContent = location.hostname;
loadLibrary(); refresh(); loadLog();
setInterval(refresh, 2000);
setInterval(loadLog, 5000);
</script>
</body>
</html>
`
