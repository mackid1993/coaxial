<p align="center">
  <img src="assets/coaxial-icon.svg" width="88" alt="">
  &nbsp;&nbsp;
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/coaxial-wordmark-dark.svg">
    <img src="assets/coaxial-wordmark-light.svg" width="360" alt="coaxial">
  </picture>
</p>

Coaxial puts your IPTV providers behind a VPN without installing anything on your devices. It is one container with the
VPN ([gluetun](https://github.com/qdm12/gluetun)) built in, so there is no stack of containers to wire together: one
image, one folder, one settings file. Your players connect to Coaxial with one easy login instead of to the provider,
and everything Coaxial fetches leaves through the VPN.

For your home network only. The ports have no protection of their own, and neither does the settings page. Never
forward them from the internet.

## Run it

Build the image from this repository:

```sh
docker build -t coaxial coaxial/
```

`compose.yaml`:

```yaml
services:
  coaxial:
    image: coaxial
    container_name: coaxial
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    volumes:
      - ./config:/config
    ports:
      - "8095-8096:8095-8096" # one port per provider
      - "8195:8195" # the web interface
```

Start it, open `http://<this machine>:8195`, enter your VPN and provider settings, and save. That is all. In each
player, add an Xtream login with the address `http://<this machine>:<the provider's port>` and the player username and
password you chose.

Saving provider settings restarts only the proxy; the VPN stays connected. Saving VPN settings reconnects the VPN.
Live channels that are playing are cut off by either one.

## settings.json

The page writes `config/settings.json`. You can also edit it by hand and restart the container. If the file is missing,
Coaxial writes `settings.sample.json` next to where it should be.

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
    "extra": { "ANY_GLUETUN_VARIABLE": "value" }
  }
}
```

| Setting | What it is |
| --- | --- |
| `player` | The easy login you type into every player. Letters, digits, `-` and `_`. |
| `providers` | One entry per provider: a name, its address (`http://name` or `https://name`, optionally `:port`), the login it gave you, the port your players use for it, and `live_tv`. Publish every provider port in `compose.yaml`. |
| `live_tv` | `ts` if your players watch live TV as MPEG-TS, `hls` if they use HLS. |
| `ui.port` | The port of the settings page and of the health check. |
| `vpn` | gluetun's settings. Every key but `extra` and `update_hours` is a gluetun variable written in small letters: `wireguard_private_key` is `WIREGUARD_PRIVATE_KEY`, `service_provider` and `type` are `VPN_SERVICE_PROVIDER` and `VPN_TYPE`. |
| `vpn.update_hours` | How often gluetun refreshes its list of your VPN service's servers (its `UPDATER_PERIOD`). 24 if left out, 0 for never. gluetun's own documentation recommends 360 or more, so that the VPN service does not block the updates. |
| `vpn.extra` | Any other gluetun variable under its own name, such as `OPENVPN_USER`. It is applied last, so it wins over the settings above, and no name is refused. |

Passwords and keys are stored in `settings.json` as written. The page never shows a saved one again; leave such a field
empty to keep what is saved.

gluetun keeps its server lists, and the updates it makes to them, in `config/gluetun`, so they survive a restart.

## How it behaves

- **One provider connection per live channel.** Every device watching or recording a channel shares it. Video is
  handed on as the provider sends it; nothing is held back. A reader who joins a channel that is already open is
  started from the last 10 seconds or so of video, is caught up within its first seconds, and from then on gets
  everything as it arrives, like the reader who opened the channel.
- **Mixed formats.** On a `ts` provider, a player that asks for HLS is given HLS cut from the same shared MPEG-TS
  stream. On an `hls` provider, a reader that asks for MPEG-TS is fed from the players' one HLS session. Either way the
  provider sees one connection per channel.
- **Redirects stay inside the VPN.** When a provider sends a player to another server, Coaxial rewrites the address so
  the player comes back to Coaxial for it. Addresses in playlists and logos and posters in the provider's lists are
  pointed back the same way. It refuses to fetch from this machine or a home network.
- **Movies, series, catch-up and the guide** are passed through untouched, seeking included.
- **Xtream logins only.** A provider's ready-made M3U link (`get.php`) is refused, because the addresses inside it skip
  the VPN.
- A status page is at `http://<this machine>:<any provider's port>/status`.

## Acknowledgments

Coaxial stands on two projects:

- [gluetun](https://github.com/qdm12/gluetun) by Quentin McGaw, the VPN client built into the container.
- [AH4C](https://github.com/sullrich/ah4c), originally written by Aman Karmani and since developed by
  Scott Ullrich and Scott Ueland, whose stream copying code Coaxial carries.

Their licenses and the full credits are in [ACKNOWLEDGMENTS.md](ACKNOWLEDGMENTS.md).

## License

[MIT](LICENSE)
