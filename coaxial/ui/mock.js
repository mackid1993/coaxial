/* Coaxial UI mock API. Only loaded when the page URL has ?mock=...
   ?mock=1                the sample data, three streams playing
   ?mock=1&state=empty    nothing playing
   ?mock=1&state=down     one provider down
   ?mock=1&state=vpndown  VPN tunnel down
   ?mock=1&state=firstrun no providers yet (first run)
   &refresh=fail          the server list refresh fails with a message
   &servers=none          the server list request fails, so the VPN fields are free text
   Everything lives in memory. Saving works and validates, reloading resets it. */
(function () {
  'use strict';
  var q = new URLSearchParams(location.search);
  if (!q.has('mock')) return;
  var state = q.get('state') || 'playing';
  var started = Date.now();

  var secrets = { player: 'x', wg: 'x', providers: ['x', 'x'] };
  var settings = {
    player: { user: 'david' },
    providers: [
      { name: 'Provider One', url: 'https://example.com', user: 'abc', port: 8095, connections: 2, live_tv: 'ts',
        advanced: { stay_under_limit: true, one_per_channel: true, one_per_item: false, locked_to_address: false,
          address_wait_seconds: 90, send_app_name: true, app_name: 'IPTVSmartersPro', share_lists: false, share_minutes: 10,
          limit_movie_speed: false, movie_mbps: 20 } },
      { name: 'Provider Two', url: 'https://example.net', user: 'def', port: 8096, live_tv: 'hls' }
    ],
    ui: { port: 8195 },
    vpn: {
      service_provider: 'protonvpn', type: 'wireguard', wireguard_addresses: '10.2.0.2/32',
      server_countries: '', server_cities: 'New York', extra: { STREAM_ONLY: 'on' }
    }
  };
  if (state === 'firstrun') {
    secrets = { player: '', wg: '', providers: [] };
    settings.player.user = '';
    settings.providers = [];
    settings.vpn = { service_provider: '', type: 'wireguard', wireguard_addresses: '', server_countries: '', server_cities: '', extra: {} };
  }

  // What /api/vpn/options returns: the VPN client's own server data. "custom" has no lists.
  var options = {
    providers: ['custom', 'mullvad', 'nordvpn', 'private internet access', 'protonvpn'],
    types: ['wireguard', 'openvpn'],
    countries: {
      mullvad: ['Australia', 'Canada', 'Germany', 'Netherlands', 'Sweden', 'Switzerland', 'United Kingdom', 'USA'],
      nordvpn: ['Canada', 'France', 'Germany', 'Japan', 'Netherlands', 'Spain', 'United Kingdom', 'United States'],
      'private internet access': ['CA Toronto', 'DE Frankfurt', 'Netherlands', 'UK London', 'US East', 'US New York', 'US West'],
      protonvpn: ['Canada', 'France', 'Germany', 'Japan', 'Netherlands', 'Sweden', 'Switzerland', 'United Kingdom', 'United States']
    },
    cities: {
      mullvad: ['Amsterdam', 'Chicago IL', 'Frankfurt', 'Gothenburg', 'London', 'New York NY', 'Stockholm', 'Sydney', 'Toronto', 'Zurich'],
      nordvpn: ['Amsterdam', 'Chicago', 'Frankfurt', 'London', 'Madrid', 'New York', 'Paris', 'Tokyo', 'Toronto'],
      'private internet access': ['Amsterdam', 'Frankfurt', 'London', 'New York', 'Toronto'],
      protonvpn: ['Amsterdam', 'Chicago', 'Frankfurt', 'London', 'Los Angeles', 'Miami', 'New York', 'Paris', 'Stockholm', 'Tokyo', 'Toronto', 'Zurich']
    },
    updated_at: new Date(started - 3 * 3600 * 1000).toISOString()
  };
  function refreshOptions() {
    if (q.get('refresh') === 'fail') return [502, { ok: false, message: 'Could not download the server list. Try again in a minute.' }];
    options.updated_at = new Date().toISOString();
    return [200, { ok: true, updated_at: options.updated_at }];
  }

  var streams = [
    { device: '192.168.1.31', channel_id: '3144', channel_name: 'News HD', p: 0, seconds: 4210, mb: 2288.5, rate: 0.54 },
    { device: '192.168.1.44', channel_id: '5120', channel_name: 'Sports 1 HD', p: 0, seconds: 1272, mb: 912.4, rate: 0.72 },
    { device: '192.168.1.52', channel_id: '207', channel_name: 'Movies Central', p: 1, seconds: 318, mb: 141, rate: 0.44 }
  ];

  function getSettings() {
    var out = JSON.parse(JSON.stringify(settings));
    out.player.password = ''; out.player.password_set = !!secrets.player;
    out.providers.forEach(function (p, i) { p.password = ''; p.password_set = !!secrets.providers[i]; });
    out.vpn.wireguard_private_key = ''; out.vpn.wireguard_private_key_set = !!secrets.wg;
    return out;
  }

  function putSettings(body) {
    var errors = [], ports = {};
    function bad(field, message) { errors.push({ field: field, message: message }); }
    var player = body.player || {}, vpn = body.vpn || {}, list = body.providers || [];
    if (!player.user) bad('player.user', 'Choose a username for your players.');
    if (!player.password && !secrets.player) bad('player.password', 'Choose a password for your players.');
    list.forEach(function (p, i) {
      var at = 'providers[' + i + '].';
      if (!p.name) bad(at + 'name', 'Give this provider a name.');
      if (!/^https?:\/\/[^\s/]+/.test(p.url || '')) bad(at + 'url', 'Enter the full address, starting with http:// or https://.');
      if (!p.user) bad(at + 'user', 'Enter the username from your provider.');
      if (!p.password && !(p.password_set && secrets.providers[i])) bad(at + 'password', 'Enter the password from your provider.');
      if (!(p.connections >= 1 && p.connections <= 999 && p.connections % 1 === 0)) bad(at + 'connections', 'Enter how many connections you bought');
      if (ports[p.port]) bad(at + 'port', 'Port ' + p.port + ' is already used by ' + ports[p.port] + '.');
      else ports[p.port] = p.name || 'another provider';
    });
    if (body.ui && ports[body.ui.port]) bad('ui.port', 'Port ' + body.ui.port + ' is already used by ' + ports[body.ui.port] + '.');
    if (!vpn.service_provider) bad('vpn.service_provider', 'Pick your VPN service.');
    if (vpn.type === 'wireguard' && !vpn.wireguard_private_key && !secrets.wg) bad('vpn.wireguard_private_key', 'Paste the WireGuard private key from your VPN account.');
    if (errors.length) return [400, { ok: false, errors: errors }];

    if (player.password) secrets.player = player.password;
    if (vpn.wireguard_private_key) secrets.wg = vpn.wireguard_private_key;
    secrets.providers = list.map(function (p, i) { return p.password || secrets.providers[i] || ''; });
    settings = {
      player: { user: player.user },
      providers: list.map(function (p) { return { name: p.name, url: p.url, user: p.user, port: p.port, connections: p.connections, live_tv: p.live_tv, advanced: p.advanced }; }),
      ui: { port: body.ui.port },
      vpn: {
        service_provider: vpn.service_provider, type: vpn.type, wireguard_addresses: vpn.wireguard_addresses,
        server_countries: vpn.server_countries, server_cities: vpn.server_cities, extra: vpn.extra || {}
      }
    };
    return [200, { ok: true }];
  }

  function getStatus() {
    var t = (Date.now() - started) / 1000;
    var vpnUp = state !== 'vpndown';
    var providers = settings.providers.map(function (p, i) {
      return { name: p.name, port: p.port, up: vpnUp && !(state === 'down' && i === 1), live_tv: p.live_tv, channels_open: 0,
        connections_missing: p.connections == null || p.connections === '' };
    });
    var playing = [];
    if (state !== 'empty' && vpnUp) {
      streams.forEach(function (s) {
        var p = providers[s.p];
        if (!p || !p.up) return;
        p.channels_open++;
        playing.push({
          device: s.device, channel_id: s.channel_id, channel_name: s.channel_name, provider: p.name,
          format: p.live_tv === 'hls' ? 'HLS' : 'MPEG-TS',
          seconds: Math.floor(s.seconds + t), mb_sent: Math.round((s.mb + s.rate * t) * 10) / 10
        });
      });
    }
    return {
      vpn: vpnUp
        ? { up: true, city: 'New York', country: 'US', ip: '203.0.113.31', ping_ms: Math.round(9 + 2 * Math.sin(t / 5) + Math.random() * 2) }
        : { up: false, city: '', country: '', ping_ms: 0 },
      providers: providers, playing: playing, version: '0.1.0'
    };
  }

  var realFetch = window.fetch;
  window.fetch = function (input, init) {
    var url = new URL(typeof input === 'string' ? input : input.url, location.href);
    var method = ((init && init.method) || 'GET').toUpperCase();
    if (url.pathname.indexOf('/api/') !== 0) return realFetch.apply(this, arguments);
    var out;
    if (url.pathname === '/api/status' && method === 'GET') out = [200, getStatus()];
    else if (url.pathname === '/api/settings' && method === 'GET') out = [200, getSettings()];
    else if (url.pathname === '/api/settings' && method === 'PUT') out = putSettings(JSON.parse(init.body));
    else if (url.pathname === '/api/vpn/options' && method === 'GET' && q.get('servers') !== 'none') out = [200, options];
    else if (url.pathname === '/api/vpn/refresh' && method === 'POST') out = refreshOptions();
    else out = [404, { ok: false }];
    return new Promise(function (resolve) {
      setTimeout(function () {
        resolve(new Response(JSON.stringify(out[1]), { status: out[0], headers: { 'Content-Type': 'application/json' } }));
      }, method === 'POST' ? 1200 : method === 'PUT' ? 500 : 120);
    });
  };
})();
