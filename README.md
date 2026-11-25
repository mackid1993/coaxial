<p align="center">
  <img src="assets/coaxial-icon.svg" width="88" alt="">
  &nbsp;&nbsp;
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/coaxial-wordmark-dark.svg">
    <img src="assets/coaxial-wordmark-light.svg" width="360" alt="coaxial">
  </picture>
</p>

Coaxial puts your IPTV providers behind a VPN without installing anything on your devices. It is one container with the
VPN ([gluetun](https://github.com/qdm12/gluetun)) built in: one image, one folder, one settings file. Your players log
in to Coaxial with one login that you choose, instead of logging in to the provider. The part of Coaxial that talks to
your providers is started only after the VPN is up.

What it does:

- **One VPN for every device.** TVs, phones and media servers need no VPN app. They reach the providers through
  Coaxial, and Coaxial reaches them through the VPN.
- **One provider connection per channel.** Every device watching or recording a channel shares it, in MPEG-TS or HLS,
  whichever each device asks for.
- **Drops are mended.** When a provider ends a stream someone is watching, Coaxial asks for it again and joins the new
  one where the old one stopped, so players and recorders carry on.
- **The closest healthy VPN server, kept up to date.** Pick your cities and, whenever nothing is playing, Coaxial
  pings every server in them once an hour and keeps you on the closest one that is not dropping packets. It moves only
  once nothing is playing, or when you press `Switch now`.
- **The VPN client stays current.** A newer gluetun is downloaded, checked and installed by itself when nothing is
  playing, and undone if the VPN does not connect with it.
- **Your provider login stays private.** Players only ever see the login you chose for them.
- **Set up in a web page.** No files to edit, and a status page that shows what is playing and where the VPN comes out.

## Read this first: there is no login

Coaxial has no authentication at all. Anyone who can reach its web page can change every setting. Anyone who can reach
a provider port can open the status page on it and can try the player login.

- Only run it in an environment you fully control and trust.
- Keep it on your own network. Never forward its ports from the internet.
- Your provider logins, VPN keys and passwords are stored in `settings.json`, in the folder you mount, as plain text.
  They cannot be hashed, because Coaxial has to send them on to the provider and the VPN service.
- Protect that file and that folder. Only the account that runs the container should be able to read them. When the
  web page saves the file, it is written readable by its owner only (mode 600). A file you write by hand keeps the
  permissions you gave it.

## Run it

Put the two files below in a folder, name the second one `.env`, and start it with `docker compose up -d`. Both are
also in this repository as [`compose.yaml`](compose.yaml) and [`.env.example`](.env.example).

<details>
<summary><code>compose.yaml</code></summary>

```yaml
# Coaxial: IPTV through a VPN, in one container.
# Providers and VPN settings are entered in the web page and kept in config/settings.json.
# The ports Docker opens are set in .env, next to this file. You never edit ports here.
services:
  coaxial:
    image: ghcr.io/mackid1993/coaxial:latest
    container_name: coaxial
    restart: unless-stopped
    cap_add:
      - NET_ADMIN                     # the built-in VPN needs it
    devices:
      - /dev/net/tun:/dev/net/tun     # the built-in VPN needs it
    volumes:
      - ./config:/config              # the one bind mount: settings.json and the VPN's files
    environment:                      # the page's port until one is saved in the page: UI_PORT in .env
      - COAXIAL_UI_PORT=${UI_PORT:-8195}
    logging:
      options:                        # the log writes a line for every request: keep at most 3 x 10 MB
        max-size: "10m"
        max-file: "3"
    ports:
      # Twenty slots, PORT_1 to PORT_20, filled from .env. A slot left out of .env opens nothing new
      # (it falls back to PORT_1, which is already open). To add a provider port, add one PORT_n line
      # to .env. Any free port, in any order.
      - "${PORT_1}:${PORT_1}"
      - "${PORT_2:-${PORT_1}}:${PORT_2:-${PORT_1}}"
      - "${PORT_3:-${PORT_1}}:${PORT_3:-${PORT_1}}"
      - "${PORT_4:-${PORT_1}}:${PORT_4:-${PORT_1}}"
      - "${PORT_5:-${PORT_1}}:${PORT_5:-${PORT_1}}"
      - "${PORT_6:-${PORT_1}}:${PORT_6:-${PORT_1}}"
      - "${PORT_7:-${PORT_1}}:${PORT_7:-${PORT_1}}"
      - "${PORT_8:-${PORT_1}}:${PORT_8:-${PORT_1}}"
      - "${PORT_9:-${PORT_1}}:${PORT_9:-${PORT_1}}"
      - "${PORT_10:-${PORT_1}}:${PORT_10:-${PORT_1}}"
      - "${PORT_11:-${PORT_1}}:${PORT_11:-${PORT_1}}"
      - "${PORT_12:-${PORT_1}}:${PORT_12:-${PORT_1}}"
      - "${PORT_13:-${PORT_1}}:${PORT_13:-${PORT_1}}"
      - "${PORT_14:-${PORT_1}}:${PORT_14:-${PORT_1}}"
      - "${PORT_15:-${PORT_1}}:${PORT_15:-${PORT_1}}"
      - "${PORT_16:-${PORT_1}}:${PORT_16:-${PORT_1}}"
      - "${PORT_17:-${PORT_1}}:${PORT_17:-${PORT_1}}"
      - "${PORT_18:-${PORT_1}}:${PORT_18:-${PORT_1}}"
      - "${PORT_19:-${PORT_1}}:${PORT_19:-${PORT_1}}"
      - "${PORT_20:-${PORT_1}}:${PORT_20:-${PORT_1}}"
      - "${UI_PORT:-8195}:${UI_PORT:-8195}"   # the settings and status page
```

</details>

<details>
<summary><code>.env</code></summary>

```
# Coaxial: the ports Docker opens for it. Copy this file to .env, change or add a line,
# then run: docker compose up -d
# Provider logins and VPN settings are NOT here: they are entered in the web page.

# ---- Ports for IPTV providers -------------------------------------------------
# One line per port: PORT_1, PORT_2, ... up to PORT_20. Any free port on this
# machine, in any order; they need not be next to each other. To add a provider,
# add the next PORT_n line, run docker compose up -d, then pick that port for it
# in the web page.
PORT_1=8095
PORT_2=8096

# ---- Port for the settings and status page -------------------------------------
UI_PORT=8195
```

</details>

Ports are set in `.env`, never in the compose file. Each provider needs its own. To add one, add the next `PORT_n` line
(up to `PORT_20`), run `docker compose up -d` again, and pick that port for the provider in the web page.

Open `http://<this machine>:8195` (or the `UI_PORT` you set in `.env`). Enter your VPN and provider settings and save.
In each player, add an Xtream login with the address `http://<this machine>:<the provider's port>` and the player login
you chose.

## The web page

**Status** shows whether the VPN is up, where it comes out (city, country, IP address, network, ping), each provider
(with `Enter how many connections you bought` under one that has no number yet), what is playing right now, and the
Coaxial version. It also has the gluetun version with `Check for updates` and the chosen VPN server with
`Check servers now`, and `Switch now` when a better server is found. An open Status view loads itself again when the Coaxial version
changes, unless settings are unsaved. When the VPN does not come up, Status says why: a `settings.json` that could
not be read, VPN settings Coaxial refused, or a VPN client that stopped before it connected.

On the first start, Settings opens on a setup panel. If a `settings.json` is there but cannot be read, the panel says
so, and warns that finishing setup replaces the file. **Settings** has:

- **Providers.** Add them, remove them, and reorder them with the up and down arrows. A new port is tried before
  saving: Coaxial listens on it for 8 seconds and the page tries to reach it from your browser. If that fails, the page
  does not save and tells you to add a `PORT_n` line for it to `.env`. Ports are tried again after a save.
  Each provider needs `Connections you bought`: how many connections your plan allows. Coaxial never opens more than
  this, and the page does not save a provider without it. Each provider also has `Advanced options`, closed until you
  open it (see below).
- **VPN.** The service and the connection type. WireGuard takes a private key and addresses. OpenVPN takes a username
  and a password. Then where to connect: countries, cities, regions, servers by hostname and servers by name. All of
  these are drop-downs filled from gluetun's own server lists, and the two server lists narrow to the places you
  picked. You can also type. `Refresh server list` has gluetun fetch a new list now, which needs the VPN to be running.
- **Extra VPN variables.** Free-form names and values for any other gluetun variable, except the few Coaxial refuses
  (see `vpn.extra` below).
- **Player login**, the port of the web page, and the **Log detail** (see [The log](#the-log)).

Saving player, provider or port settings restarts only the proxy. Saving VPN settings reconnects the VPN. Either one
cuts off live channels that are playing. Changing only the log detail restarts nothing: it applies at once. A saved password or key is never shown again: leave the field empty to keep
it. A provider's saved password is kept only while its address stays the same.

## settings.json

The page writes `settings.json` in the mounted folder (`./config` in the example). You can also edit it by hand and
restart the container. If the file is missing at start, Coaxial writes `settings.sample.json` beside it.

```json
{
  "player": { "user": "tv", "password": "choose-your-own" },
  "providers": [
    { "name": "Example", "url": "https://provider.example", "user": "...", "password": "...", "port": 8095, "connections": 2, "live_tv": "ts" }
  ],
  "ui": { "port": 8195, "log_level": "detailed" },
  "vpn": {
    "service_provider": "mullvad",
    "type": "wireguard",
    "wireguard_private_key": "...",
    "wireguard_addresses": "10.64.0.2/32",
    "server_countries": "",
    "server_cities": "",
    "update_hours": 24,
    "gluetun_version": "latest",
    "extra": { "ANY_GLUETUN_VARIABLE": "value" }
  }
}
```

| Setting | What it is |
| --- | --- |
| `player` | The login you type into every player. Letters, digits, `-` and `_` only. The username cannot be `live`, `movie`, `series` or `timeshift`. |
| `providers` | One entry per provider: a name, its address (`http://name` or `https://name`, optionally `:port`, with nothing after it), the login it gave you, the port your players use for it, and `live_tv`. A name is up to 40 letters, digits, spaces, `-`, `.` or `_`, starts with a letter or digit, and is not used by another provider. The provider's username and password cannot contain `/` or a line break. Ports 53, 8000 and 9999 and the web page's port cannot be used. |
| `connections` | How many connections you bought from this provider: a whole number from 1 to 999. Coaxial never opens more than this (see `stay_under_limit`) and never asks the provider for it. The page does not save a provider without it. A provider without it still runs, but no limit is kept and Status asks you to enter it. An older file with `connections` under `advanced` has it moved here (0 there meant no limit, and turns `stay_under_limit` off). |
| `live_tv` | `ts` if your players watch live TV as MPEG-TS, `hls` if they use HLS. |
| `advanced` | Options some providers need, all off but `stay_under_limit`. Left out, the defaults apply. See [Advanced options](#advanced-options-per-provider). |
| `ui.port` | The port of the web page and of the health check. If left out, `UI_PORT` from `.env` (8195 if that is not set either). If you change it, change `UI_PORT` in `.env` to match. |
| `ui.log_level` | How much goes into the log: `detailed` (also when left out), `normal` or `quiet`. See [The log](#the-log). |
| `vpn` | gluetun's settings. Every key but `extra`, `update_hours` and `gluetun_version` is a gluetun variable written in small letters: `wireguard_private_key` is `WIREGUARD_PRIVATE_KEY`, `service_provider` and `type` are `VPN_SERVICE_PROVIDER` and `VPN_TYPE`. The VPN service must be set. |
| `vpn.update_hours` | How often gluetun refreshes its list of your VPN service's servers (its `UPDATER_PERIOD`). 24 if left out, 0 for never. |
| `vpn.gluetun_version` | Which gluetun program runs: `latest` (also when left out), a tag of gluetun's image such as `v3.41.3` to stay on one version, or `builtin` to fetch nothing and run the one in this image. |
| `WIREGUARD_MTU` (under `vpn.extra`) | WireGuard's packet size. Coaxial sets 1320, gluetun's long-standing default, so the tunnel never depends on a size found at start. Set a number here only if your VPN service asks for one. |
| `vpn.extra` | Any other gluetun variable under its own name, such as `OPENVPN_PROTOCOL`. It is applied last, so it wins over the settings above. Refused here and under `vpn`, because anyone who can reach the web page could use them: gluetun's `..._COMMAND` variables, OpenVPN options that run programs (such as `--up`) in `OPENVPN_FLAGS`, `HTTP_CONTROL_SERVER_*` settings other than `HTTP_CONTROL_SERVER_LOG`, and values on more than one line (except the OpenVPN certificate and keys). |

gluetun keeps its server lists in `gluetun/servers` in the mounted folder, so they survive a restart.

## Advanced options (per provider)

Most providers need none of these. If something goes wrong, find it below and turn on what it points to. They are
under `Advanced options` on each provider in Settings, and under `advanced` in that provider's entry in `settings.json`.

| Having trouble? | Turn on |
| --- | --- |
| A recording stops when someone rewinds, or plays the same channel from earlier | One connection per channel |
| Someone gets kicked off when another TV starts watching | Stay under my connection limit |
| Two TVs on the same movie keep stopping each other | One connection per movie or episode |
| Nothing plays for a while after the VPN changes server | My provider locks my account to one address |
| Channels list fine but will not play | Send an app name to my provider |
| The guide or channel list fails or is slow when several TVs start | Share list and guide downloads |
| Your account gets warned or blocked after watching movies | Limit movie download speed |

- **Stay under my connection limit** (`stay_under_limit`, on by default). Coaxial does not open more streams than the
  connections you bought (`Connections you bought` on the provider, `connections` in `settings.json`), so nobody gets
  kicked off. Turn it off only if your provider has no limit.
- **One connection per channel** (`one_per_channel`, off). Only one stream of each channel at a time, live or catch-up.
  Rewinding a channel someone else is watching or recording waits until they finish. An older `settings.json` with
  `"catch_up_while_live": "refuse"` gets this turned on.
- **One connection per movie or episode** (`one_per_item`, off). Only one TV plays a movie or episode at a time.
- **My provider locks my account to one address** (`locked_to_address`, off). Coaxial does not switch VPN servers on its
  own, and after a switch new streams wait `address_wait_seconds` (90) so the provider can let go of the old address.
- **Send an app name to my provider** (`send_app_name`, off). Every request to the provider names the app in `app_name`
  instead of the player's own. The page lists common player apps, each written the way that app really sends it, and `Custom...` for any other name. Plain
  text, up to 200 characters.
- **Share list and guide downloads** (`share_lists`, off). The channel lists and the guide are downloaded once and the
  same copy goes to every TV for `share_minutes` (10).
- **Limit movie download speed** (`limit_movie_speed`, off). Movies and episodes download no faster than `movie_mbps`
  Mbit/s (100), so it looks like normal watching. Set it above the movie's own bitrate, or it stops to buffer: 100 suits
  4K remuxes; 20 is enough for most plain 1080p movies.

<details>
<summary>The defaults in <code>settings.json</code></summary>

```json
"advanced": {
  "stay_under_limit": true,
  "one_per_channel": false, "one_per_item": false,
  "locked_to_address": false, "address_wait_seconds": 90,
  "send_app_name": false, "app_name": "",
  "share_lists": false, "share_minutes": 10,
  "limit_movie_speed": false, "movie_mbps": 100
}
```

</details>

## Choosing the VPN server

gluetun picks one of the matching servers at random, near or far, busy or not. Coaxial pings every one of them
instead and keeps you on the closest healthy one, without cutting off anything that plays.

If you name a server in the settings, Coaxial uses that one and leaves it alone. If you pick one or more cities and
leave both server boxes empty, Coaxial chooses the server for you. It does not do this for the `custom` service.

- **A city is required.** With only a country or a region picked, gluetun picks a server at random, as before.
- **Every server that fits is checked.** That means every server in the cities you picked that fits your settings:
  the connection type, and `STREAM_ONLY`, `PORT_FORWARD_ONLY` and `OWNED_ONLY` when they are on. Secure core, Tor,
  free and multihop servers are left out unless the matching `..._ONLY` setting is on.
- **Checking is pinging, not connecting.** Each server gets 10 pings, a quarter of a second apart, all servers at
  once, so a round takes about 3 seconds however many servers there are. It needs no login and no connection to the
  servers, sends about 1 KB per server, and goes out the way the VPN's own packets do, outside the tunnel, so each
  server is measured as the VPN would reach it. The VPN in use is not touched, so nobody notices it.
- **What a round measures.** For each server: how many pings were lost, the middle answer time, how much the answer
  times spread (jitter), its lowest answer time over the last day (its distance), and how far the middle answer sits
  above that lowest one (time spent waiting at a busy server). Servers are ranked by lost pings first, then by answer
  time plus twice the jitter. A server that lost 10% or more in two rounds running counts as bad.
- **How often.** Once an hour, and only while nothing plays or records. While anything plays, nothing is pinged, so
  the network is left to the streams. Servers in one city rarely change places, and a constant sweep of a VPN
  service's servers from one address can look like scanning to it, so once an hour is enough.
  `Check servers now` runs a round at any time.
- **When Coaxial moves you.** When no server is chosen yet for your settings, when the one in use has been losing
  pings for two rounds running, or when another server has answered at least 15 ms sooner for two hourly rounds in a
  row. A server that is merely a few milliseconds closer is not worth a reconnect. Coaxial moves by itself only once
  nothing has played or recorded for 2 minutes; until then, the Status view shows the better server and a
  `Switch now` button, which asks first because anything playing stops for a few seconds.
- **Staying put is normal.** Servers in one city are usually within a few milliseconds of each other, and the one
  at the top of the ranking changes from round to round. Coaxial does not chase that: as long as the server in use
  is not losing pings and no other is 15 ms closer, it stays, so a good server can be kept for days.
- **If the new server does not connect** within 45 seconds, Coaxial goes back to the one before and leaves the new
  one out for an hour.
- **The firewall.** gluetun's firewall lets nothing leave outside the tunnel but the VPN's own packets. Coaxial adds
  one rule to it: outgoing pings that carry gluetun's own mark, on the container's network card. Nothing else is let
  out, and the answers come back under gluetun's own rule for answers.
- **The choice is saved** in `gluetun/pick.json` in the mounted folder. When the container starts, it goes straight to
  that server, so you can watch right away.
- **What counts as playing.** A live channel open on any device, a device that asked for an HLS playlist in the last
  30 seconds, and a movie, episode or catch-up until a minute after its last part was sent. The same rule decides when
  a VPN client update may install (below).
- **`Check servers now`** on the Status view runs a round right away. It never reconnects anything.
- **A provider locked to one address.** Once a server is chosen, Coaxial does not move by itself; `Switch now` still
  works.
- If the chosen server stops connecting for 2 minutes, Coaxial drops its choice and lets gluetun pick.
- **The pinging** is done by [pro-bing](https://github.com/prometheus-community/pro-bing) (MIT), the ping library of
  the Prometheus project. See `ACKNOWLEDGMENTS.md`.

## VPN client updates

gluetun is published only as a container image, and its program is one file in that image. So that a new gluetun does
not need a new Coaxial image, Coaxial fetches that file itself.

- **At every start of the container**, before the VPN is started, Coaxial asks gluetun's image what the tag in
  `vpn.gluetun_version` (`latest` unless you set another) is now: at `ghcr.io/qdm12/gluetun` first, and at
  `qmcgaw/gluetun` on Docker Hub if that fails. If it does not have that build yet, it downloads the layer that holds
  the program for this machine's processor. The VPN is not up yet, so this request does not go through it. When the
  VPN is started again for a settings change or another server, the registry is asked again only if the last answer
  is over 6 hours old (10 minutes after a failed one), and then before the VPN is stopped, through the VPN.
- **The download is checked** against the registry's own SHA-256 checksums, over HTTPS: each manifest against its
  digest, and the layer against the digest and size in the manifest. The file must be a program for this processor and
  must answer a harmless command before it is used. No signature is checked. This is the same trust you place in
  `docker pull`: in the registry and in the publisher's account.
- **It is kept** in `gluetun/bin` in the mounted folder: the build in use and the one before it. If the registry cannot
  be reached, a check fails or the new program does not run, Coaxial uses the last good copy, or else the program built
  into the image. The look-up and download get two minutes at most.
- **While it runs**, Coaxial asks the registry once an hour whether a newer build is out, and Status says so. It skips
  this while the VPN is down or servers are being tried. `Check for updates` asks at once. Neither reconnects the VPN.
- **A newer build installs by itself, but only when nothing is playing.** Once no one has watched or recorded anything
  for 10 minutes, Coaxial restarts the VPN with the new build.
- **If the VPN does not connect** within 2 minutes on the new build, Coaxial goes back to the build before it and skips
  the new one (`gluetun/bin/skip.json`), also after a restart, until another is out or you click `Update now`. The
  same goes for a new build taken when the container starts: if the VPN has never connected with it and stays down for
  2 minutes, Coaxial goes back to the build it last connected with.
- **`Update now`** appears when a newer build is out. It asks you first, because the VPN reconnects and anything
  playing stops. If the newer build cannot be downloaded, nothing is changed and the VPN stays connected. A newer
  build is also taken whenever the container starts again.
- **To stay on one version**, set `vpn.gluetun_version` to a tag such as `v3.41.3`, or to `builtin`. Coaxial then
  looks only at that tag, and with `builtin` it fetches nothing.

The downloaded program runs with the VPN's privileges, like the one in the image. Every start of the VPN logs which
program runs and why, on a line containing `VPN program:`. Only gluetun's own program is fetched. The system tools it
relies on, such as `iptables` and `openvpn`, come from the image and change only when the image is rebuilt.

## How it behaves

- **One provider connection per live channel.** On a `ts` provider, every device watching or recording a channel
  shares one MPEG-TS stream. A reader that joins a channel already open starts about 10 seconds back, at a point where
  a decoder can begin, and is then brought up to live. From then on it is sent video as it arrives. On an `hls`
  provider, the provider is asked for a channel once and every player's playlist comes from that one session.
- **Mixed formats.** On a `ts` provider, a player that asks for a live channel as HLS is given HLS cut from the same
  shared MPEG-TS stream, in pieces of about 4 seconds that each start at a keyframe. That HLS leaves the channel 30
  seconds after its players stop asking for it. On an `hls` provider, a reader that asks for MPEG-TS is fed from the
  players' one HLS session, at playing speed, with everything waiting sent just before each whole minute so a
  recording that stops on the minute loses nothing. If the provider's pieces start numbering their
  streams afresh (another packager, an advert), they are handed on under the numbers the reader started with, so
  recorders carry on.
- **When a provider drops a channel.** If a stream that ran at least 30 seconds ends while someone is watching,
  Coaxial asks for it again at once. The new stream is joined at its first keyframe past the last picture sent, and
  the part the provider repeated is left out, so the picture does not jump back. Up to one keyframe interval, often a
  second or two, is skipped there. If no keyframe comes within 12 seconds, it is joined where its clock passes the
  old one. Past the join, the new stream is passed on exactly as the provider sends it. On an account that allows one
  connection (or with no connections entered, or `stay_under_limit` off), Coaxial does not ask again if another
  channel of the same provider was opened in the last 15 seconds, since asking would end that one. It also stops
  asking once the channel has gone silent twice in a row.
- **When a connection goes quiet.** A connection to a provider can stop sending without closing. Coaxial never
  waits on one longer than a player would: a live MPEG-TS channel that sends nothing for 20 seconds is asked for
  again at once, however long it ran. An HLS playlist or piece of video that sends nothing for 8 seconds is asked
  for again at once on a new connection, and a piece that stopped part way is fetched on from where it stopped, so
  the player gets it whole. A quiet playlist never opens the channel again at the provider.
- **When a provider restarts a channel.** Right after ending a stream, a provider is often restarting that channel on
  its side, and for a few seconds it answers "not found" or a server error. While someone is still watching, Coaxial
  asks again a few more times, further apart each time, for up to a minute, and the picture comes back on its own.
  The same rules as above apply to every extra ask. A refusal that means the login or the channel is wrong ends the
  stream at once, as before.
- **24/7 channels and new episodes.** Some channels play one file after another, and the provider starts each file's
  clock over from zero inside the same stream. Many players take that as a huge jump and loop or freeze. Coaxial
  shifts the timestamps from that point on so the clock simply carries on, and the next episode plays. A stream whose
  clock never starts over is passed on byte for byte. If the provider cuts a packet short, Coaxial finds where the
  next packet starts and carries on from there.
- **Changing channel.** A channel nobody is on any more is let go before the next one is asked for, and the provider
  gets a tenth of a second to see it, so one-connection accounts can change channel. A channel in its first seconds is
  kept for 0.4 seconds after its last reader leaves, because players often hang up and ask again while tuning.
- **Slow devices.** A device that falls more than 16 MB behind for 15 seconds, or 64 MB at any moment, is cut off, so
  it cannot hold memory without end. Up to 100 live streams are sent at once, all channels together.
- **Redirects stay inside the VPN.** When a provider sends a player to another server, Coaxial rewrites the address so
  the player comes back to Coaxial for it. Addresses in playlists, logos and posters in the provider's lists, and the
  pictures in the guide are pointed back the same way. Coaxial refuses such an address if it leads to this machine or
  a home network.
- **Your provider login stays private.** Players log in with the player login, and Coaxial puts the provider's login
  in its place on the way out. On the way back, the provider's login is replaced with the player login wherever it
  appears: in playlists, redirects, error pages and the account details apps ask for. The account details also name
  Coaxial's address as the server, so apps build their stream addresses through Coaxial.
- **Movies, series, catch-up and the guide** are passed on as they come, seeking included. Older apps that log in
  through `panel_api.php` work too. Certificates are not checked, as most players do not check them either.
- **Xtream logins only.** A provider's ready-made M3U link (`get.php`) is refused: its addresses skip the VPN.
- **If the VPN fails.** If the VPN program stops before it has connected, the web page stays up and the VPN is started
  again after 30 seconds. If it stops after it had connected, Coaxial exits so that Docker restarts the container.
- **Kept apart.** The part that talks to your providers runs as an unprivileged user and is never given the VPN
  settings. Saving provider settings restarts only that part, so the VPN stays connected.
- **Health check.** Docker shows the container as healthy when it is set up, the VPN is up and the provider part
  answers.
- **Status page.** `http://<this machine>:<any provider's port>/status` is a plain page with the VPN's exit address,
  each provider's account status and what is playing. The exit address is asked of `ipinfo.io` through the VPN.
- **The log.** See [The log](#the-log).

## The log

Read it with `docker logs coaxial`. Coaxial's lines have `coaxial:` in them; the others are the VPN's own. Each line is
the time in UTC, a level (`DBG`, `INF`, `WRN`, `ERR`), a sentence, and the facts as `key=value`:

```
2026-11-25T01:20:00Z INF coaxial: 192.0.2.20 started channel 1234 (provider.example), first video in 0.8 s channel=1234 device=192.0.2.20 first_video_s=0.8 provider=Example
2026-11-25T01:31:12Z WRN coaxial: the provider sent nothing for 17 s on channel 1234, then carried on channel=1234 device=192.0.2.20 provider=Example seconds=17.2
2026-11-25T01:40:03Z WRN coaxial: the provider answered 403 (forbidden) channel=5678 device=192.0.2.21 kind=stream ms=212 provider=Example status=403
2026-11-25T01:40:09Z WRN coaxial: background: provider account read for the status page answered 429 (too many requests) kind="provider account read for the status page" ms=95 provider=Example status=429
```

Problems are `WRN` lines, so `docker logs coaxial 2>&1 | grep WRN` shows them all: every request a provider answered
with an error (403, 404, 429, 5xx and so on) or did not answer, whatever it was for (a channel, a movie, catch-up, a
list, the guide, the login), with what was asked (kind and number, never the address or the login), for which device
and how long it took; and every live channel that got nothing from its provider for 15 seconds or more. Pauses of a
few seconds between a provider's lumps of video are normal and never logged.

`Log detail` in the web page (`ui.log_level` in `settings.json`) sets how much is written. It applies at once,
without a restart:

| Log detail | What is written |
| --- | --- |
| Detailed (the default) | Everything below, plus each answer from a provider, each stream's end (why, how long, MB, the longest pause), a line every 10 minutes per channel playing, and what Coaxial asks on its own (status page account reads, VPN server pings, the VPN program's update check, the public address look-up), marked `background:`. |
| Normal | Who watches what, channels starting and stopping, one line per request, and every problem. |
| Quiet | Problems only. |

A line for every video piece of an HLS stream is never written. The compose file keeps Docker's log to at most three
files of 10 MB each (`logging:` in `compose.yaml`), so the log never fills the disk; keep that block if you write
your own compose file.

## Versions and builds

A version is the time the image was built, in UTC: `vYYYY.MM.DD.HHMM`. It is stamped into the program at build time and
shown on the web page and in the first line of the log. The workflow in `.github/workflows/docker-build.yml` rebuilds
the image on the 1st of every month, without a cache, so it takes in the newest Go and gluetun images. It builds for
amd64 and arm64 and pushes to GitHub's container registry under this repository's name, tagged `latest` and with the
version. Started by hand, it takes a `tag` to use in place of `latest` and a `fresh` box to build without the cache.
A push to the repository does not start it.

## Acknowledgments

See [ACKNOWLEDGMENTS.md](ACKNOWLEDGMENTS.md).

## License

[MIT](LICENSE)