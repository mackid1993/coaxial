# iptv-vpn-proxy

Put all of your IPTV behind one VPN, on every device, without installing a VPN
on any of them.

> [!NOTE]
> **Changelog** · Last updated: November 21, 2026

<details>
<summary>Changelog</summary>

### November 21, 2026

- **The relay is now written in Go.** It does the same jobs as the Python
  relay, with the same settings and the same status page, using only Go's
  standard library. Its source is still text inside the compose file: the
  stock `golang:alpine` image builds it each time the container starts
  (about 6 seconds, no network needed), and it then runs on its own. The
  status page also lists what is playing right now: the device, the channel's
  name, the provider, the format, how long it has been on and how much was
  sent.
- **HLS and MPEG-TS can share a channel with `NAME_LIVE_TV=ts`.** A device
  that asks for a channel as HLS while another device (or a recorder) has
  it as MPEG-TS is given HLS built from the same stream, in pieces of about
  4 seconds. The provider still sees one connection, so neither device is
  cut off. The other way round already worked: with `NAME_LIVE_TV=hls`, a
  device that asks for MPEG-TS shares the HLS session.
- **A device joining a channel already playing starts on a whole picture.**
  Its first video begins at the program table just before a keyframe, so its
  player does not throw away up to a second of video it cannot decode.
- **A player that hangs up and asks again while tuning keeps its full start.**
  A channel nobody is on any more is opened for the next request as for
  the first one: everything the provider sent so far, at once. Before, the
  same player asking again was treated as a second device joining and was
  given a smaller cushion, which a long pause from the provider could use up.
- **Changing channel frees the provider connection at once for HLS too.**
  On an account with one connection, an HLS device changing channel no
  longer finds the new channel refused, and another device watching the old
  channel as HLS is not cut off by it.
- **Live MPEG-TS reaches players the way the provider sends it**: not cut
  into chunks, and ended by closing the connection, as nginx did. Replies no
  longer carry a `Via: 1.1 Caddy` line.
- **Smaller fixes.** Channels whose program table lists the network table
  first work as HLS. An HLS player's piece numbers keep growing after a
  pause instead of starting again at 0. A refusal from the provider reaches
  an HLS player with the provider's own status. The relay keeps its memory
  under its 512 MB ceiling, and the containers notice a restarted tunnel
  within about 25 seconds.
- **nginx is replaced by Caddy.** The proxy container is now `iptv-caddy`. It
  runs the stock Caddy image with settings only: no plugins and no custom
  build.
  **Why:** the nginx version needed a lot of custom configuration to do its
  job, and every line of it was something that could break when nginx was
  updated. Caddy needs far less, so the proxy can take every security update.
  Caddy also keeps its connections to the provider's video servers open and
  reuses them by itself. So the list of learned video servers, the loop that
  kept it, and the nginx reloads it caused are all gone.
- **More of the work moved into the relay.** It now:
  - keeps the session memory for live HLS channels (one provider session per
    channel), which nginx used to keep
  - rewrites HLS playlists itself. It works out every address in a playlist
    the way a player does, from where the playlist really came from, so every
    way of writing an address is handled. The rare forms the old version
    could not handle (`./../x`, `folder/../../x`, a bare `..`, a line that
    climbs more than 32 folders) now work too.
  - points the logos and posters in the provider's lists (`player_api.php`)
    back at the proxy, as nginx did
