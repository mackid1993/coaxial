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

**Status** shows whether the VPN is up, where it comes out (city, country, IP address, network, ping), each provider,
what is playing right now, and the Coaxial version. It also has the gluetun version with `Check for updates` and the
chosen VPN server with `Find the fastest server`. An open Status view loads itself again when the Coaxial version
changes, unless settings are unsaved. **Settings** has:

- **Providers.** Add them, remove them, and reorder them with the up and down arrows. A new port is tried before
  saving: Coaxial listens on it for 8 seconds and the page tries to reach it from your browser. If that fails, the page
  does not save and tells you to add a `PORT_n` line for it to `.env`. Ports are tried again after a save.
- **VPN.** The service and the connection type. WireGuard takes a private key and addresses. OpenVPN takes a username
  and a password. Then where to connect: countries, cities, regions, servers by hostname and servers by name. All of
  these are drop-downs filled from gluetun's own server lists, and the two server lists narrow to the places you
  picked. You can also type. `Refresh server list` has gluetun fetch a new list now, which needs the VPN to be running.
- **Extra VPN variables.** Free-form names and values for any other gluetun variable, except the few Coaxial refuses
  (see `vpn.extra` below).
- **Player login**, and the port of the web page.

Saving player, provider or port settings restarts only the proxy. Saving VPN settings reconnects the VPN. Either one
cuts off live channels that are playing. A saved password or key is never shown again: leave the field empty to keep
it. A provider's saved password is kept only while its address stays the same.

## settings.json

The page writes `settings.json` in the mounted folder (`./config` in the example). You can also edit it by hand and
restart the container. If the file is missing at start, Coaxial writes `settings.sample.json` beside it.

```json
{
  "player": { "user": "tv", "password": "choose-your-own" },
  "providers": [
    { "name": "Example", "url": "https://provider.example", "user": "...", "password": "...", "port": 8095, "live_tv": "ts" }
  ],
  "ui": { "port": 8195 },
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
| `providers` | One entry per provider: a name, its address (`http://name` or `https://name`, optionally `:port`, with nothing after it), the login it gave you, the port your players use for it, and `live_tv`. Ports 53, 8000 and 9999 and the web page's port cannot be used. |
| `live_tv` | `ts` if your players watch live TV as MPEG-TS, `hls` if they use HLS. |
| `ui.port` | The port of the web page and of the health check. If left out, `UI_PORT` from `.env` (8195 if that is not set either). If you change it, change `UI_PORT` in `.env` to match. |
| `vpn` | gluetun's settings. Every key but `extra`, `update_hours` and `gluetun_version` is a gluetun variable written in small letters: `wireguard_private_key` is `WIREGUARD_PRIVATE_KEY`, `service_provider` and `type` are `VPN_SERVICE_PROVIDER` and `VPN_TYPE`. The VPN service must be set. |
| `vpn.update_hours` | How often gluetun refreshes its list of your VPN service's servers (its `UPDATER_PERIOD`). 24 if left out, 0 for never. |
| `vpn.gluetun_version` | Which gluetun program runs: `latest` (also when left out), a tag of gluetun's image such as `v3.41.3` to stay on one version, or `builtin` to fetch nothing and run the one in this image. |
| `vpn.extra` | Any other gluetun variable under its own name, such as `OPENVPN_PROTOCOL`. It is applied last, so it wins over the settings above. Refused here and under `vpn`, because anyone who can reach the web page could use them: gluetun's `..._COMMAND` variables, OpenVPN options that run programs (such as `--up`) in `OPENVPN_FLAGS`, `HTTP_CONTROL_SERVER_*` settings other than `HTTP_CONTROL_SERVER_LOG`, and values on more than one line (except the OpenVPN certificate and keys). |

gluetun keeps its server lists in `gluetun/servers` in the mounted folder, so they survive a restart.

## Choosing the VPN server

