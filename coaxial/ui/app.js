/* Coaxial UI. Plain JavaScript, no dependencies, no outside requests. */
(function () {
  'use strict';

  var params = new URLSearchParams(location.search);

  // ---------- small helpers ----------
  function $(sel, root) { return (root || document).querySelector(sel); }
  function add(el, kid) {
    if (kid == null || kid === false) return;
    if (Array.isArray(kid)) { kid.forEach(function (k) { add(el, k); }); return; }
    el.appendChild(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  function h(tag, attrs) {
    var el = document.createElement(tag), k, v, i;
    for (k in attrs || {}) {
      v = attrs[k];
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'html') el.innerHTML = v; // fixed icon markup only, never user data
      else if (k.slice(0, 2) === 'on') el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? '' : v);
    }
    for (i = 2; i < arguments.length; i++) add(el, arguments[i]);
    return el;
  }
  function setText(el, text) { if (el.textContent !== text) el.textContent = text; }
  function clone(o) { return JSON.parse(JSON.stringify(o)); }

  var PATHS = {
    plus: '<path d="M12 5v14M5 12h14"/>',
    up: '<path d="M12 19V5M6 11l6-6 6 6"/>',
    down: '<path d="M12 5v14M6 13l6 6 6-6"/>',
    trash: '<path d="M4 7h16M10 11v6M14 11v6M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12M9 7V4h6v3"/>',
    eye: '<path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/>',
    eyeOff: '<path d="M3 3l18 18M10.6 5.1A10.9 10.9 0 0 1 12 5c6.4 0 10 7 10 7a17.6 17.6 0 0 1-3.2 4.1M6.5 6.6C3.6 8.5 2 12 2 12s3.6 7 10 7c1.8 0 3.4-.5 4.8-1.3M9.9 9.9a3 3 0 0 0 4.2 4.2"/>',
    check: '<path d="M5 12.5l4.5 4.5L19 7.5"/>',
    alert: '<circle cx="12" cy="12" r="9"/><path d="M12 8v5M12 16.2v.3"/>',
    info: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 7.8v.3"/>',
    chevron: '<path d="M6 9l6 6 6-6"/>',
    arrow: '<path d="M5 12h14M13 6l6 6-6 6"/>',
    x: '<path d="M6 6l12 12M18 6L6 18"/>',
    refresh: '<path d="M20 11a8 8 0 0 0-14.3-4.3L4 8.5M4 4v4.5h4.5M4 13a8 8 0 0 0 14.3 4.3L20 15.5M20 20v-4.5h-4.5"/>'
  };
  function icon(name, size) {
    size = size || 16;
    return '<svg class="ic" width="' + size + '" height="' + size + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" ' +
      'stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + PATHS[name] + '</svg>';
  }

  async function api(method, path, body) {
    var res = await fetch(path, {
      method: method,
      cache: 'no-store',
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined
    });
    var data = null;
    try { data = await res.json(); } catch (e) { /* not JSON */ }
    return { ok: res.ok, status: res.status, data: data };
  }

  var toastTimer;
  function toast(message, bad) {
    var el = $('#toast');
    clearTimeout(toastTimer);
    el.className = 'toast' + (bad ? ' bad' : '');
    el.innerHTML = icon(bad ? 'alert' : 'check', 18);
    el.appendChild(h('span', { text: message }));
    el.hidden = false;
    toastTimer = setTimeout(function () {
      el.classList.add('out');
      toastTimer = setTimeout(function () { el.hidden = true; }, 260);
    }, 4200);
  }

  // ---------- status view ----------
  var st = { active: false, timer: null, clock: null, data: null, at: 0, fails: 0, pings: [] };
  var FORMAT = { ts: 'MPEG-TS', hls: 'HLS' };
  // Ring colors from the logo, one per provider. Green and rose come last because they also mean up and down.
  var RING = ['#22D3EE', '#E879F9', '#3B82F6', '#FFD60A', '#34D399', '#F43F5E'];

  function fmtDuration(total) {
    total = Math.max(0, Math.floor(total));
    var hrs = Math.floor(total / 3600), min = Math.floor(total % 3600 / 60), sec = total % 60;
    function two(n) { return n < 10 ? '0' + n : '' + n; }
    if (hrs) return hrs + 'h ' + two(min) + 'm ' + two(sec) + 's';
    if (min) return min + 'm ' + two(sec) + 's';
    return sec + 's';
  }
  function fmtData(mb) {
    mb = Number(mb) || 0;
    if (mb >= 1024) return (mb / 1024).toFixed(2) + ' GB';
    return (mb >= 100 ? Math.round(mb) : mb.toFixed(1)) + ' MB';
  }
  function plural(n, word) { return n + ' ' + word + (n === 1 ? '' : 's'); }
  function fmtAgo(iso) {
    var t = Date.parse(iso);
    if (isNaN(t)) return '';
    var s = Math.max(0, (Date.now() - t) / 1000);
    if (s < 60) return 'just now';
    if (s < 3600) return plural(Math.floor(s / 60), 'minute') + ' ago';
    if (s < 86400) return plural(Math.floor(s / 3600), 'hour') + ' ago';
    return plural(Math.floor(s / 86400), 'day') + ' ago';
  }

  // Keyed list update: rows keep their DOM nodes so nothing jumps between polls.
  function reconcile(box, items, keyOf, create, update) {
    var old = {}, prev = null;
    Array.prototype.slice.call(box.children).forEach(function (el) {
      if (el.dataset.key == null) el.remove(); else old[el.dataset.key] = el;
    });
    items.forEach(function (item) {
      var key = keyOf(item), el = old[key];
      if (el) delete old[key];
      else { el = create(item); el.dataset.key = key; if (st.painted) el.classList.add('enter'); }
      update(el, item);
      var want = prev ? prev.nextSibling : box.firstChild;
      if (el !== want) box.insertBefore(el, want);
      prev = el;
    });
    Object.keys(old).forEach(function (k) { old[k].remove(); });
  }

  function drawSpark() {
    var pts = st.pings, line = $('#spark-line'), area = $('#spark-area');
    var vals = pts.filter(function (p) { return p != null; });
    if (vals.length < 2) { line.setAttribute('d', ''); area.setAttribute('d', ''); return; }
    var lo = Math.min.apply(null, vals), hi = Math.max.apply(null, vals);
    var pad = Math.max(4, (hi - lo) * 0.3); lo -= pad; hi += pad;
    var step = 120 / (Math.max(pts.length, 10) - 1), x0 = 120 - (pts.length - 1) * step, d = '', first = null, last = 0, pen = false;
    pts.forEach(function (p, i) {
      if (p == null) { pen = false; return; }
      var x = (x0 + i * step).toFixed(1), y = (26 - (p - lo) / (hi - lo) * 24).toFixed(1);
      d += (pen ? 'L' : 'M') + x + ' ' + y;
      if (first == null) first = x;
      last = x; pen = true;
    });
    line.setAttribute('d', d);
    area.setAttribute('d', d.indexOf('M', 1) < 0 ? d + 'L' + last + ' 28L' + first + ' 28Z' : '');
  }

  function renderLive() {
    var live = $('#live'), lost = st.fails >= 2;
    live.dataset.state = lost ? 'lost' : st.data ? 'live' : 'wait';
    setText($('#live-text'), lost ? 'Reconnecting' : st.data ? 'Live' : 'Connecting');
    $('#status-error').hidden = !lost;
    $('#status-body').classList.toggle('stale', lost);
  }

  // The VPN client (its program is fetched at start; a newer one is looked for every hour).
  var progBusy = '';
  function when(iso) {
    var t = Date.parse(iso || ''); if (!t) return '';
    var m = Math.max(0, Math.round((Date.now() - t) / 60000));
    return m < 1 ? 'just now' : m < 60 ? m + ' min ago' : Math.round(m / 60) + ' h ago';
  }
  function paintProgram(vpn) {
    var p = vpn.program, u = vpn.update || {}, box = $('#vpn-prog');
    if (!p || !p.version) { box.hidden = true; return; }
    box.hidden = false;
    var chk = $('#vpn-prog-check'), upd = $('#vpn-prog-update');
    if (!chk.dataset.ready) {
      chk.dataset.ready = '1';
      chk.querySelector('.ico').innerHTML = icon('refresh', 14);
      upd.querySelector('.ico').innerHTML = icon('up', 14);
    }
    setText($('#vpn-prog-ver'), 'gluetun ' + p.version);
    var sv = vpn.server, pickLine = $('#vpn-pick'), pickBtn = $('#vpn-pick-go');
    pickLine.hidden = !sv;
    if (sv) {
      if (!pickBtn.dataset.ready) { pickBtn.dataset.ready = '1'; pickBtn.querySelector('.ico').innerHTML = icon('refresh', 14); }
      var most = sv.most || 8;
      setText($('#vpn-pick-text'), sv.testing ? 'Trying up to ' + most + ' servers in your city. This takes about a minute.'
        : sv.why_not === 'named' ? 'You named a server in Settings, so Coaxial uses that one and does not look for a faster one.'
        : sv.why_not === 'city' ? 'Pick a city in Settings and Coaxial will find the fastest server there for you. It does not do this for a whole country or region: there are too many servers.'
        : sv.name ? 'Server ' + sv.name + (sv.of > 1 ? ': the fastest of ' + sv.of + ' tried' : '') + (sv.mbps ? ', ' + Math.round(sv.mbps) + ' Mbit/s' : '') + (sv.at ? ', checked ' + when(sv.at) : '') + '. Up to ' + most + ' servers in your city are tried per run.'
        : 'Coaxial picks the server: it tries up to ' + most + ' in your city and stays on the fastest. In quiet hours it checks that server and looks for a better one only if it has become slow. The button does it now.');
      pickBtn.hidden = !sv.possible; pickBtn.disabled = !!sv.testing; pickBtn.classList.toggle('busy', !!sv.testing);
    }
    var text = '', state = 'ok';
    if (progBusy === 'update') { text = 'Updating. The VPN is reconnecting.'; state = 'new'; }
    else if (progBusy === 'check') text = 'Checking for a newer one.';
    else if (u.available) { text = 'A newer VPN client is out. It installs by itself once nothing has played for 10 minutes.'; state = 'new'; }
    else if (u.error) { text = 'The last check failed, ' + when(u.checked_at) + '.'; state = 'bad'; }
    else if (u.checked_at) text = 'Up to date. Checked ' + when(u.checked_at) + '.';
    setText($('#vpn-prog-text'), text);
    box.dataset.state = state;
    var showUpd = !!u.available || progBusy === 'update';
    upd.hidden = !showUpd; chk.hidden = showUpd;
    chk.classList.toggle('busy', progBusy === 'check'); upd.classList.toggle('busy', progBusy === 'update');
    chk.disabled = upd.disabled = !!progBusy;
  }
  async function programAct(kind) {
    if (progBusy) return;
    if (kind === 'update' && !window.confirm('Update the VPN client now? The VPN reconnects, so anything playing stops for a few seconds.')) return;
    progBusy = kind; if (st.data) paintProgram(st.data.vpn || {});
    try {
      var r = await api('POST', '/api/vpn/program/' + kind);
      if (kind === 'update') toast(r.ok ? 'The VPN client was updated. The VPN is reconnecting.' : 'The update did not go through.', !r.ok);
      else if (r.ok && r.data && r.data.update && !r.data.update.available) toast(r.data.update.error ? 'Could not reach the registry to check.' : 'The VPN client is up to date.', !!r.data.update.error);
    } catch (e) { toast('Could not reach Coaxial.', true); }
    progBusy = '';
    if (typeof poll === "function") poll(); else if (st.data) paintProgram(st.data.vpn || {});
  }
  async function pickNow() {
    if (!window.confirm('Find the fastest server now? Up to 8 servers in your city are tried, and the VPN reconnects once for each. Anything playing or recording stops for a minute or two.')) return;
    try {
      var r = await api('POST', '/api/vpn/server/pick');
      var d = r.data || {};
      toast(d.ok ? (d.message || 'Trying the servers. Watch this page.') : (d.message || 'That did not start.'), !d.ok);
    } catch (e) { toast('Could not reach Coaxial.', true); }
    if (typeof poll === 'function') poll();
  }
  document.addEventListener('click', function (e) {
    if (e.target.closest && e.target.closest('#vpn-pick-go')) pickNow();
    if (e.target.closest && e.target.closest('#vpn-prog-check')) programAct('check');
    if (e.target.closest && e.target.closest('#vpn-prog-update')) programAct('update');
  });

  function renderStatus() {
    var d = st.data, vpn = d.vpn || {}, provs = d.providers || [], playing = d.playing || [];
    var place = [vpn.city, vpn.country].filter(Boolean).join(', ');

    var finding = !!(vpn.server && vpn.server.testing); // the tunnel comes and goes on purpose: one steady state, not up, down, up
    $('#vpn').dataset.state = finding ? 'finding' : vpn.up ? 'up' : 'down';
    setText($('#vpn-state'), finding ? 'Finding the fastest server' : vpn.up ? 'Tunnel is up' : 'Tunnel is down');
    setText($('#vpn-sub'), finding ? 'Trying the servers in your city, one after another. Streams are on hold until the fastest is chosen.'
      : vpn.up ? (place ? 'Your streams leave through ' + place + '.' : 'Your streams leave through the VPN.')
      : 'Waiting for the VPN to reconnect. Streams are on hold until it does.');
    setText($('#vpn-city'), vpn.up && vpn.city ? vpn.city : 'n/a');
    setText($('#vpn-country'), vpn.up && vpn.country ? vpn.country : 'n/a');
    var cc = vpn.up && /^[A-Za-z]{2}$/.test(vpn.country || '') ? vpn.country.toUpperCase() : '';
    var flag = $('#vpn-flag'), src = cc ? 'flags/' + cc.toLowerCase() + '.svg' : '';
    if (flag.getAttribute('data-src') !== src) {
      flag.setAttribute('data-src', src);
      flag.onerror = function () { flag.hidden = true; };
      flag.onload = function () { flag.hidden = false; };
      if (src) flag.src = src; else { flag.removeAttribute('src'); flag.hidden = true; }
    }
    setText($('#vpn-ip'), vpn.up && vpn.ip ? vpn.ip : 'n/a');
    var net = vpn.up && vpn.network ? String(vpn.network).replace(/^AS\d+\s+/, '') : '';
    setText($('#vpn-net'), net); $('#vpn-net').hidden = !net; $('#vpn-net').title = vpn.network || '';
    setText($('#vpn-sub'), !finding && vpn.up && place && net ? 'Your streams leave through ' + place + ', on ' + net + '.' : $('#vpn-sub').textContent);
    paintProgram(vpn);
    var hasPing = vpn.up && vpn.ping_ms != null;
    setText($('#vpn-ping'), hasPing ? String(Math.round(vpn.ping_ms)) : 'n/a');
    $('#vpn-ping-unit').hidden = !hasPing;
    drawSpark();

    var devices = {};
    playing.forEach(function (p) { devices[p.device] = 1; });
    var nDev = Object.keys(devices).length;
    setText($('#status-summary'), finding ? 'Finding the fastest server. Streams are on hold for a moment.' : !vpn.up ? 'The tunnel is down.'
      : !provs.length ? 'The tunnel is up. Add a provider to start watching.'
      : !playing.length ? 'The tunnel is up and nothing is playing.'
      : plural(playing.length, 'stream') + ' playing on ' + plural(nDev, 'device') + '.');

    if (d.version) { var v = $('#version'); setText(v, /^v/.test(d.version) ? d.version : 'v' + d.version); v.hidden = false; }

    // Providers
    $('#providers').hidden = !provs.length;
    $('#providers-empty').hidden = !!provs.length;
    reconcile($('#providers'), provs, function (p) { return p.name + '|' + p.port; }, function () {
      var el = h('article', { class: 'card prov' },
        h('div', { class: 'prov-top' }, h('div', { class: 'prov-id' }, h('span', { class: 'tap' }), h('h3', { class: 'prov-name' })), h('span', { class: 'pill' })),
        h('dl', { class: 'prov-stats' },
          h('div', null, h('dt', { text: 'Port' }), h('dd', { class: 'mono' })),
          h('div', null, h('dt', { text: 'Format' }), h('dd')),
          h('div', null, h('dt', { text: 'Open channels' }), h('dd'))));
      return el;
    }, function (el, p) {
      var dd = el.querySelectorAll('dd'), pill = $('.pill', el), up = p.up ? '1' : '0';
      $('.tap', el).style.setProperty('--c', RING[provs.indexOf(p) % RING.length]);
      if (el.dataset.up !== up) { el.dataset.up = up; pill.dataset.up = up; }
      setText($('.prov-name', el), p.name || 'Unnamed provider');
      setText(pill, p.up ? 'Up' : 'Down');
      setText(dd[0], String(p.port));
      setText(dd[1], FORMAT[p.live_tv] || String(p.live_tv || 'n/a').toUpperCase());
      setText(dd[2], String(p.channels_open || 0));
    });

    // Playing
    $('#playing-skel').hidden = true;
    $('#playing-table').hidden = !playing.length;
    $('#playing-empty').hidden = !!playing.length;
    var count = $('#playing-count');
    count.hidden = !playing.length; setText(count, String(playing.length));
    reconcile($('#playing'), playing, function (p) { return p.device + '|' + p.provider + '|' + p.channel_id; }, function () {
      return h('tr', null,
        h('td', { class: 'c-device mono' }),
        h('td', { class: 'c-channel' }, h('span', { class: 'ch-name' }), h('span', { class: 'ch-id' })),
        h('td', { class: 'c-provider' }, h('span', { class: 'pv' }, h('span', { class: 'tap' }), h('span', { class: 'pv-name' }))),
        h('td', { class: 'c-format' }, h('span', { class: 'chip' })),
        h('td', { class: 'c-time r' }),
        h('td', { class: 'c-data r' }));
    }, function (el, p) {
      var td = el.children;
      setText(td[0], p.device || '');
      setText($('.ch-name', el), p.channel_name || 'Channel ' + p.channel_id);
      setText($('.ch-id', el), p.channel_name && p.channel_id ? '#' + p.channel_id : '');
      setText($('.pv-name', el), p.provider || '');
      var pi = provs.findIndex(function (q) { return q.name === p.provider; });
      $('.tap', el).style.setProperty('--c', pi < 0 ? '' : RING[pi % RING.length]);
      setText($('.chip', el), p.format || '');
      el._secs = Number(p.seconds) || 0;
      setText(td[4], fmtDuration(el._secs));
      setText(td[5], fmtData(p.mb_sent));
    });
    st.painted = true;
    renderLive();
  }

  // Durations tick every second between polls so the table feels live.
  function tickClock() {
    if (!st.data || st.fails) return;
    var extra = (Date.now() - st.at) / 1000;
    Array.prototype.forEach.call($('#playing').children, function (tr) {
      setText(tr.children[4], fmtDuration(tr._secs + extra));
    });
  }

  async function poll() {
    try {
      var r = await api('GET', '/api/status');
      if (!r.ok || !r.data) throw new Error('bad status');
      st.data = r.data; st.at = Date.now(); st.fails = 0;
      // A page left open across an update of the container still runs the old page's code. When the version
      // the server reports changes, the page loads itself again (not over unsaved settings).
      if (r.data.version) {
        if (!st.version) st.version = r.data.version;
        else if (st.version !== r.data.version && !isDirty()) { location.reload(); return; }
      }
      var vpn = r.data.vpn || {};
      st.pings.push(vpn.up && vpn.ping_ms != null ? Number(vpn.ping_ms) : null);
      if (st.pings.length > 40) st.pings.shift();
      renderStatus();
    } catch (e) {
      st.fails++;
      renderLive();
    }
    clearTimeout(st.timer);
    if (st.active && !document.hidden) st.timer = setTimeout(poll, 2000);
  }
  function startStatus() {
    if (st.active) return;
    st.active = true;
    poll();
    st.clock = setInterval(tickClock, 1000);
  }
  function stopStatus() {
    st.active = false;
    clearTimeout(st.timer); clearInterval(st.clock);
  }
  document.addEventListener('visibilitychange', function () {
    if (!st.active) return;
    clearTimeout(st.timer);
    if (!document.hidden) poll();
  });

  // ---------- settings view ----------
  var S = { base: null, cur: null, setup: false, saving: false, errors: {}, general: [], failed: false };

  // Server data from the VPN client: /api/vpn/options. Without it the VPN fields are plain free text.
  var O = { data: null, tried: false, busy: false, err: '' }, combos = [];
  // Display names for the service ids the VPN client reports.
  var VPNS = {
    airvpn: 'AirVPN', cyberghost: 'CyberGhost', expressvpn: 'ExpressVPN', fastestvpn: 'FastestVPN', giganews: 'Giganews',
    hidemyass: 'HideMyAss', ipvanish: 'IPVanish', ivpn: 'IVPN', mullvad: 'Mullvad', nordvpn: 'NordVPN',
    'perfect privacy': 'Perfect Privacy', privado: 'Privado', 'private internet access': 'PIA', privatevpn: 'PrivateVPN',
    protonvpn: 'Proton VPN', purevpn: 'PureVPN', slickvpn: 'SlickVPN', surfshark: 'Surfshark', torguard: 'TorGuard',
    'vpn unlimited': 'VPN Unlimited', vyprvpn: 'VyprVPN', windscribe: 'Windscribe', custom: 'Your own config'
  };
  function lower(s) { return s.toLowerCase(); }
  // kind: providers, or countries / cities for the service typed in right now.
  // The servers of the service typed in, narrowed to the places picked: asked for when those change.
  function serverKey() { var v = S.cur.vpn; return [v.service_provider.trim().toLowerCase(), v.server_countries, v.server_regions, v.server_cities].join('|'); }
  async function loadServers() {
    if (!S.cur || !S.cur.vpn.service_provider.trim()) return;
    var k = serverKey(), v = S.cur.vpn;
    O.servers = O.servers || {};
    if (O.servers[k] || O.serversBusy === k) return;
    O.serversBusy = k;
    try {
      var r = await api('GET', '/api/vpn/servers?provider=' + encodeURIComponent(v.service_provider.trim()) + '&countries=' + encodeURIComponent(v.server_countries) +
        '&regions=' + encodeURIComponent(v.server_regions) + '&cities=' + encodeURIComponent(v.server_cities));
      if (r.ok && r.data) O.servers[k] = r.data;
    } catch (e) { /* the fields take free text */ }
    if (O.serversBusy === k) O.serversBusy = '';
    combos.forEach(function (sync) { sync(); });
  }
  function vpnList(kind) {
    if (kind === 'names' || kind === 'hostnames') {
      var got = ((O.servers || {})[serverKey()] || {})[kind];
      return (Array.isArray(got) ? got : []).map(function (x) { x = String(x); return [x, x]; });
    }
    var d = O.data || {}, list = kind === 'providers' ? d.providers : (d[kind] || {})[S.cur.vpn.service_provider.trim().toLowerCase()];
    return (Array.isArray(list) ? list : []).map(function (x) { x = String(x); return [x, kind === 'providers' && VPNS[x] || x]; });
  }
  function syncCombos() { combos.forEach(function (sync) { sync(); }); loadServers(); }

  function normalize(d) {
    d = d || {};
    var p = d.player || {}, v = d.vpn || {}, x = v.extra || {};
    return {
      player: { user: p.user || '', password: '', password_set: !!p.password_set },
      providers: (d.providers || []).map(function (q) {
        return {
          name: q.name || '', url: q.url || '', user: q.user || '', password: '', password_set: !!q.password_set,
          port: q.port == null ? '' : String(q.port), live_tv: q.live_tv === 'hls' ? 'hls' : 'ts'
        };
      }),
      ui: { port: d.ui && d.ui.port != null ? String(d.ui.port) : '' },
      vpn: {
        service_provider: v.service_provider || '', type: v.type === 'openvpn' ? 'openvpn' : 'wireguard',
        wireguard_private_key: '', wireguard_private_key_set: !!v.wireguard_private_key_set,
        wireguard_addresses: v.wireguard_addresses || '', server_countries: v.server_countries || '',
        server_cities: v.server_cities || '', server_regions: v.server_regions || '',
        server_names: v.server_names || '', server_hostnames: v.server_hostnames || '',
        openvpn_user: v.openvpn_user || '', openvpn_password: '', openvpn_password_set: !!v.openvpn_password_set,
        extra: Object.keys(x).map(function (k) { return { k: k, v: String(x[k]) }; })
      }
    };
  }
  function portOut(s) { s = String(s).trim(); return /^\d+$/.test(s) ? parseInt(s, 10) : s; }
  function payload(m) {
    var out = clone(m), extra = {};
    out.providers.forEach(function (p) {
      p.name = p.name.trim(); p.url = p.url.trim(); p.user = p.user.trim(); p.port = portOut(p.port);
    });
    out.player.user = out.player.user.trim();
    out.ui.port = portOut(out.ui.port);
    m.vpn.extra.forEach(function (row) { var k = row.k.trim(); if (k) extra[k] = row.v; });
    out.vpn.extra = extra;
    ['service_provider', 'wireguard_addresses', 'server_countries', 'server_cities'].forEach(function (k) {
      out.vpn[k] = out.vpn[k].trim();
    });
    return out;
  }
  function isDirty() { return !!S.cur && JSON.stringify(payload(S.cur)) !== JSON.stringify(payload(S.base)); }
  function vpnChanged() { return !!S.cur && JSON.stringify(payload(S.cur).vpn) !== JSON.stringify(payload(S.base).vpn); }

  function refreshBar() {
    var dirty = isDirty(), onSettings = route() === 'settings';
    var nErr = Object.keys(S.errors).length + S.general.length;
    $('#savebar').hidden = !(dirty && onSettings);
    $('#nav-dirty').hidden = !dirty;
    document.body.classList.toggle('dirty', dirty && onSettings);
    $('#savebar').classList.toggle('bad', nErr > 0);
    setText($('#save-title'), nErr ? plural(nErr, 'thing') + ' to fix before saving' : 'Unsaved changes');
    setText($('#save-sub'), S.general.length ? S.general.join(' ')
      : nErr ? 'Check the fields marked in red.'
      : vpnChanged() ? 'Saving restarts the proxy and reconnects the VPN.'
      : 'Saving restarts the proxy. The VPN stays connected.');
    $('#save').classList.toggle('busy', S.saving);
    $('#save').disabled = S.saving; $('#discard').disabled = S.saving;
    setText($('#save-label'), S.saving ? 'Saving' : 'Save changes');
  }

  function fieldBox(path) {
    var all = document.querySelectorAll('[data-path]');
    for (var i = 0; i < all.length; i++) if (all[i].dataset.path === path) return all[i];
    return null;
  }
  function paintErrors() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-path]'), function (box) {
      var msg = S.errors[box.dataset.path];
      var input = box.querySelector(':scope > .control input, :scope > .seg input, :scope > input');
      var err = box.querySelector(':scope > .msg > .err, :scope > .err');
      box.classList.toggle('invalid', !!msg);
      if (err) err.textContent = msg || '';
      if (input) { if (msg) input.setAttribute('aria-invalid', 'true'); else input.removeAttribute('aria-invalid'); }
    });
  }
  function setErrors(list) {
    S.errors = {}; S.general = [];
    (list || []).forEach(function (e) {
      var path = String(e.field || ''), m = /^vpn\.extra(?:\.|\[["']?)(.+?)(?:["']?\])?$/.exec(path);
      if (m) { // extra variables are reported by key or by row number
        var idx = /^\d+$/.test(m[1]) ? +m[1] : S.cur.vpn.extra.findIndex(function (r) { return r.k.trim() === m[1]; });
        if (idx >= 0) path = 'vpn.extra[' + idx + ']';
      }
      if (path && fieldBox(path)) S.errors[path] = e.message || 'Not valid.';
      else S.general.push((path ? path + ': ' : '') + (e.message || 'Not valid.'));
    });
    paintErrors(); refreshBar();
    var first = $('.invalid input');
    if (first) first.focus();
  }
  function clearError(path) {
    if (S.general.length) S.general = [];
    if (!S.errors[path]) return;
    delete S.errors[path];
    paintErrors();
  }
  function touched(path) { clearError(path); refreshBar(); }

  var uid = 0;
  // One labeled input. o: label, path, value, set, hint, placeholder, mono, secret, isSet, span, numeric
  function field(o) {
    var id = 'f' + (++uid);
    var input = h('input', {
      id: id, class: 'input' + (o.mono ? ' mono' : '') + (o.secret ? ' has-btn' : '') + (o.secret && o.isSet ? ' saved' : ''),
      type: o.secret ? 'password' : 'text',
      placeholder: o.secret && o.isSet ? 'Saved. Type to replace it.' : o.placeholder,
      autocomplete: o.secret ? 'new-password' : 'off', autocapitalize: 'off', autocorrect: 'off', spellcheck: 'false',
      inputmode: o.numeric ? 'numeric' : null, 'aria-describedby': id + '-m'
    });
    input.value = o.value || '';
    var reveal = null;
    if (o.secret) {
      reveal = h('button', { type: 'button', class: 'icon-btn reveal', 'aria-label': 'Show ' + o.label.toLowerCase(), 'aria-pressed': 'false', hidden: true, html: icon('eye') });
      reveal.addEventListener('click', function () {
        var show = input.type === 'password';
        input.type = show ? 'text' : 'password';
        reveal.setAttribute('aria-pressed', String(show));
        reveal.innerHTML = icon(show ? 'eyeOff' : 'eye');
        input.focus();
      });
    }
    input.addEventListener('input', function () {
      if (o.numeric) { var digits = input.value.replace(/\D+/g, '').slice(0, 5); if (digits !== input.value) input.value = digits; }
      o.set(input.value);
      if (reveal) {
        reveal.hidden = !input.value;
        if (!input.value && input.type !== 'password') { input.type = 'password'; reveal.setAttribute('aria-pressed', 'false'); reveal.innerHTML = icon('eye'); }
      }
      touched(o.path);
    });
    return h('div', { class: 'field' + (o.span ? ' span-2' : ''), 'data-path': o.path },
      h('label', { for: id, text: o.label }),
      h('div', { class: 'control' }, input, reveal),
      h('p', { class: 'msg', id: id + '-m' }, o.hint ? h('span', { class: 'hint', text: o.hint }) : null, h('span', { class: 'err' })));
  }

  function segmented(o) {
    var name = 's' + (++uid), labelId = name + '-l';
    var group = h('div', { class: 'seg', role: 'radiogroup', 'aria-labelledby': labelId });
    o.options.forEach(function (opt) {
      var radio = h('input', { type: 'radio', name: name, value: opt[0] });
      radio.checked = o.value === opt[0];
      radio.addEventListener('change', function () { if (radio.checked) { o.set(opt[0]); touched(o.path); } });
      group.appendChild(h('label', null, radio, h('span', { text: opt[1] })));
    });
    return h('div', { class: 'field', 'data-path': o.path },
      h('span', { class: 'label', id: labelId, text: o.label }), group,
      h('p', { class: 'msg' }, o.hint ? h('span', { class: 'hint', text: o.hint }) : null, h('span', { class: 'err' })));
  }

  // Text input with a suggestion list. Any typed value is accepted. With o.multi it holds several
  // values, shown as chips and kept as one comma separated string.
  // o: label, path, get, set, options (function), hint (function of "has a list"), placeholder, mono, multi
  function combo(o) {
    var id = 'c' + (++uid), active = -1, shown = [];
    var input = h('input', {
      id: id, class: o.multi ? 'tags-input' : 'input' + (o.mono ? ' mono' : ''), type: 'text', role: 'combobox', 'aria-autocomplete': 'list',
      'aria-expanded': 'false', 'aria-controls': id + '-list', 'aria-describedby': id + '-m', placeholder: o.placeholder,
      autocomplete: 'off', autocapitalize: 'off', autocorrect: 'off', spellcheck: 'false'
    });
    var list = h('ul', { class: 'combo-list', id: id + '-list', role: 'listbox', 'aria-label': o.label, 'aria-multiselectable': o.multi ? 'true' : null, hidden: true });
    var tags = o.multi ? h('div', { class: 'input tags' }, input) : null;
    var hint = h('span', { class: 'hint' });
    var box = h('div', { class: 'control combo' }, tags || input, h('span', { class: 'combo-caret', html: icon('chevron') }), list);

    function vals() { return o.get().split(',').map(function (s) { return s.trim(); }).filter(Boolean); }
    function chips() {
      var cur = vals();
      Array.prototype.forEach.call(tags.querySelectorAll('.tag'), function (t) { t.remove(); });
      cur.forEach(function (name, i) {
        var x = h('button', { type: 'button', tabindex: '-1', 'aria-label': 'Remove ' + name, html: icon('x', 12) });
        x.addEventListener('click', function () { cur.splice(i, 1); store(cur); input.focus(); });
        tags.insertBefore(h('span', { class: 'tag' }, h('span', { text: name }), x), input);
      });
      input.placeholder = cur.length ? '' : o.placeholder;
    }
    function store(cur) { o.set(cur.join(',')); chips(); touched(o.path); if (!list.hidden) draw(true); }
    // Add what was typed. A name that is in the list takes the list's spelling.
    function commit() {
      var cur = vals(), low = cur.map(lower), opts = o.options(), typed = input.value;
      input.value = '';
      typed.split(',').forEach(function (s) {
        s = s.trim();
        var hit = opts.filter(function (v) { return v[0].toLowerCase() === s.toLowerCase(); })[0];
        if (hit) s = hit[0];
        if (s && low.indexOf(s.toLowerCase()) < 0) { cur.push(s); low.push(s.toLowerCase()); }
      });
      if (typed.trim()) store(cur);
    }

    function draw(filter) {
      var q = input.value.trim().toLowerCase(), cur = o.multi ? vals().map(lower) : [q], opts = o.options();
      function at(v) { var a = v[0].toLowerCase().indexOf(q), b = v[1].toLowerCase().indexOf(q); return a < 0 ? b : b < 0 ? a : Math.min(a, b); }
      // Names that start with the typed text come first.
      shown = filter && q ? opts.filter(function (v) { return at(v) === 0; }).concat(opts.filter(function (v) { return at(v) > 0; })) : opts;
      var more = shown.length > 300; // (a service can have thousands of servers)
      if (more) shown = shown.slice(0, 300);
      list.textContent = '';
      shown.forEach(function (v, i) {
        var li = h('li', { role: 'option', id: id + '-o' + i, 'aria-selected': String(cur.indexOf(v[0].toLowerCase()) >= 0) }, h('span', { text: v[1] }),
          o.multi ? h('span', { class: 'combo-check', html: icon('check', 15) }) : v[1] !== v[0] ? h('small', { text: v[0] }) : null);
        li.addEventListener('mousedown', function (e) { e.preventDefault(); pick(i); });
        list.appendChild(li);
      });
      if (more) list.appendChild(h('li', { class: 'combo-none', text: 'Showing the first 300. Type to narrow the list.' }));
      if (!shown.length) list.appendChild(h('li', { class: 'combo-none', text: o.multi ? 'Not in the list. Press Enter to add it as typed.' : 'Not in the list. It will be used as typed.' }));
      // While filtering a multi-pick list, Enter takes the first match.
      mark(o.multi && filter && q && shown.length ? 0 : -1);
    }
    function mark(i) {
      active = i;
      Array.prototype.forEach.call(list.children, function (li, n) { li.classList.toggle('active', n === i); });
      if (i >= 0) { input.setAttribute('aria-activedescendant', id + '-o' + i); list.children[i].scrollIntoView({ block: 'nearest' }); }
      else input.removeAttribute('aria-activedescendant');
    }
    function open(filter) {
      if (!o.options().length) { close(); return; }
      draw(filter); list.hidden = false; box.classList.add('open'); input.setAttribute('aria-expanded', 'true');
    }
    function close() { list.hidden = true; box.classList.remove('open'); input.setAttribute('aria-expanded', 'false'); mark(-1); }
    function pick(i) {
      var name = shown[i][0];
      if (!o.multi) { input.value = name; o.set(name); touched(o.path); close(); return; }
      var cur = vals(), at = cur.map(lower).indexOf(name.toLowerCase());
      if (at < 0) cur.push(name); else cur.splice(at, 1);
      input.value = '';
      store(cur); // the list stays open for the next pick
      mark(shown.findIndex(function (v) { return v[0] === name; }));
    }
    // Runs when the server data arrives or the chosen service changes.
    function sync() {
      var has = o.options().length > 0;
      box.classList.toggle('free', !has);
      setText(hint, o.hint(has));
      if (!list.hidden) { if (has) draw(true); else close(); }
    }

    input.addEventListener('input', function () {
      if (!o.multi) { o.set(input.value); touched(o.path); }
      else if (input.value.indexOf(',') >= 0) commit();
      open(true);
    });
    if (tags) tags.addEventListener('mousedown', function (e) { if (e.target !== input) e.preventDefault(); }); // keep the focus in the box
    (tags || input).addEventListener('click', function (e) {
      if (e.target.closest('button')) return;
      input.focus();
      if (list.hidden) open(false);
    });
    input.addEventListener('blur', function () { if (o.multi) commit(); close(); });
    input.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        if (list.hidden) { open(false); return; }
        if (shown.length) mark((active + (e.key === 'ArrowDown' ? 1 : -1) + shown.length + (active < 0 && e.key === 'ArrowUp' ? 1 : 0)) % shown.length);
      } else if (e.key === 'Enter') {
        if (!list.hidden && active >= 0) { e.preventDefault(); pick(active); }
        else if (o.multi && input.value.trim()) { e.preventDefault(); commit(); }
      } else if (e.key === 'Escape' && !list.hidden) { e.preventDefault(); close(); }
      else if (o.multi && e.key === 'Backspace' && !input.value) { var cur = vals(); if (cur.length) { cur.pop(); store(cur); } }
    });
    if (o.multi) chips(); else input.value = o.get();
    sync(); combos.push(sync);
    return h('div', { class: 'field' + (o.span ? ' span-2' : ''), 'data-path': o.path },
      h('label', { for: id, text: o.label }), box,
      h('p', { class: 'msg', id: id + '-m' }, hint, h('span', { class: 'err' })));
  }

  // Refresh button and the age of the server list. State lives in O so it survives a redraw.
  function serverRow() {
    var btn = h('button', { type: 'button', class: 'btn', id: 'vpn-refresh' }, h('span', { class: 'spin', 'aria-hidden': 'true' }), h('span', { html: icon('refresh') }), 'Refresh server list');
    btn.addEventListener('click', refreshServers);
    return h('div', { class: 'servers span-2' }, btn,
      h('div', { class: 'servers-text' },
        h('p', { id: 'vpn-updated', 'aria-live': 'polite' }),
        h('p', { class: 'hint', text: 'Coaxial also checks for a new server list on its own once a day.' })));
  }
  function paintServers() {
    var btn = $('#vpn-refresh'), line = $('#vpn-updated');
    if (!btn) return;
    var at = O.data && O.data.updated_at, ago = at ? fmtAgo(at) : '';
    btn.classList.toggle('busy', O.busy);
    if (O.busy) btn.setAttribute('aria-disabled', 'true'); else btn.removeAttribute('aria-disabled');
    line.classList.toggle('bad', !!O.err && !O.busy);
    line.title = ago && !O.err ? new Date(at).toLocaleString() : '';
    setText(line, O.busy ? 'Refreshing the server list.' : O.err ? O.err
      : ago ? 'Server list updated ' + ago
      : !O.tried ? 'Checking the server list.'
      : O.data ? 'Server list loaded.' : 'No server list yet. You can type the names instead.');
  }
  async function loadOptions() {
    try {
      var r = await api('GET', '/api/vpn/options');
      if (r.ok && r.data) O.data = r.data;
    } catch (e) { /* keep what we have; the fields take free text */ }
    O.tried = true;
    syncCombos(); paintServers();
  }
  async function refreshServers() {
    if (O.busy) return;
    O.busy = true; O.err = ''; paintServers();
    try {
      var r = await api('POST', '/api/vpn/refresh');
      if (r.ok && r.data && r.data.ok) {
        if (O.data && r.data.updated_at) O.data.updated_at = r.data.updated_at;
        await loadOptions();
      } else {
        O.err = (r.data && r.data.message) || 'Could not refresh the server list (error ' + r.status + ').';
      }
    } catch (e) {
      O.err = 'Could not reach Coaxial. The server list was not refreshed.';
    }
    O.busy = false; paintServers();
  }

  function section(title, text) {
    var body = h('div', { class: 'set-body' });
    for (var i = 2; i < arguments.length; i++) add(body, arguments[i]);
    return h('section', { class: 'set' }, h('div', { class: 'set-info' }, h('h2', { text: title }), h('p', { text: text })), body);
  }

  function providerCard(p, i) {
    var base = 'providers[' + i + ']';
    var title = h('h3', { class: 'pcard-title' + (p.name ? '' : ' blank'), text: p.name || 'New provider' });
    var remove = h('button', { type: 'button', class: 'icon-btn danger', 'aria-label': 'Remove ' + (p.name || 'this provider'), title: 'Remove provider', html: icon('trash') });
    remove.addEventListener('click', function () {
      S.cur.providers.splice(i, 1);
      S.errors = {}; S.general = [];
      renderSettings();
      var next = document.querySelectorAll('.pcard .icon-btn.danger')[Math.min(i, S.cur.providers.length - 1)] || $('#add-provider');
      if (next) next.focus();
    });
    function mover(dir, label, name) {
      var to = i + dir, b = h('button', { type: 'button', class: 'icon-btn move', 'aria-label': label + ' ' + (p.name || 'this provider'), title: label, html: icon(name) });
      b.disabled = to < 0 || to >= S.cur.providers.length;
      b.addEventListener('click', function () {
        var list = S.cur.providers, x = list[i]; list[i] = list[to]; list[to] = x;
        S.errors = {}; S.general = [];
        renderSettings(); refreshBar();
        var again = document.querySelectorAll('.pcard')[to];
        again = again && again.querySelector('.icon-btn.move' + (dir < 0 ? '' : ':last-of-type'));
        if (again && !again.disabled) again.focus();
      });
      return b;
    }
    var moves = h('span', { class: 'pcard-moves' }, mover(-1, 'Move up', 'up'), mover(1, 'Move down', 'down'));
    return h('article', { class: 'card panel pcard', 'data-path': base },
      h('div', { class: 'pcard-head' }, h('span', { class: 'pcard-num', text: String(i + 1) }), title, moves, remove),
      h('div', { class: 'grid' },
        field({ label: 'Name', path: base + '.name', value: p.name, placeholder: 'Provider One', set: function (v) {
          p.name = v; title.textContent = v.trim() || 'New provider'; title.classList.toggle('blank', !v.trim());
        } }),
        field({ label: 'Server URL', path: base + '.url', value: p.url, placeholder: 'https://example.com', mono: true, set: function (v) { p.url = v; } }),
        field({ label: 'Username', path: base + '.user', value: p.user, set: function (v) { p.user = v; } }),
        field({ label: 'Password', path: base + '.password', value: p.password, secret: true, isSet: p.password_set, set: function (v) { p.password = v; } }),
        field({ label: 'Local port', path: base + '.port', value: p.port, numeric: true, mono: true, placeholder: '8095', hint: 'Players reach this provider on this port. Docker must open it too: add it as a PORT_n line in the .env file next to your compose file (or as -p in docker run).', set: function (v) { p.port = v; } }),
        segmented({ label: 'Live format', path: base + '.live_tv', value: p.live_tv, options: [['ts', 'MPEG-TS'], ['hls', 'HLS']], hint: 'How live channels are sent to players.', set: function (v) { p.live_tv = v; } })),
      h('p', { class: 'err' }));
  }

  function extraEditor() {
    var rows = S.cur.vpn.extra, box = h('div', { class: 'card panel' });
    if (rows.length) {
      box.appendChild(h('div', { class: 'kv-head', 'aria-hidden': 'true' }, h('span', { class: 'label-cap', text: 'Variable' }), h('span', { class: 'label-cap', text: 'Value' }), h('span')));
      var list = h('div', { class: 'kv-rows' });
      rows.forEach(function (row, i) {
        var path = 'vpn.extra[' + i + ']';
        var key = h('input', { class: 'input mono', type: 'text', placeholder: 'VARIABLE_NAME', 'aria-label': 'Variable name', autocomplete: 'off', autocapitalize: 'characters', spellcheck: 'false' });
        var val = h('input', { class: 'input mono', type: 'text', placeholder: 'value', 'aria-label': 'Value', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
        key.value = row.k; val.value = row.v;
        key.addEventListener('input', function () {
          var clean = key.value.toUpperCase().replace(/[\s-]+/g, '_').replace(/[^A-Z0-9_]/g, '');
          if (clean !== key.value) key.value = clean;
          row.k = clean; touched(path);
        });
        val.addEventListener('input', function () { row.v = val.value; touched(path); });
        var del = h('button', { type: 'button', class: 'icon-btn danger', 'aria-label': 'Remove variable ' + (row.k || i + 1), title: 'Remove variable', html: icon('trash') });
        del.addEventListener('click', function () {
          rows.splice(i, 1); S.errors = {}; S.general = [];
          renderSettings(); refreshBar();
          var keep = document.querySelectorAll('.kv-row .icon-btn')[Math.min(i, rows.length - 1)] || $('#add-extra');
          if (keep) keep.focus();
        });
        list.appendChild(h('div', { class: 'kv-row', 'data-path': path }, key, val, del, h('p', { class: 'err' })));
      });
      box.appendChild(list);
    } else {
      box.appendChild(h('p', { class: 'kv-empty', text: 'No extra variables. Most setups do not need any.' }));
    }
    var addBtn = h('button', { type: 'button', class: 'btn', id: 'add-extra', html: icon('plus') }, 'Add variable');
    addBtn.addEventListener('click', function () {
      rows.push({ k: '', v: '' });
      renderSettings(); refreshBar();
      var keys = document.querySelectorAll('.kv-row input:first-child');
      if (keys.length) keys[keys.length - 1].focus();
    });
    box.appendChild(addBtn);
    box.appendChild(h('p', { class: 'hint kv-note', text: 'Anything the VPN client supports can go here. A variable set here overrides the fields above.' }));
    return box;
  }

  function addProvider() {
    var ports = S.cur.providers.map(function (p) { return parseInt(p.port, 10) || 0; });
    var next = ports.length ? Math.max.apply(null, ports) + 1 : 8095;
    S.cur.providers.push({ name: '', url: '', user: '', password: '', password_set: false, port: String(next), live_tv: 'ts' });
    renderSettings();
    var cards = document.querySelectorAll('.pcard');
    var last = cards[cards.length - 1];
    if (last) { last.classList.add('enter'); $('input', last).focus(); }
  }

  function setupPanel() {
    var start = h('button', { type: 'button', class: 'btn primary lg' }, 'Start setup', h('span', { html: icon('arrow') }));
    start.addEventListener('click', function () { S.setup = true; addProvider(); refreshBar(); });
    return h('div', { class: 'card setup' },
      h('div', null,
        h('div', { class: 'setup-brand', role: 'img', 'aria-label': 'Coaxial' },
          h('img', { class: 'brand-icon', src: 'coaxial-icon.svg', width: '64', height: '64', alt: '' }),
          h('img', { class: 'brand-word', src: 'coaxial-wordmark-dark.svg', width: '215', height: '52', alt: '' })),
        h('h2', { text: 'Welcome. Let\u2019s get you watching.' }),
        h('p', { class: 'setup-lead', text: 'Coaxial carries your IPTV streams through a VPN tunnel and shares one provider connection per channel between all of your devices. Setup takes about two minutes.' }),
        start,
        h('p', { class: 'setup-foot' }, 'Everything is kept in ', h('code', { text: 'settings.json' }), ' in your own folder. Nothing is sent anywhere else.')),
      h('ol', { class: 'steps' },
        h('li', null, h('strong', { text: 'Add an IPTV provider' }), h('span', { text: 'The server URL, username and password your provider gave you.' })),
        h('li', null, h('strong', { text: 'Connect the VPN' }), h('span', { text: 'Pick your VPN service and paste its WireGuard key.' })),
        h('li', null, h('strong', { text: 'Choose a player login' }), h('span', { text: 'The username and password your apps use to reach Coaxial.' }))));
  }

  function renderSettings() {
    var root = $('#settings-root');
    root.textContent = ''; combos = [];
    if (S.failed) {
      var retry = h('button', { type: 'button', class: 'btn' }, 'Try again');
      retry.addEventListener('click', loadSettings);
      root.appendChild(h('div', { class: 'card load-fail' }, h('p', { class: 'empty-title', text: 'Could not load settings' }),
        h('p', { class: 'empty-text', text: 'Coaxial did not answer. Check that the container is running.' }), retry));
      return;
    }
    if (!S.cur) { root.appendChild(h('div', { class: 'set-skel skeleton' })); return; }
    if (!S.base.providers.length && !S.setup) { root.appendChild(setupPanel()); return; }

    var m = S.cur, v = m.vpn, wg = v.type === 'wireguard';
    function placeHint(has) { return has ? 'Pick from the list or type. Leave empty for any.' : 'Comma separated. Leave empty for any.'; }
    var addBtn = h('button', { type: 'button', class: 'btn dashed', id: 'add-provider', html: icon('plus') }, m.providers.length ? 'Add another provider' : 'Add a provider');
    addBtn.addEventListener('click', addProvider);

    add(root, [
      section('Providers', 'Your IPTV services. Each one gets its own local port, and Coaxial opens one connection per channel no matter how many devices watch.',
        m.providers.map(providerCard), addBtn),

      section('VPN', 'The tunnel every stream travels through. Changing anything here reconnects the VPN when you save.',
        h('div', { class: 'card panel' }, h('div', { class: 'grid' },
          combo({ label: 'VPN service', path: 'vpn.service_provider', mono: true, placeholder: 'protonvpn',
            options: function () { return vpnList('providers'); }, get: function () { return v.service_provider; }, set: function (x) { v.service_provider = x; syncCombos(); },
            hint: function (has) { return has ? 'Pick one or type the name your VPN service goes by.' : 'Type the name your VPN service goes by.'; } }),
          segmented({ label: 'Connection type', path: 'vpn.type', value: v.type, options: [['wireguard', 'WireGuard'], ['openvpn', 'OpenVPN']], set: function (x) { v.type = x; renderSettings(); refreshBar(); } }),
          wg ? field({ label: 'WireGuard private key', path: 'vpn.wireguard_private_key', value: v.wireguard_private_key, secret: true, isSet: v.wireguard_private_key_set, mono: true, span: true, set: function (x) { v.wireguard_private_key = x; } }) : null,
          wg ? field({ label: 'WireGuard addresses', path: 'vpn.wireguard_addresses', value: v.wireguard_addresses, mono: true, span: true, placeholder: '10.2.0.2/32', hint: 'The interface address from your VPN account.', set: function (x) { v.wireguard_addresses = x; } }) : null,
          !wg ? field({ label: 'OpenVPN username', path: 'vpn.openvpn_user', value: v.openvpn_user, mono: true, hint: 'The OpenVPN username from your VPN account. It is often not your account email.', set: function (x) { v.openvpn_user = x; } }) : null,
          !wg ? field({ label: 'OpenVPN password', path: 'vpn.openvpn_password', value: v.openvpn_password, secret: true, isSet: v.openvpn_password_set, mono: true, set: function (x) { v.openvpn_password = x; } }) : null,
          combo({ label: 'Server countries', path: 'vpn.server_countries', multi: true, placeholder: 'Any',
            options: function () { return vpnList('countries'); }, get: function () { return v.server_countries; }, set: function (x) { v.server_countries = x; loadServers(); }, hint: placeHint }),
          combo({ label: 'Server cities', path: 'vpn.server_cities', multi: true, placeholder: 'Any',
            options: function () { return vpnList('cities'); }, get: function () { return v.server_cities; }, set: function (x) { v.server_cities = x; loadServers(); }, hint: placeHint }),
          combo({ label: 'Server regions', path: 'vpn.server_regions', multi: true, placeholder: 'Any',
            options: function () { return vpnList('regions'); }, get: function () { return v.server_regions; }, set: function (x) { v.server_regions = x; loadServers(); },
            hint: function (has) { return has ? 'Pick from the list or type. Leave empty for any.' : 'Only some services have regions. Leave empty for any.'; } }),
          combo({ label: 'Servers by hostname', path: 'vpn.server_hostnames', multi: true, mono: true, placeholder: 'Any',
            options: function () { return vpnList('hostnames'); }, get: function () { return v.server_hostnames; }, set: function (x) { v.server_hostnames = x; },
            hint: function (has) { return has ? 'The servers in the places picked above. Leave empty for any.' : 'Comma separated. Leave empty for any.'; } }),
          combo({ label: 'Servers by name', path: 'vpn.server_names', multi: true, mono: true, placeholder: 'Any', span: true,
            options: function () { return vpnList('names'); }, get: function () { return v.server_names; }, set: function (x) { v.server_names = x; },
            hint: function (has) { return (has ? 'For services that name their servers.' : 'Only some services name their servers.') + ' Pick a city and leave both server boxes empty, and Coaxial tries up to 8 servers in that city and stays on the fastest.'; } }),
          serverRow()))),

      section('Extra VPN variables', 'Anything else the built-in VPN client understands, passed straight through as environment variables.', extraEditor()),

      section('Player login', 'The username and password your TV apps use to connect to Coaxial. This is separate from your provider logins.',
        h('div', { class: 'card panel' }, h('div', { class: 'grid' },
          field({ label: 'Username', path: 'player.user', value: m.player.user, set: function (x) { m.player.user = x; } }),
          field({ label: 'Password', path: 'player.password', value: m.player.password, secret: true, isSet: m.player.password_set, set: function (x) { m.player.password = x; } })))),

      section('Web interface', 'Where this page is served.',
        h('div', { class: 'card panel' }, h('div', { class: 'grid' },
          field({ label: 'Port', path: 'ui.port', value: m.ui.port, numeric: true, mono: true, placeholder: '8195', hint: 'After a change, this page moves to the new port.', set: function (x) { m.ui.port = x; } }))))
    ]);
    paintErrors(); paintServers();
    paintPortWarn();
  }

  async function loadSettings() {
    S.failed = false;
    try {
      var r = await api('GET', '/api/settings');
      if (!r.ok || !r.data) throw new Error('bad settings');
      S.base = normalize(r.data); S.cur = clone(S.base);
      S.errors = {}; S.general = [];
    } catch (e) {
      S.failed = true;
    }
    renderSettings(); refreshBar();
  }

  function localCheck() {
    var errs = [], seen = {};
    function port(path, val) {
      var n = portOut(val);
      if (typeof n !== 'number' || n < 1 || n > 65535) errs.push({ field: path, message: 'Enter a port from 1 to 65535.' });
    }
    S.cur.providers.forEach(function (p, i) { port('providers[' + i + '].port', p.port); });
    port('ui.port', S.cur.ui.port);
    S.cur.vpn.extra.forEach(function (row, i) {
      var k = row.k.trim();
      if (!k && row.v.trim()) errs.push({ field: 'vpn.extra[' + i + ']', message: 'Give this variable a name.' });
      else if (k && seen[k]) errs.push({ field: 'vpn.extra[' + i + ']', message: k + ' is listed twice.' });
      seen[k] = 1;
    });
    return errs;
  }

  // A port typed here is only reachable if Docker publishes it. The page cannot see Docker's settings, so
  // after a save it tries each provider's port from this browser and says so when one does not answer.
  var portCheck = 0;
  function reach(port) {
    var ctl = new AbortController(), t = setTimeout(function () { ctl.abort(); }, 2500);
    return fetch(location.protocol + '//' + location.hostname + ':' + port + '/healthz', { mode: 'no-cors', cache: 'no-store', signal: ctl.signal })
      .then(function () { clearTimeout(t); return true; }, function () { clearTimeout(t); return false; });
  }
  async function checkPorts(seconds) {
    var mine = ++portCheck, until = Date.now() + seconds * 1000, bad = [];
    S.portWarn = {}; paintPortWarn();
    for (;;) {
      bad = [];
      var list = (S.base && S.base.providers) || [];
      for (var i = 0; i < list.length; i++) {
        if (!list[i].port) continue;
        if (!(await reach(list[i].port))) bad.push(String(list[i].port));
        if (mine !== portCheck) return;
      }
      if (!bad.length || Date.now() > until) break;
      await new Promise(function (r) { setTimeout(r, 1500); });
      if (mine !== portCheck) return;
    }
    S.portWarn = {}; bad.forEach(function (p) { S.portWarn[p] = 1; });
    paintPortWarn();
    if (bad.length) toast('Saved, but port ' + bad.join(', ') + (bad.length > 1 ? ' do' : ' does') + ' not answer from this browser. Docker is probably not publishing ' + (bad.length > 1 ? 'them' : 'it') + '.', true);
  }
  function paintPortWarn() {
    document.querySelectorAll('.port-warn').forEach(function (n) { n.remove(); });
    var warn = S.portWarn || {};
    document.querySelectorAll('.pcard').forEach(function (card, i) {
      var p = S.cur && S.cur.providers[i];
      if (!p || !warn[String(p.port)]) return;
      card.appendChild(h('div', { class: 'note port-warn', html: icon('alert') }, h('span', null,
        'Port ' + p.port + ' does not answer from this browser. If Docker has not opened it, add a line such as ',
        h('code', { text: 'PORT_3=' + p.port }), ' (the next free PORT_n) to the ', h('code', { text: '.env' }), ' file next to your compose file, or ',
        h('code', { text: '-p ' + p.port + ':' + p.port }), ' to docker run, then recreate the container.')));
    });
  }

  async function save() {
    if (S.saving || !isDirty()) return;
    var errs = localCheck();
    if (errs.length) { setErrors(errs); return; }
    var vpnToo = vpnChanged();
    S.saving = true; S.errors = {}; S.general = []; paintErrors(); refreshBar();
    // Ports that are new: ask Coaxial to listen there for a moment and try them from this browser first.
    var had = {}, shut = [];
    ((S.base && S.base.providers) || []).forEach(function (p) { had[String(p.port)] = 1; });
    for (var pi = 0; pi < S.cur.providers.length; pi++) {
      var port = String(S.cur.providers[pi].port || '').trim();
      if (!/^[1-9][0-9]{0,4}$/.test(port) || had[port]) continue;
      try {
        var t = await api('POST', '/api/port/try?port=' + port);
        if (t.ok && !(await reach(port)) && !(await reach(port))) shut.push({ field: 'providers[' + pi + '].port',
          message: 'Port ' + port + ' is not opened by Docker. Add a line such as PORT_3=' + port + ' (the next free PORT_n) to the .env file next to your compose file (or -p ' + port + ':' + port + ' to docker run), recreate the container, then save again.' });
      } catch (e) { /* the check could not run: let the save go on, the check after saving still warns */ }
    }
    if (shut.length) { S.saving = false; setErrors(shut); return; }
    try {
      var r = await api('PUT', '/api/settings', payload(S.cur));
      S.saving = false;
      if (r.ok) {
        S.setup = false;
        await loadSettings();
        toast(vpnToo ? 'Saved. The proxy is restarting and the VPN is reconnecting.' : 'Saved. The proxy is restarting.');
        checkPorts(vpnToo ? 45 : 20);
      } else if (r.status === 400 && r.data && r.data.errors && r.data.errors.length) {
        setErrors(r.data.errors);
      } else {
        S.general = ['Coaxial answered with an error (' + r.status + '). Nothing was saved.'];
        refreshBar();
      }
    } catch (e) {
      S.saving = false;
      S.general = ['Could not reach Coaxial. Nothing was saved.'];
      refreshBar();
    }
  }

  function discard() {
    S.cur = clone(S.base); S.errors = {}; S.general = []; S.setup = false;
    renderSettings(); refreshBar();
  }

  // ---------- routing ----------
  function route() { return location.hash.replace(/^#\/?/, '') === 'settings' ? 'settings' : 'status'; }
  function show() {
    var r = route();
    $('#view-status').hidden = r !== 'status';
    $('#view-settings').hidden = r !== 'settings';
    Array.prototype.forEach.call(document.querySelectorAll('[data-nav]'), function (a) {
      if (a.dataset.nav === r) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
    document.title = (r === 'settings' ? 'Settings' : 'Status') + ' \u00b7 Coaxial';
    if (r === 'status') startStatus(); else stopStatus();
    refreshBar();
    window.scrollTo(0, 0);
  }

  async function boot() {
    $('#save').addEventListener('click', save);
    $('#discard').addEventListener('click', discard);
    document.addEventListener('keydown', function (e) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's' && route() === 'settings') { e.preventDefault(); save(); }
    });
    window.addEventListener('beforeunload', function (e) { if (isDirty()) { e.preventDefault(); e.returnValue = ''; } });
    window.addEventListener('hashchange', show);
    renderSettings();
    loadOptions();
    setInterval(paintServers, 30000);
    var fresh = !location.hash;
    if (!fresh) show();
    await loadSettings();
    // First run lands on Settings, where the setup panel is.
    if (fresh) {
      if (S.base && !S.base.providers.length) history.replaceState(null, '', '#/settings');
      show();
    }
  }

  if (params.has('mock')) {
    var tag = document.createElement('script');
    tag.src = 'mock.js'; tag.onload = boot; tag.onerror = boot;
    document.head.appendChild(tag);
  } else {
    boot();
  }
})();