- **What you notice:** nothing in `.env` changes. The ports, the easy login
  and the status page address are the same. The proxy's log is now
  `docker logs iptv-caddy`, with one line per request (see
  [When something is wrong](#when-something-is-wrong)). The two Docker
  volumes the nginx version used, for the session memory and the learned
  video servers, are no longer used.
- **A safety gap closed.** The proxy refuses to fetch from this machine or
  your home network, but like the nginx version it used to check only
  addresses written as numbers. A server name that leads to this machine
  (such as `localtest.me`) got through. Now the relay looks every such name
  up before Caddy connects, and refuses it. Found with an adversarial test.
- **The log is easier to read.** One line per request, your local time
  first, only the useful fields, and two new ones: which server answered and
  how long it took to start. A player hanging up mid-stream is normal and no
  longer shows as a warning.
- **One kind of login is not supported any more.** A provider whose username
  or password contains a slash (`/`) is skipped at start, with that reason in
  the log and on the status page: Caddy cannot pass a slash on inside an
  address. Every other character still works, in both the address and the
  `username=`/`password=` form.
- **Two things behave differently.** The session memory is now kept in the
  relay's memory instead of on disk, so restarting the relay opens playing
  HLS channels afresh. And live TV, playlists, the provider's lists and the
  check of where a server's name leads now go through the relay, so if the
  relay is not running they do not work; the nginx version had a fallback
  for some of those.
- **Measured on one real setup, with two real providers.** The same requests
  went through both versions, taking turns, and the middle value of 4 rounds
  is shown. It is a short sample, so read it as a direction only:
  - a movie, time to the first byte: 184 ms with Caddy, 235 ms with nginx
  - a seek to the middle of a movie, time to the first byte: 130 ms against
    239 ms
  - the first 2 MB after that seek: 0.16 s against 0.78 s
  - the first 32 MB of a movie: 1.35 s against 1.17 s (the provider's speed
    decides most of this one)
  - a channel list: 0.32 s against 0.27 s
  - the account check: 78 ms against 65 ms (the provider's lists now go
    through the relay)
- **If you are updating:** replace `compose.yaml`, then run
  `docker compose up -d --remove-orphans --force-recreate`. A plain `up -d`
  does not notice changes to the configuration inside `compose.yaml`, and
  `--remove-orphans` removes the old `iptv-nginx` container. Leave `.env` as
  it is. When you like, remove the two old volumes with
  `docker volume rm <folder>_hls-memory <folder>_video-servers` (for example
  `iptv-vpn_hls-memory` when the folder is named `iptv-vpn`).

### November 16, 2026

- **The status page shows the ping to your VPN server.** A new line under
  "Your VPN" gives the round trip from this machine to the VPN server, so a
  far-away or slow server shows at a glance. It is measured each time the page
  loads and is left out if the VPN server does not answer.

### November 15, 2026

- **Fixes from a review of the code:**
  - A device joining a low-bitrate channel (radio, audio-only, very low
    bitrate SD) that was already playing elsewhere got nothing. It now plays.
  - With a provider whose address is `http://` and which also sends video
    over `https://` from the same server name, the proxy's requests to that
    provider could stop working after it had played video. They now go to
    the right place.
  - On the HLS setting, a device asking for MPEG-TS with the short form of
    a channel's address (without `/live/`) opened a second session at the
    provider for a channel already being watched, so the two ended each
    other. It now shares the one session.
  - On the HLS setting, a single piece of video the relay was not allowed to
    fetch ended the channel. It is now skipped like any other missing piece.
- **Smoother joining.** A device that joins a channel already playing on
  another one now gets its 10-second cushion at playing speed instead of all
  at once, so it starts after a short buffer and plays smoothly instead of
  showing a picture, freezing and catching up.
- **MPEG-TS built from an HLS provider is smoother.** When a device asks for
  MPEG-TS and the provider is set to `NAME_LIVE_TV=hls`, the relay builds that
  stream from the channel's HLS. It used to check the channel's playlist more
  slowly than the HLS specification tells players to, so it picked up the
  provider's pieces late and in bunches, and it handed each piece over all at
  once. The stream grew in jumps of several seconds, and a device reading it
  at its newest point stopped and waited between them. Now:
  - It checks the playlist at the pace the HLS specification sets for
    players.
  - It hands the video on at playing speed from a small queue, so the stream
    grows steadily and a piece the provider publishes a little late is
    covered by the one still going out.
  - From one second before each whole minute of the clock until just after
    it, it holds nothing back. Anything that stops reading on the minute, as
    a device following a TV guide does, has been given everything the relay
    had.
- Nothing changes for HLS players or for providers set to
  `NAME_LIVE_TV=ts`, and nothing more is asked of the provider than a player
  would ask.

### November 14, 2026

- **There are now three containers instead of two.** A small helper, called
  the relay, has joined the VPN and nginx. It is a short Python program that
  runs in the official Python image with nothing installed on top, and its
  whole text sits inside `compose.yaml`, the same way the nginx settings do.
  You still fill in one file, `.env`, and nothing else.
- **Each provider needs one more line in `.env`:** `NAME_LIVE_TV=hls` or
  `NAME_LIVE_TV=ts`. It says which live format your players use for that
  provider, so set it to match what is chosen in the player. There is no
  default. A provider without this line is skipped, and the status page tells
  you which line to add.
  **Why it is needed:** it fixes a real fault. Many providers allow one
  session per channel on an account, and an MPEG-TS request and an HLS
  request for the same channel count as two. So when a recorder asked for
  MPEG-TS while a player was watching that channel on HLS, each new request
  ended the other one: the picture froze and recordings broke. The proxy can
  keep everyone on a channel on a single provider session, but only if it
  knows which format your players use, and it cannot work that out by itself.
- **New status page.** Open
  `http://<the machine's address>:<any provider's port>/status` in a web
  browser. It shows what is set up, what is wrong and how to fix it, where the
  VPN comes out, the state of each provider account, and what is playing. To
  show this it asks ipinfo.io (through the VPN) where the VPN comes out, and
  asks each provider for its account details, only while the page is open.
- **MPEG-TS: one connection per channel.** Several devices watching or
  recording the same channel now share one connection to the provider. A
  device that joins a channel that is already playing starts about 10 seconds
  behind live, so it has some video in hand if the provider pauses. When you
  change channel, the old connection is let go before the new one is opened.
- **HLS: watch and record a channel at once.** A recorder that asks for
  MPEG-TS is now fed from the channel's HLS session, the one your players are
  already on. A recorder and a viewer on one channel no longer knock each
  other off.
- **HLS: the proxy lets go of the provider sooner when you stop.** The proxy
  keeps its connection to a provider's video server open for a while after
  the last request, so the next one can reuse it. That time is now 20 seconds;
  it was 55. This is only the proxy's side. How long the provider itself goes
  on counting you as connected after you stop is up to the provider and is
  usually longer; no fixed figure is claimed for it here. Connections are
  still reused while a channel plays.
- **nginx is still plain nginx.** It is the stock image with settings only and
  no add-on scripting. Everything new that needed a program lives in the
  relay. If the relay is not running, TV still plays, without the sharing and
  without the status page.
- **Safety.** The proxy is stricter about refusing addresses that point at the
  machine itself or at your home network: it catches another way of writing
  such an address, and the relay checks where a provider's redirect really leads
  before it connects. The notes nginx and the relay pass to each other are
  never accepted from a player or from a provider's server, so a server
  cannot use them to steer your player outside the VPN.
- **If you are updating:** replace `compose.yaml`, add the `NAME_LIVE_TV` line
  to every provider in `.env`, then run
  `docker compose up -d --force-recreate`. Until a provider has that line it
  is skipped, and with none left nothing plays; the status page says which
  line is missing. A provider can no longer use port 8197, which the relay
  now uses inside the stack. A change to `.env` now restarts the relay along
  with nginx, which ends any shared MPEG-TS stream for a moment.

</details>

> [!TIP]
> **There is a status page.** Open
> `http://<the machine's address>:<any provider's port>/status` in a web
> browser, for example `http://192.168.1.50:8095/status`. It shows what is set
> up, what is wrong and the exact line to fix, where the VPN comes out, the
> state of each provider account, and what is playing. Look there first when
> something does not work.

Three containers run on a machine at home. Your players connect to that
machine instead of to your IPTV provider, and everything they fetch leaves
through the VPN. On the phone, TV or streaming box you type an address and a
login into your player, the same way you would with the provider's own
details. That is the whole setup on the device.

- **gluetun** holds the VPN tunnel.
- **Caddy** sits inside that tunnel and passes your players' requests on to the provider.
- **the relay**, a small Go helper, sits inside the tunnel too. It keeps
  one provider session per live channel, so several devices can watch or
  record the same channel. It points the addresses inside playlists and the
  provider's lists back at the proxy, so those come through the VPN too. It
  also draws the status page.

> **For your home network only.** The proxy ports have no protection of their
> own. Never forward them from the internet. To watch away from home, reach
> your home network privately (Tailscale, your own WireGuard, and so on).

Use it only with services you are entitled to use.

## Contents

- [Design goals](#design-goals)
- [Why this exists](#why-this-exists)
- [What you need](#what-you-need)
- [Set it up](#set-it-up)
- [The two files](#the-two-files)
- [Everyday use](#everyday-use)
- [With a playlist manager](#with-a-playlist-manager)
- [How it works](#how-it-works)
- [What was tested, and how](#what-was-tested-and-how)
- [Trade-offs and things to know](#trade-offs-and-things-to-know)
- [What it stores, updating, removing](#what-it-stores-updating-removing)
- [Extras that need one more file](#extras-that-need-one-more-file)
- [Platform notes](#platform-notes)
- [When something is wrong](#when-something-is-wrong)
- [How it works (technical)](#how-it-works-technical)
- [Support](#support)
- [License](#license)

## Design goals

1. **Parity with direct credentials.** A player pointed at the proxy should
   behave exactly as if you had typed the provider's own Xtream login into it.
   Not better, not different: the same, with the VPN added.
2. **One VPN for everything.** Every device and every player goes out through
   the same tunnel, and no device needs a VPN app.
3. **The easiest setup that can work.** One settings file. You never edit the
   compose file. Adding a provider is five lines.
4. **Works with any player and any Xtream provider.** Nothing in it is written
   for one provider or one app.
5. **Nothing to build or install.** It is stock gluetun, stock Caddy and the
   stock Go image, with nothing added to any of them. Everything that
   makes them a proxy is text inside the compose file, where you can read it:
   the Caddy settings, a few lines of shell that read your settings at start
   and write one Caddy block per provider, and one Go program (the relay)
   that uses only Go's standard library and is built when it starts.

## Why this exists

A plain reverse proxy in front of an IPTV provider usually works for MPEG-TS
and then freezes every few seconds on HLS. The cause:

- Many providers start a **new session** every time a channel's address is asked for.
- A new session **ends the previous one** on that channel.
- An HLS player asks for that address again every few seconds. A second device
  or a recording on the same channel asks too.

A proxy that makes a fresh trip to the provider for each of those requests
keeps ending its own session, so the pieces the player was about to download
stop existing. A player connected directly does not have this problem, because
it follows the provider's redirect once and then stays on that session.

This proxy does what the direct player does. It remembers where the provider
sent each channel and keeps every reader of that channel on that one session.

MPEG-TS has the same problem as soon as a second device or a recorder opens a
channel that is already playing. With a provider that allows one connection
per channel, the readers end each other over and over. So the relay opens each
live MPEG-TS channel at the provider once and gives every reader of that
channel the same video.

## What you need

- A machine at home that is always on, with Docker and Docker Compose 2.24 or newer.
- A VPN that gluetun supports, or a WireGuard or OpenVPN config file from any VPN.
- IPTV providers that give you an **Xtream login**: a server address, a username and a password.

## Set it up

1. Make a folder and put `compose.yaml` and `.env.example` in it. Both are in
   this repository.
2. Copy `.env.example` to `.env`.
3. Fill in `.env`. It has four sections and explains each one:
   your VPN, the easy login you choose, the port range, and your providers.
   Each provider is five lines. The fifth, `NAME_LIVE_TV`, is `hls` or `ts`:
   whichever live format is chosen in your player for that provider.
4. Start it:
   ```
   docker compose up -d
   ```
5. Check that it works. Open the status page in a web browser:
   ```
   http://<the machine's address>:<any provider's port>/status
   ```
   It says whether the proxy is working, lists each provider, and names
   anything that is wrong along with the line to fix. The log of the
   `iptv-caddy` container says the same in short at each start:
   ```
   PROVIDER PROVIDER1 on port 8095 -> https://provider.example
   STATUS PAGE: open http://<this machine's address>:8095/status in a web browser
   ```
   A provider with a problem is listed as `SKIPPED` with the reason, and the
   others still start. Each of the three containers keeps a log:
   ```
   docker logs iptv-caddy
   docker logs iptv-relay
   docker logs iptv-vpn
   ```
6. In your player or playlist manager, add an **Xtream** source:
   - server: `http://<the machine's address>:<that provider's port>`
   - username and password: the `PLAYER_USER` and `PLAYER_PASSWORD` you chose

Each provider gets its own port. The same easy login works for all of them.

## The two files

These are the same files that sit in this repository, shown here in full so
you can read and copy them without leaving the page. Click a heading to open
it.

### `.env.example`: every setting, with instructions

Copy it to `.env` and fill it in. This is the only file you edit. It covers the
VPN, the login your players use, the ports, and one block of five lines per
provider.

<details>
<summary><b>Show .env.example</b></summary>

```sh
##############################################################################
# Settings for the IPTV VPN proxy. Copy this file to ".env" and fill it in.
#
# This one file holds everything:
#   1. your VPN (the tunnel everything goes out through)
#   2. the one easy login your apps use with the proxy
#   3. the ports the proxy may use
#   4. one block of five lines per IPTV provider
#
# You never edit compose.yaml. Change this file, then bring the stack up
# again ("docker compose up -d", or the Up / Deploy / Build button of your
# Docker manager). Compose notices the change and restarts what needs it:
#   - a change to a provider or to the easy login restarts only the proxy
#     and its helper (the relay); the VPN stays up
#   - a change to a VPN setting or to the port range also restarts the tunnel,
#     which interrupts whatever is playing for a few seconds
#
# Lines starting with # are notes and are ignored.
# Write values plainly after the = sign, with no spaces around the =.
# A value containing anything besides letters, digits and  . - _ : / ,
# goes between single quotes:   SOMETHING='p@ss w#rd$1'
##############################################################################

# Your time zone, for the times in the logs (e.g. America/New_York, Europe/London).
TZ=Etc/UTC


##############################################################################
# 1. THE VPN
#
# The tunnel is made by gluetun, and every line in this section is one of
# gluetun's own settings, described in its documentation:
#     https://github.com/qdm12/gluetun-wiki   (look up your VPN provider
#     under "setup/providers" for the exact lines it needs)
# The settings shown in this section are most of the ones this stack passes on
# to gluetun (SERVER_REGIONS and SERVER_HOSTNAMES are passed on too). The
# README says how to add a different one.
#
# Pick ONE of the four ways below, remove the # from its lines and fill them
# in. Leave the others commented out.
##############################################################################

# ---- Way 1: WireGuard, a VPN provider gluetun knows by name ---------------
# The name is your VPN company in lower case, for example mullvad, protonvpn,
# nordvpn, surfshark, airvpn or ivpn. WIREGUARD_ADDRESSES is the
# "Address" line of the WireGuard file your provider gives you. The preshared
# key is only for providers that give one. The last two lines are optional
# and choose where you exit.
# VPN_SERVICE_PROVIDER=mullvad
# VPN_TYPE=wireguard
# WIREGUARD_PRIVATE_KEY=
# WIREGUARD_ADDRESSES=10.64.222.21/32
# WIREGUARD_PRESHARED_KEY=
# SERVER_COUNTRIES=Netherlands
# SERVER_CITIES=Amsterdam

# ---- Way 2: WireGuard, from a .conf file of any provider or your own server
# Copy each value from the .conf file:
#   WIREGUARD_PRIVATE_KEY     "PrivateKey" under [Interface]
#   WIREGUARD_ADDRESSES       "Address" under [Interface]
#   WIREGUARD_PUBLIC_KEY      "PublicKey" under [Peer]
#   WIREGUARD_PRESHARED_KEY   "PresharedKey" under [Peer], if there is one
#   WIREGUARD_ENDPOINT_IP     "Endpoint", the part before the colon
#   WIREGUARD_ENDPOINT_PORT   "Endpoint", the part after the colon
# VPN_SERVICE_PROVIDER=custom
# VPN_TYPE=wireguard
# WIREGUARD_PRIVATE_KEY=
# WIREGUARD_ADDRESSES=10.0.0.2/32
# WIREGUARD_PUBLIC_KEY=
# WIREGUARD_PRESHARED_KEY=
# WIREGUARD_ENDPOINT_IP=203.0.113.10
# WIREGUARD_ENDPOINT_PORT=51820

# ---- Way 3: OpenVPN, a VPN provider gluetun knows by name ------------------
# For example nordvpn, expressvpn, cyberghost or surfshark. The protocol is
# udp (faster) or tcp (gets through more networks). SERVER_COUNTRIES is
# optional.
# VPN_SERVICE_PROVIDER=nordvpn
# VPN_TYPE=openvpn
# OPENVPN_USER=
# OPENVPN_PASSWORD=
# OPENVPN_PROTOCOL=udp
# SERVER_COUNTRIES=Netherlands

# ---- Way 4: OpenVPN, from an .ovpn file ------------------------------------
# gluetun has to be handed the file itself. That takes one small extra file
# next to compose.yaml; the README shows it ("Extras that need one more
# file"). The login lines are only needed if the file asks for one.
# VPN_SERVICE_PROVIDER=custom
# VPN_TYPE=openvpn
# OPENVPN_CUSTOM_CONFIG=/gluetun/custom.ovpn
# OPENVPN_USER=
# OPENVPN_PASSWORD=

# ---- Sometimes needed with WireGuard ---------------------------------------
# Lower the MTU if streams stall or pages half-load (try 1320, then 1280).
# WIREGUARD_MTU=1320
# Keep the tunnel awake if it drops when idle.
# WIREGUARD_PERSISTENT_KEEPALIVE_INTERVAL=25s
# Use this if the machine's kernel has no WireGuard (some NAS models, WSL 2).
# WIREGUARD_IMPLEMENTATION=userspace

# ---- Optional, for any of the four ways -----------------------------------
# Your players reach the proxy without any setting here.
#
# FIREWALL_OUTBOUND_SUBNETS is for the other direction: networks the
# containers may connect OUT to directly, outside the tunnel. Leave it out
# unless you know you need it.
# FIREWALL_OUTBOUND_SUBNETS=192.168.1.0/24
#
# Which DNS the tunnel uses. By default gluetun uses encrypted DNS and blocks
# known-malicious names; leave these out unless you know you need them.
#
# To use one particular DNS server instead (your VPN provider's own, for
# example), set the two lines below. THE ADDRESS MUST END IN :53, the DNS
# port. Without the port gluetun does not accept the address and nothing can
# look up names. For several servers, separate them with commas.
#     DNS_UPSTREAM_RESOLVER_TYPE=plain
#     DNS_UPSTREAM_PLAIN_ADDRESSES=10.0.0.1:53
# (for example: DNS_UPSTREAM_RESOLVERS=quad9   or   BLOCK_MALICIOUS=off)


##############################################################################
# 2. THE LOGIN THE PROXY ANSWERS TO
#
# This is the easy username and password you type into your player or
# playlist manager, for every provider. The proxy swaps it for that
# provider's real login on the way out, so the real logins are typed in one
# place only: this file. The same easy login works for every provider; the
# PORT picks the provider.
# You choose these two. Use letters, digits, - and _ only.
##############################################################################
PLAYER_USER=choose-a-name
PLAYER_PASSWORD=choose-a-password


##############################################################################
# 3. THE PORTS THE PROXY MAY USE, as a range: FIRST-LAST
#
# Every provider below needs its own port inside this range. Set it to
# whatever ports are free on your machine: 8095-8096 is room for two
# providers, 8095-8099 for five. Widen or narrow it any time and bring the
# stack up again (that one change also restarts the VPN).
##############################################################################
PROXY_PORTS=8095-8096


##############################################################################
# 4. YOUR PROVIDERS
#
# Each provider is a block of five lines that share one NAME:
#
#     NAME_URL=https://provider.example      the server address the provider
#                                            gave you: http:// or https://,
#                                            the server name, optionally
#                                            :port, and nothing after that
#     NAME_PORT=8095                         a free port inside PROXY_PORTS
#     NAME_USER=the-username-they-gave-you
#     NAME_PASSWORD=the-password-they-gave-you
#     NAME_LIVE_TV=hls                       how your players watch its live
#                                            channels: hls or ts (see below)
#
# NAME is any word you like in CAPITALS (letters, digits, _), for example
# PROVIDER1 or SPORTS. It is only a label; all five lines must use the same
# one.
#
# NAME_LIVE_TV: HLS OR MPEG-TS. Say which live format your players use for
# this provider. There is no default, and a provider without this line is
# left out.
#     hls   Your players use HLS. Everyone watching a channel shares one
#           connection to the provider. A recorder that asks for MPEG-TS is
#           given the same video as your players, so you can watch and
#           record a channel at once.
#     ts    Your players use MPEG-TS. Everyone on a channel shares one
#           connection to the provider and gets the provider's own stream,
#           so you can watch and record a channel at once. A device that
#           asks for HLS instead is given HLS built from that same stream,
#           so it shares the connection too.
# Use the format chosen in your players, and in your playlist manager if you
# have one. They all have to agree. If a provider offers only one of the
# two, use that one.
#
# PROVIDER LOGINS CAN CONTAIN ANY CHARACTERS. You do not get to choose them,
# so the proxy takes whatever the seller gave you. One rule for THIS FILE: if
# a username or password has anything in it besides letters and digits, put
# it between single quotes, exactly as given:
#        NAME_PASSWORD='p@ss w#rd$1%'
# Without the quotes, a $ or a # in the value is read as something else and
# part of the login is silently lost. If the login itself contains a single
# quote, write that one character as \'  (backslash, then the quote).
#
# TO ADD A PROVIDER
#   1. Copy the example block below, remove the # in front of its five
#      lines, change NAME, and fill in the values.
#   2. Bring the stack up again.
#   3. Check it was picked up: the iptv-caddy container's log lists the
#      providers found at every start, like
#          PROVIDER PROVIDER1 on port 8095 -> https://provider.example
#      A provider with a problem is listed as SKIPPED with the reason, and
#      the other providers keep working.
#      The status page shows the same and more. Open it in a web browser:
#          http://<this machine's address>:<any provider's port>/status
#   4. In your player or playlist manager add an Xtream source with
#          server    http://<this machine's address>:<NAME_PORT>
#          username  the PLAYER_USER above
#          password  the PLAYER_PASSWORD above
#
# TO REMOVE A PROVIDER: delete its five lines (or put # in front of them)
# and bring the stack up again.
#
# A provider's port is what tells the proxy which provider you mean, so if
# you change a port, change it in your apps too.
##############################################################################

# ---------- example (remove the # signs to use it) ----------
# PROVIDER1_URL=https://provider.example
# PROVIDER1_PORT=8095
# PROVIDER1_USER=the-username-they-gave-you
# PROVIDER1_PASSWORD=the-password-they-gave-you
# PROVIDER1_LIVE_TV=hls

# ---------- a second one ----------
# PROVIDER2_URL=http://another-provider.example:8080
# PROVIDER2_PORT=8096
# PROVIDER2_USER=the-username-they-gave-you
# PROVIDER2_PASSWORD='the-p@ssword-they-gave-you'
# PROVIDER2_LIVE_TV=hls

```

</details>

### `compose.yaml`: the stack itself

You do not edit this one. It is in this repository as
[compose.yaml](compose.yaml) and shown in full below. It defines the three
containers and holds, as plain text with a comment on each part saying what it
is for:

- Caddy's settings, and the block written once for each provider
- the short shell script that reads your settings when Caddy starts and
  writes those blocks
- the relay, a Go program of about 3,100 lines in four files that uses only
  Go's standard library

Nothing is downloaded or installed when the stack starts except the three
stock images.

<details>
<summary><b>Show compose.yaml</b></summary>

```yaml
##############################################################################
# IPTV through a VPN, with nothing to install on your devices
#
#   iptv-vpn     gluetun. Holds the VPN tunnel (WireGuard or OpenVPN, any
#                provider gluetun supports).
#   iptv-caddy   Caddy, a reverse proxy in front of your IPTV providers. It
#                lives inside the tunnel's network, so everything it fetches
#                from a provider goes out through the VPN and nothing else can.
#   iptv-relay   A small helper beside Caddy, also inside the tunnel's
#                network. It keeps one provider session per live channel (HLS
#                and MPEG-TS alike), so several of your devices can watch or
#                record the same channel; it points the addresses inside
#                playlists and the provider's lists back at the proxy; and it
#                draws the status page. It is a small Go program, standard
#                library only, built at every start in the stock Go image; its
#                whole text is at the bottom of this file.
#
# Your players and playlist managers connect to this machine instead of to
# the provider:
#        http://<this machine>:<the provider's port>
#        username / password = PLAYER_USER / PLAYER_PASSWORD from .env
# and behave as if the provider's own Xtream login had been typed into them.
#
# YOU NEVER EDIT THIS FILE. Every setting is in .env (copy .env.example to
# .env and fill it in). To add a provider, add five lines to .env:
#        NAME_URL=https://provider.example
#        NAME_PORT=8095
#        NAME_USER=...
#        NAME_PASSWORD=...
#        NAME_LIVE_TV=hls          (or ts: how your players watch its live TV)
# then bring the stack up again. The iptv-caddy log lists the providers it
# found at each start, and names any it skipped and why.
#
# What the proxy does besides passing bytes on (each part is explained where
# it sits in the config at the bottom of this file):
#   1. keeps redirects inside the tunnel
#   2. refuses to be pointed at this machine or your home network
#   3. points the pieces of an HLS playlist back at itself
#   4. keeps ONE provider session per live HLS channel
#   5. swaps the easy login for the provider's
#   6. keeps ONE provider connection per live channel for MPEG-TS readers
#      and recorders too
# Caddy does 1, 2 and 5 and passes every byte on; the relay does 3, 4 and 6.
#
# To see what is set up, and what is wrong when something is, open
#        http://<this machine>:<any provider's port>/status
# in a web browser. The iptv-caddy log prints that address at every start.
#
# Each container keeps a log of what it is doing:
#        docker logs iptv-vpn        the VPN tunnel
#        docker logs iptv-caddy      the providers it found, and every request
#                                    (one line each, never the address)
#        docker logs iptv-relay      each live channel it opens and closes,
#                                    and anything that went wrong with one
#
# Works with HLS, MPEG-TS, movies, series, catch-up and the guide.
#
# FOR YOUR HOME NETWORK ONLY. The proxy ports have no protection of their
# own. Never forward them from the internet; reach them from outside with
# your own private network (Tailscale, WireGuard, ...) instead.
#
# Good to know:
#   - Xtream logins only. A provider's ready-made M3U link (get.php) is
#     refused, because the addresses inside it skip the VPN.
#   - When a player logs in, the provider's reply includes the provider's own
#     server address, and the proxy passes that reply on unchanged. A player
#     that connects to that address instead of the one you typed would skip
#     the VPN.
#   - Logos and posters named in the provider's lists are pointed back at
#     the proxy, so they come through the VPN too. Pictures named inside the
#     guide file are not; the player fetches those directly.
##############################################################################

services:

  # ---------------------------------------------------------------------------
  # The VPN tunnel. Anything sharing this container's network has no other way
  # out, so if the tunnel is down nothing leaks. All of its settings are
  # gluetun's own, read from .env (see the notes there).
  # ---------------------------------------------------------------------------
  vpn:
    image: qmcgaw/gluetun:latest
    container_name: iptv-vpn
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    ports:
      # Every provider port, same number inside and out. One range covers all
      # providers, present and future. The range is PROXY_PORTS in .env: set it
      # to whatever ports are free on your machine.
      - "${PROXY_PORTS:-8095-8096}:${PROXY_PORTS:-8095-8096}"
    # The VPN settings, taken from .env by name. A setting you leave out of
    # .env is simply not passed on, and gluetun uses its own default. Only a
    # change to one of these (or to PROXY_PORTS) restarts the tunnel; adding or
    # changing an IPTV provider does not.
    environment:
      - TZ
      - VPN_SERVICE_PROVIDER
      - VPN_TYPE
      - SERVER_COUNTRIES
      - SERVER_REGIONS
      - SERVER_CITIES
      - SERVER_HOSTNAMES
      - WIREGUARD_PRIVATE_KEY
      - WIREGUARD_ADDRESSES
      - WIREGUARD_PRESHARED_KEY
      - WIREGUARD_PUBLIC_KEY
      - WIREGUARD_ENDPOINT_IP
      - WIREGUARD_ENDPOINT_PORT
      - WIREGUARD_MTU
      - WIREGUARD_PERSISTENT_KEEPALIVE_INTERVAL
      - WIREGUARD_IMPLEMENTATION
      - OPENVPN_USER
      - OPENVPN_PASSWORD
      - OPENVPN_PROTOCOL
      - OPENVPN_CUSTOM_CONFIG
      - FIREWALL_OUTBOUND_SUBNETS
      - DNS_UPSTREAM_RESOLVER_TYPE
      - DNS_UPSTREAM_RESOLVERS
      - DNS_UPSTREAM_PLAIN_ADDRESSES
      - BLOCK_MALICIOUS
      # gluetun's own control page stays inside the container.
      - HTTP_CONTROL_SERVER_ADDRESS=127.0.0.1:8000

  # ---------------------------------------------------------------------------
  # Caddy. Shares the tunnel's network. Its whole configuration is the text
  # under "configs:" at the bottom of this file: Caddy's settings, the block
  # written once for each provider, and the few lines that write those blocks
  # from .env at every start.
  # ---------------------------------------------------------------------------
  caddy:
    image: caddy:alpine
    container_name: iptv-caddy
    restart: unless-stopped
    network_mode: "service:vpn"         # all its traffic goes through the tunnel
    depends_on:
      vpn:
        condition: service_healthy
        restart: true                   # comes back with the tunnel
    # The provider lines are read from your settings file. That file is .env;
    # Portainer saves the one you upload as stack.env. Whichever exists is used.
    env_file:
      - path: .env
        required: false
      - path: stack.env
        required: false
    # The machine's own clock setting, so the log shows your local time (the Caddy image has no time zones of its own).
    volumes:
      - /etc/localtime:/etc/localtime:ro
    configs:
      - source: caddyfile
        target: /etc/caddy/Caddyfile
      - source: provider-block
        target: /etc/caddy/provider.tmpl
      - source: caddy-start
        target: /start.sh
    entrypoint: ["sh", "/start.sh"]
    # Healthy = Caddy is still inside the tunnel container's network AND the
    # settings were usable (see the status page) AND it answers. If the tunnel
    # container was restarted underneath Caddy, Caddy is left in a dead network
    # with no address of its own; then the check waits a moment for the tunnel
    # container to be back and stops Caddy, so Docker starts it again inside
    # the tunnel's new network.
    healthcheck:
      test: ["CMD-SHELL", "ip -o addr show scope global | grep -q . || { sleep 12; kill 1; exit 1; }; [ ! -s /run/iptv-proxy/problem ] || exit 1; wget -q -T 5 -O /dev/null http://127.0.0.1:$${HEALTH_PORT:-8195}/healthz || exit 1"]
      interval: 10s
      timeout: 20s
      start_period: 20s
      retries: 3

  # ---------------------------------------------------------------------------
  # The relay. Go, standard library only (the files under "relay-*" at the bottom
  # of this file), built at every start in the stock Go image, with nothing to
  # download. It shares the tunnel's network like Caddy does, so it has no other
  # way out. Caddy hands it live MPEG-TS (part 6), HLS playlists (parts 3 and 4),
  # the provider's lists (player_api.php) and the status page, and asks it where a
  # server's name really leads before fetching from a server the provider sent a
  # player to (part 2). Movies, series, catch-up, the guide and every video piece
  # go from Caddy to the provider directly.
  # ---------------------------------------------------------------------------
  relay:
    image: golang:alpine
    container_name: iptv-relay
    restart: unless-stopped
    network_mode: "service:vpn"
    user: "65534:65534"                 # needs no privileges
    depends_on:
      vpn:
        condition: service_healthy
        restart: true
    mem_limit: 512m                     # a ceiling for the build at start and the channels being watched (GOMEMLIMIT keeps the heap under it)
    env_file:
      - path: .env
        required: false
      - path: stack.env
        required: false
    configs:
      - source: relay-main
        target: /src/main.go
      - source: relay-hls
        target: /src/hls.go
      - source: relay-status
        target: /src/status.go
      - source: relay-tshls
        target: /src/tshls.go
      - source: relay-gomod
        target: /src/go.mod
    # Of everything in .env, the relay is given only the addresses of the
    # providers that are set up (checked the way Caddy's start checks them). No login,
    # no password and no VPN key is kept in its environment. It is built once at
    # start (no network needed: standard library only) and then runs on its own.
    command:
      - sh
      - -c
      - |
        list=""
        for n in $$(env | sed -n 's/^\([A-Z][A-Z0-9_]*\)_URL=.*/\1/p'); do
          case "$$n" in PROXY|PLAYER|VPN|OPENVPN|WIREGUARD|SERVER|FIREWALL|DNS|RELAY|IPTV) continue;; esac
          case "$$(printenv "$${n}_LIVE_TV")" in hls|ts) ;; *) continue;; esac
          printenv "$${n}_URL" | grep -Eqx 'https?://[A-Za-z0-9._-]+(:[0-9]{1,5})?/?' || continue
          printenv "$${n}_PORT" | grep -Eqx '[1-9][0-9]{0,4}' || continue
          [ -n "$$(printenv "$${n}_PORT")" ] && [ -n "$$(printenv "$${n}_USER")" ] && [ -n "$$(printenv "$${n}_PASSWORD")" ] || continue
          list="$$list $$(printenv "$${n}_URL")"
        done
        cd /src && CGO_ENABLED=0 GOCACHE=/tmp/gocache GOFLAGS=-trimpath GOTOOLCHAIN=local go build -p 4 -o /tmp/relay . || exit 1
        exec env -i PATH="$$PATH" GOMEMLIMIT=384MiB RELAY_PROVIDERS="$$list" RELAY_PORT="$${RELAY_PORT:-8197}" /tmp/relay
    # Healthy = still inside the tunnel container's network AND answering. As
    # with Caddy: if the tunnel was restarted underneath it, it stops so that
    # Docker starts it again inside the tunnel's new network. (start_period
    # covers the build at start.)
    healthcheck:
      test: ["CMD-SHELL", "ip -o addr show scope global | grep -q . || { sleep 12; kill 1; exit 1; }; wget -q -T 5 -O /dev/null http://127.0.0.1:$${RELAY_PORT:-8197}/healthz || exit 1"]
      interval: 10s
      timeout: 20s
      start_period: 90s
      retries: 3


configs:
  # Caddy's own settings. The provider blocks are read in at the bottom.
  caddyfile:
    content: |
      # Caddy's settings for the IPTV proxy. The providers' own blocks are written at every start from .env
      # (see "entrypoint" in the compose file) and read in at the bottom.
      {
      	admin off # nothing to manage from outside; settings come from this file only
      	auto_https off # a home proxy on plain http; the providers are reached over https as they say
      	persist_config off
      	servers {
      		protocols h1 # players speak plain HTTP/1.1
      	}
      	# Caddy's own notes: only real errors (a server that could not be reached, a broken setting). A player
      	# hanging up in the middle of a stream (changing channel, a connection test) is normal and is not
      	# reported. Never with the address asked for (Xtream addresses contain the login) or the headers.
      	log default {
      		output stderr
      		level ERROR
      		format filter {
      			wrap console {
      				time_local
      				time_format wall_milli
      			}
      			request>uri delete
      			request>headers delete
      			request>tls delete
      		}
      	}
      }

      # What Caddy hands the relay carries no notes about the player's address or about Caddy (the relay passes the
      # player's own headers on to the provider for live channels and lists).
      (relay_headers) {
      	header_up -X-Forwarded-For
      	header_up -X-Forwarded-Proto
      	header_up -X-Forwarded-Host
      	header_up -Via
      }

      # Toward a provider or a server it sent the player to.
      #   args[0]: http or https, args[1]: the server (for a redirect that names no server)
      #  - Nothing about the player or this proxy is added (no X-Forwarded-*, no Via): the provider sees the
      #    player's own request, from the VPN. (With "compression off" in each transport below, the player's
      #    own Accept-Encoding goes through as it is, and nothing is added or unpacked on the way.)
      #  - 1. Redirects. A provider answers "go to that other server". The address is rewritten so the
      #    player comes back to this proxy for it: /__cdn/<http or https>/<server>/<path>.
      #  - Bytes are passed on as they arrive (flush_interval -1): live video must not wait in a buffer.
      #  - Certificates are not checked. Many players do not check them either, and some providers' servers
      #    have expired or self-made ones; checking here would refuse servers that play fine without the proxy.
      #  - Connections to every server are kept open and reused (Caddy does that by itself, for any server, named
      #    here or not), and an idle one is let go after 20 seconds (keepalive in each transport below).
      (upstream) {
      	header_up -X-Relay-Fetch
      	header_up -X-Forwarded-For
      	header_up -X-Forwarded-Proto
      	header_up -X-Forwarded-Host
      	header_up -Via
      	header_down Location ^/([^/].*)$$ /__cdn/{args[0]}/{args[1]}/$$1
      	header_down Location ^//(.*)$$ /__cdn/{args[0]}/$$1
      	header_down Location (?i)^(https?)://(.*)$$ /__cdn/$$1/$$2
      	flush_interval -1
      }

      # The containers' health check asks here.
      http://127.0.0.1:{$$HEALTH_PORT:8195} {
      	respond /healthz 200
      }

      import /run/iptv-proxy/providers/*.caddy

  # The block written once for each provider in .env (the @...@ words are filled in at start).
  provider-block:
    content: |
      # ---- One provider: @NAME@ on port @PORT@ (written at start from .env; see "entrypoint")
      :@PORT@ {
      	# One line per request, written when the request ends (a live stream: when the player stops it), in local
      	# time: who asked, which provider, what kind of request ("k"), the answer's status, its size in bytes, how
      	# long it took in seconds, and for anything fetched from a server the provider uses, which server answered
      	# ("server") and how many milliseconds it took to start answering ("wait_ms"). The address asked for is
      	# left out: Xtream addresses contain the login.
      	log {
      		output stdout
      		format filter {
      			wrap console {
      				time_local
      				time_format wall_milli
      			}
      			request>uri delete
      			request>headers delete
      			request>tls delete
      			request>remote_port delete
      			request>client_ip delete
      			request>proto delete
      			request>host delete
      			resp_headers delete
      			bytes_read delete
      			user_id delete
      		}
      	}
      	log_append provider @NAME@
      	header -Via # players see what the provider sends, without a note that a proxy passed it on

      	route {
      		respond /healthz 200

      		# The relay reads this start's notes (what was found in .env) from here, for the status page.
      		@notes {
      			path /__status.json
      			remote_ip 127.0.0.1
      		}
      		handle @notes {
      			rewrite * /status.json
      			file_server {
      				root /run/iptv-proxy
      			}
      		}

      		# ---- 5. One easy login for everything behind this proxy. A request that logs in as
      		# ---- PLAYER_USER is sent on with this provider's account; any other login is passed on
      		# ---- untouched (and refused by the provider).
      		@login_path path_regexp login_path ^/+(live|movie|series|timeshift)/+@PU@/+@PP@/+(.*)$$
      		rewrite @login_path /{re.login_path.1}/{env.IPTV_@NAME@_U}/{env.IPTV_@NAME@_P}/{re.login_path.2}?{query}
      		@login_short path_regexp login_short ^/+@PU@/+@PP@/+([^/]+)$$
      		rewrite @login_short /{env.IPTV_@NAME@_U}/{env.IPTV_@NAME@_P}/{re.login_short.1}?{query}
      		@login_user query username=@PU@
      		uri @login_user query username {env.IPTV_@NAME@_RU}
      		@login_pass query password=@PP@
      		uri @login_pass query password {env.IPTV_@NAME@_RP}
      		vars up {uri}
      		# A live channel's own folder (part 4): the channel's address in it, with the login swapped.
      		@ch_any path_regexp ch_any ^/__ch(/.+?\.m3u8)/
      		vars @ch_any up {re.ch_any.1}
      		@ch_login path_regexp ch_login ^/__ch(/+(?:live/+)?)@PU@/+@PP@/+(.+?\.m3u8)/
      		vars @ch_login up {re.ch_login.1}{env.IPTV_@NAME@_U}/{env.IPTV_@NAME@_P}/{re.ch_login.2}

      		# A provider's ready-made M3U playlist lists every stream at the provider's own address with
      		# the real login, so a player using it would skip the VPN. Refused on purpose.
      		respond /get.php 403

      		# ---- The status page, drawn by the relay.
      		handle /status {
      			reverse_proxy 127.0.0.1:@RELAY@ {
      				header_up X-Relay-Port {http.request.local.port}
      				header_up X-Relay-User @PU@
      				header_up X-Relay-Pass @PP@
      				import relay_headers
      			}
      		}

      		# ---- 6. Live MPEG-TS: one provider connection per channel, shared by every reader (the relay).
      		@live_ts vars_regexp {http.request.orig_uri.path} ^/+(live/+@PU@/+@PP@/+[^/]+\.ts|@PU@/+@PP@/+[0-9]+(\.ts)?)$$
      		handle @live_ts {
      			log_append k media
      			rewrite * /@MODE@
      			reverse_proxy 127.0.0.1:@RELAY@ {
      				header_up X-Relay-Provider @URL@
      				header_up X-Relay-Uri {vars.up}
      				header_up X-Relay-Orig {http.request.orig_uri}
      				header_up X-Relay-Port {http.request.local.port}
      				header_up X-Relay-Client {http.request.remote.host}
      				header_up -Range
      				import relay_headers
      				flush_interval -1
      				# Sent as the provider sends it: not chunked, ended by closing the connection (as nginx did).
      				header_down Transfer-Encoding identity
      				transport http {
      					read_timeout 100s
      				}
      			}
      		}

      		# ---- 3 and 4. HLS playlists and live channels' folders (the relay keeps one session per channel
      		# ---- and points every address in a playlist back at this proxy).
      		# (The relay's own fetch of a playlist on a video server comes back through here so that it uses the
      		# connection Caddy keeps open to that server: from inside this container, marked X-Relay-Fetch, it is
      		# passed straight on below instead of being handed to the relay again.)
      		@playlist {
      			vars_regexp {http.request.orig_uri.path} (?i)(\.m3u8$$|^/__ch/)
      			not {
      				remote_ip 127.0.0.1
      				header X-Relay-Fetch 1
      			}
      		}
      		handle @playlist {
      			log_append k hls
      			rewrite * /hl
      			reverse_proxy 127.0.0.1:@RELAY@ {
      				header_up X-Relay-Provider @URL@
      				header_up X-Relay-Uri {vars.up}
      				header_up X-Relay-Orig {http.request.orig_uri}
      				header_up X-Relay-Port {http.request.local.port}
      				header_up X-Relay-Client {http.request.remote.host}
      				header_up X-Relay-Mode @MODE@
      				import relay_headers
      				transport http {
      					read_timeout 60s
      				}
      			}
      		}

      		# ---- The provider's lists: logos and posters in them are pointed back at this proxy (the relay).
      		handle /player_api.php {
      			log_append k api
      			rewrite * /api
      			reverse_proxy 127.0.0.1:@RELAY@ {
      				header_up X-Relay-Provider @URL@
      				header_up X-Relay-Uri {vars.up}
      				header_up X-Relay-Host {http.request.hostport}
      				import relay_headers
      				flush_interval -1
      				transport http {
      					read_timeout 330s
      				}
      			}
      		}

      		# ---- 2. Everything a provider pointed at: video pieces, MPEG-TS, movies, catch-up, the guide.
      		# ---- /__cdn/<http or https>/<server>/<path>. Never this machine or a home network.
      		@cdn_forbidden path_regexp (?i)^/__cdn/https?/(localhost|127\.|10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|169\.254\.|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.|0\.|([0-9]+\.)*0[0-9]+(\.[0-9]+)*([:/?]|$$)|\[|[^/]*[@%])
      		respond @cdn_forbidden 403
      		@cdn_other {
      			path /__cdn/*
      			not method GET HEAD
      		}
      		respond @cdn_other 405
      		@cdn_https path_regexp cdn ^/__cdn/https/([^/?]+)(?:/(.*))?$$
      		handle @cdn_https {
      			log_append k cdn
      			# Where the server's name really leads is checked first (the relay looks it up; see /check there).
      			forward_auth 127.0.0.1:@RELAY@ {
      				uri /check
      				header_up X-Relay-Check {re.cdn.1}
      			}
      			log_append server {re.cdn.1}
      			log_append wait_ms {http.reverse_proxy.upstream.latency_ms}
      			map {re.cdn.1} {dial} {
      				~^(.+:[0-9]+)$$ $${1}
      				~^(.+)$$ $${1}:443
      			}
      			rewrite * /{re.cdn.2}?{query}
      			reverse_proxy {dial} {
      				header_up Host {re.cdn.1}
      				import upstream https {re.cdn.1}
      				transport http {
      					compression off
      					keepalive 20s
      					tls
      					tls_insecure_skip_verify
      					read_timeout 60s
      				}
      			}
      		}
      		@cdn_http path_regexp cdn ^/__cdn/http/([^/?]+)(?:/(.*))?$$
      		handle @cdn_http {
      			log_append k cdn
      			# Where the server's name really leads is checked first (the relay looks it up; see /check there).
      			forward_auth 127.0.0.1:@RELAY@ {
      				uri /check
      				header_up X-Relay-Check {re.cdn.1}
      			}
      			log_append server {re.cdn.1}
      			log_append wait_ms {http.reverse_proxy.upstream.latency_ms}
      			map {re.cdn.1} {dial} {
      				~^(.+:[0-9]+)$$ $${1}
      				~^(.+)$$ $${1}:80
      			}
      			rewrite * /{re.cdn.2}?{query}
      			reverse_proxy {dial} {
      				header_up Host {re.cdn.1}
      				import upstream http {re.cdn.1}
      				transport http {
      					compression off
      					keepalive 20s
      					read_timeout 60s
      				}
      			}
      		}
      		respond /__cdn/* 404
      		respond /__cdu/* 404

      		# ---- The guide: large, and slow for some providers to start.
      		handle /xmltv.php {
      			log_append k guide
      			log_append wait_ms {http.reverse_proxy.upstream.latency_ms}
      			reverse_proxy @URL@ {
      				@HOSTHDR@
      				import upstream @SCHEME@ @HOSTPORT@
      				transport http {
      					compression off
      					keepalive 20s
      					@TLS@
      					read_timeout 300s
      				}
      			}
      		}

      		# ---- Everything else (movies, series, catch-up, account): straight to the provider.
      		handle {
      			log_append k media
      			log_append wait_ms {http.reverse_proxy.upstream.latency_ms}
      			reverse_proxy @URL@ {
      				@HOSTHDR@
      				import upstream @SCHEME@ @HOSTPORT@
      				transport http {
      					compression off
      					keepalive 20s
      					@TLS@
      					read_timeout 60s
      				}
      			}
      		}
      	}
      }

  # At every start: check .env, write one block per provider, start Caddy.
  caddy-start:
    content: |
      # At every start: build one Caddy site per provider found in .env (any NAME with NAME_URL, NAME_PORT,
      # NAME_USER, NAME_PASSWORD and NAME_LIVE_TV), then start Caddy. A provider with a missing or unusable
      # line is skipped and named in the log; the others still start.
      #
      # Provider logins may contain ANY characters. Each one is percent-encoded for the address (letters,
      # digits and - . _ ~ stay as they are; everything else becomes %XX) and handed to Caddy in its own
      # variable, so the provider receives exactly the login it issued.
      set -u
      run=/run/iptv-proxy; rm -rf "$$run"; mkdir -p "$$run/providers"
      relay="$${RELAY_PORT:-8197}"
      enc() {
        printf '%s' "$$1" | hexdump -v -e '1/1 "%02x\n"' | while read -r h; do
          case "$$h" in
            2d|2e|5f|7e|3[0-9]|4[1-9a-f]|5[0-9a]|6[1-9a-f]|7[0-9a]) printf "\\x$$h" ;;
            *) printf '%%%s' "$$(echo "$$h" | tr a-f A-F)" ;;
          esac
        done
      }
      ports="$${PROXY_PORTS:-8095-8096}"; stop=""
      notes=/tmp/status-notes; rm -rf "$$notes"; mkdir -p "$$notes"; : > "$$notes/problems"; : > "$$notes/providers"
      clean() { printf '%s' "$$1" | tr -d '"\\' | tr '\n\r\t' '   '; }
      PLAYER_USER="$${PLAYER_USER:-}"; PLAYER_PASSWORD="$${PLAYER_PASSWORD:-}"
      echo "$$ports" | grep -Eqx '[1-9][0-9]{0,4}(-[1-9][0-9]{0,4})?' || stop="PROXY_PORTS must be one port or a range like 8095-8099 (found: $$ports)"
      [ -z "$$stop" ] && [ "$$(printf '%s' "$$PLAYER_USER$$PLAYER_PASSWORD$$ports" | wc -l)" != 0 ] && stop="PLAYER_USER, PLAYER_PASSWORD and PROXY_PORTS must each be on one line"
      [ -z "$$stop" ] && { echo "$$PLAYER_USER" | grep -Eqx '[A-Za-z0-9_-]+' && echo "$$PLAYER_PASSWORD" | grep -Eqx '[A-Za-z0-9_-]+' || stop="PLAYER_USER and PLAYER_PASSWORD must be letters, digits, - or _ only (they are yours to choose)"; }
      [ -z "$$stop" ] && case "$$PLAYER_USER" in live|movie|series|timeshift) stop="PLAYER_USER cannot be live, movie, series or timeshift";; esac
      lo=$${ports%-*}; hi=$${ports#*-}; used=" "; first=""
      [ -n "$$stop" ] && { echo "PROXY SETTINGS PROBLEM: $$stop"; printf '"%s."\n' "$$(clean "$$stop")" >> "$$notes/problems"; }
      env | sed -n 's/^\([A-Za-z0-9_]*[a-z][A-Za-z0-9_]*\)_URL=.*/\1/p' | while read -r low; do echo "PROVIDER $$low SKIPPED: the name must be in CAPITALS"; printf '{"name":"%s","port":"","ok":false,"why":"the name must be in CAPITALS"}\n' "$$(clean "$$low")" >> "$$notes/providers"; done
      for name in $$( [ -z "$$stop" ] && env | sed -n 's/^\([A-Z][A-Z0-9_]*\)_URL=.*/\1/p' | sort ); do
        url=$$(printenv "$${name}_URL"); port=$$(printenv "$${name}_PORT" || true)
        user=$$(printenv "$${name}_USER" || true); pass=$$(printenv "$${name}_PASSWORD" || true)
        mode=$$(printenv "$${name}_LIVE_TV" || true)
        bad=""
        case "$$name" in PROXY|PLAYER|VPN|OPENVPN|WIREGUARD|SERVER|FIREWALL|DNS|RELAY|IPTV) bad="$$name is one of the proxy's own names; pick another";; esac
        [ -z "$$bad" ] && { [ -n "$$url" ] && [ -n "$$port" ] && [ -n "$$user" ] && [ -n "$$pass" ] || bad="needs $${name}_URL, _PORT, _USER and _PASSWORD"; }
        [ -z "$$bad" ] && { echo "$$port" | grep -Eqx '[1-9][0-9]{0,4}' && [ "$$port" -le 65535 ] || bad="port must be a number from 1 to 65535"; }
        [ -z "$$bad" ] && { echo "$$url" | grep -Eqx 'https?://[A-Za-z0-9._-]+(:[0-9]{1,5})?/?' || bad="address must be http://name or https://name, optionally :port, with nothing after it"; }
        [ -z "$$bad" ] && case " 53 8000 $${HEALTH_PORT:-8195} $$relay 9999 " in *" $$port "*) bad="port $$port is used inside the VPN container itself";; esac
        [ -z "$$bad" ] && case "$$used" in *" $$port "*) bad="port $$port is already used by another provider";; esac
        [ -z "$$bad" ] && { [ "$$port" -ge "$$lo" ] && [ "$$port" -le "$$hi" ] || bad="port $$port is outside PROXY_PORTS ($$ports)"; }
        [ -z "$$bad" ] && [ "$$(printf '%s' "$$url$$port$$user$$pass$$mode" | wc -l)" != 0 ] && bad="one of its lines is broken over two lines"
        [ -z "$$bad" ] && case "$$mode" in hls|ts) ;; *) bad="needs the line $${name}_LIVE_TV=hls if your players use HLS, or $${name}_LIVE_TV=ts if they use MPEG-TS";; esac
        [ -z "$$bad" ] && case "$$user$$pass" in */*) bad="its username or password contains a slash (/), which Caddy cannot pass on inside an address";; esac
        if [ -n "$$bad" ]; then echo "PROVIDER $$name SKIPPED: $$bad"; printf '{"name":"%s","port":"","ok":false,"why":"%s"}\n' "$$name" "$$(clean "$$bad")" >> "$$notes/providers"; continue; fi
        used="$$used$$port "
        base="$${url%/}"; scheme="$${base%%://*}"; hostport="$${base#*://}"
        export "IPTV_$${name}_U=$$(enc "$$user")" "IPTV_$${name}_P=$$(enc "$$pass")" "IPTV_$${name}_RU=$$user" "IPTV_$${name}_RP=$$pass"
        tls=""; hosthdr="header_up Host {upstream_hostport}"   # (over https Caddy sends the provider its own name by itself)
        [ "$$scheme" = https ] && { tls="tls_insecure_skip_verify"; hosthdr=""; }
        sed -e "s|@NAME@|$$name|g" -e "s|@PORT@|$$port|g" -e "s|@URL@|$$base|g" -e "s|@SCHEME@|$$scheme|g" \
            -e "s|@HOSTPORT@|$$hostport|g" -e "s|@MODE@|$$mode|g" -e "s|@PU@|$$PLAYER_USER|g" -e "s|@PP@|$$PLAYER_PASSWORD|g" \
            -e "s|@RELAY@|$$relay|g" -e "s|@TLS@|$$tls|g" -e "s|@HOSTHDR@|$$hosthdr|g" /etc/caddy/provider.tmpl > "$$run/providers/$$name.caddy"
        echo "PROVIDER $$name on port $$port -> $$base"
        printf '{"name":"%s","port":"%s","ok":true,"mode":"%s"}\n' "$$name" "$$port" "$$mode" >> "$$notes/providers"
        { [ -z "$$first" ] || [ "$$port" -lt "$$first" ]; } && first="$$port"
      done
      # With no provider running, one port still answers so the status page can say what is wrong, and the
      # container reports itself unhealthy.
      : > "$$run/problem"
      if [ "$$used" = " " ]; then
        echo "NO PROVIDERS STARTED. Caddy is running with none: fix .env and bring the stack up again."
        echo "not set up" > "$$run/problem"
        first=$$(echo "$$lo" | grep -Ex '[1-9][0-9]{0,4}' || echo 8095)
        cat > "$$run/providers/none.caddy" <<EOF
      :$$first {
      	@notes {
      		path /__status.json
      		remote_ip 127.0.0.1
      	}
      	handle @notes {
      		rewrite * /status.json
      		file_server {
      			root /run/iptv-proxy
      		}
      	}
      	handle /status {
      		reverse_proxy 127.0.0.1:$$relay {
      			header_up X-Relay-Port $$first
      			header_up -X-Relay-User
      			header_up -X-Relay-Pass
      		}
      	}
      	respond "The IPTV proxy is not set up yet. Open /status on this address in a web browser to see what is missing." 503
      }
      EOF
      fi
      printf '{"problems":[%s],"providers":[%s]}\n' "$$(paste -sd, "$$notes/problems")" "$$(paste -sd, "$$notes/providers")" > "$$run/status.json"
      rm -rf "$$notes"
      echo "STATUS PAGE: open http://<this machine's address>:$$first/status in a web browser"
      # Caddy is given only what it needs: the logins it sends on (IPTV_...), not the rest of .env (VPN keys and so on).
      for v in $$(env | sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p'); do
        case "$$v" in PATH|HOME|XDG_*|HEALTH_PORT|IPTV_*) ;; *) unset "$$v" ;; esac   # (not TZ: Caddy reads /etc/localtime)
      done
      exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile

  # The relay (see the "relay" service above). Go, standard library only, in four files.
  relay-main:
    content: |
      // The relay: the jobs a plain reverse proxy cannot do, for Caddy (it listens on 127.0.0.1 only).
      //
      //	/ts      one provider MPEG-TS stream per channel, shared by every reader
      //	/hls     MPEG-TS for recorders, made from the channel's HLS (shares the players' one HLS session)
      //	/hl      HLS playlists: one provider session per live channel, every address pointed back here
      //	/api     player_api.php lists: logos and posters pointed back here
      //	/check   whether a /__cdn/ server name leads to this machine or a home network
      //	/status  the status page
      //
      // Go port of relay.py: same endpoints, status codes, timing rules and log lines. Standard library only.
      // main.go: server, channels and their sources. hls.go: playlists, sessions, lists. status.go: the page.
      package main

      import (
      	"bytes"
      	"context"
      	"crypto/tls"
      	"errors"
      	"fmt"
      	"io"
      	"log"
      	"math"
      	"net"
      	"net/http"
      	"net/netip"
      	"os"
      	"os/signal"
      	"regexp"
      	"strconv"
      	"strings"
      	"sync"
      	"sync/atomic"
      	"syscall"
      	"time"
      )

      const (
      	joinSeconds   = 10               // a joiner starts this far behind live: its cushion, handed on at playing speed
      	joinStart     = 2                // seconds of that cushion handed on at once, so its player can start
      	keepSeconds   = 15               // what is kept of each channel for joiners...
      	ringMin       = 12 << 20         // ...never less than this
      	ringMax       = 64 << 20         // ...nor more than this
      	burstSeconds  = 2.5              // the provider's opening burst is measured over this and logged
      	reopenAfter   = 30               // a stream that ran this long and ended while watched is asked for again
      	reopenWait    = 5                // seconds to answer when asked again (twice that to send video)
      	dropGap       = 0.1              // time a provider gets to see a hang-up before another channel is asked for
      	linger        = 0.4              // a channel in its first seconds is kept this long after its last reader (tuning)
      	readersMax    = 100              // readers served at once, all channels together
      	behind        = 16 << 20         // a reader this far behind for behindSeconds is disconnected...
      	behindSeconds = 15               //
      	farBehind     = 64 << 20         // ...or this far behind at any moment
      	connectWait   = 10 * time.Second // to accept a connection
      	headerWait    = 20               // seconds for an answer's heading
      	playlistWait  = 30               // the same for a playlist
      	startWait     = 60               // seconds a reader waits for a channel's first video
      	quietWait     = 60               // seconds a stream may send nothing before it is given up on
      )

      var mu sync.Mutex // all shared state; held only between waits (the Python ran on one thread)

      var logger = log.New(os.Stdout, "RELAY: ", 0)

      func say(format string, a ...any) { logger.Printf(format, a...) } // never an address or a login

      // The providers this relay may connect to (passed from .env by the compose command).
      var providers = func() map[string]bool {
      	m := map[string]bool{}
      	for _, u := range strings.Fields(os.Getenv("RELAY_PROVIDERS")) {
      		if l := strings.ToLower(u); strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
      			m[strings.TrimRight(u, "/")] = true
      		}
      	}
      	return m
      }()

      var clockStart = time.Now()

      func mono() float64 { return time.Since(clockStart).Seconds() + 1e6 } // starts high so "never" (0) is long ago

      func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

      func sleep(ctx context.Context, s float64) error {
      	t := time.NewTimer(secs(math.Max(0, s)))
      	defer t.Stop()
      	select {
      	case <-t.C:
      		return nil
      	case <-ctx.Done():
      		return ctx.Err()
      	}
      }

      // ---------------------------------------------------------------------------
      // Fetching: net/http, one connection per request, certificates not checked (as Caddy)
      // ---------------------------------------------------------------------------

      type refused struct { // the other side said no; carries what to tell the reader
      	status int
      	kind   string
      	text   []byte
      }

      func (e *refused) Error() string { return "refused " + strconv.Itoa(e.status) }

      type timeoutError struct{}

      func (timeoutError) Error() string { return "timed out" }
      func (timeoutError) Timeout() bool { return true }

      var errStop = errors.New("the feed's reader stopped")

      func isTimeout(err error) bool {
      	var ne net.Error
      	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
      }

      func pyName(err error) string { // the names the Python log used
      	var dns *net.DNSError
      	switch {
      	case isTimeout(err):
      		return "TimeoutError"
      	case errors.As(err, &dns):
      		return "gaierror"
      	case errors.Is(err, syscall.ECONNREFUSED):
      		return "ConnectionRefusedError"
      	case errors.Is(err, syscall.ECONNRESET):
      		return "ConnectionResetError"
      	}
      	return "OSError"
      }

      var privateNets = func() (out []netip.Prefix) {
      	for _, n := range strings.Fields("0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.168.0.0/16 ::/127 fc00::/7 fe80::/10") {
      		out = append(out, netip.MustParsePrefix(n))
      	}
      	return
      }()

      // isPrivate: this machine, a home or private network (false for anything not written as an address).
      func isPrivate(host string) bool {
      	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
      	if err != nil {
      		return false
      	}
      	ip = ip.WithZone("").Unmap()
      	for _, n := range privateNets {
      		if n.Contains(ip) {
      			return true
      		}
      	}
      	return false
      }

      // looseNumbers: 127.1, 2130706433, 0x7f000001: the C library reads these as addresses, DNS never answers them.
      var looseNumbers = regexp.MustCompile(`(?i)^(0x[0-9a-f]+|[0-9]+)(\.(0x[0-9a-f]+|[0-9]+)){0,3}$$`)

      // leadsHome looks a server name up and says whether any answer is this machine or a home network.
      func leadsHome(ctx context.Context, host string) (bool, error) {
      	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
      		return isPrivate(host), nil
      	}
      	if strings.EqualFold(host, "localhost") || looseNumbers.MatchString(host) {
      		return true, nil
      	}
      	ctx, cancel := context.WithTimeout(ctx, connectWait)
      	defer cancel()
      	found, err := net.DefaultResolver.LookupIPAddr(ctx, host)
      	for _, f := range found {
      		if isPrivate(f.IP.String()) {
      			return true, nil
      		}
      	}
      	return false, err
      }

      type guardKey struct{}

      // The private-address check happens at connect time, on the address actually dialed.
      var dialer = &net.Dialer{Timeout: connectWait, ControlContext: func(ctx context.Context, _, addr string, _ syscall.RawConn) error {
      	if host, _, _ := net.SplitHostPort(addr); ctx.Value(guardKey{}) != nil && isPrivate(host) {
      		return &refused{status: 403}
      	}
      	return nil
      }}

      func newTransport(verify bool) *http.Transport {
      	return &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: &tls.Config{InsecureSkipVerify: !verify},
      		TLSHandshakeTimeout: connectWait, DisableKeepAlives: true, DisableCompression: true,
      		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
      }

      var plainTransport, checkedTransport = newTransport(false), newTransport(true)

      type opts struct {
      	guardAll bool // every connection checked (otherwise only those a redirect leads to)
      	noFollow bool
      	stay     bool // redirects may not leave the first server
      	verify   bool // check the certificate (only ipinfo.io)
      	wait     float64
      }

      // idleBody gives up when one read waits longer than wait (a stream that went quiet).
      type idleBody struct {
      	io.ReadCloser
      	cancel context.CancelFunc
      	wait   atomic.Int64
      	timed  atomic.Bool
      }

      func (b *idleBody) Read(p []byte) (int, error) {
      	t := time.AfterFunc(time.Duration(b.wait.Load()), func() { b.timed.Store(true); b.cancel() })
      	n, err := b.ReadCloser.Read(p)
      	t.Stop()
      	if err != nil && err != io.EOF && b.timed.Load() {
      		err = timeoutError{}
      	}
      	return n, err
      }

      func (b *idleBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

      func setWait(r *http.Response, s float64) { r.Body.(*idleBody).wait.Store(int64(secs(s))) }

      func isRedirect(s int) bool { return s == 301 || s == 302 || s == 303 || s == 307 || s == 308 }

      // fetch opens url with the given headers (the player's own, or just a name). The caller closes the body.
      func fetch(ctx context.Context, url string, h http.Header, o opts) (*http.Response, error) {
      	ctx, cancel := context.WithCancel(ctx)
      	if o.guardAll {
      		ctx = context.WithValue(ctx, guardKey{}, true)
      	}
      	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
      	if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Hostname() == "" || req.URL.User != nil {
      		cancel()
      		return nil, &refused{status: 502}
      	}
      	req.Header = h.Clone()
      	if req.Header == nil {
      		req.Header = http.Header{}
      	}
      	if req.Header.Get("User-Agent") == "" {
      		req.Header["User-Agent"] = []string{""} // no Go-http-client name
      	}
      	tr := plainTransport
      	if o.verify {
      		tr = checkedTransport
      	}
      	client := &http.Client{Transport: tr, CheckRedirect: func(r *http.Request, via []*http.Request) error {
      		if o.noFollow || len(via) >= 10 {
      			return http.ErrUseLastResponse
      		}
      		if o.stay && r.URL.Host != via[0].URL.Host {
      			return &refused{status: 502}
      		}
      		*r = *r.WithContext(context.WithValue(r.Context(), guardKey{}, true)) // a redirect may not lead home
      		return nil
      	}}
      	var timed atomic.Bool
      	t := time.AfterFunc(connectWait+secs(o.wait), func() { timed.Store(true); cancel() })
      	resp, err := client.Do(req)
      	t.Stop()
      	if err != nil {
      		cancel()
      		var r *refused
      		if errors.As(err, &r) {
      			return nil, r
      		}
      		if timed.Load() {
      			return nil, timeoutError{}
      		}
      		return nil, err
      	}
      	b := &idleBody{ReadCloser: resp.Body, cancel: cancel}
      	b.wait.Store(int64(secs(o.wait)))
      	resp.Body = b
      	return resp, nil
      }

      type tooLarge struct{}

      func (tooLarge) Error() string { return "too large" }

      // readAll reads a whole small answer (a playlist, a piece, a list) and closes it.
      func readAll(r *http.Response, limit int64, wait float64) ([]byte, error) {
      	defer r.Body.Close()
      	setWait(r, wait)
      	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
      	if err == nil && int64(len(data)) > limit {
      		err = tooLarge{}
      	}
      	return data, err
      }

      // get fetches a whole small answer from the proxy in this stack (never sent elsewhere).
      func get(ctx context.Context, url string, h http.Header, limit int64) (int, []byte, error) {
      	r, err := fetch(ctx, url, h, opts{stay: true, wait: headerWait})
      	if err != nil {
      		return 0, nil, err
      	}
      	data, err := readAll(r, limit, headerWait)
      	return r.StatusCode, data, err
      }

      func agentOf(name string) http.Header { // the relay's own fetches: just a name
      	h := http.Header{"Accept": {"*/*"}}
      	if name != "" {
      		h.Set("User-Agent", name)
      	}
      	return h
      }

      // ---------------------------------------------------------------------------
      // A channel: one source of video, any number of readers
      // ---------------------------------------------------------------------------

      // cleanStart: where a reader can start in MPEG-TS: a program table nearby, else the first whole packet.
      func cleanStart(buf []byte) int {
      	end := len(buf) - 376
      	for i, tries := bytes.IndexByte(buf, 0x47), 0; 0 <= i && i < end && tries < 2000; tries++ {
      		if buf[i+188] == 0x47 && buf[i+376] == 0x47 {
      			for k := i; k < min(end, i+188*3000); k += 188 {
      				if buf[k] != 0x47 {
      					break
      				}
      				if buf[k+1]&0x1f == 0 && buf[k+2] == 0 {
      					return k
      				}
      			}
      			return i
      		}
      		j := bytes.IndexByte(buf[i+1:], 0x47)
      		if j < 0 {
      			break
      		}
      		i += 1 + j
      	}
      	return 0
      }

      // keyStart: where a joiner's decoder can start at once: the program table (PAT) just before the first video
      // keyframe. Started at any PAT, the player was handed up to a whole keyframe interval of video it could not
      // decode, out of the few seconds it gets at once, and stalled. Streams that mark no keyframes: cleanStart.
      func keyStart(buf []byte) int {
      	start := cleanStart(buf)
      	pmtPID, vid, lastPAT := -1, -1, -1
      	for i := start; i+188 <= len(buf) && buf[i] == 0x47; i += 188 {
      		pkt := buf[i : i+188]
      		pid, afc, off, rai := int(pkt[1]&0x1f)<<8|int(pkt[2]), pkt[3]>>4&3, 4, false
      		if afc&2 != 0 {
      			rai, off = pkt[4] > 0 && pkt[5]&0x40 != 0, 5+int(pkt[4])
      		}
      		if afc&1 == 0 || off >= 188 || pkt[1]&0x40 == 0 {
      			continue
      		}
      		switch body := pkt[off:]; {
      		case pid == 0:
      			lastPAT = i
      			if pmt := patPMT(tableStart(body)); pmt >= 0 {
      				pmtPID = pmt
      			}
      		case pid == pmtPID && vid < 0:
      			vid = pickStream(tableStart(body))
      		case pid == vid && rai && lastPAT >= 0:
      			return lastPAT
      		}
      	}
      	return start
      }

      type pace struct{ have, credit, at, guess float64 } // a joiner's cushion, allowance, when counted, speed guessed

      type reader struct {
      	q                   [][]byte
      	n, given            int
      	wake                chan struct{}
      	over, dropped       bool
      	behindAt            float64
      	who, id, port, kind string // for the status page
      	came                float64
      	written             atomic.Int64
      	pace                *pace
      	cut                 func() // cuts its connection
      }

      func (rd *reader) signal() {
      	select {
      	case rd.wake <- struct{}{}:
      	default:
      	}
      }

      func (rd *reader) push(d []byte) { rd.q = append(rd.q, d); rd.n += len(d); rd.signal() }

      func (rd *reader) end(drop bool) {
      	if drop { // nothing more is sent, not even what was waiting, and the connection is cut
      		rd.q, rd.n, rd.dropped = nil, 0, true
      		if rd.cut != nil {
      			rd.cut()
      		}
      	}
      	rd.over = true
      	rd.signal()
      }

      type item struct {
      	t    float64
      	data []byte
      }

      type source func(ctx context.Context, ch *channel, yield func([]byte) bool) error

      type channel struct {
      	key, provider      string
      	readers            map[*reader]bool
      	timer              *time.Timer
      	ring               []item
      	kept               int
      	status             int // 200 once video flows, or the refusal; 0 not yet
      	refusal            *refused
      	ready              chan struct{}
      	done, paced        bool
      	total, burst       int64
      	marks              []mark // (time, total so far) once a second for a minute
      	born, asked        float64
      	hasBorn, readySent bool
      	ctx                context.Context
      	cancel             context.CancelFunc
      }

      type mark struct {
      	t     float64
      	total int64
      }

      var (
      	channels = map[string]*channel{}
      	lastDrop = map[string]float64{} // per provider: when one of its streams was last let go
      	lastOpen = map[string]opened{}  // per provider: when a channel was last opened, and which
      )

      type opened struct {
      	at  float64
      	key string
      }

      func allReaders() (n int) {
      	for _, c := range channels {
      		n += len(c.readers)
      	}
      	return
      }

      func newChannel(key string, src source, provider string) *channel { // (mu held)
      	ch := &channel{key: key, provider: provider, readers: map[*reader]bool{}, ready: make(chan struct{}), asked: mono()}
      	ch.ctx, ch.cancel = context.WithCancel(context.Background())
      	say("a channel was opened (%d open now)", len(channels)+1)
      	go ch.run(src)
      	return ch
      }

      func (ch *channel) setReady() {
      	if !ch.readySent {
      		ch.readySent = true
      		close(ch.ready)
      	}
      }

      func (ch *channel) run(src source) {
      	err := src(ch.ctx, ch, ch.yield)
      	mu.Lock()
      	defer mu.Unlock()
      	var r *refused
      	switch {
      	case err == nil || errors.Is(err, errStop):
      		if ch.status == 0 {
      			ch.status = 502
      		}
      	case ch.ctx.Err() != nil: // let go of on purpose
      	case errors.As(err, &r):
      		if ch.status == 0 {
      			ch.status, ch.refusal = r.status, r
      		}
      	case isTimeout(err):
      		if ch.status == 0 {
      			ch.status = 504
      		}
      		say("a channel sent nothing for too long and was closed")
      	default:
      		if ch.status == 0 {
      			ch.status = 502
      		}
      		say("a channel ended: %s", pyName(err))
      	}
      	ch.done = true
      	lastDrop[ch.provider] = mono()
      	if channels[ch.key] == ch {
      		delete(channels, ch.key)
      	}
      	say("a channel was closed (%d open now)", len(channels))
      	ch.setReady()
      	for rd := range ch.readers {
      		rd.end(false)
      	}
      }

      func (ch *channel) yield(data []byte) bool {
      	mu.Lock()
      	defer mu.Unlock()
      	if !ch.done {
      		ch.feed(data)
      	}
      	return !ch.done
      }

      func (ch *channel) feed(data []byte) {
      	if ch.status == 0 {
      		ch.status = 200
      		ch.setReady()
      	}
      	now := mono()
      	if !ch.hasBorn {
      		ch.born, ch.hasBorn = now, true
      		say("a channel gave its first video %.1f s after it was asked for", now-ch.asked)
      	}
      	ch.total += int64(len(data))
      	if len(ch.marks) == 0 || now-ch.marks[len(ch.marks)-1].t >= 1 {
      		ch.marks = append(ch.marks, mark{now, ch.total})
      		for now-ch.marks[0].t > 60 {
      			ch.marks = ch.marks[1:]
      		}
      	}
      	if len(ch.ring) > 0 && now-ch.ring[len(ch.ring)-1].t > 4 && !ch.paced { // (a bridged channel notes this itself)
      		say("a provider sent nothing for %.0f s, then carried on", now-ch.ring[len(ch.ring)-1].t)
      	}
      	if now-ch.born < burstSeconds {
      		ch.burst += int64(len(data))
      	} else if ch.burst >= 0 {
      		say("its opening burst was %d KB", ch.burst/1024)
      		ch.burst = -ch.burst // (noted once)
      	}
      	ch.ring = append(ch.ring, item{now, data})
      	ch.kept += len(data)
      	keep := ringMin
      	if speed, ok := ch.speed(now); ok {
      		keep = min(max(ringMin, int(speed*keepSeconds)), ringMax)
      	}
      	for ch.kept-len(ch.ring[0].data) >= keep {
      		ch.kept -= len(ch.ring[0].data)
      		ch.ring[0] = item{}
      		ch.ring = ch.ring[1:]
      	}
      	for rd := range ch.readers {
      		if rd.n <= behind+rd.given { // (what a joiner was handed at the start is not "falling behind")
      			rd.behindAt = 0
      		} else if rd.behindAt == 0 {
      			rd.behindAt = now
      		}
      		if rd.n > farBehind+rd.given || (rd.behindAt != 0 && now-rd.behindAt > behindSeconds) {
      			say("a reader was too slow to keep up with its channel and was disconnected")
      			rd.end(true)
      			delete(ch.readers, rd)
      			ch.dropIfEmpty()
      		} else {
      			rd.push(data)
      		}
      	}
      }

      // speed: bytes a second over the last minute or last 15 s, whichever is larger; false in the first seconds.
      func (ch *channel) speed(now float64) (float64, bool) {
      	if len(ch.marks) < 2 || now-ch.marks[0].t < 8 {
      		return 0, false
      	}
      	end := now
      	if last := ch.ring[len(ch.ring)-1].t; now-last >= 15 { // a stopped feed is measured up to its last video
      		end = last
      	}
      	older := ch.marks[0]
      	for i := len(ch.marks) - 1; i >= 0; i-- {
      		if end-ch.marks[i].t >= 15 {
      			older = ch.marks[i]
      			break
      		}
      	}
      	best := 0.0
      	for _, m := range []mark{ch.marks[0], older} {
      		if end-m.t >= 8 {
      			best = math.Max(best, float64(ch.total-m.total)/(end-m.t))
      		}
      	}
      	return best, best != 0
      }

      // join: the opener gets everything from the first byte; a joiner the newest joinSeconds, on a whole packet.
      func (ch *channel) join(first bool) *reader {
      	if ch.timer != nil { // somebody is here after all
      		ch.timer.Stop()
      		ch.timer = nil
      	}
      	rd := &reader{wake: make(chan struct{}, 1)}
      	// Nobody is on the channel any more (it is kept a moment after its last reader left, in its first
      	// seconds): this is the same tune carrying on, most often a player that hung up and asked again. It is
      	// given everything kept so far at once, as the reader who opened it was, not a joiner's paced cushion:
      	// that cushion is there for a second device on a channel someone is watching, and here nobody is.
      	if !first && len(ch.readers) == 0 && len(ch.ring) > 0 {
      		var head []byte // (the ring may have been trimmed: start where a decoder can)
      		n := 0
      		for ; n < len(ch.ring) && len(head) < 2<<20; n++ {
      			head = append(head, ch.ring[n].data...)
      		}
      		rd.push(head[keyStart(head):])
      		for _, it := range ch.ring[n:] {
      			rd.push(it.data)
      		}
      		rd.given = rd.n
      		say("a reader came back to a channel nobody else was on and was given %d KB at once", rd.n/1024)
      	} else if first {
      		for _, it := range ch.ring {
      			rd.push(it.data)
      		}
      	} else if len(ch.ring) > 0 {
      		speed, measured := ch.speed(mono())
      		want := ch.kept // (no speed yet: all of it)
      		if measured {
      			want = int(speed * joinSeconds)
      		}
      		var take [][]byte
      		size, oldest := 0, ch.ring[len(ch.ring)-1].t-2*joinSeconds
      		for i := len(ch.ring) - 1; i >= 0; i-- {
      			if d := ch.ring[i].data; size == 0 || (size+len(d) <= want && ch.ring[i].t >= oldest) {
      				take, size = append([][]byte{d}, take...), size+len(d)
      			} else {
      				break
      			}
      		}
      		var head []byte // only the beginning is copied, to find where a decoder can start
      		n := 0
      		for ; n < len(take) && len(head) < 2<<20; n++ {
      			head = append(head, take[n]...)
      		}
      		rd.push(head[keyStart(head):])
      		for _, d := range take[n:] {
      			rd.push(d)
      		}
      		rd.given = rd.n
      		// The cushion is handed on at playing speed (a player given seconds at once jumps through it and then
      		// stalls at the provider's next pause); joinStart seconds go at once so it can begin.
      		guess := speed
      		if !measured {
      			guess = float64(rd.n) / joinSeconds
      		}
      		if guess > 0 {
      			rd.pace = &pace{float64(rd.n), guess * joinStart, mono(), guess}
      		}
      		extra := ""
      		if measured {
      			extra = fmt.Sprintf(" (%d s at %d KB a second)", joinSeconds, int64(speed/1024))
      		}
      		say("a reader joined a channel and was given %d KB to start%s", rd.n/1024, extra)
      	}
      	ch.readers[rd] = true
      	return rd
      }

      func (ch *channel) leave(rd *reader) { // (mu held)
      	delete(ch.readers, rd)
      	if len(ch.readers) > 0 || ch.done {
      		return
      	}
      	now, op := mono(), lastOpen[ch.provider]
      	tuning := !ch.hasBorn || now-ch.born < 5     // a player tunes in a channel's first seconds
      	moved := now-op.at < 1.0 && op.key != ch.key // it has just gone to another channel
      	if !tuning || moved {
      		ch.drop()
      		return
      	}
      	if ch.timer != nil {
      		ch.timer.Stop()
      	}
      	var t *time.Timer
      	t = time.AfterFunc(secs(linger), func() {
      		mu.Lock()
      		defer mu.Unlock()
      		if ch.timer == t {
      			ch.dropIfEmpty()
      		}
      	})
      	ch.timer = t
      }

      func (ch *channel) dropIfEmpty() {
      	if len(ch.readers) == 0 {
      		ch.drop()
      	}
      }

      func (ch *channel) drop() { // let go of the provider's stream now (mu held)
      	if ch.timer != nil {
      		ch.timer.Stop()
      		ch.timer = nil
      	}
      	if !ch.done {
      		ch.cancel()
      		ch.done = true
      		lastDrop[ch.provider] = mono()
      		if channels[ch.key] == ch {
      			delete(channels, ch.key)
      		}
      	}
      }

      // tsSource: the provider's own MPEG-TS stream, asked for again if it ends while people watch.
      func tsSource(target string, h http.Header) source {
      	return func(ctx context.Context, ch *channel, yield func([]byte) bool) error {
      		again, sent, quiet := false, int64(0), 0
      		for {
      			began, got := mono(), false
      			wait := float64(headerWait)
      			if again {
      				wait = reopenWait // readers are waiting
      			}
      			r, err := fetch(ctx, target, h, opts{wait: wait})
      			if err != nil {
      				return err
      			}
      			if r.StatusCode != 200 && r.StatusCode != 206 {
      				say("a provider answered %d when asked for a channel", r.StatusCode)
      				text, _ := readAll(r, 65536, 5) // what the provider said, passed on to the player
      				return &refused{r.StatusCode, r.Header.Get("Content-Type"), text[:min(len(text), 65536)]}
      			}
      			err = func() error {
      				defer r.Body.Close()
      				setWait(r, quietWait)
      				if again {
      					setWait(r, 2*reopenWait)
      				}
      				var head []byte // a stream opened again is joined on at a whole packet
      				joining := again
      				buf := make([]byte, 65536)
      				for {
      					n, err := r.Body.Read(buf)
      					setWait(r, quietWait)
      					data := append([]byte(nil), buf[:n]...)
      					if joining {
      						head = append(head, data...)
      						if err == nil && len(head) < 16384 {
      							continue
      						}
      						data, head, joining = head[cleanStart(head):], nil, false
      						if len(data) > 0 && sent%188 != 0 { // the old stream stopped mid-packet: fill it out
      							fill := make([]byte, 188-sent%188)
      							for i := range fill {
      								fill[i] = 0xff
      							}
      							sent += int64(len(fill))
      							if !yield(fill) {
      								return errStop
      							}
      						}
      					}
      					if len(data) > 0 {
      						got = true
      						sent += int64(len(data))
      						if !yield(data) {
      							return errStop
      						}
      					}
      					if err == io.EOF {
      						return nil
      					} else if err != nil {
      						return err
      					}
      				}
      			}()
      			if errors.Is(err, errStop) || ctx.Err() != nil || (err != nil && !got) {
      				return err
      			}
      			// The provider ended it. Ask again if anyone still watches, unless it ran under reopenAfter, or another
      			// channel of this provider opened in the last 15 s (one-connection accounts: asking would end that one).
      			mu.Lock()
      			ran, op := mono()-began, lastOpen[ch.provider]
      			if isTimeout(err) && ran < quietWait+reopenAfter { // silent twice in a row: the channel is dead
      				quiet++
      			} else {
      				quiet = 0
      			}
      			stop := quiet >= 2 || len(ch.readers) == 0 || ran < reopenAfter || (op.at > math.Max(began, mono()-15) && op.key != ch.key)
      			mu.Unlock()
      			if quiet >= 2 {
      				say("a channel went silent twice in a row; letting its readers go")
      			}
      			if stop {
      				return nil
      			}
      			again = true
      			say("a provider ended a channel that was being watched; asking for it again")
      			if err := sleep(ctx, dropGap); err != nil {
      				return err
      			}
      		}
      	}
      }

      // ---------------------------------------------------------------------------
      // Requests from Caddy
      // ---------------------------------------------------------------------------

      // What a player sent that is not passed on: what belongs to one connection, and this proxy's notes.
      var skipRe = regexp.MustCompile(`(?i)^(host|connection|keep-alive|proxy-.*|te|trailer|transfer-encoding|upgrade|content-length` +
      	`|range|if-range|accept-encoding|via|x-relay-.*|x-hls-.*|x-real-ip|x-forwarded-.*)$$`)
      var notPrintable = regexp.MustCompile(`[^\x20-\x7e]`)

      const notProxy = "Not a request from the proxy.\n"

      func answer(w http.ResponseWriter, status int, kind, text string) {
      	if kind == "" {
      		kind = "text/plain"
      	}
      	w.Header().Set("Content-Type", kind)
      	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
      	w.Header().Set("Connection", "close")
      	w.WriteHeader(status)
      	io.WriteString(w, text)
      }

      // channelKey: one name per channel, whichever address form (/live/u/p/1.ts, /u/p/1.ts, /u/p/1) is used.
      func channelKey(path, provider, uri string) string {
      	parts := strings.FieldsFunc(strings.SplitN(uri, "?", 2)[0], func(r rune) bool { return r == '/' })
      	if len(parts) > 0 && parts[0] == "live" {
      		parts = parts[1:]
      	}
      	if len(parts) > 0 {
      		parts[len(parts)-1] = strings.TrimSuffix(parts[len(parts)-1], ".ts")
      	}
      	return path + " " + provider + " " + strings.Join(parts, "/")
      }

      // channelID: the channel's number (last address part without extension); never a login.
      func channelID(uri string) string {
      	parts := strings.Split(strings.SplitN(uri, "?", 2)[0], "/")
      	id, _, _ := strings.Cut(parts[len(parts)-1], ".")
      	if id == "" || len(id) > 12 || strings.Trim(id, "0123456789") != "" {
      		return "?"
      	}
      	return id
      }

      func whoIs(r *http.Request) string { // the player's address as Caddy passed it
      	if ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Relay-Client"))); err == nil {
      		return ip.String()
      	}
      	return "a player"
      }

      func hostOf(provider string) string {
      	if i := strings.Index(provider, "://"); i >= 0 {
      		h, _, _ := strings.Cut(provider[i+3:], "/")
      		if host, _, err := net.SplitHostPort(h); err == nil {
      			return host
      		}
      		return h
      	}
      	return "?"
      }

      func hms(s float64) string {
      	n := int64(s)
      	switch {
      	case n >= 3600:
      		return fmt.Sprintf("%dh %02dm %02ds", n/3600, n/60%60, n%60)
      	case n >= 60:
      		return fmt.Sprintf("%dm %02ds", n/60, n%60)
      	}
      	return fmt.Sprintf("%ds", n)
      }

      func serveHTTP(w http.ResponseWriter, r *http.Request) {
      	switch r.URL.Path {
      	case "/healthz":
      		answer(w, 200, "", "ok\n")
      	case "/status":
      		statusPage(w, r)
      	case "/check":
      		check(w, r)
      	case "/hl":
      		failed(w, playlistRequest(w, r))
      	case "/api":
      		failed(w, api(w, r))
      	case "/stat":
      		mu.Lock()
      		text := fmt.Sprintf("%d %d\n", len(channels), allReaders())
      		mu.Unlock()
      		answer(w, 200, "", text)
      	case "/ts", "/hls":
      		live(w, r)
      	default:
      		answer(w, 403, "", notProxy)
      	}
      }

      // failed answers a refusal with its status; anything else that broke before an answer, 502.
      func failed(w http.ResponseWriter, err error) {
      	var r *refused
      	if errors.As(err, &r) {
      		answer(w, r.status, r.kind, string(r.text))
      	} else if err != nil {
      		answer(w, 502, "", "")
      	}
      }

      func live(w http.ResponseWriter, r *http.Request) {
      	path := r.URL.Path
      	provider, uri := strings.TrimRight(r.Header.Get("X-Relay-Provider"), "/"), r.Header.Get("X-Relay-Uri")
      	if !providers[provider] || !strings.HasPrefix(uri, "/") {
      		answer(w, 403, "", notProxy)
      		return
      	}
      	if r.Method == "HEAD" { // always 200 here: asking the provider would open a connection
      		w.Header().Set("Content-Type", "video/mp2t")
      		w.WriteHeader(200)
      		return
      	}
      	if r.Method != "GET" {
      		answer(w, 400, "", "")
      		return
      	}
      	sent := http.Header{} // the player's own headers, for the provider
      	for k, vs := range r.Header {
      		if !skipRe.MatchString(k) {
      			for _, v := range vs {
      				sent.Add(k, notPrintable.ReplaceAllString(v, ""))
      			}
      		}
      	}
      	key, gone := channelKey(path, provider, uri), r.Context().Done()
      	mu.Lock()
      	ch := channels[key]
      	first := ch == nil
      	if !first { // moving onto a channel already open: an HLS builder this device left goes too
      		releaseFor(provider, whoIs(r), key)
      	}
      	if first {
      		// Changing channel: the hang-up on the old one and this request arrive in either order. Channels of
      		// this provider nobody reads go now; a hang-up just behind gets 30 ms; the provider gets dropGap to see it.
      		lastOpen[provider] = opened{mono(), key}
      		releaseFor(provider, whoIs(r), key)
      		var mine []*channel
      		live := false
      		for _, c := range channels {
      			if c.provider == provider {
      				mine = append(mine, c)
      				c.dropIfEmpty()
      				live = live || !c.done
      			}
      		}
      		mu.Unlock()
      		if live {
      			time.Sleep(30 * time.Millisecond)
      			mu.Lock()
      			for _, c := range mine {
      				c.dropIfEmpty()
      			}
      			mu.Unlock()
      		}
      		mu.Lock()
      		wait := lastDrop[provider] + dropGap - mono()
      		mu.Unlock()
      		time.Sleep(secs(math.Max(0, wait)))
      		if r.Context().Err() != nil { // the player has already moved on
      			return
      		}
      		mu.Lock()
      		for _, c := range channels { // once more, for hang-ups during the waits
      			if c.provider == provider && c.key != key {
      				c.dropIfEmpty()
      			}
      		}
      		if ch = channels[key]; ch == nil {
      			src := tsSource(provider+uri, sent)
      			if path == "/hls" {
      				orig, port := r.Header.Get("X-Relay-Orig"), r.Header.Get("X-Relay-Port")
      				if _, err := strconv.Atoi(port); !strings.HasPrefix(orig, "/") || err != nil {
      					mu.Unlock()
      					answer(w, 400, "", "")
      					return
      				}
      				plain, query, _ := strings.Cut(orig, "?")
      				if query != "" {
      					query = "?" + query
      				}
      				// One HLS address per channel whatever form the reader used: a second form would be a
      				// second provider session, which ends the players' one.
      				parts := strings.FieldsFunc(strings.TrimSuffix(plain, ".ts"), func(r rune) bool { return r == '/' })
      				if len(parts) > 0 && parts[0] == "live" {
      					parts = parts[1:]
      				}
      				plain = strings.TrimSuffix(plain, ".ts")
      				if len(parts) == 3 {
      					plain = "/live/" + strings.Join(parts, "/")
      				}
      				src = hlsSource(fmt.Sprintf("http://127.0.0.1:%s/__ch%s.m3u8/__index.m3u8%s", port, plain, query),
      					notPrintable.ReplaceAllString(r.Header.Get("User-Agent"), ""))
      			}
      			ch = newChannel(key, src, provider)
      			channels[key] = ch
      		} else {
      			first = false
      		}
      	}
      	if allReaders() >= readersMax {
      		if first {
      			ch.drop()
      		}
      		mu.Unlock()
      		answer(w, 503, "", "Too many readers at once.\n")
      		return
      	}
      	rd := ch.join(first)
      	rd.who, rd.id, rd.port, rd.came = whoIs(r), channelID(uri), r.Header.Get("X-Relay-Port"), mono()
      	rd.kind = map[string]string{"/ts": "MPEG-TS", "/hls": "MPEG-TS fed from HLS"}[path]
      	mu.Unlock()
      	why, started := "the player hung up", false
      	defer func() {
      		mu.Lock()
      		still := max(0, len(ch.readers)-1)
      		ch.leave(rd)
      		mu.Unlock()
      		say("a reader left after %d s: %s (%d still on its channel)", int64(mono()-rd.came), why, still)
      		if started {
      			say("%s left channel %s after %s, %.1f MB sent: %s", rd.who, rd.id, hms(mono()-rd.came), float64(rd.written.Load())/1048576, why)
      		}
      	}()
      	t := time.NewTimer(startWait * time.Second)
      	select {
      	case <-ch.ready:
      	case <-gone:
      	case <-t.C:
      	}
      	t.Stop()
      	if r.Context().Err() != nil {
      		return
      	}
      	mu.Lock()
      	status, e, born, asked := ch.status, ch.refusal, ch.born, ch.asked
      	mu.Unlock()
      	if status != 200 {
      		switch {
      		case e != nil && e.status >= 400 && e.status < 600: // the provider's own answer, passed on
      			answer(w, e.status, e.kind, string(e.text))
      		case status == 0:
      			answer(w, 504, "", "")
      		case status >= 400 && status < 600:
      			answer(w, status, "", "")
      		default:
      			answer(w, 502, "", "")
      		}
      		return
      	}
      	rc := http.NewResponseController(w)
      	defer context.AfterFunc(r.Context(), func() { rc.SetWriteDeadline(time.Now()) })() // a blocked write gives up on hang-up
      	w.Header().Set("Content-Type", "video/mp2t")
      	w.WriteHeader(200)
      	rc.Flush()
      	mu.Lock()
      	rd.cut = func() { rc.SetWriteDeadline(time.Now()) }
      	mu.Unlock()
      	started = true
      	if first {
      		say("%s started channel %s (%s), first video in %.1f s", rd.who, rd.id, hostOf(provider), born-asked)
      	} else {
      		say("%s joined channel %s (%s)", rd.who, rd.id, hostOf(provider))
      	}
      	for {
      		mu.Lock()
      		if rd.dropped { // too slow: cut the connection, as the Python did
      			mu.Unlock()
      			panic(http.ErrAbortHandler)
      		}
      		if p := rd.pace; len(rd.q) > 0 && p != nil { // a joiner: at playing speed, holding its cushion here
      			now := mono()
      			speed, ok := ch.speed(now)
      			if !ok {
      				speed = p.guess
      			}
      			// a little faster when more than the cushion waits, a little slower when less
      			rate := speed * math.Min(1.5, math.Max(0.8, 1+0.5*(float64(rd.n)-p.have)/math.Max(p.have, 1)))
      			most := speed * joinStart
      			p.credit, p.at = math.Min(p.credit+(now-p.at)*rate, most), now
      			// an item longer than joinStart seconds goes once the allowance is full; its play time is waited after
      			if need := math.Min(float64(len(rd.q[0])), most); p.credit < need {
      				mu.Unlock()
      				t := time.NewTimer(secs((need - p.credit) / rate))
      				select {
      				case <-gone:
      					t.Stop()
      					return
      				case <-t.C:
      				}
      				continue
      			}
      			p.credit -= float64(len(rd.q[0]))
      		}
      		if len(rd.q) > 0 {
      			data := rd.q[0]
      			rd.q[0], rd.q = nil, rd.q[1:]
      			rd.n -= len(data)
      			if rd.pace == nil {
      				rd.given = max(0, rd.given-len(data))
      			}
      			mu.Unlock()
      			n, err := w.Write(data)
      			if err == nil {
      				err = rc.Flush()
      			}
      			rd.written.Add(int64(n))
      			if err != nil {
      				mu.Lock()
      				dropped := rd.dropped
      				mu.Unlock()
      				if dropped {
      					panic(http.ErrAbortHandler)
      				}
      				select {
      				case <-gone:
      				case <-time.After(50 * time.Millisecond): // (the hang-up is usually seen a moment later)
      					why = "the line to the player broke"
      				}
      				return
      			}
      			continue
      		}
      		if rd.over {
      			why = "its channel ended"
      			mu.Unlock()
      			return
      		}
      		mu.Unlock()
      		select {
      		case <-rd.wake:
      		case <-gone:
      			return
      		}
      	}
      }

      func main() {
      	port := os.Getenv("RELAY_PORT")
      	if port == "" {
      		port = "8197"
      	}
      	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
      	if err != nil {
      		log.Fatal(err)
      	}
      	srv := &http.Server{Handler: http.HandlerFunc(serveHTTP), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10,
      		ErrorLog: log.New(io.Discard, "", 0)}
      	s := "s"
      	if len(providers) == 1 {
      		s = ""
      	}
      	say("ready, for %d provider%s", len(providers), s)
      	go srv.Serve(ln)
      	stop := make(chan os.Signal, 1) // stop when told to (the container's first process)
      	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
      	<-stop
      	srv.Close() // let go of every stream at once
      	mu.Lock()
      	for _, ch := range channels {
      		ch.cancel()
      	}
      	mu.Unlock()
      	time.Sleep(100 * time.Millisecond)
      }

  relay-hls:
    content: |
      // HLS: the /hls bridge's source, playlists with one provider session per live channel, the provider's
      // lists (/api) and the /__cdn/ name check (/check).
      package main

      import (
      	"bytes"
      	"compress/gzip"
      	"context"
      	"errors"
      	"io"
      	"math"
      	"net/http"
      	"net/url"
      	"regexp"
      	"strconv"
      	"strings"
      	"time"
      )

      var errNotHLS = errors.New("not joinable HLS")

      type playlist struct {
      	seq            int64
      	target, newest float64
      	pieces         []string
      	best           string // the best quality on offer, when the playlist lists qualities
      	ended          bool
      }

      var (
      	extinfRe = regexp.MustCompile(`^#EXTINF:\s*(\d+(?:\.\d*)?)`)
      	nonDigit = regexp.MustCompile(`\D`)
      	nonNum   = regexp.MustCompile(`[^0-9.]`)
      	bwRe     = regexp.MustCompile(`[:,]BANDWIDTH=(\d+)`)
      )

      func readPlaylist(text string) (p playlist, err error) {
      	if !strings.Contains(text, "#EXTM3U") {
      		return p, errNotHLS
      	}
      	p.target = 4
      	length, rate, bestRate := 0.0, int64(-1), int64(-1)
      	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
      		line = strings.TrimSpace(line)
      		switch {
      		case strings.HasPrefix(line, "#EXTINF:"): // a piece's playing time (0 if unreadable)
      			length = 0
      			if m := extinfRe.FindStringSubmatch(line); m != nil {
      				length, _ = strconv.ParseFloat(m[1], 64)
      			}
      		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
      			p.seq, _ = strconv.ParseInt(nonDigit.ReplaceAllString(line[22:], ""), 10, 64)
      		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
      			if s := nonNum.ReplaceAllString(line[22:], ""); s != "" {
      				if p.target, err = strconv.ParseFloat(s, 64); err != nil {
      					return p, err
      				}
      			}
      		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
      			rate = 0
      			if m := bwRe.FindStringSubmatch(line); m != nil {
      				rate, _ = strconv.ParseInt(m[1], 10, 64)
      			}
      		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
      			p.ended = true
      		case strings.HasPrefix(line, "#EXT-X-MAP") || strings.HasPrefix(line, "#EXT-X-BYTERANGE") ||
      			strings.HasPrefix(line, "#EXT-X-KEY") && !strings.Contains(line, "METHOD=NONE"):
      			return p, errNotHLS // locked, or not whole MPEG-TS files: cannot be joined end to end
      		case line != "" && !strings.HasPrefix(line, "#"):
      			if rate >= 0 {
      				if rate > bestRate {
      					p.best, bestRate = line, rate
      				}
      				rate = -1
      			} else {
      				p.pieces = append(p.pieces, line)
      				p.newest, length = length, 0
      			}
      		}
      	}
      	return p, nil
      }

      // beforeMinute: seconds until just before the next whole minute; 0 from 1 s before it to 0.3 s after.
      // Recorders stop at whole minutes and cut at once, so around then nothing is held back.
      func beforeMinute() float64 {
      	s := math.Mod(float64(time.Now().UnixNano())/1e9, 60)
      	if s >= 59 || s < 0.3 {
      		return 0
      	}
      	return 59 - s
      }

      // hlsSource: the channel's HLS pieces end to end, fetched from this stack's proxy at the players' own
      // addresses so the reader shares their one provider session; handed on at playing speed.
      func hlsSource(start, agent string) source {
      	h, home := agentOf(agent), ""
      	if u, err := url.Parse(start); err == nil {
      		home = u.Host
      	}
      	inside := func(base, ref string) (*url.URL, error) { // the playlist's addresses must be this proxy's own
      		b, err := url.Parse(base)
      		if err != nil {
      			return nil, err
      		}
      		u, err := b.Parse(ref)
      		if err == nil && u.Host != home {
      			err = errNotHLS
      		}
      		return u, err
      	}
      	look := func(ctx context.Context) (string, playlist, error) { // always from the channel's own address
      		at := start
      		for i := 0; i < 4; i++ {
      			status, data, err := get(ctx, at, h, 2<<20)
      			if err != nil {
      				return "", playlist{}, err
      			}
      			if status != 200 {
      				return "", playlist{}, &refused{status: status}
      			}
      			pl, err := readPlaylist(string(data))
      			if err != nil || pl.best == "" {
      				return at, pl, err
      			}
      			u, err := inside(at, pl.best)
      			if err != nil {
      				return "", playlist{}, err
      			}
      			at = u.String()
      		}
      		return "", playlist{}, errNotHLS
      	}
      	return func(ctx context.Context, ch *channel, yield func([]byte) bool) error {
      		// Video waiting, (due, bytes), oldest first: each piece goes out over its playing time, everything
      		// at once before each whole minute and when the feed ends, so a recording that stops loses nothing.
      		var queue []item
      		pop := func() []byte { d := queue[0].data; queue = queue[1:]; return d }
      		due := func() bool {
      			now, flush := mono(), beforeMinute() == 0
      			for len(queue) > 0 && (flush || queue[0].t <= now) {
      				if !yield(pop()) {
      					return false
      				}
      			}
      			return true
      		}
      		flushAll := func() bool {
      			for len(queue) > 0 {
      				if !yield(pop()) {
      					return false
      				}
      			}
      			return true
      		}
      		// wait until `until`, or until job is done, handing on what falls due meanwhile (a fetch can take
      		// seconds; a whole minute passing meanwhile must not hold video back).
      		wait := func(until float64, job chan struct{}) error {
      			for {
      				if !due() {
      					return errStop
      				}
      				nap := 3600.0
      				if job == nil {
      					if nap = until - mono(); nap <= 0 {
      						return nil
      					}
      				}
      				select {
      				case <-job:
      					return nil
      				default:
      				}
      				if len(queue) > 0 {
      					nap = math.Min(nap, math.Min(queue[0].t-mono(), beforeMinute()))
      				}
      				t := time.NewTimer(secs(math.Max(0, nap)))
      				select {
      				case <-job:
      				case <-t.C:
      				case <-ctx.Done():
      					t.Stop()
      					return ctx.Err()
      				}
      				t.Stop()
      			}
      		}
      		do := func(f func(context.Context)) error { // a request under way; called off if the feed ends
      			jctx, cancel := context.WithCancel(ctx)
      			defer cancel()
      			done := make(chan struct{})
      			go func() { f(jctx); close(done) }()
      			return wait(0, done)
      		}
      		fail := func(err error) error { // the feed ends with a fault: what waits is handed on first
      			if !flushAll() {
      				return errStop
      			}
      			return err
      		}
      		mu.Lock()
      		ch.paced = true // its own pauses are not the provider's
      		mu.Unlock()
      		var last int64
      		hasLast, misses, lost, got, fresh := false, 0, 0, 0.0, mono()
      		var sent []string // names of the newest 60 pieces
      		for {
      			began := mono()
      			var at string
      			var pl playlist
      			var err error
      			if e := do(func(c context.Context) { at, pl, err = look(c) }); e != nil {
      				return e
      			}
      			if ctx.Err() != nil {
      				return ctx.Err()
      			}
      			if err != nil { // no playlist this time: ask again, as a player would (4 tries at start, 9 once playing)
      				misses++
      				var r *refused
      				status := 502
      				if errors.As(err, &r) {
      					status = r.status
      				}
      				if (hasLast && misses > 8) || (!hasLast && misses > 3) {
      					return fail(&refused{status: status})
      				}
      				nap := 1.0
      				if hasLast {
      					nap = 2
      				}
      				if e := wait(mono()+nap, nil); e != nil {
      					return e
      				}
      				continue
      			}
      			misses = 0
      			top := pl.seq + int64(len(pl.pieces)) - 1
      			afresh := hasLast && (top < last || pl.seq > last+1)
      			opening := !hasLast || afresh
      			if opening { // joining, or the numbering started afresh: start near the newest video
      				last, hasLast = max(pl.seq-1, top-3), true
      			}
      			first := last + 1 - pl.seq
      			newest := int64(len(pl.pieces)) - 1 + pl.seq
      			length := pl.target // a piece's playing time, as the playlist gives it
      			if 0 < pl.newest && pl.newest <= 2*pl.target {
      				length = pl.newest
      			}
      			isNew := first < int64(len(pl.pieces))
      			for i := max(first, 0); i < int64(len(pl.pieces)); i++ {
      				number := pl.seq + i
      				last = number
      				u, err := inside(at, pl.pieces[i])
      				if errors.Is(err, errNotHLS) { // a line that does not lead back here: not fetched
      					lost++
      					continue
      				} else if err != nil {
      					return fail(err)
      				}
      				if afresh && contains(sent, u.Path) { // already passed on under its old number
      					continue
      				}
      				var data []byte
      				for try := 0; try < 2 && data == nil; try++ {
      					if e := do(func(c context.Context) {
      						if status, d, err := get(c, u.String(), h, 64<<20); err == nil && status == 200 && len(d) > 0 {
      							data = d
      						}
      					}); e != nil {
      						return e
      					}
      				}
      				if data == nil {
      					lost++
      					say("a piece of video could not be fetched and is missing from a stream")
      					if lost >= 3 {
      						return fail(&refused{status: 502})
      					}
      					continue
      				}
      				lost = 0
      				if sent = append(sent, u.Path); len(sent) > 60 {
      					sent = sent[1:]
      				}
      				now := mono()
      				if got != 0 && now-got > math.Max(4, 1.5*length) { // a piece well later than its length
      					say("a provider sent nothing for %.0f s, then carried on", now-got)
      				}
      				got, fresh = now, now
      				var parts [][]byte // whole 188-byte packets
      				for j := 0; j < len(data); j += 65424 {
      					parts = append(parts, data[j:min(j+65424, len(data))])
      				}
      				// A channel's start goes at once, except its newest piece, which goes over its playing time
      				// so the stream has video in hand from its first second.
      				if opening {
      					if !flushAll() {
      						return errStop
      					}
      					if number != newest {
      						for _, p := range parts {
      							if !yield(p) {
      								return errStop
      							}
      						}
      						continue
      					}
      				}
      				startAt := now
      				if len(queue) > 0 {
      					startAt = math.Max(now, queue[len(queue)-1].t)
      				}
      				if shift := startAt - now - 2*pl.target; shift > 0 { // never more than two target durations waiting
      					for k := range queue {
      						queue[k].t -= shift
      					}
      					startAt -= shift
      				}
      				for k, p := range parts {
      					queue = append(queue, item{startAt + length*float64(k)/float64(len(parts)), p})
      				}
      				if !due() {
      					return errStop
      				}
      			}
      			if !isNew && (pl.ended || mono()-fresh > math.Max(30, 6*pl.target)) {
      				if !flushAll() {
      					return errStop
      				}
      				return nil // the playlist finished, or stopped moving
      			}
      			// Ask again at the HLS spec's pace (RFC 8216bis 6.3.4): after a change, the newest piece's length
      			// from when this look began; after none, half the target duration.
      			nap := pl.target / 2
      			if isNew {
      				nap = pl.target
      				if 0 < pl.newest && pl.newest < pl.target {
      					nap = pl.newest
      				}
      			}
      			if e := wait(began+math.Max(0.5, nap), nil); e != nil {
      				return e
      			}
      		}
      	}
      }

      func contains(list []string, s string) bool {
      	for _, x := range list {
      		if x == s {
      			return true
      		}
      	}
      	return false
      }

      // ---------------------------------------------------------------------------
      // Playlists: every address pointed back here; ONE provider session per live channel
      // ---------------------------------------------------------------------------
      // Addresses become /__cdn/<scheme>/<server>/<path>, resolved as a player resolves them. Many providers start
      // a new session (ending the last) at every open of a channel's address, so the first open is remembered and
      // the player is given /__ch/<channel>/__index.m3u8; later looks go to the remembered session. A session
      // that fails is opened again ONCE; one unused for a minute is forgotten.

      var (
      	uriInTag  = regexp.MustCompile(`(?i)(URI=")([^"]*)(")`)
      	hasScheme = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
      	plSkipRe  = regexp.MustCompile(`(?i)^(host|connection|keep-alive|proxy-.*|te|trailer|transfer-encoding|upgrade|content-length|content-encoding` +
      		`|range|if-range|content-range|accept-ranges|accept-encoding|x-accel-.*|server|date|via|x-relay-.*|x-hls-.*|x-pl-.*|x-real-ip|x-forwarded-.*)$$`)
      	realRe   = regexp.MustCompile(`(?i)^/__cdn/(https?)/([^/?]+)(/[^?]*)?(\?.*)?$$`)
      	cdnRe    = regexp.MustCompile(`(?i)^/__cdn/(https?)/([^/?]+)(/[^?]*)?$$`)
      	chRe     = regexp.MustCompile(`^/__ch(/.+?\.m3u8)/(.*)$$`)
      	renewOn  = map[int]bool{400: true, 401: true, 403: true, 404: true, 410: true, 429: true, 500: true, 502: true, 503: true, 504: true, 509: true, 520: true, 521: true, 522: true, 523: true, 524: true}
      	sessions = map[string]*session{}
      	locks    = map[string]chan struct{}{}
      	hlsSeen  = map[string]float64{} // channels whose playlist was just asked for (status page count)
      	hlsWho   = map[string]float64{} // (player, channel) -> when its "opened" line was written
      	hlsRows  = map[string]*hlsRow{} // (player, provider, channel) -> its playlist requests (status page table)
      )

      type session struct {
      	loc, final string // where the provider sent the channel; the playlist's real address
      	used       float64
      }

      type hlsRow struct {
      	first, last             float64
      	who, id, provider, port string
      }

      // cdnAddress: this proxy's address for a full http(s) address ("" for anything else).
      func cdnAddress(u *url.URL) string {
      	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
      		return ""
      	}
      	host, path := u.Host, u.EscapedPath()
      	if u.User != nil {
      		host = u.User.String() + "@" + host // (Caddy refuses these)
      	}
      	if path == "" {
      		path = "/"
      	}
      	if u.RawQuery != "" {
      		path += "?" + u.RawQuery
      	}
      	return "/__cdn/" + u.Scheme + "/" + host + path
      }

      // realAddress: the real address behind /__cdn/<scheme>/<server>/<path> ("" for anything else).
      func realAddress(local string) string {
      	m := realRe.FindStringSubmatch(local)
      	if m == nil {
      		return ""
      	}
      	if m[3] == "" {
      		m[3] = "/"
      	}
      	return strings.ToLower(m[1]) + "://" + m[2] + m[3] + m[4]
      }

      // rewritePlaylist points every address back at this proxy. With home (the player's own path, for a playlist the
      // provider answered directly), addresses on the provider's own server resolve against the player's path, so
      // they come back with the easy login and the real one is never written into the playlist.
      func rewritePlaylist(text, base, home string) (string, error) {
      	b, err := url.Parse(base)
      	if err != nil {
      		return "", err
      	}
      	hu, err := url.Parse("http://home.invalid" + home)
      	if err != nil {
      		return "", err
      	}
      	move := func(s string) (string, error) {
      		ref := strings.TrimSpace(s)
      		full, err := b.Parse(ref)
      		if err != nil && !hasScheme.MatchString(ref) {
      			// A relative address Go will not read as written, which players read anyway: a colon in its
      			// first part ("chunk_23:00:00.ts", RFC 3986 wants "./chunk_..."), or a stray control
      			// character. Read it the way a player does.
      			ref = "./" + strings.Map(func(r rune) rune {
      				if r < 0x20 || r == 0x7f {
      					return -1
      				}
      				return r
      			}, ref)
      			full, err = b.Parse(ref)
      		}
      		if err != nil {
      			return "", err // an absolute address nothing could reach: refuse rather than pass a login through
      		}
      		if home != "" && full.Scheme == b.Scheme && full.Host == b.Host && !hasScheme.MatchString(ref) {
      			mine, _ := hu.Parse(ref)
      			p := mine.EscapedPath()
      			if p == "" {
      				p = "/"
      			}
      			if mine.RawQuery != "" {
      				p += "?" + mine.RawQuery
      			}
      			return p, nil
      		}
      		if c := cdnAddress(full); c != "" {
      			return c, nil
      		}
      		return s, nil
      	}
      	lines := strings.Split(text, "\n")
      	for n, line := range lines {
      		core := strings.TrimSpace(line)
      		if n == 0 || core == "" {
      			continue
      		}
      		if !strings.HasPrefix(core, "#") { // an address
      			moved, err := move(core)
      			if err != nil {
      				return "", err
      			}
      			if strings.HasSuffix(line, "\r") {
      				moved += "\r"
      			}
      			lines[n] = moved
      			continue
      		}
      		var out strings.Builder // a tag: addresses sit in URI="..."
      		at := 0
      		for _, m := range uriInTag.FindAllStringSubmatchIndex(line, -1) {
      			moved, err := move(line[m[4]:m[5]])
      			if err != nil {
      				return "", err
      			}
      			out.WriteString(line[at:m[4]] + moved + line[m[6]:m[7]])
      			at = m[1]
      		}
      		lines[n] = out.String() + line[at:]
      	}
      	return strings.Join(lines, "\n"), nil
      }

      // send passes an answer on: the server's headers except what belongs to one connection.
      func send(w http.ResponseWriter, status int, h http.Header, data []byte) {
      	for k, vs := range h {
      		if !plSkipRe.MatchString(k) {
      			w.Header()[k] = vs
      		}
      	}
      	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
      	w.Header().Set("Connection", "close")
      	w.WriteHeader(status)
      	w.Write(data)
      }

      // servePlaylist rewrites a playlist; anything else is passed on as it came.
      func servePlaylist(w http.ResponseWriter, status int, h http.Header, data []byte, base, home string) error {
      	if bytes.Contains(data[:min(len(data), 1024)], []byte("#EXTM")) {
      		text, err := rewritePlaylist(string(data), base, home)
      		if err != nil {
      			return err
      		}
      		data = []byte(text)
      	}
      	send(w, status, h, data)
      	return nil
      }

      // fetchViaProxy fetches a playlist on a video server through Caddy's /__cdn/ (riding Caddy's kept-open
      // connection), after checking where the name leads. Returns the answer and where it really came from.
      func fetchViaProxy(ctx context.Context, target string, h http.Header, port string, wait float64) (*http.Response, string, error) {
      	for hop := 0; hop < 10; hop++ {
      		u, err := url.Parse(target)
      		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
      			return nil, "", &refused{status: 403}
      		}
      		if home, err := leadsHome(ctx, u.Hostname()); err != nil {
      			return nil, "", err
      		} else if home {
      			return nil, "", &refused{status: 403}
      		}
      		local := "http://127.0.0.1:" + port + cdnAddress(u)
      		hh := h.Clone()
      		hh.Set("X-Relay-Fetch", "1")
      		r, err := fetch(ctx, local, hh, opts{noFollow: true, wait: wait})
      		if err != nil || !isRedirect(r.StatusCode) || r.Header.Get("Location") == "" {
      			return r, target, err
      		}
      		r.Body.Close()
      		next, err := r.Request.URL.Parse(r.Header.Get("Location"))
      		if err != nil {
      			return nil, "", &refused{status: 502}
      		}
      		if target = next.String(); next.Host == "127.0.0.1:"+port {
      			target = realAddress(next.RequestURI())
      		}
      		if target == "" {
      			return nil, "", &refused{status: 502}
      		}
      	}
      	return nil, "", &refused{status: 502}
      }

      func forgetIdle() { // (mu held)
      	now := mono()
      	for k, s := range sessions {
      		if now-s.used > 60 {
      			delete(sessions, k)
      		}
      	}
      	for k, l := range locks {
      		if _, ok := sessions[k]; !ok && len(l) == 0 {
      			delete(locks, k)
      		}
      	}
      	for k, t := range hlsWho {
      		if now-t > 60 {
      			delete(hlsWho, k)
      		}
      	}
      	for k, row := range hlsRows {
      		if now-row.last > 30 {
      			delete(hlsRows, k)
      		}
      	}
      }

      type direct struct { // the provider answered without sending the channel elsewhere: passed on as is
      	status int
      	h      http.Header
      	data   []byte
      }

      // openChannel asks the provider once and remembers where it sent the channel; later opens wait up to 20 s.
      func openChannel(key, target string, h http.Header, renewFrom string) (*session, *direct, error) {
      	mu.Lock()
      	lock := locks[key]
      	if lock == nil {
      		lock = make(chan struct{}, 1)
      		locks[key] = lock
      	}
      	mu.Unlock()
      	select {
      	case lock <- struct{}{}:
      	case <-time.After(20 * time.Second):
      		return nil, nil, &refused{status: 504}
      	}
      	defer func() { <-lock }()
      	mu.Lock()
      	if s := sessions[key]; s != nil && (renewFrom == "" || s.loc != renewFrom) {
      		s.used = mono() // opened (or renewed) by someone else meanwhile
      		mu.Unlock()
      		return s, nil, nil
      	}
      	mu.Unlock()
      	r, err := fetch(context.Background(), target, h, opts{noFollow: true, wait: playlistWait})
      	if err != nil {
      		return nil, nil, err
      	}
      	if loc := r.Header.Get("Location"); isRedirect(r.StatusCode) && loc != "" {
      		r.Body.Close()
      		u, err := r.Request.URL.Parse(loc)
      		if err != nil {
      			return nil, nil, &refused{status: 502}
      		}
      		if isPrivate(u.Hostname()) || strings.EqualFold(u.Hostname(), "localhost") || (u.Scheme != "http" && u.Scheme != "https") {
      			return nil, nil, &refused{status: 403}
      		}
      		s := &session{u.String(), u.String(), mono()}
      		mu.Lock()
      		sessions[key] = s
      		mu.Unlock()
      		if renewFrom != "" {
      			say("a live HLS channel was opened again at the provider")
      		} else {
      			say("a live HLS channel was opened at the provider")
      		}
      		return s, nil, nil
      	}
      	data, err := readAll(r, 4<<20, playlistWait)
      	return nil, &direct{r.StatusCode, r.Header, data}, err
      }

      func playlistRequest(w http.ResponseWriter, r *http.Request) error {
      	provider, uri, orig := strings.TrimRight(r.Header.Get("X-Relay-Provider"), "/"), r.Header.Get("X-Relay-Uri"), r.Header.Get("X-Relay-Orig")
      	port := r.Header.Get("X-Relay-Port")
      	if _, err := strconv.Atoi(port); !providers[provider] || !strings.HasPrefix(uri, "/") || !strings.HasPrefix(orig, "/") || err != nil {
      		answer(w, 403, "", notProxy)
      		return nil
      	}
      	mu.Lock()
      	forgetIdle()
      	mu.Unlock()
      	h := http.Header{}
      	for k, vs := range r.Header {
      		if !plSkipRe.MatchString(k) {
      			h[k] = vs
      		}
      	}
      	opath, oquery, _ := strings.Cut(orig, "?")
      	q := ""
      	if oquery != "" {
      		q = "?" + oquery
      	}
      	ctx := r.Context()
      	if m := cdnRe.FindStringSubmatch(opath); m != nil { // a playlist on a server the provider sent the player to
      		if m[3] == "" {
      			m[3] = "/"
      		}
      		resp, final, err := fetchViaProxy(ctx, strings.ToLower(m[1])+"://"+m[2]+m[3]+q, h, port, playlistWait)
      		if err != nil {
      			return err
      		}
      		data, err := readAll(resp, 4<<20, playlistWait)
      		if err != nil {
      			return err
      		}
      		return servePlaylist(w, resp.StatusCode, resp.Header, data, final, "")
      	}
      	if r.Header.Get("X-Relay-Mode") == "ts" { // a ts provider: live HLS is built from the shared MPEG-TS stream
      		if done, err := hlsFromTS(w, r, provider, uri, opath, q, port); done {
      			return err
      		}
      	}
      	m := chRe.FindStringSubmatch(opath) // a channel's own folder: /__ch/<channel>/<__index.m3u8 or a piece>
      	chanOrig := opath
      	if m != nil {
      		chanOrig = m[1]
      	}
      	plainURI := strings.SplitN(uri, "?", 2)[0]
      	if p := strings.FieldsFunc(plainURI, func(r rune) bool { return r == '/' }); len(p) == 3 && strings.HasSuffix(p[2], ".m3u8") {
      		plainURI = "/live/" + strings.Join(p, "/") // the same channel as its /live/ form: one provider session
      	}
      	key, target := provider+" "+plainURI, provider+uri
      	if m != nil && oquery != "" && !strings.Contains(uri, "?") {
      		target += q
      	}
      	mu.Lock()
      	s := sessions[key]
      	mu.Unlock()
      	if s == nil {
      		var d *direct
      		var err error
      		if s, d, err = openChannel(key, target, h, ""); err != nil {
      			return err
      		} else if d != nil {
      			return servePlaylist(w, d.status, d.h, d.data, target, chanOrig)
      		}
      	}
      	mu.Lock()
      	s.used = mono()
      	who := whoIs(r)
      	if rk := who + " " + provider + " " + channelID(uri); who != "127.0.0.1" && who != "::1" { // (not the relay's own bridge)
      		if row := hlsRows[rk]; row != nil && mono()-row.last <= 30 {
      			row.last = mono()
      		} else {
      			hlsRows[rk] = &hlsRow{mono(), mono(), who, channelID(uri), provider, port}
      		}
      	}
      	t, said := hlsWho[who+" "+key]
      	hlsWho[who+" "+key] = mono()
      	mu.Unlock()
      	if m == nil { // the player opened the channel: give it the channel's folder
      		if !said || mono()-t > 60 { // (once a minute: some players ask the channel's own address at each refresh)
      			say("%s opened HLS channel %s (%s)", who, channelID(uri), hostOf(provider))
      		}
      		w.Header().Set("Location", "/__ch"+opath+"/__index.m3u8"+q)
      		answer(w, 302, "", "")
      		return nil
      	}
      	if m[2] != "__index.m3u8" { // a piece named next to the playlist: where it really is
      		mu.Lock()
      		final := s.final
      		mu.Unlock()
      		b, _ := url.Parse(final)
      		u, err := b.Parse(m[2] + q)
      		if err != nil || cdnAddress(u) == "" {
      			answer(w, 404, "", "")
      			return nil
      		}
      		w.Header().Set("Location", cdnAddress(u))
      		answer(w, 302, "", "")
      		return nil
      	}
      	var status int
      	var hd http.Header
      	var data []byte
      	var final string
      	for attempt := 1; attempt <= 2; attempt++ {
      		mu.Lock()
      		loc := s.loc
      		mu.Unlock()
      		resp, fin, err := fetchViaProxy(ctx, loc, h, port, 25)
      		if err == nil {
      			data, err = readAll(resp, 4<<20, 25)
      			status, hd, final = resp.StatusCode, resp.Header, fin
      		}
      		if err != nil {
      			if attempt == 2 {
      				return err
      			}
      			var rf *refused
      			status, hd, data, final = 502, nil, nil, loc
      			if errors.As(err, &rf) {
      				status = rf.status
      			}
      		}
      		if !renewOn[status] || attempt == 2 {
      			break
      		}
      		ns, d, err := openChannel(key, target, h, loc) // the remembered session is gone
      		if err != nil {
      			return err
      		} else if d != nil {
      			return servePlaylist(w, d.status, d.h, d.data, target, chanOrig)
      		}
      		s = ns
      	}
      	mu.Lock()
      	s.final = final
      	if status == 200 {
      		hlsSeen[port+chanOrig] = mono()
      		if len(hlsSeen) > 300 {
      			for k, t := range hlsSeen {
      				if mono()-t > 30 {
      					delete(hlsSeen, k)
      				}
      			}
      		}
      	}
      	mu.Unlock()
      	return servePlaylist(w, status, hd, data, final, "")
      }

      // ---------------------------------------------------------------------------
      // /check: where a /__cdn/ server name really leads (Caddy asks before it connects); kept a minute
      // ---------------------------------------------------------------------------
      type checked struct {
      	at float64
      	ok bool
      }

      var checks = map[string]checked{}

      func check(w http.ResponseWriter, r *http.Request) {
      	u, err := url.Parse("//" + r.Header.Get("X-Relay-Check"))
      	if err != nil || u.Hostname() == "" {
      		answer(w, 403, "", "No server named.\n")
      		return
      	}
      	host, now := strings.ToLower(u.Hostname()), mono()
      	mu.Lock()
      	c, have := checks[host]
      	mu.Unlock()
      	if !have || now-c.at >= 60 {
      		home, _ := leadsHome(r.Context(), host) // no answer: Caddy's own lookup fails the same way
      		c = checked{now, !home}
      		mu.Lock()
      		if len(checks) > 2000 {
      			checks = map[string]checked{}
      		}
      		checks[host] = c
      		mu.Unlock()
      	}
      	if c.ok {
      		answer(w, 200, "", "ok\n")
      	} else {
      		answer(w, 403, "", "That address leads to this machine or a home network.\n")
      	}
      }

      // ---------------------------------------------------------------------------
      // /api: player_api.php lists, every "http(s)://... (also JSON's \/ form) pointed back here, piece by piece
      // ---------------------------------------------------------------------------
      var (
      	apiLink  = regexp.MustCompile(`"(https?):(?:\\/\\/|//)`)
      	hostKeep = regexp.MustCompile(`[^A-Za-z0-9.:\[\]-]`)
      )

      func api(w http.ResponseWriter, r *http.Request) error {
      	provider, uri := strings.TrimRight(r.Header.Get("X-Relay-Provider"), "/"), r.Header.Get("X-Relay-Uri")
      	home := hostKeep.ReplaceAllString(r.Header.Get("X-Relay-Host"), "")
      	if !providers[provider] || !strings.HasPrefix(uri, "/") || home == "" {
      		answer(w, 403, "", notProxy)
      		return nil
      	}
      	h := http.Header{}
      	for k, vs := range r.Header {
      		if !skipRe.MatchString(k) {
      			h[k] = vs
      		}
      	}
      	resp, err := fetch(r.Context(), provider+uri, h, opts{guardAll: true, wait: 300})
      	if err != nil {
      		return err
      	}
      	defer resp.Body.Close()
      	if ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce != "" && ce != "identity" {
      		return &refused{status: 502} // asked for plain text; a compressed answer cannot be read
      	}
      	for k, vs := range resp.Header {
      		if !plSkipRe.MatchString(k) {
      			w.Header()[k] = vs
      		}
      	}
      	gz := strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip")
      	if gz {
      		w.Header().Set("Content-Encoding", "gzip")
      		w.Header().Set("Vary", "Accept-Encoding")
      	}
      	w.WriteHeader(resp.StatusCode)
      	if err := apiStream(w, resp.Body, []byte(`"http://`+home+"/__cdn/"), gz); err != nil {
      		panic(http.ErrAbortHandler) // cut off: the app must see a broken answer, not a short complete one
      	}
      	return nil
      }

      func apiStream(w io.Writer, body io.Reader, link []byte, gz bool) error {
      	repl := append(append([]byte{}, link...), "$${1}/"...)
      	out := w
      	var z *gzip.Writer
      	if gz {
      		z, _ = gzip.NewWriterLevel(w, 1)
      		out = z
      	}
      	var tail []byte
      	buf := make([]byte, 65536)
      	for {
      		n, err := body.Read(buf)
      		data := append(tail, buf[:n]...)
      		tail = nil
      		// every link starts with a quote: hold back a quote near the end until the next piece
      		if cut := bytes.LastIndexByte(data, '"'); err == nil && cut >= 0 && len(data)-cut < 12 {
      			data, tail = data[:cut], append([]byte{}, data[cut:]...)
      		}
      		if _, werr := out.Write(apiLink.ReplaceAll(data, repl)); werr != nil {
      			return werr
      		}
      		if err == io.EOF {
      			break
      		} else if err != nil {
      			return err
      		}
      	}
      	if z != nil {
      		return z.Close()
      	}
      	return nil
      }

  relay-tshls:
    content: |
      package main

      // HLS built from the shared MPEG-TS stream, for a provider set to NAME_LIVE_TV=ts.
      //
      // Most providers allow one session per channel, and an HLS request and an MPEG-TS request for the same
      // channel count as two: each ends the other. So on a ts provider the provider only ever sees MPEG-TS. A
      // player that asks for a live channel as HLS joins the channel's one shared MPEG-TS stream like any other
      // reader, and the relay cuts that stream into short pieces at keyframes (timed by the video's own clock)
      // and serves a normal live playlist from memory. Everyone on the channel stays on one provider connection.

      import (
      	"context"
      	"fmt"
      	"math"
      	"net/http"
      	"regexp"
      	"strconv"
      	"strings"
      	"time"
      )

      const (
      	segTarget   = 4.0      // seconds of video per piece (cut at the first keyframe after this)
      	segKeep     = 8        // pieces kept (about half a minute)
      	segIdle     = 30.0     // seconds without a playlist request before the HLS reader leaves the channel
      	segMinFirst = 2        // pieces a playlist waits for first (a provider's opening burst holds them)
      	segWait     = 20       // seconds a playlist request waits for them
      	segBytes    = 40 << 20 // pieces kept, at most, in bytes (a 4K channel's pieces are much larger)
      )

      // a live channel address that can be built from MPEG-TS: /live/U/P/<n>.m3u8 or /U/P/<n>.m3u8
      var liveM3U8 = regexp.MustCompile(`^/+(live/+)?[^/]+/+[^/]+/+[0-9]+\.m3u8$$`)

      type segment struct {
      	seq   int
      	dur   float64
      	disc  bool
      	data  []byte
      	ready time.Time
      }

      type segmenter struct {
      	key, provider, who, port, id string
      	ch                           *channel
      	rd                           *reader
      	segs                         []*segment
      	next                         int
      	asked                        float64 // last playlist or piece request
      	grew                         chan struct{}
      	over, started                bool
      	discSeq                      int
      	longest                      float64            // the longest piece so far: TARGETDURATION never goes down
      	players                      map[string]float64 // device -> its last request
      	waiting                      map[string]int     // device -> its playlist requests still waiting for pieces
      }

      var segmenters = map[string]*segmenter{} // TS channel key -> its HLS builder

      // hlsFromTS answers a playlist or piece request for a live channel of a ts provider. ok=false: not a live
      // channel address (catch-up and the like), handled the usual way.
      func hlsFromTS(w http.ResponseWriter, r *http.Request, provider, uri, opath, q, port string) (bool, error) {
      	chURI := strings.SplitN(uri, "?", 2)[0]
      	if !liveM3U8.MatchString(chURI) {
      		return false, nil
      	}
      	m := chRe.FindStringSubmatch(opath)
      	who, id := whoIs(r), channelID(chURI)
      	if m == nil { // the player opened the channel: give it the channel's folder (nothing asked of the provider yet)
      		mu.Lock()
      		t, said := hlsWho[who+" ts "+provider+" "+id]
      		hlsWho[who+" ts "+provider+" "+id] = mono()
      		mu.Unlock()
      		if !said || mono()-t > 60 { // (once a minute: some players ask the channel's own address at each refresh)
      			say("%s opened channel %s as HLS (%s); it is built from the shared MPEG-TS stream", who, id, hostOf(provider))
      		}
      		w.Header().Set("Location", "/__ch"+opath+"/__index.m3u8"+q)
      		answer(w, 302, "", "")
      		return true, nil
      	}
      	tsURI := strings.TrimSuffix(chURI, ".m3u8") + ".ts"
      	key := channelKey("/ts", provider, tsURI)
      	var sg *segmenter
      	if m[2] != "__index.m3u8" { // a piece comes only from a running builder: asking for one never opens the channel
      		mu.Lock()
      		sg = segmenters[key]
      		mu.Unlock()
      		if sg == nil {
      			answer(w, 404, "", "")
      			return true, nil
      		}
      	} else if s, err := segmenterFor(r.Context(), key, provider, tsURI, who, port, id, r.Header); err != nil {
      		return true, err
      	} else {
      		sg = s
      	}
      	mu.Lock()
      	sg.asked = mono()
      	sg.players[who] = mono()
      	if rk := who + " " + provider + " " + id; who != "127.0.0.1" && who != "::1" {
      		if row := hlsRows[rk]; row != nil && mono()-row.last <= 30 {
      			row.last = mono()
      		} else {
      			hlsRows[rk] = &hlsRow{mono(), mono(), who, id, provider, port}
      		}
      	}
      	mu.Unlock()
      	if name := m[2]; name != "__index.m3u8" { // a piece
      		n, perr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg_"), ".ts"))
      		mu.Lock()
      		var data []byte
      		for _, s := range sg.segs {
      			if perr == nil && s.seq == n {
      				data = s.data
      			}
      		}
      		mu.Unlock()
      		if data == nil {
      			answer(w, 404, "", "")
      			return true, nil
      		}
      		w.Header().Set("Content-Type", "video/mp2t")
      		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
      		w.WriteHeader(200)
      		w.Write(data)
      		return true, nil
      	}
      	deadline := time.After(segWait * time.Second) // the playlist: wait for the first pieces
      	mu.Lock()
      	sg.waiting[who]++ // (a device waiting here is not quiet: releaseFor leaves its builder alone)
      	mu.Unlock()
      	defer func() {
      		mu.Lock()
      		if sg.waiting[who]--; sg.waiting[who] <= 0 {
      			delete(sg.waiting, who)
      		}
      		sg.players[who] = mono()
      		mu.Unlock()
      	}()
      wait:
      	for {
      		mu.Lock()
      		n, over, grew := len(sg.segs), sg.over, sg.grew
      		mu.Unlock()
      		switch {
      		case n >= segMinFirst || (over && n > 0):
      			break wait
      		case over: // the provider's own refusal is passed on, as for MPEG-TS
      			mu.Lock()
      			e := sg.ch.refusal
      			mu.Unlock()
      			if e != nil && e.status >= 400 && e.status < 600 {
      				answer(w, e.status, e.kind, string(e.text))
      			} else {
      				answer(w, 502, "", "")
      			}
      			return true, nil
      		}
      		select {
      		case <-grew:
      		case <-deadline:
      			if n == 0 {
      				answer(w, 504, "", "")
      				return true, nil
      			}
      			break wait
      		case <-r.Context().Done():
      			return true, nil
      		}
      	}
      	mu.Lock()
      	var b strings.Builder
      	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n", int(sg.target()))
      	if len(sg.segs) > 0 {
      		fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", sg.segs[0].seq, sg.discSeq)
      	}
      	for _, s := range sg.segs {
      		if s.disc {
      			b.WriteString("#EXT-X-DISCONTINUITY\n")
      		}
      		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg_%d.ts%s\n", s.dur, s.seq, q)
      	}
      	if sg.over {
      		b.WriteString("#EXT-X-ENDLIST\n")
      	}
      	mu.Unlock()
      	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
      	w.Header().Set("Cache-Control", "no-cache")
      	answer(w, 200, "application/vnd.apple.mpegurl", b.String())
      	return true, nil
      }

      // segmenterFor finds the channel's HLS builder, or joins the channel's shared MPEG-TS stream (opening it at
      // the provider if nobody is on it) and starts one.
      func segmenterFor(ctx context.Context, key, provider, tsURI, who, port, id string, rh http.Header) (*segmenter, error) {
      	mu.Lock()
      	if sg := segmenters[key]; sg != nil && !sg.over {
      		if sg.players[who] == 0 { // a device new to this builder has left any builder of its own
      			releaseFor(provider, who, key)
      		}
      		mu.Unlock()
      		return sg, nil
      	}
      	ch := channels[key]
      	first := ch == nil
      	if !first { // moving onto a channel already open: an HLS builder this device left goes too
      		releaseFor(provider, who, key)
      	}
      	if first {
      		lastOpen[provider] = opened{mono(), key}
      		releaseFor(provider, who, key)
      		for _, c := range channels { // channels of this provider that nobody reads go first (changing channel)
      			if c.provider == provider {
      				c.dropIfEmpty()
      			}
      		}
      		wait := lastDrop[provider] + dropGap - mono()
      		mu.Unlock()
      		time.Sleep(secs(math.Max(0, wait)))
      		mu.Lock()
      		if sg := segmenters[key]; sg != nil && !sg.over { // another request got here first
      			mu.Unlock()
      			return sg, nil
      		}
      		if ch = channels[key]; ch == nil {
      			sent := http.Header{}
      			for k, vs := range rh {
      				if !skipRe.MatchString(k) {
      					for _, v := range vs {
      						sent.Add(k, notPrintable.ReplaceAllString(v, ""))
      					}
      				}
      			}
      			ch = newChannel(key, tsSource(provider+tsURI, sent), provider)
      			channels[key] = ch
      		} else {
      			first = false
      		}
      	}
      	rd := ch.join(first)
      	rd.pace = nil // the builder takes the joiner's cushion at once: the first pieces are ready straight away
      	rd.who, rd.id, rd.port, rd.came, rd.kind = who, id, port, mono(), "HLS built from MPEG-TS"
      	// Piece numbers keep growing from one builder to the next (a new one starting again at 0 left players
      	// frozen after a pause), and a new builder's first piece is marked as not following on from the last.
      	now := int(time.Now().Unix())
      	sg := &segmenter{key: key, provider: provider, who: who, port: port, id: id, ch: ch, rd: rd, asked: mono(),
      		grew: make(chan struct{}), next: now, discSeq: now, players: map[string]float64{who: mono()},
      		waiting: map[string]int{}}
      	segmenters[key] = sg
      	mu.Unlock()
      	if first {
      		say("%s started channel %s (%s) for HLS from MPEG-TS", who, id, hostOf(provider))
      	} else {
      		say("%s joined channel %s (%s) for HLS from MPEG-TS", who, id, hostOf(provider))
      	}
      	go sg.run()
      	return sg, nil
      }

      // target: the playlist's TARGETDURATION, in whole seconds. (mu held)
      func (sg *segmenter) target() float64 { return math.Ceil(math.Max(segTarget, sg.longest)) }

      // patPMT: the PMT PID of a PAT section's first program (program 0 is the network table, not a program).
      func patPMT(p []byte) int {
      	if len(p) < 12 {
      		return -1
      	}
      	end := min(len(p)-4, 3+(int(p[1]&0x0f)<<8|int(p[2]))-4) // (minus the CRC)
      	for i := 8; i+4 <= end; i += 4 {
      		if p[i] != 0 || p[i+1] != 0 {
      			return int(p[i+2]&0x1f)<<8 | int(p[i+3])
      		}
      	}
      	return -1
      }

      // run reads the channel's MPEG-TS as it arrives and cuts it into pieces.
      func (sg *segmenter) run() {
      	var cut tsCutter
      	start := mono()
      	defer func() {
      		mu.Lock()
      		sg.over = true
      		close(sg.grew)
      		sg.grew = make(chan struct{})
      		sg.ch.leave(sg.rd)
      		if segmenters[sg.key] == sg {
      			delete(segmenters, sg.key)
      		}
      		written := sg.rd.written.Load()
      		mu.Unlock()
      		say("%s left channel %s (HLS from MPEG-TS) after %s, %.1f MB cut into pieces", sg.who, sg.id, hms(mono()-start), float64(written)/1048576)
      	}()
      	for {
      		mu.Lock()
      		if sg.rd.dropped || (mono()-sg.asked > segIdle) {
      			mu.Unlock()
      			return
      		}
      		if len(sg.rd.q) == 0 {
      			if sg.rd.over {
      				mu.Unlock()
      				if seg := cut.flush(); seg != nil {
      					sg.add(seg)
      				}
      				return
      			}
      			mu.Unlock()
      			select {
      			case <-sg.rd.wake:
      			case <-time.After(5 * time.Second): // (to notice a player that stopped asking)
      			}
      			continue
      		}
      		data := sg.rd.q[0]
      		sg.rd.q[0], sg.rd.q = nil, sg.rd.q[1:]
      		sg.rd.n -= len(data)
      		sg.rd.given = max(0, sg.rd.given-len(data))
      		mu.Unlock()
      		sg.rd.written.Add(int64(len(data)))
      		for _, seg := range cut.feed(data) {
      			sg.add(seg)
      		}
      	}
      }

      func (sg *segmenter) add(s *segment) {
      	mu.Lock()
      	defer mu.Unlock()
      	s.seq = sg.next
      	sg.next++
      	sg.longest = math.Max(sg.longest, s.dur)
      	if !sg.started {
      		s.disc, sg.started = true, true
      	}
      	sg.segs = append(sg.segs, s)
      	total := 0
      	for _, x := range sg.segs {
      		total += len(x.data)
      	}
      	for len(sg.segs) > segKeep || (len(sg.segs) > 4 && total > segBytes) { // (a playlist keeps 3 target durations and the piece being fetched)
      		total -= len(sg.segs[0].data)
      		if sg.segs[0].disc {
      			sg.discSeq++
      		}
      		sg.segs[0] = nil
      		sg.segs = sg.segs[1:]
      	}
      	close(sg.grew)
      	sg.grew = make(chan struct{})
      }

      // ---------------------------------------------------------------------------
      // Cutting MPEG-TS into pieces
      // ---------------------------------------------------------------------------
      // Each piece starts with the stream's program tables (PAT and PMT) and, when the stream marks them, at a
      // keyframe of the video, so a player can start playing any piece. Durations come from the video's own
      // timestamps (PTS). A jump in the timestamps (the provider's stream was asked for again) starts a new piece
      // marked as a discontinuity.
      type tsCutter struct {
      	buf           []byte
      	pmtPID, avPID int
      	pat, pmt      []byte
      	cur           []byte  // the piece being built
      	curStart      float64 // PTS (seconds) of its first frame
      	lastPTS       float64 // PTS of the newest frame start; -1 = none yet
      	firstPTS      float64 // the first frame seen; -1 = none yet
      	sawKey        bool    // the stream marks keyframes
      	nextDis       bool
      }

      func (c *tsCutter) feed(data []byte) []*segment {
      	if c.avPID == 0 { // (first call)
      		c.pmtPID, c.avPID, c.lastPTS, c.firstPTS = -1, -1, -1, -1
      	}
      	c.buf = append(c.buf, data...)
      	var out []*segment
      	for len(c.buf) >= 188 {
      		if c.buf[0] != 0x47 { // lost the packet boundary: find it again
      			i := 1
      			for ; i+376 < len(c.buf); i++ {
      				if c.buf[i] == 0x47 && c.buf[i+188] == 0x47 && c.buf[i+376] == 0x47 {
      					break
      				}
      			}
      			if i+376 >= len(c.buf) {
      				c.buf = append([]byte(nil), c.buf[len(c.buf)-min(len(c.buf), 376):]...)
      				return out
      			}
      			c.buf = c.buf[i:]
      			continue
      		}
      		if seg := c.packet(c.buf[:188:188]); seg != nil {
      			out = append(out, seg)
      		}
      		c.buf = c.buf[188:]
      	}
      	c.buf = append([]byte(nil), c.buf...) // (keep only the partial packet)
      	return out
      }

      func (c *tsCutter) packet(pkt []byte) *segment {
      	pid := int(pkt[1]&0x1f)<<8 | int(pkt[2])
      	pusi := pkt[1]&0x40 != 0
      	afc := (pkt[3] >> 4) & 3
      	payload, rai := 4, false
      	if afc&2 != 0 {
      		alen := int(pkt[4])
      		rai = alen > 0 && pkt[5]&0x40 != 0
      		payload = 5 + alen
      	}
      	if afc&1 == 0 || payload >= 188 {
      		payload = 188
      	}
      	body := pkt[payload:]
      	switch {
      	case pid == 0 && pusi:
      		c.pat = append([]byte(nil), pkt...)
      		if pmt := patPMT(tableStart(body)); pmt >= 0 {
      			c.pmtPID = pmt // the first program
      		}
      		return nil // (every piece starts with its own copy)
      	case pid == c.pmtPID && pusi:
      		c.pmt = append([]byte(nil), pkt...)
      		if s := pickStream(tableStart(body)); s >= 0 { // (read at every PMT: a stream asked for again may use new PIDs)
      			c.avPID = s
      		}
      		return nil
      	case pid == 0 || pid == c.pmtPID:
      		return nil
      	}
      	var seg *segment
      	if pid == c.avPID && pusi && c.pat != nil && c.pmt != nil {
      		if pts, ok := pesPTS(body); ok {
      			c.sawKey = c.sawKey || rai
      			if c.firstPTS < 0 {
      				c.firstPTS = pts
      			}
      			jump := c.lastPTS >= 0 && (pts < c.lastPTS-1 || pts > c.lastPTS+10)
      			if jump {
      				c.firstPTS = pts // (counted again from here, for a stream that marks no keyframes)
      			}
      			// a frame a piece may start at: a keyframe, or any frame if the stream marks none (after a while)
      			// (a piece is also cut when keyframe marks stop coming, so it cannot grow without end)
      			startable := rai || (!c.sawKey && pts-c.firstPTS >= 2*segTarget) || jump || pts-c.curStart >= 5*segTarget
      			if c.cur != nil && startable && (pts-c.curStart >= segTarget || jump) {
      				dur := pts - c.curStart
      				if jump {
      					dur = c.lastPTS - c.curStart + 0.04
      				}
      				seg = &segment{dur: math.Max(dur, 0.1), disc: c.nextDis, data: c.cur}
      				c.cur, c.nextDis = nil, jump
      			}
      			if c.cur == nil && startable {
      				c.cur = append(append(make([]byte, 0, 1<<20), c.pat...), c.pmt...)
      				c.curStart = pts
      			}
      			c.lastPTS = pts
      		}
      	}
      	if c.cur != nil {
      		c.cur = append(c.cur, pkt...)
      	}
      	return seg
      }

      // flush: what is left when the stream ends.
      func (c *tsCutter) flush() *segment {
      	if c.cur == nil || c.lastPTS <= c.curStart {
      		return nil
      	}
      	s := &segment{dur: c.lastPTS - c.curStart, disc: c.nextDis, data: c.cur}
      	c.cur = nil
      	return s
      }

      // tableStart: the section after a PSI pointer field.
      func tableStart(body []byte) []byte {
      	if len(body) < 1 || int(body[0])+1 > len(body) {
      		return nil
      	}
      	return body[1+int(body[0]):]
      }

      // pickStream: the video stream of a PMT section, or else its first audio stream.
      func pickStream(s []byte) int {
      	if len(s) < 12 {
      		return -1
      	}
      	end := min(len(s)-4, 3+int(s[1]&0x0f)<<8|int(s[2])-4) // (minus the CRC)
      	i := 12 + (int(s[10]&0x0f)<<8 | int(s[11]))
      	audio := -1
      	for i+5 <= end {
      		typ, pid, info := s[i], int(s[i+1]&0x1f)<<8|int(s[i+2]), int(s[i+3]&0x0f)<<8|int(s[i+4])
      		switch typ {
      		case 0x01, 0x02, 0x10, 0x1b, 0x24, 0x42, 0xea:
      			return pid
      		case 0x03, 0x04, 0x0f, 0x11, 0x81, 0x87:
      			if audio < 0 {
      				audio = pid
      			}
      		}
      		i += 5 + info
      	}
      	return audio
      }

      // pesPTS: the presentation time, in seconds, of a PES packet that starts in this payload.
      func pesPTS(b []byte) (float64, bool) {
      	if len(b) < 14 || b[0] != 0 || b[1] != 0 || b[2] != 1 || b[7]&0x80 == 0 {
      		return 0, false
      	}
      	p := int64(b[9]&0x0e)<<29 | int64(b[10])<<22 | int64(b[11]&0xfe)<<14 | int64(b[12])<<7 | int64(b[13])>>1
      	return float64(p) / 90000, true
      }

      // releaseFor: a device that opens another channel of a provider has left its old one. A builder only that
      // device was using, or one every device has stopped asking for, lets go now, so its provider connection is
      // free at once (one-connection accounts). (mu held)
      func releaseFor(provider, who, key string) {
      	for k, sg := range segmenters {
      		if sg.provider != provider || k == key {
      			continue
      		}
      		alone := true
      		for p, t := range sg.players { // (a player asks about once per target duration)
      			alone = alone && (p == who || (mono()-t > 3*sg.target() && sg.waiting[p] == 0))
      		}
      		if alone {
      			delete(segmenters, k)
      			sg.asked = 0       // (its loop sees this and ends)
      			sg.ch.leave(sg.rd) // let go now: the provider gets dropGap to see it before the next channel
      			sg.rd.signal()
      		}
      	}
      }

  relay-status:
    content: |
      // The status page: what is set up, what is wrong, where the VPN comes out, and who is watching what.
      package main

      import (
      	"bytes"
      	"context"
      	"encoding/json"
      	"fmt"
      	"math"
      	"net/http"
      	"net/url"
      	"sort"
      	"strconv"
      	"strings"
      	"sync"
      	"syscall"
      	"time"
      )

      func parseJSON(data []byte) (any, error) {
      	d := json.NewDecoder(bytes.NewReader(data))
      	d.UseNumber()
      	var v any
      	return v, d.Decode(&v)
      }

      func pyStr(v any) string { // str() as Python writes a JSON value
      	switch v := v.(type) {
      	case nil:
      		return "None"
      	case bool:
      		return map[bool]string{true: "True", false: "False"}[v]
      	case string:
      		return v
      	}
      	return fmt.Sprint(v)
      }

      func truthy(v any) bool {
      	switch v := v.(type) {
      	case nil:
      		return false
      	case bool:
      		return v
      	case string:
      		return v != ""
      	case json.Number:
      		f, _ := v.Float64()
      		return f != 0
      	case []any:
      		return len(v) > 0
      	case map[string]any:
      		return len(v) > 0
      	}
      	return true
      }

      func esc(v any) string { // html.escape(str(v), quote=True)
      	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(pyStr(v))
      }

      func field(v any, k string) any { m, _ := v.(map[string]any); return m[k] }

      // ---- answers from outside, kept so the page is quick (failures are retried after a minute)
      type keptValue struct {
      	at    float64
      	value any
      	busy  chan struct{}
      }

      var keptValues = map[string]*keptValue{}

      func kept(name string, keep float64, ask func(context.Context) (any, error)) any {
      	mu.Lock()
      	k := keptValues[name]
      	if k == nil {
      		k = &keptValue{at: -1e9}
      		keptValues[name] = k
      	}
      	limit := keep
      	if k.value == nil {
      		limit = 60
      	}
      	if k.busy == nil && mono()-k.at >= limit {
      		k.busy = make(chan struct{})
      		go func(done chan struct{}) {
      			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
      			v, err := ask(ctx)
      			cancel()
      			if err != nil {
      				v = nil
      			}
      			mu.Lock()
      			k.at, k.value, k.busy = mono(), v, nil
      			mu.Unlock()
      			close(done)
      		}(k.busy)
      	}
      	busy := k.busy
      	mu.Unlock()
      	if busy != nil {
      		<-busy
      	}
      	mu.Lock()
      	defer mu.Unlock()
      	return k.value
      }

      func whereAmI() any { // what the internet sees, asked through the VPN (certificate checked)
      	return kept("where", 300, func(ctx context.Context) (any, error) {
      		r, err := fetch(ctx, "https://ipinfo.io/json", agentOf("curl/8"), opts{verify: true, wait: headerWait})
      		if err != nil {
      			return nil, err
      		}
      		data, err := readAll(r, 1<<20, 6)
      		if err != nil || r.StatusCode != 200 {
      			return nil, err
      		}
      		return parseJSON(data)
      	})
      }

      // vpnPing: round trip to the VPN server in ms. A packet allowed one hop dies there and its "time exceeded"
      // lands in the socket's error queue (IP_RECVERR): no raw socket or privilege needed.
      func vpnPing() (float64, bool) {
      	var times []float64
      	for n := 0; n < 3; n++ {
      		func() {
      			fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
      			if err != nil {
      				return
      			}
      			defer syscall.Close(fd)
      			syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TTL, 1)
      			syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_RECVERR, 1)
      			syscall.SetNonblock(fd, true)
      			start := mono()
      			if syscall.Sendto(fd, []byte{0}, 0, &syscall.SockaddrInet4{Port: 33434 + n, Addr: [4]byte{1, 1, 1, 1}}) != nil {
      				return
      			}
      			oob := make([]byte, 512)
      			for mono()-start < 1 {
      				_, oobn, _, _, err := syscall.Recvmsg(fd, make([]byte, 64), oob, syscall.MSG_ERRQUEUE)
      				if err == syscall.EAGAIN {
      					time.Sleep(2 * time.Millisecond)
      					continue
      				}
      				notes, _ := syscall.ParseSocketControlMessage(oob[:oobn])
      				for _, m := range notes {
      					if err == nil && len(m.Data) > 5 && m.Data[4] == 2 && m.Data[5] == 11 { // ICMP time exceeded
      						times = append(times, (mono()-start)*1000)
      						break
      					}
      				}
      				return
      			}
      		}()
      	}
      	if len(times) == 0 {
      		return 0, false
      	}
      	sort.Float64s(times)
      	return times[len(times)/2], true
      }

      // ---- the provider's account page, through the proxy with the easy login: at most every 15 s while the
      // page is viewed; after failures 2, 4, 8... at most 15 minutes apart; the last good answer shown with its age.
      type accountState struct {
      	good, last map[string]any
      	at, tried  float64
      	fails      int
      	task       chan struct{}
      }

      var accounts = map[string]*accountState{}

      func playerAPI(ctx context.Context, port, user, password, extra string) (int, []byte, error) {
      	u := fmt.Sprintf("http://127.0.0.1:%s/player_api.php?username=%s&password=%s%s", port, url.QueryEscape(user), url.QueryEscape(password), extra)
      	status, data, err := get(ctx, u, agentOf(""), 64<<20)
      	if err == nil && status == 502 { // most often a name lookup that failed for a moment
      		time.Sleep(500 * time.Millisecond)
      		status, data, err = get(ctx, u, agentOf(""), 64<<20)
      	}
      	return status, data, err
      }

      func account(port, user, password string) map[string]any {
      	mu.Lock()
      	st := accounts[port]
      	if st == nil {
      		st = &accountState{tried: -1e9}
      		accounts[port] = st
      	}
      	every := 15.0
      	if st.fails > 0 {
      		every = math.Min(60*math.Pow(2, float64(st.fails)), 900)
      	}
      	if st.task == nil && mono()-st.tried >= every {
      		st.tried, st.task = mono(), make(chan struct{})
      		go func(done chan struct{}) {
      			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
      			defer cancel()
      			var value map[string]any
      			if status, data, err := playerAPI(ctx, port, user, password, ""); err == nil && status != 200 {
      				value = map[string]any{"answer": status}
      			} else if err == nil {
      				if v, err := parseJSON(data); err == nil {
      					info := field(v, "user_info")
      					value = map[string]any{"answer": 200, "status": field(info, "status"), "auth": field(info, "auth"),
      						"exp": field(info, "exp_date"), "used": field(info, "active_cons"), "max": field(info, "max_connections")}
      				}
      			}
      			mu.Lock()
      			if value != nil && value["answer"] == 200 {
      				st.good, st.at, st.fails, st.last = value, mono(), 0, nil
      			} else {
      				st.fails, st.last = st.fails+1, value
      			}
      			st.task = nil
      			mu.Unlock()
      			close(done)
      		}(st.task)
      	}
      	task := st.task
      	mu.Unlock()
      	if task != nil { // an answer is on its way: wait a little for it
      		select {
      		case <-task:
      		case <-time.After(6 * time.Second):
      		}
      	}
      	mu.Lock()
      	defer mu.Unlock()
      	if st.good != nil && mono()-st.at < 900 {
      		a := map[string]any{"age": mono() - st.at}
      		for k, v := range st.good {
      			a[k] = v
      		}
      		return a
      	}
      	return st.last
      }

      // ---- channel names for the table: each provider's live list, kept 6 hours, fetched in the background
      type nameList struct {
      	at    float64
      	names map[string]string
      	busy  bool
      }

      var channelNames = map[string]*nameList{}

      func names(port, user, password string) chan struct{} {
      	mu.Lock()
      	defer mu.Unlock()
      	n := channelNames[port]
      	if n == nil {
      		n = &nameList{at: -1e9}
      		channelNames[port] = n
      	}
      	done := make(chan struct{})
      	if n.busy || (len(n.names) > 0 && mono()-n.at < 6*3600) || (len(n.names) == 0 && mono()-n.at < 60) {
      		close(done)
      		return done
      	}
      	n.busy = true
      	go func() {
      		defer close(done)
      		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
      		defer cancel()
      		got := map[string]string{}
      		if status, data, err := playerAPI(ctx, port, user, password, "&action=get_live_streams"); err == nil && status == 200 {
      			var list []struct {
      				ID   json.RawMessage `json:"stream_id"`
      				Name string          `json:"name"`
      			}
      			json.Unmarshal(data, &list)
      			for _, s := range list {
      				got[strings.Trim(string(s.ID), `"`)] = s.Name
      			}
      		}
      		mu.Lock()
      		n.at, n.busy = mono(), false
      		if len(got) > 0 {
      			n.names = got
      		}
      		mu.Unlock()
      	}()
      	return done
      }

      const style = `
      :root{--bg:#f5f6f8;--card:#fff;--ink:#16181d;--soft:#5b6270;--line:#e6e8ee;--chip:#eef0f5;--good:#12805c;--goodbg:#e7f6ef;
      --bad:#c0362c;--badbg:#fdecea;--warn:#8a5a00;--warnbg:#fff4d6;--link:#2457d6;--shadow:0 1px 2px rgba(16,24,40,.06),0 4px 16px rgba(16,24,40,.06)}
      @media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#181b22;--ink:#eceef2;--soft:#9aa3b2;--line:#272b35;--chip:#232733;
      --good:#4ad295;--goodbg:#11281f;--bad:#ff8a80;--badbg:#2b1614;--warn:#ffcf66;--warnbg:#2a2210;--link:#8ab0ff;--shadow:0 1px 2px rgba(0,0,0,.4)}}
      *{box-sizing:border-box}html{-webkit-text-size-adjust:100%}
      body{margin:0;background:var(--bg);color:var(--ink);font:17px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
      main{max-width:860px;margin:0 auto;padding:28px 18px 48px}
      header{display:flex;align-items:center;gap:12px;margin:0 0 18px}
      header .dot{width:14px;height:14px;border-radius:50%;background:var(--good);box-shadow:0 0 0 5px var(--goodbg)}
      header.down .dot{background:var(--bad);box-shadow:0 0 0 5px var(--badbg)}
      h1{font-size:24px;letter-spacing:-.01em;margin:0;font-weight:700}
      h2{font-size:13px;letter-spacing:.08em;text-transform:uppercase;color:var(--soft);margin:0 0 12px;font-weight:700}
      .card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:20px 22px;margin:14px 0;box-shadow:var(--shadow)}
      .banner{border:0;font-size:20px;font-weight:700;display:flex;gap:12px;align-items:flex-start}
      .banner small{display:block;font-size:16px;font-weight:500;margin-top:2px}
      .good{background:var(--goodbg);color:var(--good)}.bad{background:var(--badbg);color:var(--bad)}.warn{background:var(--warnbg);color:var(--warn)}
      .bad ul,.warn ul{font-size:16px;font-weight:500;margin:8px 0 0;padding-left:20px}
      .pair{display:grid;grid-template-columns:max-content 1fr;gap:6px 18px;margin:0}
      .pair dt{color:var(--soft)}.pair dd{margin:0;font-weight:600}
      .provider{border-top:1px solid var(--line);padding:16px 0}.provider:first-of-type{border-top:0;padding-top:2px}.provider:last-of-type{padding-bottom:2px}
      .provider .top{display:flex;flex-wrap:wrap;align-items:center;gap:8px 10px;margin-bottom:8px}
      .provider .name{font-size:19px;font-weight:700;margin-right:4px}
      .pill{display:inline-block;font-size:13px;font-weight:700;padding:3px 10px;border-radius:999px;background:var(--chip);color:var(--soft);white-space:nowrap}
      .pill.good{background:var(--goodbg);color:var(--good)}.pill.bad{background:var(--badbg);color:var(--bad)}.pill.warn{background:var(--warnbg);color:var(--warn)}
      code{font:14.5px/1.4 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;background:var(--chip);padding:3px 8px;border-radius:7px;word-break:break-all}
      .facts{display:flex;flex-wrap:wrap;gap:6px 22px;color:var(--soft);font-size:15.5px;margin-top:8px}.facts b{color:var(--ink);font-weight:600}
      p{margin:10px 0 0}.note{color:var(--soft);font-size:15px}ul{margin:8px 0 0;padding-left:20px}li{margin:7px 0}
      a{color:var(--link);text-decoration:none}a:hover{text-decoration:underline}
      footer{color:var(--soft);font-size:14px;text-align:center;margin-top:22px}
      `

      func plural(n int, one, many string) string {
      	if n == 1 {
      		return one
      	}
      	return many
      }

      func statusPage(w http.ResponseWriter, r *http.Request) {
      	port := r.Header.Get("X-Relay-Port")
      	_, perr := strconv.Atoi(port)
      	host := "this-machine"
      	if perr == nil && r.Host != "" {
      		host = r.Host
      		if i := strings.LastIndexByte(host, ':'); i >= 0 {
      			host = host[:i]
      		}
      	}
      	info := any(map[string]any{"problems": []any{"The proxy could not read its own start-up notes. Look at the log: docker logs iptv-caddy"}, "providers": []any{}})
      	if perr == nil {
      		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
      		if status, data, err := get(ctx, "http://127.0.0.1:"+port+"/__status.json", agentOf(""), 1<<20); err == nil && status == 200 {
      			if v, err := parseJSON(data); err == nil {
      				info = v
      			}
      		}
      		cancel()
      	}
      	var problems []string
      	ps, _ := field(info, "problems").([]any)
      	for _, p := range ps {
      		problems = append(problems, pyStr(p))
      	}
      	var good, skipped []any
      	provs, _ := field(info, "providers").([]any)
      	provName := map[string]string{} // port -> NAME, for the table
      	for _, p := range provs {
      		if truthy(field(p, "ok")) {
      			good = append(good, p)
      			provName[pyStr(field(p, "port"))] = pyStr(field(p, "name"))
      		} else {
      			skipped = append(skipped, p)
      		}
      	}
      	working := len(problems) == 0 && len(good) > 0
      	user, password := r.Header.Get("X-Relay-User"), r.Header.Get("X-Relay-Pass")

      	var where any
      	accts := make([]map[string]any, len(good))
      	var ping float64
      	var pinged bool
      	var wg sync.WaitGroup
      	wg.Add(2 + 2*len(good))
      	go func() { defer wg.Done(); where = whereAmI() }()
      	go func() { defer wg.Done(); ping, pinged = vpnPing() }()
      	for i, p := range good {
      		go func() { defer wg.Done(); accts[i] = account(pyStr(field(p, "port")), user, password) }()
      		go func() { // names wait at most 2 s: missing ones show as ids and fill in on a later refresh
      			defer wg.Done()
      			select {
      			case <-names(pyStr(field(p, "port")), user, password):
      			case <-time.After(2 * time.Second):
      			}
      		}()
      	}
      	wg.Wait()

      	var o strings.Builder
      	add := func(f string, a ...any) { fmt.Fprintf(&o, f, a...) }
      	add(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">`)
      	add(`<meta name="color-scheme" content="light dark"><meta http-equiv="refresh" content="30"><title>IPTV proxy status</title><style>%s</style></head><body><main>`, style)
      	add(`<header class="%s"><span class="dot"></span><h1>IPTV proxy status</h1></header>`, map[bool]string{true: "", false: "down"}[working])
      	if working {
      		add(`<div class="card banner good"><div>&#9989;</div><div>The proxy is working.<small>%d provider%s loaded.</small></div></div>`, len(good), plural(len(good), " is", "s are"))
      	} else {
      		add(`<div class="card banner bad"><div>&#10060;</div><div>The proxy is NOT working yet.<ul>`)
      		for _, p := range problems {
      			add(`<li>%s</li>`, esc(p))
      		}
      		if len(problems) == 0 && len(good) == 0 {
      			add(`<li>No provider is set up. Open your settings file (<code>.env</code>) and add the five lines for a provider. The file itself shows how.</li>`)
      		}
      		add(`</ul><small>After you fix the settings file, start the stack again and reload this page.</small></div></div>`)
      	}
      	if len(skipped) > 0 {
      		add(`<div class="card warn"><h2 style="color:inherit">&#9888;&#65039; These providers were left out</h2><ul>`)
      		for _, p := range skipped {
      			add(`<li><b>%s</b>: %s</li>`, esc(field(p, "name")), esc(field(p, "why")))
      		}
      		add(`</ul><p>Fix the line in your settings file (<code>.env</code>) and start the stack again.</p></div>`)
      	}

      	add(`<div class="card"><h2>Your VPN</h2>`)
      	if truthy(field(where, "ip")) {
      		var place []string
      		for _, k := range []string{"city", "region", "country"} {
      			if truthy(field(where, k)) {
      				place = append(place, pyStr(field(where, k)))
      			}
      		}
      		org, pl := "", ""
      		if truthy(field(where, "org")) {
      			org = "<dt>Network</dt><dd>" + esc(field(where, "org")) + "</dd>"
      		}
      		if pinged {
      			pl = fmt.Sprintf("<dt>Ping to the VPN server</dt><dd>%.0f ms</dd>", ping)
      		}
      		add(`<dl class="pair"><dt>The internet sees you as</dt><dd>%s</dd><dt>Location</dt><dd>%s</dd>%s%s</dl>`, esc(field(where, "ip")), esc(strings.Join(place, ", ")), org, pl)
      		add(`<p class="note">This should be your VPN, not your home. If you see your own town and your own internet company here, stop and check your VPN settings.</p>`)
      	} else {
      		add(`The proxy could not look up its public address just now. Reload this page in a minute.<p class="note">The VPN container still blocks all traffic when the VPN is down, so nothing leaves from your home address.</p>`)
      	}
      	add(`</div><div class="card"><h2>Your providers</h2>`)
      	if len(good) == 0 {
      		add(`None loaded.`)
      	}
      	for i, p := range good {
      		a := accts[i]
      		pill, facts := `<span class="pill warn">no answer from the provider</span>`, ""
      		if a != nil && a["answer"] == 200 {
      			state := ""
      			if truthy(a["status"]) {
      				state = pyStr(a["status"])
      			}
      			switch {
      			case pyStr(a["auth"]) == "0":
      				pill = `<span class="pill bad">login refused by the provider</span>`
      			case state != "":
      				pill = fmt.Sprintf(`<span class="pill %s">%s</span>`, map[bool]string{true: "good", false: "warn"}[strings.ToLower(state) == "active"], esc(state))
      			default:
      				pill = `<span class="pill">answered</span>`
      			}
      			if !truthy(a["exp"]) {
      				facts += "<span>No end date</span>"
      			} else if t, err := strconv.ParseFloat(strings.TrimSpace(pyStr(a["exp"])), 64); err == nil {
      				facts += "<span>Expires <b>" + time.Unix(int64(t), 0).UTC().Format("2006-01-02") + "</b></span>"
      			}
      			if a["max"] != nil {
      				used := a["used"]
      				if used == nil {
      					used = "?"
      				}
      				facts += fmt.Sprintf("<span>In use <b>%s of %s</b></span>", esc(used), esc(a["max"]))
      			}
      			switch age := a["age"].(float64); {
      			case age < 5:
      				facts += "<span>Checked <b>just now</b></span>"
      			case age < 120:
      				facts += fmt.Sprintf("<span>Checked <b>%d s ago</b></span>", int(age))
      			default:
      				facts += fmt.Sprintf("<span>Checked <b>%d min ago</b></span>", int(age/60))
      			}
      		} else if a != nil && truthy(a["answer"]) {
      			pill = fmt.Sprintf(`<span class="pill bad">the provider answered %s</span>`, esc(a["answer"]))
      		}
      		add(`<div class="provider"><div class="top"><span class="name">%s</span>%s<span class="pill">%s</span></div><div>Type this into your player: <code>http://%s:%s</code></div><div class="facts">%s</div></div>`,
      			esc(field(p, "name")), pill, map[bool]string{true: "MPEG-TS", false: "HLS"}[field(p, "mode") == "ts"], esc(host), esc(field(p, "port")), facts)
      	}
      	if len(good) > 0 {
      		add(`<p class="note">In your player, add an Xtream login with that address and the easy username and password from your settings file (<code>PLAYER_USER</code> and <code>PLAYER_PASSWORD</code>).</p>`)
      	}
      	add(`</div><div class="card"><h2>The Live TV setting</h2>Each provider has one line in your settings file that says how your players watch its live channels.<ul><li><code>NAME_LIVE_TV=hls</code> Your players use <b>HLS</b>. Everyone on a channel shares one connection to the provider, so you can watch and record the same channel at once. A recorder that asks for MPEG-TS is given the same video as your players.</li><li><code>NAME_LIVE_TV=ts</code> Your players use <b>MPEG-TS</b>. You get the provider&#39;s own stream, and everyone on a channel shares one connection to the provider. A player that asks for HLS instead is given HLS built from that same stream, so the provider still sees one connection.</li></ul><p class="note">Pick the one that matches the live format chosen in your players and in your playlist manager if you use one. They have to agree. If a provider only offers one of the two, use that one.</p></div>`)

      	add(`<div class="card"><h2>Playing right now</h2>%s<p class="note">Live channels only. Movies, series and catch-up are not counted.</p></div>`, playing(provName))
      	add(`<div class="card"><h2>Still stuck?</h2>Each of the three containers keeps a log. Run the command on the machine this stack runs on.<ul><li><code>docker logs iptv-caddy</code><br>The proxy. At the top it lists every provider it found, and any it left out and why. After that, one line for every request.</li><li><code>docker logs iptv-relay</code><br>The relay. It says who is watching what, when when a live channel is opened and closed, how long the provider took to send the first video, and anything that went wrong with a channel.</li><li><code>docker logs iptv-vpn</code><br>The VPN. It says whether the tunnel is up and where it comes out.</li><li>The instructions: <a href="https://github.com/mackid1993/iptv-vpn-proxy">github.com/mackid1993/iptv-vpn-proxy</a></li></ul></div><footer>IPTV VPN proxy</footer></main></body></html>`)
      	w.Header().Set("Cache-Control", "no-store")
      	answer(w, map[bool]int{true: 200, false: 503}[working], "text/html; charset=utf-8", o.String())
      }

      // playing: the Python summary line, then one table row per live reader and per HLS player.
      func playing(provName map[string]string) string {
      	type row struct{ who, id, port, provider, kind, since, data string }
      	var rows []row
      	mu.Lock()
      	now := mono()
      	for k, t := range hlsSeen {
      		if now-t > 30 {
      			delete(hlsSeen, k)
      		}
      	}
      	hls, ts, readers, recorders := len(hlsSeen), 0, 0, 0
      	for _, c := range channels {
      		if strings.HasPrefix(c.key, "/ts ") {
      			ts, readers = ts+1, readers+len(c.readers)
      		} else {
      			recorders += len(c.readers)
      		}
      		for rd := range c.readers {
      			if rd.kind == "HLS built from MPEG-TS" { // (its players have rows of their own, below)
      				continue
      			}
      			rows = append(rows, row{rd.who, rd.id, rd.port, c.provider, rd.kind, hms(now - rd.came), fmt.Sprintf("%.1f MB", float64(rd.written.Load())/1048576)})
      		}
      	}
      	for _, h := range hlsRows {
      		if now-h.last <= 30 {
      			rows = append(rows, row{h.who, h.id, h.port, h.provider, "HLS", hms(h.last - h.first), "-"})
      		}
      	}
      	for i, x := range rows { // channel names, provider NAMEs
      		if n := channelNames[x.port]; n != nil && n.names[x.id] != "" {
      			rows[i].id = n.names[x.id]
      		}
      		if name := provName[x.port]; name != "" {
      			rows[i].provider = name
      		} else {
      			rows[i].provider = hostOf(x.provider)
      		}
      	}
      	mu.Unlock()
      	var lines []string
      	if hls > 0 {
      		lines = append(lines, fmt.Sprintf("<b>%d</b> channel%s as HLS", hls, plural(hls, "", "s")))
      	}
      	if ts > 0 {
      		lines = append(lines, fmt.Sprintf("<b>%d</b> channel%s as MPEG-TS, shared by <b>%d</b> reader%s", ts, plural(ts, "", "s"), readers, plural(readers, "", "s")))
      	}
      	if recorders > 0 {
      		lines = append(lines, fmt.Sprintf("<b>%d</b> MPEG-TS reader%s being fed from HLS", recorders, plural(recorders, "", "s")))
      	}
      	if len(lines) == 0 && len(rows) == 0 {
      		return "Nothing."
      	}
      	sort.Slice(rows, func(i, j int) bool { return rows[i].who+rows[i].id < rows[j].who+rows[j].id })
      	cell := `<td style="padding:6px 10px;border-top:1px solid var(--line)">`
      	t := `<table style="width:100%;border-collapse:collapse;margin-top:12px;font-size:15px"><tr style="color:var(--soft);text-align:left">` +
      		`<th style="padding:6px 10px">Device</th><th style="padding:6px 10px">Channel</th><th style="padding:6px 10px">Provider</th>` +
      		`<th style="padding:6px 10px">Format</th><th style="padding:6px 10px">Watching for</th><th style="padding:6px 10px">Data sent</th></tr>`
      	for _, x := range rows {
      		t += "<tr>"
      		for _, v := range []string{x.who, x.id, x.provider, x.kind, x.since, x.data} {
      			t += cell + esc(v) + "</td>"
      		}
      		t += "</tr>"
      	}
      	return strings.Join(lines, "<br>") + t + "</table>"
      }

  relay-gomod:
    content: |
      module relay

      go 1.22
```

</details>

## Everyday use

**Add a provider.** Add its five lines to `.env`, using a name of your choice
in capitals and a free port inside `PROXY_PORTS`:

```
SPORTS_URL=https://provider.example
SPORTS_PORT=8096
SPORTS_USER=the-username-they-gave-you
SPORTS_PASSWORD='the-p@ssword-they-gave-you'
SPORTS_LIVE_TV=hls
```

Then run `docker compose up -d`. Only the proxy and the relay restart; the VPN
stays up.

**Pick the live format.** `NAME_LIVE_TV` says how your players watch that
provider's live channels. There is no default, and a provider without the
line is left out.

- `hls`: your players use HLS. Everyone watching a channel shares one
  connection to the provider. A recorder that asks for MPEG-TS is given the
  same video as your players, so you can watch and record a channel at once.
- `ts`: your players use MPEG-TS. Everyone on a channel shares one connection
  to the provider and gets the provider's own stream, so you can watch and
  record a channel at once. A device that asks for HLS instead is given HLS
  built from that same stream, so it shares the connection too.

Use the format chosen in your players, and in your playlist manager if you
have one. They all have to agree. If a provider offers only one of the two,
use that one. If it offers both and you have no reason to prefer one, use
`hls` (see [Trade-offs and things to know](#trade-offs-and-things-to-know)).

**Remove a provider.** Delete its five lines and run `docker compose up -d`.

**Logins with odd characters.** Provider logins can contain almost anything.
You do not get to choose them, so the proxy takes whatever you were given,
with one exception: a slash (`/`). Caddy cannot pass a slash on inside an
address, so a provider whose username or password contains one is skipped at
start, and the log and the status page say why. One rule for the `.env` file:
if a username or password has anything besides letters and digits, put it
between single quotes. Without them, a `$` or `#` is read as something else
and part of the login is silently lost.

**More providers than ports.** Widen `PROXY_PORTS` (for example `8095-8099`)
and run `docker compose up -d`. This one restarts the VPN too.

**See what is happening.** The status page
(`http://<the machine's address>:<any provider's port>/status`) shows what is
set up, where the VPN comes out, each provider's account, and how many live
channels are playing. For more detail, each container has a log:

- `docker logs iptv-caddy` has the providers found at the last start, then one
  line per request, with the time first.
- `docker logs iptv-relay` has a line each time a live MPEG-TS channel
  (shared, or fed from HLS) is opened or closed, how long the provider took to
  send the first video, a line each time a live HLS channel is opened at the
  provider, and anything that went wrong with a channel.
- `docker logs iptv-vpn` is the VPN tunnel's own log.

Addresses are never logged by Caddy or the relay, because they contain logins.

## With a playlist manager

A playlist manager is the tool some people use to trim, rename and reorder
what a provider offers. The proxy works with one, and where the proxy goes
depends on where the manager runs.

**A manager you run yourself, on your own network**

```
player  ->  playlist manager  ->  proxy  ->  provider
```

- In the manager, each provider source uses the proxy's address and port,
  with your easy login.
- Your players point at the manager, as they would without the proxy.
- The provider's real login lives only in the proxy's `.env`.
- The live format has to agree everywhere: the one chosen in your players,
  the one the manager hands out, and the provider's `NAME_LIVE_TV` line.

**A hosted manager, running on someone else's servers**

```
player  ->  proxy  ->  playlist manager  ->  provider
```

A hosted manager cannot reach a proxy inside your house, so the proxy goes in
front of it and treats the manager as the provider.

- In `.env`, add the manager as a provider: its server address as the URL,
  and your login for the manager as the username and password.
- Your players point at the proxy, with your easy login.
- When something is played, the manager answers "go to the provider's
  server". The proxy rewrites that answer as it does any redirect, so the
  video goes out through the VPN.
- The hosted manager itself holds your provider's real login and fetches the
  provider's lists from its own servers. That part does not go through your
  VPN; only what your players fetch does.

The first arrangement is the one this was built and tested with. The second
is the same mechanism but has not been run on this version. In particular,
live HLS through a hosted manager adds one more redirect on the way to the
video. The relay remembers the first redirect a channel is given, and whether
a channel's session is still shared when a second one follows has not been
tested.

In either arrangement, see the note on pictures under
[Trade-offs and things to know](#trade-offs-and-things-to-know).

## How it works

Most of what the proxy does is pass bytes along. Eight things go beyond that.
The first seven exist to keep the behavior the same as a direct connection,
and the last is there to help you. Engineers will find the details under
[How it works (technical)](#how-it-works-technical).

1. **Redirects stay inside the tunnel.** Providers answer most requests with
   "go to that other server". The proxy rewrites that answer so the player
   comes back to the proxy, which then fetches from the other server through
   the VPN. Without this the player would go to that server directly.
2. **Playlists and picture addresses are pointed back at the proxy.** An HLS
   playlist is a list of video pieces, and a provider's channel lists name a
   logo or poster for each entry. The relay rewrites those addresses for the
   same reason.
3. **One session per live HLS channel.** Described [above](#why-this-exists).
   The relay remembers where the provider sent each channel. When the
   provider ends a session, the relay opens a new one once and carries on.
4. **One easy login.** You choose a simple username and password for your
   players. The proxy swaps it for the provider's real login on the way out,
   so the real logins are typed in one place only.
5. **It refuses to be pointed at your own network.** A request cannot make the
   proxy fetch from the machine it runs on or from private addresses.
6. **Connections are kept open and reused.** A player connected directly keeps
   its connection to a server open and sends one request after another over
   it. The proxy does the same. It leaves the player's connection open for
   the next request, and Caddy keeps its own connections to the provider's
   servers open, so a video piece or the next part of a movie does not wait
   for a new connection to be set up. Setting up an encrypted connection
   takes time, and one slow setup can use up a player's small live buffer and
   show as a stutter. Playlist refreshes ride on the same kept-open
   connections: the relay fetches each one through Caddy. A connection that
   has been idle for 20 seconds is let go.
7. **One provider connection per live channel for MPEG-TS too.** This is the
   relay's other main job. With `NAME_LIVE_TV=ts`, the first reader of a
   channel makes the relay open it at the provider, and everyone else on that
   channel is given the same video, also a device that asks for HLS (it gets
   HLS built from that stream). With `NAME_LIVE_TV=hls`, a reader that
   asks for MPEG-TS (many recorders do) is given the channel's HLS video
   joined end to end, so it shares the session your players are on.
8. **A status page.** The relay draws a page that shows what is set up, what
   is wrong, where the VPN comes out and what is playing.

Your settings are read once, when the proxy starts. A short shell loop turns
each provider block in `.env` into its own Caddy block, listening on its own
port.

No video server is named anywhere in the two files, because a provider can
move to other servers at any time. Caddy needs no list of them: it keeps
connections open to whichever servers it fetches from, by itself.

The relay is now in the path of live TV, playlists and the provider's lists.
If it is not running, those do not work until it is back. Movies, series,
catch-up and the guide do not go through it. Most players need the provider's
lists to log in, so in practice expect nothing to play until the relay is
running again. Docker starts it again if it stops.

## What was tested, and how

The goal is parity with direct credentials, and no gap has been seen so far
on the real setup it was run on. Some of that was confirmed against real
providers and some only against simulated ones, so here is which is which.

**Real providers (November 21, 2026).** Run on one real setup, against
two real providers through the VPN, and against a simulated provider. Caddy
was at version 2.11.9 at the time.
Against the real providers:

- The account check.
- Channel categories and channel lists (40,036 and 25,215 channels).
- Every logo pointed back at the proxy (33,668 and 25,215), and one logo
  fetched through it.
- Lists compressed with gzip when the player asked.
- The movie list (274,171 movies, 89 MB) and the series list (39,099).
- A movie and a series episode, with skipping, including a jump to the
  middle.
- Live MPEG-TS on both providers, 25 seconds each, at 720p 30 frames a second
  and 1080p 59.94: no gap between frames longer than 100 ms.
- Live HLS on both providers, the same, with no gaps. Each channel was opened
  at the provider once while its playlist was refreshed many times.
- After that it was put to use on the real setup, and a recorder reconnected
  through it by itself. After the fixes from an independent code review it
  was put to use again and checked once more through the ports players use:
  live HLS and MPEG-TS on both providers, 15 seconds each, no gaps, with the
  playlist refreshes going through Caddy's kept-open connections.

The checks above were made with the Python relay. The Go relay that replaced
it was checked against a simulated provider (the list below, plus a TS and
an HLS device sharing one channel through one provider connection, devices
changing channel on a one-connection account, a player that stops reading,
and a restarted tunnel), reviewed independently twice, and then put to use
on the real setup.

Against a simulated provider:

- The login swap, in the address and in the query.
- Redirects rewritten, including a redirect to a server with no path.
- Playlist addresses in every form rewritten: a bare file name, `../`,
  `/path`, `//server/path`, a full address and a `URI="..."` key.
- Session memory: a second open of a channel used the remembered session,
  and the provider was asked once.
- A piece named next to a channel's playlist, inside the channel's folder.
- Picture addresses in the provider's lists in both JSON forms, with gzip.
- One live MPEG-TS channel shared by two readers.
- Refusal of `127.0.0.1`, `192.168.x`, `10.x`, an address with `user@` in
  front of the server, `get.php`, and a `POST`, and of a playlist on a
  server whose name leads to this machine (answered 403).
- A provider that answers a channel with the playlist itself: the playlist's
  own pieces come back on the address the player used, with the easy login,
  so the real login is never written into it.
- The provider sees no `Via` and no `X-Forwarded-For`, also on the requests
  the relay makes for live channels and lists.
- Caddy's environment after start: the providers' logins and its own
  settings, and no VPN key.
- Provider logins with `! space + @ # & = % ? ; \ " ' { } $ ,` in them,
  in the address and in the query: the provider received each one exactly.
  A login with a slash (`/`) is split by Caddy, which is why such a provider
  is now skipped at start.
- No login in any log line, including an error.
- An adversarial run with a hostile player and a hostile provider: a private
  or loopback address in 20 disguises (leading zeros, short forms, IPv6,
  `user@`, capitals, an encoded colon, `0.0.0.0`, the cloud metadata
  address, one long decimal, hex, and a name that leads to `127.0.0.1`),
  the relay's own notes forged by a player, request smuggling, `TRACE` and
  `CONNECT`, a 100 KB address and a 2 MB header, and a provider sending a
  poisoned playlist, a redirect to this machine, endless redirects, a header
  pasted into a redirect and lists full of private addresses. Every one was
  refused or ignored, a stand-in for a control page inside the stack was
  never reached, no secret reached a log, and normal playback worked
  afterwards.

**Not run against real providers:**

- The guide (`xmltv.php`) against the real providers. It was run only against
  the simulated one, including a redirect to a server with no path.
- Catch-up and time shift against the real providers.
- The HLS-to-MPEG-TS bridge (`/hls`). It was run against the simulated
  provider in both orders (an HLS player first, then an MPEG-TS reader, and
  the other way round), with the provider seeing one session each time.
- A hosted playlist manager in front (see
  [With a playlist manager](#with-a-playlist-manager)).
- The [platform notes](#platform-notes) other than plain Linux with Docker
  Compose. They are written from each platform's documentation.

## Trade-offs and things to know

- **Xtream logins only.** A provider's ready-made M3U link (`get.php`) is
  refused, because the addresses inside it would skip the VPN. If an M3U link
  is all you were given, the Xtream login is inside it:
  `http://SERVER/get.php?username=USERNAME&password=PASSWORD&...`
- **Use the address you typed.** When a player logs in, the provider's reply
  includes the provider's own server address. The proxy passes that reply on
  unchanged. A player that connected to that address, instead of the proxy
  address you typed in, would reach the provider directly and skip the VPN.
- **Logos and posters go through the VPN too, with one exception.** A
  provider's channel and movie lists give each picture as a full web address,
  usually on some other site. The proxy points those addresses back at
  itself, so the player fetches the pictures through the tunnel. The
  exception is pictures named inside the guide file (`xmltv.php`): that file
  is passed on as is, so a player that shows guide artwork fetches it
  directly. They are only pictures; no video comes that way.
- **If a playlist manager sits between your player and the proxy,** the
  pictures are its business, not the proxy's. A playlist manager hands the
  player its own list, often with logos from its own picture library and with
  addresses it saved earlier. The proxy never sees those, so they are fetched
  directly. Only a player that talks to the proxy itself gets every picture
  through the VPN.
- **Pick one live format per provider and use it everywhere.** A device on
  the other format still works and shares the channel's one connection, but
  each player plays best in the format it was set up for.
- **If a provider offers both formats, HLS is the recommended setting.**
  MPEG-TS is one continuous stream, so how smoothly it plays depends on how
  evenly the provider sends video. The relay passes it on as it arrives and
  cannot smooth out a provider that sends in fits and starts.
- **A device that joins a channel is a little behind live.** With `ts`, a
  device that joins a channel someone else is already on starts about 10
  seconds behind. With `hls`, a recorder that asks for MPEG-TS gets the HLS
  video, which is a few seconds further behind live than the provider's own
  MPEG-TS.
- **The status page asks two outside parties questions.** To show where the
  VPN comes out it asks `ipinfo.io`, through the VPN. To show each account's
  state it asks the provider, through the proxy, at most once every 15
  seconds and only while someone has the page open. The page shows no logins, but like
  the proxy ports it is open to anyone on your home network.
- **If the relay stops, playback stops with it.** Live channels, playlists
  and the provider's lists all go through the relay, and Caddy asks the
  relay where a server's name leads before it fetches from any server the
  provider sent a player to (video pieces, a movie after the provider's
  redirect, pictures). There is no fallback for any of those. Docker starts
  the relay again if it stops.
- **Restarting the relay opens playing HLS channels afresh.** The session
  memory is kept in the relay's memory, not on disk, so a channel that is
  playing when the relay restarts is opened at the provider again. That can
  show as a short buffering.
- **Your logins are in `.env` in plain text.** Keep the folder private. The
  session memory holds channel addresses with your real login in them too,
  but only in the relay's memory while a channel is in use; nothing with a
  login is written to disk.
- **A slow provider is still slow.** If a provider's server falls behind, the
  proxy cannot make up the difference. See
  [When something is wrong](#when-something-is-wrong) for how to tell that
  apart from a proxy problem.
- **Changing a VPN setting interrupts playback** for a few seconds while the
  tunnel reconnects. Changing a provider does not touch the VPN.

## What it stores, updating, removing

**What it stores.** Nothing of its own besides the two files you put there.
The Caddy image declares two volumes of its own (`/data` and `/config`), so
Docker creates two unnamed volumes for the `iptv-caddy` container. This stack
keeps nothing it needs in them.

The relay stores nothing on disk. It keeps the newest video of each open
live channel in memory (about 15 seconds, between 12 MB and 64 MB a channel),
and the session memory for live HLS channels, and is capped at 512 MB in all.

**Updating.**

- You changed `.env`: run `docker compose up -d`.
- You replaced `compose.yaml` with a newer version: run
  `docker compose up -d --force-recreate`. A plain `up -d` can leave the proxy
  running its old configuration.
- New versions of gluetun, Caddy or Go: `docker compose pull`, then
  `docker compose up -d`. The images are not pinned to a version on purpose: a
  VPN client and a proxy should get their security fixes.
- Coming from the nginx version: replace `compose.yaml` and run
  `docker compose up -d --remove-orphans --force-recreate`.
  `--remove-orphans` removes the old `iptv-nginx` container. `.env` stays as
  it is. The two volumes the nginx version used, `<folder>_hls-memory` and
  `<folder>_video-servers`, are no longer used; remove them when you like
  with `docker volume rm <folder>_hls-memory <folder>_video-servers`.
- Coming from the two-container version: add a `NAME_LIVE_TV=hls` or
  `NAME_LIVE_TV=ts` line to every provider in `.env` (a provider without it
  is skipped), then do as for the nginx version above.

**Removing.**

```
docker compose down -v
```

The `-v` matters: it removes the Caddy container's two unnamed volumes too.
A plain `down` leaves them behind. Then delete the folder.

## Extras that need one more file

Two less common needs are met by a second file next to `compose.yaml`, named
`compose.override.yaml`. Docker Compose reads it automatically, so
`compose.yaml` still stays untouched.

**A VPN set up from an .ovpn file.** Put the file next to `compose.yaml`, name
it `my-vpn.ovpn`, and create `compose.override.yaml` with:

```yaml
services:
  vpn:
    volumes:
      - ./my-vpn.ovpn:/gluetun/custom.ovpn:ro
```

Then in `.env` use "Way 4" with `OPENVPN_CUSTOM_CONFIG=/gluetun/custom.ovpn`.

**A gluetun setting that is not in `.env.example`.** Add it the same way:

```yaml
services:
  vpn:
    environment:
      - SOME_GLUETUN_SETTING=value
```

## Platform notes

This is plain Docker Compose: two files in a folder and
`docker compose up -d`. That is how it was built and tested. The files are the
same everywhere; what differs is where they live and how you start them.

| Platform | Where to put the folder | How to start | Notes |
|---|---|---|---|
| Linux with Docker | anywhere, for example `/opt/iptv-vpn` | `docker compose up -d` | The reference setup. |
| Synology (Container Manager) | `/volume1/docker/iptv-vpn` | Project > Create, point it at the folder | Some models have no WireGuard in the kernel: set `WIREGUARD_IMPLEMENTATION=userspace`. |
| Proxmox | inside a VM running Docker | `docker compose up -d` | A VM is the simple route. In an LXC container you must pass `/dev/net/tun` through and allow nesting. |
| Windows (Docker Desktop with WSL 2) | a folder inside your WSL distribution | `docker compose up -d` | Use `WIREGUARD_IMPLEMENTATION=userspace`. Other devices reach it at the Windows machine's address; allow the ports in Windows Firewall. |
| Portainer | Stacks > Add stack, paste `compose.yaml` into the web editor | Deploy the stack | Under "Environment variables", use "Load variables from .env file" and pick your filled-in `.env`. |

## When something is wrong

- **Start with the status page.** Open
  `http://<the machine's address>:<any provider's port>/status` in a web
  browser. It says whether the proxy is working, names every provider that
  was left out and the line to fix, shows where the VPN comes out, and shows
  whether each provider accepted its login. If no provider could be started
  at all, the first port of `PROXY_PORTS` still answers so the page can tell
  you what is missing.
- **The status page does not open, with an error 502.** The relay is not
  answering. Look at `docker logs iptv-relay`. Live TV and the provider's
  lists do not work until it is back.
- **A provider does not answer.** Read the `iptv-caddy` log from its last
  start. Its first lines are plain text: they name each provider it found and
  each one it skipped, with the reason. A provider with no `NAME_LIVE_TV`
  line is skipped.
- **Nothing answers.** Check that `iptv-vpn` is healthy. If the tunnel is
  down the proxy has no way out, by design.
- **A login with odd characters is refused.** Put it between single quotes in
  `.env`.
- **One channel buffers and the others are fine.** That is almost always the
  provider's server for that channel, not the proxy. In the `iptv-caddy` log,
  each line shows how long the request took (`duration`) and what kind of
  request it was (`k`). Slow video pieces (`"k":"cdn"`) on one channel, while
  other channels are quick, point at that channel's server. The same channel
  will be just as slow with the provider's login typed straight into a
  player.
- **Every channel buffers, on every device.** Suspect the VPN server before
  the provider. A VPN server you pick by host name can turn out to be a
  far-away entry point even when its exit shows a nearby city. A slow or
  lossy VPN server causes buffering on every device at once. Check the
  round-trip time from the machine that runs the stack to the VPN server it
  connected to (with `ping`, for example), and look for lost packets. If the
  time is high or packets go missing, choose another server.
- **Two devices cut each other off on one channel.** Check that the
  provider's `NAME_LIVE_TV` matches the live format your players really use.
  Devices on the proxy share one connection per channel in either format; a
  device that skips the proxy and goes to the provider directly is a second
  connection, which the provider may refuse.
- **Reading the Caddy log.** After the start lines, Caddy writes one line per
  request when the request ends (a live stream: when it is stopped), with
  the date and time in your time zone first and the details after it. A
  line looks like this:
  ```
  2026/11/20 23:31:05.123  INFO  http.log.access.log0  handled request  {"request": {"remote_ip": "192.168.1.60", "method": "GET"}, "duration": 0.08, "size": 1880000, "status": 200, "wait_ms": 41.2, "server": "video.provider.example", "k": "cdn", "provider": "PROVIDER1"}
  ```
  The fields that matter:
  - `provider` is the provider's `NAME` from `.env`, which tells you the port
    the request came in on.
  - `k` is the kind of request: `media` (live MPEG-TS, movies, series,
    catch-up and the account), `hls` (a playlist), `api` (the provider's
    lists, `player_api.php`), `cdn` (anything fetched from a server the
    provider sent the player to, such as video pieces and pictures) or
    `guide`.
  - `status` is the answer's status code, and `size` how many bytes were
    sent.
  - `duration` is how long the request took, in seconds. A live MPEG-TS
    stream that the player stops (changing channel, turning off) is logged
    with `duration` 0 and without `provider` and `k`; the relay's log line
    for it gives how long it was watched.
  - `request.remote_ip` is the device that asked. `127.0.0.1` is the stack
    itself: the relay fetching a playlist or reading a channel for a
    recorder, or the status page checking an account.
  - `server` (on `cdn` lines) is the server that answered, and `wait_ms`
    (on lines that went to a provider's server) is how many milliseconds it
    took to start answering. Slow lines that all name one server, while
    other channels are quick, point at that server.

  The address asked for and the request's headers are left out of every
  line, because Xtream addresses contain the login. Caddy's own messages, in
  the same log, are only about real errors (a server that could not be
  reached, for example), and leave them out too. A player hanging up in the
  middle of a stream is normal and is not reported.
- **Reading the relay log.** Every line starts with `RELAY:`. It says when a
  channel was opened and closed and how many are open, how long the provider
  took to send the first video, when a reader joined or left and why, when a
  provider sent nothing for more than a few seconds and then carried on, and
  when a provider ended a channel that was being watched and the relay asked
  for it again. A live HLS channel leaves a line when the relay opens it at
  the provider, and another when it opens it again because the remembered
  session had gone. The playlist refreshes themselves are in the Caddy log
  (`"k":"hls"`).

## How it works (technical)

This part is for engineers. Everything in it is taken from `compose.yaml`,
which holds Caddy's settings, the block written for each provider, the start
script and the relay's source, each with comments. Where this page and the
file disagree, the file is right.

### Three containers, one network namespace

| Service | Container | Image | Job |
|---|---|---|---|
| `vpn` | `iptv-vpn` | `qmcgaw/gluetun` | Holds the tunnel and publishes the `PROXY_PORTS` range. |
| `caddy` | `iptv-caddy` | `caddy:alpine` | Every player connection. One listener per provider. The login swap, redirects, and all proxying that is not live TV, playlists or the provider's lists. |
| `relay` | `iptv-relay` | `golang:alpine` | Long-lived live streams, HLS built from MPEG-TS for `ts` providers, HLS session memory and playlist rewriting, picture addresses in the provider's lists, the status page. Built from its inline source at every start. |

- `caddy` and `relay` both run with `network_mode: "service:vpn"`. They have no
  network of their own: they share the gluetun container's interfaces,
  firewall and loopback. Anything they fetch leaves through the tunnel, and if
  the tunnel is down they have no other way out.
- Because the loopback is shared, the pieces talk to each other on
  `127.0.0.1` with no Docker network between them. Those connections never
  reach a network card or the tunnel, so opening one costs a few
  microseconds. That is why the relay closes its connection from Caddy after
  each answer: it keeps the relay simple, and keeping them open would save
  nothing measurable. (A Unix socket was ruled out for the same reason, and
  because socket files in a shared folder do not work on every Docker setup.)
  Caddy reaches the relay on `127.0.0.1:8197`, the relay reaches Caddy on a
  provider's own port, and Caddy answers its health check on
  `127.0.0.1:8195`. gluetun's control server is held to `127.0.0.1:8000`.
  None of these is published.
- The only published ports are the `PROXY_PORTS` range, on the `vpn` service.
  A provider port may not be 53, 8000, 8195, 8197 or 9999, which are in use
  inside the namespace.
- `caddy` and `relay` each wait for `vpn` to be healthy and are restarted with
  it. Each one's health check also looks for a global-scope address. If the
  tunnel container was recreated underneath it, the container is left in a
  dead namespace; the check then waits 12 seconds and stops the main process,
  so Docker starts the container again inside the tunnel's new network.
- `vpn` is given a fixed list of gluetun settings by name. `caddy` and `relay`
  read the whole settings file (`.env`, or `stack.env` under Portainer). The
  relay's start command strips the environment down before the relay runs,
  to `PATH`, a memory target (`GOMEMLIMIT`) and a list of provider addresses. Caddy's start script does the same
  before Caddy runs: what is left is `PATH`, `TZ`, Caddy's own folders and the
  providers' logins (each provider's block reads its login from there when a
  request comes in). The VPN keys and the rest of `.env` are not in Caddy's
  environment.

### What Caddy does

**At start.** The start script finds every `NAME` that has `NAME_URL` in the
environment and checks it: all five lines present, a port inside
`PROXY_PORTS` that is not reserved and not used twice, an address of the form
`http(s)://name[:port]`, `NAME_LIVE_TV` equal to `hls` or `ts`, and no slash
in the login (Caddy would split it). For each
good one it fills in the provider block with the listen port, the provider
address and the live format, and puts the provider's login, percent-encoded,
in a variable of its own. For each bad one it prints `PROVIDER NAME SKIPPED:`
with the reason. The same findings are written to a small JSON file that
Caddy serves to loopback callers only, which is where the status page gets
them. If no provider could be started, one listener on the first port of the
range still serves `/status` (and answers everything else with 503), and the
container reports itself unhealthy. Then the script starts Caddy.

**Settings.** Caddy's admin interface is off, so nothing can change its
settings from outside; they come from the file only. Automatic HTTPS is off:
players talk to it over plain HTTP/1.1, and it reaches each provider over
whatever the provider's address says.

**Proxying.** Bytes are passed on as they arrive (`flush_interval -1`), so
live video does not wait in a buffer. Toward a provider and the servers it
sends players to, Caddy allows 60 seconds without data (300 seconds for the
guide). Nothing about the player or the proxy is added to the request: the
`X-Forwarded-*` and `Via` headers are removed, so the provider sees the
player's own request, from the VPN. Toward the player, replies carry no
`Via` line either, and a live MPEG-TS stream is sent the way the provider
sends it: not cut into chunks, and ended by closing the connection.
Compression is off in Caddy's transports,
so the player's own `Accept-Encoding` goes through as it is and nothing is
added or unpacked on the way. Upstream certificates are not verified
(`tls_insecure_skip_verify`): many players do not check them either, and
some providers' servers carry expired or self-signed ones.

**Connection reuse.** Caddy keeps its connections to every server it fetches
from open and reuses them. This is built in and needs no settings: no server
is named in the file, nothing is learned, and nothing is reloaded.

**Where each request goes.** In this order:

| Request | Where it goes |
|---|---|
| `/get.php` | Refused with 403. |
| `/status` | The relay (`/status`). |
| A live `.ts` made with the easy login | The relay: `/ts` or `/hls`, according to that provider's `NAME_LIVE_TV`. |
| An address ending in `.m3u8`, or under `/__ch/` | The relay (`/hl`). |
| `/player_api.php` | The relay (`/api`). |
| `/__cdn/<http or https>/<server>/<path>` | That server, if it is allowed (see below). |
| `/xmltv.php` | The provider. |
| Everything else (movies, series, catch-up, the account) | The provider. |

**Redirects and addresses.** A `Location` header in a provider's answer is
rewritten to `/__cdn/<scheme>/<server>/<path>` on the proxy, in each of its
three forms: a full address, `//server/path`, and `/path` (which gets the
server that answered). So the player comes back and the fetch goes through
the tunnel. A `/__cdn/` target may not be `localhost`, a loopback, private,
link-local or carrier-grade NAT address, a bracketed IPv6 literal, a host
containing `@` or `%`, or a numeric address written with leading zeros; such
a request is refused with 403. Caddy cannot look a name up before it
connects, so for every `/__cdn/` request it first asks the relay
(`forward_auth` to `/check`): the relay looks the server's name up through
the tunnel's DNS and answers 403 if it leads to this machine, a home or
private network or a link-local address (a name such as `localtest.me`,
which leads to `127.0.0.1`, or a number written as one long decimal).
Answers are kept for a minute, so a channel's pieces cost one lookup a
minute. A name that changes its answer between that lookup and Caddy's own
is not covered. `/__cdn/` takes `GET` and `HEAD` only (405 for
anything else).

**The login swap.** A request that carries `PLAYER_USER` and
`PLAYER_PASSWORD` is sent upstream with the provider login of the port it
arrived on. That covers the path forms `/live/`, `/movie/`, `/series/` and
`/timeshift/` followed by the login, the short form `/user/pass/<id>`, and
`username=` and `password=` in the query string. Nothing else in the address
is changed. Any other login is passed on untouched and refused by the
provider.

**Logging.** One line per request on standard output, in Caddy's console
format with the local time first. Caddy's own fields are trimmed to the
player's address, method, status, size and duration, and the provider's
`NAME`, the kind of request (`k`), and for requests to a provider's servers
the server (`server`) and the time to the first byte from it (`wait_ms`) are
added. The request address, the request headers and the response headers
are removed from every line, because Xtream addresses contain the login.
Caddy's own messages are limited to errors (a player hanging up mid-stream
is logged by Caddy as a warning and so does not appear), and the address
and headers are removed from those too.

**What it hands to the relay.** Four kinds of request are proxied to
`127.0.0.1:8197`, as listed above. Caddy tells the relay what it needs in
`X-Relay-*` request headers: the provider's address, the address at the
provider with the login already swapped in, the address as the player sent
it, the port, and for the provider's lists the address the player used to
reach the proxy. For every header the relay reads on a request, Caddy sets
the value itself and replaces any value a player sent, so a player cannot
steer the relay with them.

### What the relay does

The relay is one Go program, standard library only, built from its source in
the compose file each time the container starts. It listens on
`127.0.0.1:8197`, so Caddy is
its only caller. It has six endpoints that matter, plus `/healthz` for the
container's health check and `/stat`, a one-line count of open channels and
readers.

| Endpoint | Job |
|---|---|
| `/ts` | Share one provider MPEG-TS stream between every reader of a channel. |
| `/hls` | Give a reader that asks for MPEG-TS the channel's HLS video, joined end to end. |
| `/hl` | Keep one provider session per live HLS channel, and point the addresses inside an HLS playlist back at the proxy. For a `ts` provider, build a live channel's HLS from its shared MPEG-TS stream instead. |
| `/api` | Point the picture addresses in the provider's lists (`player_api.php`) back at the proxy. |
| `/check` | Say whether a server named in a `/__cdn/` address leads to this machine or a home network, before Caddy connects to it. |
| `/status` | Draw the status page. |

For `/ts`, `/hls`, `/hl` and `/api`, the relay answers 403 unless the
provider Caddy names is one of the addresses it was started with. For `/ts`
and `/hls`, a `HEAD` request is answered with 200 on the spot, because asking
the provider would open a connection. One channel has one key whichever form
of its address a player uses (`/live/user/pass/1.ts`, `/user/pass/1.ts`,
`/user/pass/1`). The relay serves at most 100 readers at once, all channels
together.

### MPEG-TS sharing (`/ts`)

- **One upstream per channel.** The first reader of a channel makes the relay
  open it at the provider, with that player's own request headers. Every
  later reader of the same channel is attached to the same upstream. The
  provider sees one connection however many readers there are, and one
  reader leaving does not disturb the rest.
- **A ring of recent video.** For each open channel the relay keeps the
  newest video in memory: about 15 seconds of it, never less than 12 MB and
  never more than 64 MB.
- **Where a joiner starts.** The reader that opens a channel gets the stream
  from its first byte. A reader that joins later starts with the newest 10
  seconds of video, handed on at playing speed after the first 2 seconds (kept
  in the relay, not dumped on the player), so it starts about 10 seconds
  behind live and has something in hand when the provider pauses, as a player
  on its own connection has from the provider's opening burst. On a channel
  slow enough that the first part of the cushion holds more than 2 seconds of
  video, that part goes at once and the pace makes up for it after. The
  amount is counted in
  seconds, not bytes: the relay measures each channel's real rate over the
  last minute and over the last 15 seconds, takes the larger, and multiplies.
  It never hands over more than the last 20 seconds of arrivals. In a channel's first 8 seconds there is no rate
  yet, and a joiner gets everything kept so far. The joiner's first byte is
  placed on the program table (PAT) just before the first video keyframe, so
  its player can start decoding at once. A stream that marks no keyframes
  starts on the first program table, or else on the first whole packet.
- **Slow readers are cut off.** Each reader has its own queue. A reader that
  has been more than 16 MB behind its channel for 15 seconds, or is ever 64
  MB behind, is disconnected, so that what is held for it stays bounded. What
  a joiner was handed at the start is not counted against it, until it has
  been sent on. This is also
  how long a paused player is waited for.
- **Last reader out.** When the last reader leaves, the provider's connection
  is dropped at once. The exception is a channel in its first 5 seconds,
  where a tuning player often hangs up and asks again within a moment: the
  channel lingers for 0.4 seconds unless that player has just opened another
  channel. A request that comes in while it lingers, with nobody on the
  channel, is the same tune carrying on: it is given everything kept so far
  at once, as the first reader was, not a joiner's cushion.
- **Changing channel.** A player hangs up on the old channel and asks for the
  new one at almost the same moment, and the two can arrive in either order.
  Before a new channel is opened, the relay drops that provider's channels
  that nobody is reading, gives a hang-up that is just behind the request a
  moment (30 ms) to arrive, and leaves the provider 0.1 seconds after the
  drop to notice it. If the player has hung up by then, nothing is opened.
  The aim is that a one-connection account never sees two connections during
  a channel change.
- **A stream the provider ends.** If the provider ends a stream, or it sends
  nothing for 60 seconds, while readers are still attached, the relay asks
  for the channel again after 0.1 seconds and carries on, so the players do
  not each have to notice and reconnect. It does this only for a stream that
  had run for at least 30 seconds, and not when another channel of the same
  provider was opened in the last 15 seconds (on a one-connection account
  that is why it ended). Asked again, the provider gets 5 seconds to answer
  and 10 to send video; after that the readers are let go, as they would be
  on a connection of their own. A channel that
  goes silent twice in a row, each time within 90 seconds of being asked for,
  is treated as dead: the readers are let go and the provider is not asked
  again. The new stream is joined on at a clean packet, and a
  packet the old stream left half-sent is filled out first so the packets
  stay aligned.
- **Refusals.** If the provider refuses the channel, its status code and the
  body of its answer are passed to the player. A reader waits up to 60
  seconds for a channel's first video.

### The HLS-to-MPEG-TS bridge (`/hls`)

With `NAME_LIVE_TV=hls`, a live `.ts` request does not go to the provider as
MPEG-TS. The relay builds the address of the same channel as HLS on this
proxy, `/__ch/<channel>.m3u8/__index.m3u8` on the provider's port, always in
the `/live/user/pass/<id>` form whichever form the reader used, and reads
it from Caddy on loopback, exactly as a player would. That puts the reader on
the session memory described under `/hl` below, so it shares the one provider
session with every HLS player on the channel.

- It reads the playlist, follows a list of qualities to the highest
  `BANDWIDTH` on offer, and starts near the newest video (the last three
  pieces).
- It fetches each new piece once, in order, and sends the pieces end to end
  as one MPEG-TS stream.
- It asks for the playlist again at the pace the HLS specification (RFC
  8216bis, section 6.3.4) sets for players: after a playlist that changed,
  the playing time of its newest piece, counted from when it began asking;
  after one that did not, half the target duration; never sooner than half a
  second.
- A channel's opening pieces go out at once, except the newest, so a reader
  starts with video in hand. After that, each piece is handed on over its own
  playing time (its `#EXTINF`, or the target duration), after the one before
  it, from a small queue. At most two target durations wait; anything beyond
  that goes at once. Video keeps going out while the relay is fetching the
  next playlist or piece.
- From one second before each whole minute of the clock until 0.3 seconds
  after it, nothing waits: whatever is queued goes out at once. TV guides,
  and anything timed by them, work in whole minutes, so a reader that stops
  on the minute has been given everything the relay had fetched. Whatever is
  waiting is also handed on before the feed ends, for any reason.
- For a bridged channel, the log line "a provider sent nothing for N s, then
  carried on" is written by the bridge, between pieces, when a piece arrives
  well after its own playing time; the queue's own pauses are not counted.
- Readers of the same bridged channel share one such feed, with the same ring
  and joiner rules as `/ts`.
- A piece that cannot be fetched after two tries, or that the relay may not
  fetch, is skipped and noted in the log; three in a row end the stream. A playlist that cannot be had is asked
  for again (4 tries at the start, 9 once playing).
- It only joins playlists made of whole, unencrypted MPEG-TS files. A
  playlist with `EXT-X-MAP`, `EXT-X-BYTERANGE` or an `EXT-X-KEY` other than
  `METHOD=NONE` cannot be joined end to end and is not bridged.
- Every address it fetches must be this proxy's own. A playlist line that
  leads anywhere else is not fetched.
- The stream ends when the playlist carries `EXT-X-ENDLIST` or stops moving
  for 30 seconds (or six target durations, if that is longer).

The bridged stream is the HLS one, and the queue keeps up to about one piece
in hand, so it runs a few seconds further behind live than the provider's own
MPEG-TS would. The bridge has been run against a simulated provider only
(see [What was tested, and how](#what-was-tested-and-how)).

### HLS built from MPEG-TS (`ts` providers)

With `NAME_LIVE_TV=ts`, a request for a live channel's HLS playlist
(`/live/user/pass/1.m3u8` and the like) is not passed to the provider. The
relay joins the channel's shared MPEG-TS stream instead, opening it at the
provider if nobody is on it yet, and cuts HLS pieces from it. So a device on
HLS and a device on MPEG-TS share the channel's one provider connection, and
neither cuts the other off.

- **The playlist.** The player is sent to the channel's own folder
  (`/__ch/...`), where the playlist and its pieces live. Pieces are cut at
  the first video keyframe after about 4 seconds. On a stream that marks no
  keyframes they are cut every 4 seconds once 8 seconds have come in, and no
  piece grows past 20 seconds. The newest 8
  pieces are kept (at least 4 if the 40 MB they may use is reached, as on a
  4K channel). The first playlist is answered once 2 pieces exist, which the
  provider's opening burst usually covers; a request waits up to 20 seconds
  for them.
- **Numbers that only go up.** Piece numbers start from the clock and keep
  growing, also when the stream is opened again, and each new stream is
  marked as a discontinuity, so a player that paused or reconnected carries
  on instead of freezing. `#EXT-X-TARGETDURATION` never goes down.
- **Letting go.** The builder leaves the channel 30 seconds after the last
  playlist or piece request. When a device opens another channel of the same
  provider, a builder only that device was using lets go at once, so an
  account with one connection can change channel. A device that is still
  asking keeps its builder.
- **Refusals.** If the provider refuses the channel, its status code and
  answer reach the HLS player, as for MPEG-TS.
- **The status page** shows a device on such a channel as HLS, with its data
  as "-".

### HLS sessions and playlist rewriting (`/hl`)

Caddy hands the relay every request for an address ending in `.m3u8` and
every request under a channel's own folder, `/__ch/`. The relay fetches from
the provider itself, through the tunnel, and handles three cases.

**Opening a channel** (`/live/user/pass/1.m3u8` and the like, on the
provider's own address):

1. The relay asks the provider for the channel once, without following the
   answer, and remembers where the provider sent it. The redirect must lead
   to `http` or `https` and not to a private address. The memory is keyed by
   provider and the channel's address at the provider.
2. It sends the player to the channel's own folder on the proxy,
   `/__ch/<channel address>/__index.m3u8`.
3. Opens of the same channel that arrive together wait for the first one, up
   to 20 seconds, and then use what it found.
4. If the provider answers with the playlist itself instead of a redirect,
   that playlist is rewritten and passed on, and nothing is remembered.

**The channel's folder** (`/__ch/<channel address>/...`):

- `__index.m3u8` is fetched from the remembered session, following any
  further redirects (each one checked the same way), and rewritten against
  the address it really came from. The provider is not asked to open the
  channel again.
- If the remembered session answers with an error (such as 403, 404, 410,
  429, or a 5xx) or does not answer, the channel is opened again once, the
  new session is remembered, and the playlist is fetched from it.
- Any other name in the folder is a piece named next to the playlist. It is
  answered with a redirect to its real `/__cdn/` address, worked out from
  where the playlist really came from.
- A channel nobody has asked about for 60 seconds is forgotten.
- The memory is in the relay's memory only. A restart of the relay forgets
  every channel, and the next look at each one opens it afresh.

**A playlist on another server** (`/__cdn/<scheme>/<server>/...m3u8`, for
example a quality listed in a channel's playlist): fetched from that server,
following redirects, each one checked against private addresses, and
rewritten against the address it really came from.

**The rewriting itself.** Every address line and every `URI="..."` inside a
tag is resolved against the address the playlist really came from, the way a
player resolves it, and then written as `/__cdn/<scheme>/<server>/<path>`.
So bare file names, `./`, `../` at any depth, `/path`, `//server/path` and
full addresses are all handled the same way. Bytes outside addresses come
out as they went in, line endings included. An answer that is not a playlist
(no `#EXTM3U` near its start) is passed on unchanged, and a playlist larger
than 4 MB is refused. The player's request headers and the server's response headers
are passed through, apart from per-connection ones, range and compression
headers, `Server`, `Date`, `Via` and the proxy's own notes. Because the
relay does not pass on `Accept-Encoding`, the provider sends the playlist as
plain text.

A playlist on a video server is fetched through Caddy, at its `/__cdn/`
address, so every refresh rides on the connection Caddy keeps open to that
server, the same as the video pieces. (A new encrypted connection for every
look at a live playlist, every few seconds, is what made players stutter
before connections were kept open.) The request comes from inside the
container with a mark that tells Caddy to pass it straight on instead of
handing it to the relay again; Caddy removes the mark before the request
leaves, and a player cannot use it, because it only counts from inside the
container. The relay still looks the server's name up first and refuses a
private address. Only the first open of a channel, and a renewal, go to the
provider directly.

### The provider's lists (`/api`)

`player_api.php` answers are the provider's channel, movie and series lists,
and a movie list can be very large (89 MB in the tests). The relay fetches
the answer from the provider, following redirects, and rewrites it piece by
piece as it streams through, without holding the whole list. Every
`"http://...` or `"https://...` (also in JSON's `\/` form) becomes
`"http://<the address the player used>/__cdn/<http or https>/...`, so the
player fetches logos and posters through the VPN. The relay asks the
provider for plain text; when the player's app asks for gzip, the relay
compresses the answer itself, at level 1. A provider that compresses anyway
cannot be read, and the request fails with 502.

### The status page (`/status`)

Caddy proxies `/status` on any provider port to the relay, with the port and
the easy login in headers. The relay then:

- reads the start-up notes from Caddy (what was found, what was skipped and
  why)
- asks `https://ipinfo.io/json` for the public address, through the tunnel,
  with certificate checking on, at most once every 5 minutes (once a minute
  while the lookup is failing)
- asks each provider for its account details through Caddy, with the easy
  login, so the request takes the same path a player's would. This happens when
  someone views the page, at most once every 15 seconds, and the page waits up
  to 6 seconds for the answer so that what it shows is current. Each answer is
  labeled with its age ("Checked just now"), and the page reloads itself every
  30 seconds. If the provider is slow, the last good answer is shown with its
  age, for up to 15 minutes. After a failure the provider is asked less and
  less often (up to 15 minutes apart).
- measures the round trip to the VPN server: three small packets that are
  allowed only one hop, so the VPN server itself answers each with "time
  exceeded", and the middle time is shown. Linux delivers that answer to the
  sending socket's error queue, so this needs no extra permission. A VPN
  server that does not answer leaves the line off the page.
- counts live channels: HLS channels whose playlist was asked for in the last
  30 seconds (by a player, or by the relay for a recorder), MPEG-TS channels and their readers, and MPEG-TS readers
  being fed from HLS. Movies, series and catch-up are not counted.
- lists what is playing right now, one row per device: the device's address,
  the channel's name, the provider, the format, how long it has been on and
  how much was sent ("-" for HLS). Channel names come from each provider's
  live list, fetched in the background through Caddy at most every 6 hours;
  until they arrive the channel's number is shown.

The page returns 200 when at least one provider is set up and there is no
settings problem, and 503 otherwise. It shows no logins.

### When the relay is not running

There is no fallback any more. Caddy answers what it would have handed to
the relay with an error (502):

| Request | What happens |
|---|---|
| Live MPEG-TS | Error. |
| An HLS playlist, or a live channel's folder | Error. |
| `player_api.php` (the account and the lists) | Error. Most players cannot log in. |
| `/status` | Error 502 from Caddy, with no text of its own. |
| `/__cdn/...` (video pieces, a movie or catch-up after the provider's redirect, pictures) | Error: Caddy cannot ask where the server's name leads, so it does not fetch. |

The guide, and a movie, series or catch-up that the provider serves without
sending the player to another server, do not touch the relay.
Docker starts the relay again if it stops.

### How the relay is fenced in

As written in `compose.yaml`:

- **Network.** It shares the tunnel's namespace and listens on loopback only.
  It connects to the providers named in `.env` and the servers they redirect
  to, to Caddy in this stack, and to `ipinfo.io` for the status page. Before
  it follows a redirect it looks the name up itself, refuses a private,
  loopback, link-local or carrier-grade NAT address, and connects to the very
  address it checked. Its fetches from Caddy are never allowed to be
  redirected to another host.
- **What it is given from `.env`.** The start command works out which
  providers are set up, using the main checks Caddy's start script uses (the
  name, the address form, the lines being present, `hls` or `ts`), builds the
  relay, and starts it with a cleared environment holding only `PATH`, a
  memory target and the list of those providers' addresses. No login, password or VPN key is in the program's
  environment. It does see a provider's login when Caddy hands it a live
  channel's, a playlist's or a list's address, and the easy login when the
  status page is drawn. The session memory holds channel addresses with the
  provider's login in them while a channel is in use; nothing is kept on
  disk.
- **User.** It runs as `65534:65534` (nobody), with no added capabilities and
  no volumes.
- **Memory.** `mem_limit: 512m`, a ceiling for the build at start and the
  channels being watched. `GOMEMLIMIT=384MiB` keeps the relay's heap under it.
- **Health check.** Every 10 seconds: it must still have an address in the
  tunnel's namespace, and `/healthz` on `127.0.0.1:8197` must answer.
- **Logs.** Its log lines never contain an address or a login.
- **Dependencies.** None. It uses only Go's standard library and is built
  with `GOTOOLCHAIN=local`, so the build never downloads anything. If a
  future `golang:alpine` could not build it, the container stops with the
  compiler's message in `docker logs iptv-relay`; live TV, playlists, the
  provider's lists and the status page are down until then, while movies,
  series and the guide still work.

### Why Caddy, and why Go beside it

The design rule is that the proxy keeps doing what it is good at, which is
HTTP proxying, and nothing else is added unless plain settings cannot do the
job. Caddy keeps connections to any server open by itself and needs no list
of video servers, so its settings stay short, and it is the stock image with
settings only: no plugins and no custom build, so it can take every security
update.

The jobs that settings cannot do well live in the relay: fanning one
upstream stream out to several readers, joining HLS pieces into one stream
and cutting one stream into HLS pieces, remembering one session per live
channel, reading a playlist line by line, rewriting a large list as it
streams, and drawing a status page. Video must not pass through anything that
grows in memory or alters the stream, so this is one small program with a
bounded memory and no dependencies. It is written in Go for durability: one
compiled program with no interpreter, standard library only, built in the
official Go image from its source inline in the compose file like the proxy
settings. Caddy terminates every player connection and still does all
proxying to providers for movies, series, catch-up, the guide and every video
piece.

### Limits, stated plainly

- **MPEG-TS smoothness depends on how evenly the provider sends video.** The
  relay passes video on as it arrives and does not pace it. A joiner has
  about 10 seconds in hand. If a provider pauses for longer than a player has
  buffered, that player stalls, as it would on a direct connection.
- **HLS is the recommended setting where a provider offers it.** With HLS the
  player fetches finished pieces and keeps its own buffer, and a recorder
  that wants MPEG-TS is still served through the bridge.
- **With `ts`, HLS is built from MPEG-TS.** A device asking for HLS gets
  pieces of about 4 seconds, cut at keyframes from the shared stream. Its
  first playlist comes once two pieces exist, which the provider's opening
  burst usually covers; a channel nobody was watching can take a few
  seconds longer.
- **The bridge needs plain HLS.** Encrypted or byte-range playlists, and ones
  with an `EXT-X-MAP` line, are not bridged.
- **The relay is in the path of live TV.** Recreating the relay container
  ends the live streams that run through it and forgets the HLS session
  memory. That happens when `.env` changes or the stack is updated. While it
  is not running, live TV and the provider's lists do not work.
- **A playlist is recognized by its address.** Only addresses ending in
  `.m3u8`, and a live channel's folder, are handed to the relay to be
  rewritten.
- **Certificates of providers are not verified**, by Caddy or by the relay,
  for the reason given above.
- **The Caddy version is new.** Some parts have not been run against a real
  provider since the change. See
  [What was tested, and how](#what-was-tested-and-how).

## Support

There is none. This was made for personal use and is shared as is, free, in
case it is useful to someone else. Issues and pull requests are turned off,
and questions will not be answered. Everything needed to use it is on this
page. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE). gluetun, Caddy and Go are separate projects with their own
licenses. This repository contains configuration for them and one small
Go program, and does not distribute any of the three.