If you name a server in the settings, Coaxial uses that one and leaves it alone. If you pick a city and leave both
server boxes empty, Coaxial chooses the server for you. It does not do this for the `custom` service.

- **A city is required.** With only a country or a region picked, gluetun picks a server at random, as before.
- **Up to 8 servers are tried per run.** When the city has more, it is a different handful each time, and the server
  Coaxial chose before is always among them. Only servers that fit your settings are tried: the connection type, and
  `STREAM_ONLY`, `PORT_FORWARD_ONLY` and `OWNED_ONLY` when they are on. Secure core, Tor, free and multihop servers are
  left out unless the matching `..._ONLY` setting is on. With fewer than two servers to choose from, nothing is tried.
- **Trying a server means connecting to it.** The VPN reconnects once for each one, and a test file is downloaded from
  `speed.cloudflare.com` through the VPN for up to 1.5 seconds. Coaxial stays on the fastest. Only speed counts: the
  share of packets out of order is logged but not counted. Each takes a few seconds, so 8 take about a minute.
- **The choice and its speed are saved** in `gluetun/pick.json` in the mounted folder. When the container starts, it
  goes straight to that server. Nothing is tried at start, so you can watch right away.
- **In quiet times it checks, it does not shop around.** Once nothing has played or recorded for 10 minutes, Coaxial
  waits an hour, then measures the server in use without reconnecting, and again every hour until someone plays. The
  other servers are tried only if no server is chosen yet for your settings, or the one in use is under 50 Mbit/s or
  under a fifth of the speed it was chosen at, or it cannot be measured twice in a row. A server that is fast but no
  longer the fastest is kept, because trying the others takes the VPN down for about a minute and a recording that
  starts then could fail. If someone starts playing during a run, it stops and is done again in the next quiet time.
- **`Find the fastest server`** on the Status view runs it now, and goes on even if someone starts playing. It asks
  first, because anything playing or recording stops for a minute or two.
- If the chosen server stops connecting for 2 minutes, Coaxial drops its choice and lets gluetun pick.

## VPN client updates

gluetun is published only as a container image, and its program is one file in that image. So that a new gluetun does
not need a new Coaxial image, Coaxial fetches that file itself.

- **At every start of the container**, before the VPN is started, Coaxial asks gluetun's image what `latest` is now:
  at `ghcr.io/qdm12/gluetun` first, and at `qmcgaw/gluetun` on Docker Hub if that fails. If it does not have that build
  yet, it downloads the layer that holds the program for this machine's processor. The VPN is not up yet, so this
  request does not go through it.
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
  the new one (`gluetun/bin/skip.json`), also after a restart, until another is out or you click `Update now`.
- **`Update now`** appears when a newer build is out. It asks you first, because the VPN reconnects and anything
  playing stops. A newer build is also taken whenever the container starts again.
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
  shared MPEG-TS stream. On an `hls` provider, a reader that asks for MPEG-TS is fed from the players' one HLS session.
- **Redirects stay inside the VPN.** When a provider sends a player to another server, Coaxial rewrites the address so
  the player comes back to Coaxial for it. Addresses in playlists, and logos and posters in the provider's lists, are
  pointed back the same way. Coaxial refuses such an address if it leads to this machine or a home network.
- **Movies, series, catch-up and the guide** are passed on as they come, seeking included.
- **Xtream logins only.** A provider's ready-made M3U link (`get.php`) is refused: its addresses skip the VPN.
- **If the VPN fails.** If the VPN program stops before it has connected, the web page stays up and the VPN is started
  again after 30 seconds. If it stops after it had connected, Coaxial exits so that Docker restarts the container.
- **Status page.** `http://<this machine>:<any provider's port>/status` is a plain page with the VPN's exit address,
  each provider's account status and what is playing. The exit address is asked of `ipinfo.io` through the VPN.
- **The log.** Normal pauses in a provider's sending are not logged. A silence of 20 seconds or more is.

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