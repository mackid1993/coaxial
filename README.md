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

Put [`compose.yaml`](compose.yaml) and [`.env.example`](.env.example) in a folder, copy `.env.example` to `.env`, and
start it with `docker compose up -d`. The service is short:

```yaml
services:
  coaxial:
    image: ghcr.io/mackid1993/coaxial:latest
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    volumes:
      - ./config:/config
    ports:
      - "${PORT_1}:${PORT_1}"
      # ... PORT_2 to PORT_20, and UI_PORT
```

That is one service and one bind mount. The container is given the `NET_ADMIN` capability and the `/dev/net/tun`
device for the VPN.

The ports are set in `.env`, never in the compose file:

```
PORT_1=8095
PORT_2=8096
UI_PORT=8195
```

Each provider needs a port of its own. To add one, add the next `PORT_n` line (up to `PORT_20`), run
`docker compose up -d` again, and pick that port for the provider in the web page. Any free port will do, in any order.

Open `http://<this machine>:8195`. Enter your VPN and provider settings and save. In each player, add an Xtream login
with the address `http://<this machine>:<the provider's port>` and the player login you chose.

## The web page

**Status** shows whether the VPN is up, where it comes out (city, country, IP address, ping), each provider, what is
playing right now, and the Coaxial and gluetun versions. **Settings** has:

- **Providers.** Add them, remove them, and reorder them with the up and down arrows. A new port is tried before
  saving: Coaxial listens on it for 8 seconds and the page tries to reach it from your browser. If that fails, Docker
  is not publishing the port, and the page does not save.
- **VPN.** The service, WireGuard or OpenVPN, the WireGuard key and addresses, and server countries and cities. The
  service, countries and cities are drop-downs filled from gluetun's own server lists. You can also type.
  `Refresh server list` has gluetun fetch a new list now, which needs the VPN to be running.
- **Extra VPN variables.** Free-form names and values for any other gluetun variable.
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
| `providers` | One entry per provider: a name, its address (`http://name` or `https://name`, optionally `:port`, with nothing after it), the login it gave you, the port your players use for it, and `live_tv`. Each provider needs a port of its own. Ports 53, 8000 and 9999 and the web page's port cannot be used. |
| `live_tv` | `ts` if your players watch live TV as MPEG-TS, `hls` if they use HLS. |
| `ui.port` | The port of the web page and of the health check. 8195 if left out. |
| `vpn` | gluetun's settings. Every key but `extra`, `update_hours` and `gluetun_version` is a gluetun variable written in small letters: `wireguard_private_key` is `WIREGUARD_PRIVATE_KEY`, `service_provider` and `type` are `VPN_SERVICE_PROVIDER` and `VPN_TYPE`. The VPN service must be set. |
| `vpn.update_hours` | How often gluetun refreshes its list of your VPN service's servers (its `UPDATER_PERIOD`). 24 if left out, 0 for never. |
| `vpn.gluetun_version` | Which gluetun program runs: `latest` (also when left out), a tag of gluetun's image such as `v3.41.3` to stay on one version, or `builtin` to fetch nothing and run the one in this image. |
| `vpn.extra` | Any other gluetun variable under its own name, such as `OPENVPN_USER`. It is applied last, so it wins over the settings above. |

gluetun keeps its server lists in `gluetun/servers` in the mounted folder, so they survive a restart.

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
- **While it runs**, Coaxial asks the registry once an hour whether a newer build is out, and the Status view says so.
  `Check for updates` asks at once. Neither installs anything: Coaxial never restarts a running VPN to update it.
- **`Update now`** appears when a newer build is out. It asks you first, because the VPN reconnects and anything
  playing stops. A newer build is also taken whenever the container starts again. A VPN restart caused by a settings
  change asks the registry again only if six hours have passed since it was last asked.

The downloaded program runs with the VPN's privileges, like the one in the image. Every start of the VPN logs which
program runs and why, on a line containing `VPN program:`. Only gluetun's own program is fetched. The system tools it
relies on, such as `iptables` and `openvpn`, come from the image and change only when the image is rebuilt.

## How it behaves

- **One provider connection per live channel.** Every device watching or recording a channel shares it. A reader that
  joins a channel already open starts about 10 seconds back, at a point where a decoder can begin, and is then brought
  up to live. After that nothing is held back: video is passed on as it arrives.
- **Mixed formats.** On a `ts` provider, a player that asks for a live channel as HLS is given HLS cut from the same
  shared MPEG-TS stream. On an `hls` provider, a reader that asks for MPEG-TS is fed from the players' one HLS session.
- **Redirects stay inside the VPN.** When a provider sends a player to another server, Coaxial rewrites the address so
  the player comes back to Coaxial for it. Addresses in playlists, and logos and posters in the provider's lists, are
  pointed back the same way. Coaxial refuses such an address if it leads to this machine or a home network.
- **Movies, series, catch-up and the guide** are passed on as they come, seeking included.
- **Xtream logins only.** A provider's ready-made M3U link (`get.php`) is refused, because the addresses inside it skip
  the VPN.
- **If the VPN fails.** If the VPN program stops before it has connected, the web page stays up and the VPN is started
  again after 30 seconds. If it stops after it had connected, Coaxial exits so that Docker restarts the container.
- **Status page.** `http://<this machine>:<any provider's port>/status` is a plain page with the VPN's exit address,
  each provider's account status and what is playing.

## Versions and builds

A version is the time the image was built, in UTC: `vYYYY.MM.DD.HHMM`. It is stamped into the program at build time and
shown on the web page and in the first line of the log. The workflow in `.github/workflows/docker-build.yml` rebuilds
the image on the 1st of every month, without a cache, so it takes in the newest Go and gluetun images. It builds for
amd64 and arm64 and pushes to GitHub's container registry under this repository's name, tagged `latest` and with the
version. It can also be started by hand, with a tag of your choice in place of `latest`. A push to the repository does
not start it.

## Acknowledgments

See [ACKNOWLEDGMENTS.md](ACKNOWLEDGMENTS.md).

## License

[MIT](LICENSE)