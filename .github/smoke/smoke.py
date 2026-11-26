#!/usr/bin/env python3
"""Smoke test of a built Coaxial image (see smoke.sh).

serve: a stand-in provider. It knows one login and one live channel, whose MPEG-TS it sends the way live
       channels come: a burst, then lumps with pauses between them, one of them long. It also has that channel
       as HLS and one catch-up as HLS, the way providers do: its own address sends the player on to a video
       server (here its second port), every open of the live channel there is a new session, and the newest
       session is the only one that still answers. A second live channel is a real one (program tables,
       video with a keyframe every 4 s, sound), sent as MPEG-TS: 24 s of it at once, then nothing more.
check: a player. Through the proxy part of the image it logs in, is refused with a wrong password, opens the
       status page and watches the channel; what it is sent must be, byte for byte, what the stand-in sent.
       Then it watches the channel as HLS and opens the catch-up, following every address the way a player
       does: none may lead anywhere but the proxy, and every piece must be what the stand-in sent.
       Last it watches the second channel as HLS on the port whose provider is set to MPEG-TS: that HLS is cut
       from the channel's MPEG-TS by the proxy itself, and its pieces must be the stand-in's packets, cut at
       its keyframes, while a recorder reads the same channel as MPEG-TS over the same one connection.
"""
import json
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urljoin, urlparse

REAL_USER, REAL_PASS = "realU", "realP"      # the stand-in provider's login (the proxy's settings carry it)
EASY_USER, EASY_PASS = "tv", "easy"          # the login players use at the proxy
VIDEO_PORT = 8081                            # the stand-in's video server: where it sends HLS players on to
CATCH_UP = "60/2026-01-01:10-00/2.m3u8"      # the catch-up: an hour of channel 2
PIECE_PACKETS = 400                          # an HLS piece: 75 kB

counting = threading.Lock()
counts = {"opens": 0, "playlists": 0,        # opens of the live HLS channel; asks for its playlist
          "real_ts": 0, "real_m3u8": 0}      # asks for the real channel as MPEG-TS, and as HLS (never)
REAL_GROUPS = 6                              # the real channel: 6 keyframes 4 s apart, 24 s of video
REAL_FRAMES = 100                            # frames from one keyframe to the next (25 a second)


def live_piece(number):
    """The live HLS piece with this number, as the stand-in sends it."""
    return packets(number * 1000, PIECE_PACKETS)


def catch_up_piece(number):
    return packets(5000000 + number * 1000, PIECE_PACKETS)


def packets(first, count):
    """count MPEG-TS packets with no programme in them, each one different, starting at number first."""
    out = bytearray()
    for i in range(first, first + count):
        p = bytearray(188)
        p[0], p[1], p[2], p[3] = 0x47, 0x1F, 0xFF, 0x10 | (i & 0x0F)
        p[4:12] = i.to_bytes(8, "big")
        for k in range(12, 188):
            p[k] = (i * 31 + k * 7) & 0xFF
        out += p
    return bytes(out)


def crc32_mpeg(data):
    crc = 0xFFFFFFFF
    for b in data:
        crc ^= b << 24
        for _ in range(8):
            crc = ((crc << 1) ^ 0x04C11DB7) & 0xFFFFFFFF if crc & 0x80000000 else (crc << 1) & 0xFFFFFFFF
    return crc.to_bytes(4, "big")


def stamp(prefix, t):
    """A PES timestamp (PTS) of t ticks of 1/90000 s."""
    return bytes([prefix << 4 | (t >> 29) & 0x0E | 1, (t >> 22) & 0xFF, (t >> 14) & 0xFE | 1, (t >> 7) & 0xFF, (t << 1) & 0xFE | 1])


def real_channel():
    """The real channel as MPEG-TS: a list of (kind, packet), kind "table" for the program tables (PAT, PMT)
    that stand before every keyframe, "key" for a keyframe's first packet, "av" for every other video or
    sound packet. H.264 video on PID 0x100 (it carries the clock), AAC sound on PID 0x101; the first frame is
    shown 10 s into the stream's clock."""
    out, cc = [], {}

    def packet(kind, pid, start, field, payload, pad):
        n = cc.get(pid, 0)
        cc[pid] = n + 1
        p = bytearray([0x47, (0x40 if start else 0) | pid >> 8, pid & 0xFF, (0x30 if field else 0x10) | n & 0x0F])
        if field:
            p += bytes([len(field)]) + field
        p += payload
        while len(p) < 188:                              # (never a zero: no start code hides in the padding)
            p.append(pad if pad else 0x11 + (len(out) * 7 + len(p) * 13) % 0xE0)
        out.append((kind, bytes(p)))

    pat = bytes([0x00, 0xB0, 13, 0, 1, 0xC1, 0, 0, 0, 1, 0xF0, 0x00])
    pmt = bytes([0x02, 0xB0, 23, 0, 1, 0xC1, 0, 0, 0xE1, 0x00, 0xF0, 0, 0x1B, 0xE1, 0x00, 0xF0, 0, 0x0F, 0xE1, 0x01, 0xF0, 0])
    key_start = bytes([0, 0, 0, 1, 0x09, 0x10, 0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1F, 0xAC, 0xD9, 0, 0, 0, 1, 0x68, 0xEE, 0x3C, 0x80, 0, 0, 0, 1, 0x65, 0x88])
    other_start = bytes([0, 0, 0, 1, 0x09, 0x30, 0, 0, 0, 1, 0x41, 0x9A])
    for f in range(REAL_GROUPS * REAL_FRAMES):
        key = f % REAL_FRAMES == 0
        t = 900000 + f * 3600
        if key:
            packet("table", 0, True, b"", b"\x00" + pat + crc32_mpeg(pat), 0xFF)
            packet("table", 0x1000, True, b"", b"\x00" + pmt + crc32_mpeg(pmt), 0xFF)
        clock = t - 45000                                # (the PCR runs half a second ahead of the pictures)
        field = bytes([0x50 if key else 0x10, clock >> 25 & 0xFF, clock >> 17 & 0xFF, clock >> 9 & 0xFF, clock >> 1 & 0xFF, (clock & 1) << 7 | 0x7E, 0])
        packet("key" if key else "av", 0x100, True, field, bytes([0, 0, 1, 0xE0, 0, 0, 0x80, 0x80, 5]) + stamp(2, t) + (key_start if key else other_start), 0)
        for _ in range(7 if key else 2):
            packet("av", 0x100, False, b"", b"", 0)
        packet("av", 0x101, True, b"", bytes([0, 0, 1, 0xC0, 0, 170, 0x80, 0x80, 5]) + stamp(2, t) + bytes([0xFF, 0xF1, 0x4C, 0x80]), 0)
    return out


def real_ts():
    return b"".join(p for _, p in real_channel())


def real_pieces():
    """The pieces the proxy must cut the real channel into: one per keyframe that is followed by another. Each
    is the tables that stood before its keyframe, then every video and sound packet up to the next keyframe."""
    pieces, tables, now = [], [], None
    for kind, p in real_channel():
        if kind == "table":
            tables.append(p)
            continue
        if kind == "key":
            if now is not None:
                pieces.append(bytes(now))
            now = bytearray(b"".join(tables[-2:]))
        now += p
    return pieces


class Provider(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def send(self, status, kind="", body=b"", location=""):
        self.send_response(status)
        if kind:
            self.send_header("Content-Type", kind)
        if location:
            self.send_header("Location", location)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def video_server(self, path):
        """The video server: the live channel's HLS sessions and the catch-up's finished playlist."""
        parts = path.strip("/").split("/")
        if len(parts) == 3 and parts[0] == "hls" and parts[1].startswith("tok"):
            with counting:
                newest = parts[1] == "tok%d" % counts["opens"]
                if newest and parts[2] == "index.m3u8":
                    counts["playlists"] += 1
                    first = 100 + counts["playlists"]     # (each ask finds the playlist one piece further on)
            if not newest:                                # an older session: gone, as at real providers
                return self.send(404)
            if parts[2] == "index.m3u8":
                # Three pieces, named the three ways playlists name them: next to the playlist, by path, in full.
                here = "http://%s" % self.headers.get("Host", "")
                text = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:%d\n" % first
                text += "#EXTINF:4.000,\np%d.ts\n" % first
                text += "#EXTINF:4.000,\n/hls/%s/p%d.ts\n" % (parts[1], first + 1)
                text += "#EXTINF:4.000,\n%s/hls/%s/p%d.ts\n" % (here, parts[1], first + 2)
                return self.send(200, "application/vnd.apple.mpegurl", text.encode())
            if parts[2].startswith("p") and parts[2].endswith(".ts") and parts[2][1:-3].isdigit():
                return self.send(200, "video/mp2t", live_piece(int(parts[2][1:-3])))
        if path == "/arch/2/index.m3u8":
            text = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n"
            for n in range(3):
                text += "#EXTINF:10.000,\na%d.ts\n" % n
            return self.send(200, "application/vnd.apple.mpegurl", (text + "#EXT-X-ENDLIST\n").encode())
        if path in ("/arch/2/a0.ts", "/arch/2/a1.ts", "/arch/2/a2.ts"):
            return self.send(200, "video/mp2t", catch_up_piece(int(path[-4])))
        self.send(404)

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if self.server.server_port == VIDEO_PORT:
            return self.video_server(u.path)
        video = "http://%s:%d" % (self.headers.get("Host", "").rsplit(":", 1)[0], VIDEO_PORT)
        if u.path == "/live/%s/%s/2.m3u8" % (REAL_USER, REAL_PASS):   # every open is a new session
            with counting:
                counts["opens"] += 1
                session = counts["opens"]
            return self.send(302, location="%s/hls/tok%d/index.m3u8" % (video, session))
        if u.path == "/timeshift/%s/%s/%s" % (REAL_USER, REAL_PASS, CATCH_UP):
            return self.send(302, location=video + "/arch/2/index.m3u8")
        if u.path == "/smoke/counts":                     # (for the check: what was asked of the stand-in)
            with counting:
                return self.send(200, "application/json", json.dumps(counts).encode())
        if u.path == "/player_api.php":
            ok = q.get("username") == [REAL_USER] and q.get("password") == [REAL_PASS]
            body = json.dumps({
                "user_info": {"username": REAL_USER, "password": REAL_PASS, "auth": 1 if ok else 0,
                              "status": "Active", "max_connections": "2", "active_cons": "0",
                              "allowed_output_formats": ["m3u8", "ts"]},
                "server_info": {"url": "provider.invalid", "port": "8080", "https_port": "8443",
                                "server_protocol": "http", "timezone": "UTC"},
            } if ok else {"user_info": {"auth": 0}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if u.path == "/live/%s/%s/3.m3u8" % (REAL_USER, REAL_PASS):   # (set to MPEG-TS, it is never asked for as HLS)
            with counting:
                counts["real_m3u8"] += 1
            return self.send(404)
        if u.path == "/live/%s/%s/3.ts" % (REAL_USER, REAL_PASS):
            with counting:
                counts["real_ts"] += 1
            self.send_response(200)
            self.send_header("Content-Type", "video/mp2t")
            self.send_header("Connection", "close")
            self.end_headers()
            self.close_connection = True
            try:
                self.wfile.write(real_ts())                # all of it at once: nothing here depends on the clock
                self.wfile.flush()
                self.connection.settimeout(300)
                self.connection.recv(1)                    # (the channel stays open until the proxy lets go of it)
            except OSError:
                pass
            return
        if u.path == "/live/%s/%s/1.ts" % (REAL_USER, REAL_PASS):
            self.send_response(200)
            self.send_header("Content-Type", "video/mp2t")
            self.send_header("Connection", "close")
            self.end_headers()
            self.close_connection = True
            n = 0
            try:
                for lump in range(100000):
                    count = 11000 if lump == 0 else 1600   # about 2 MB at once, then 300 kB lumps
                    self.wfile.write(packets(n, count))
                    self.wfile.flush()
                    n += count
                    time.sleep(3.0 if lump == 5 else 0.5)
            except OSError:
                pass
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()


def get(url, seconds=10):
    try:
        with urllib.request.urlopen(url, timeout=seconds) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def play(url, proxy, seconds=10):
    """One request made the way a player makes it: an answer that sends it elsewhere is followed, the new
    address worked out from the one just asked. Only the proxy is ever asked: an address that leads anywhere
    else is not followed, and is returned as where the player was sent away to."""
    opener = urllib.request.build_opener(NoRedirect)
    for _ in range(8):
        if urlparse(url).netloc != urlparse(proxy).netloc:
            return 0, b"", url, url
        try:
            with opener.open(url, timeout=seconds) as r:
                return r.status, r.read(), url, ""
        except urllib.error.HTTPError as e:
            if e.code in (301, 302, 303, 307, 308) and e.headers.get("Location"):
                url = urljoin(url, e.headers["Location"])
                continue
            return e.code, e.read(), url, ""
    return 0, b"", url, ""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def listed(text):
    """A playlist's piece addresses as written, and its first piece's number (None if it names none)."""
    lines = [l.strip() for l in text.splitlines()]
    first = [l.split(":", 1)[1] for l in lines if l.startswith("#EXT-X-MEDIA-SEQUENCE:")]
    return [l for l in lines if l and not l.startswith("#")], (int(first[0]) if first and first[0].isdigit() else None)


def check_hls(hls, provider, want):
    """Live HLS and one catch-up, through the proxy's port for a provider whose players use HLS."""
    def asked():
        return json.loads(get(provider + "/smoke/counts")[1])

    # The live channel as HLS.
    status, body, at, away = play("%s/live/%s/%s/2.m3u8" % (hls, EASY_USER, EASY_PASS), hls)
    text = body.decode("utf-8", "replace")
    pieces, first = listed(text)
    want(status == 200 and "#EXTM3U" in text and len(pieces) == 3 and first is not None and not away,
         "the live channel's HLS playlist is served (%d, %d pieces%s)" % (status, len(pieces), ", sent away to " + away if away else ""))
    want(REAL_USER not in text and REAL_PASS not in text, "the playlist does not carry the provider's own login")
    full = [urljoin(at, p) for p in pieces]
    off = [a for a in full if urlparse(a).netloc != urlparse(hls).netloc]
    want(not off, "every piece's address, worked out the way a player does, leads to the proxy (%s)"
         % (off[0] if off else "%d of %d" % (len(full), len(full))))
    same = 0
    for i, a in enumerate(full):
        s, data, _, away = play(a, hls)
        same += s == 200 and not away and data == live_piece((first or 0) + i)
    want(same == len(full) and same > 0, "the pieces are byte for byte what the provider sent (%d of %d)" % (same, len(full)))
    # The player asks for the playlist again, at the address it was given it at.
    status, body, _, away = play(at, hls)
    again, second = listed(body.decode("utf-8", "replace"))
    want(status == 200 and first is not None and second == first + 1,
         "asked for again, the playlist is the provider's next one (it began at piece %s, now at %s)" % (first, second))
    if again and second is not None:
        s, data, _, away = play(urljoin(at, again[-1]), hls)
        want(s == 200 and not away and data == live_piece(second + len(again) - 1),
             "the newest piece of the second playlist is what the provider sent (%d, %d bytes)" % (s, len(data)))
    n = asked()
    want(n["opens"] == 1, "the channel was opened at the provider once (%d)" % n["opens"])
    want(n["playlists"] == 2, "the provider was asked for the playlist as often as the player asked: twice (%d)" % n["playlists"])

    # A catch-up as HLS: a complete playlist. An app that was sent to the proxy by another server works the
    # pieces' addresses out from that other server's address, so a complete playlist must name every piece in
    # full, at the address the player reached the proxy at.
    status, body, at, away = play("%s/timeshift/%s/%s/%s" % (hls, EASY_USER, EASY_PASS, CATCH_UP), hls)
    text = body.decode("utf-8", "replace")
    pieces, _ = listed(text)
    want(status == 200 and "#EXT-X-ENDLIST" in text and len(pieces) == 3 and not away,
         "the catch-up's playlist is served, complete (%d, %d pieces)" % (status, len(pieces)))
    want(REAL_USER not in text and REAL_PASS not in text, "the catch-up's playlist does not carry the provider's own login")
    short = [p for p in pieces if not p.startswith(hls + "/")]
    want(pieces and not short, "the catch-up's playlist names every piece in full, at the address the player used (%s)"
         % (short[0] if short else pieces[0] if pieces else "no pieces"))
    same = 0
    for i, a in enumerate(pieces):
        s, data, _, away = play(a, hls)
        same += s == 200 and not away and data == catch_up_piece(i)
    want(same == len(pieces) and same > 0, "the catch-up's pieces are byte for byte what the provider sent (%d of %d)" % (same, len(pieces)))


def check_hls_from_ts(base, provider, want):
    """Live HLS on the port whose provider is set to MPEG-TS: the proxy cuts it from the channel's MPEG-TS."""
    expected = real_pieces()
    status, body, at, away = play("%s/live/%s/%s/3.m3u8" % (base, EASY_USER, EASY_PASS), base)
    text = body.decode("utf-8", "replace")
    pieces, first = listed(text)
    want(status == 200 and "#EXTM3U" in text and len(pieces) >= 2 and first is not None and not away,
         "a channel of the MPEG-TS provider is served as HLS (%d, %d pieces%s)" % (status, len(pieces), ", sent away to " + away if away else ""))
    # The stand-in sent all it has at once, so the playlist comes to hold every piece that can be cut from it
    # (one per keyframe that is followed by another), and stays so.
    began = first
    for _ in range(50):
        if len(pieces) >= len(expected) or status != 200:
            break
        time.sleep(0.1)
        status, body, _, away = play(at, base)
        text = body.decode("utf-8", "replace")
        pieces, first = listed(text)
    want(status == 200 and len(pieces) == len(expected) and first == began and "#EXT-X-ENDLIST" not in text,
         "its playlist names the %d pieces the stand-in's keyframes make, from the same first number, and has no end mark (%d pieces, first %s then %s)"
         % (len(expected), len(pieces), began, first))
    lengths = [l.strip() for l in text.splitlines() if l.startswith("#EXTINF:")]
    want(lengths and all(l == "#EXTINF:4.000," for l in lengths) and "#EXT-X-TARGETDURATION:4\n" in text,
         "every piece is given as 4 s long, the time from one keyframe to the next (%s)" % ", ".join(sorted(set(lengths))))
    full = [urljoin(at, p) for p in pieces]
    off = [a for a in full if urlparse(a).netloc != urlparse(base).netloc]
    want(not off, "every piece's address, worked out the way a player does, leads to the proxy (%s)"
         % (off[0] if off else "%d of %d" % (len(full), len(full))))
    same, tables = 0, 0
    for i, a in enumerate(full):
        s, data, _, away = play(a, base)
        same += s == 200 and not away and i < len(expected) and data == expected[i]
        tables += len(data) > 376 and data[1] & 0x1F == 0 and data[2] == 0 and data[189:191] == b"\x50\x00"
    want(tables == len(full) and tables > 0, "every piece starts with the program tables (%d of %d)" % (tables, len(full)))
    want(same == len(full) and same > 0,
         "the pieces are the stand-in's video and sound packet for packet, cut at its keyframes (%d of %d)" % (same, len(full)))
    # A recorder reads the same channel as MPEG-TS meanwhile: it shares the HLS viewer's connection.
    got, sent = b"", real_ts()
    try:
        with urllib.request.urlopen("%s/live/%s/%s/3.ts" % (base, EASY_USER, EASY_PASS), timeout=6) as r:
            while len(got) < 188 * 1000:
                chunk = r.read1(1 << 16)
                if not chunk:
                    break
                got += chunk
    except (OSError, urllib.error.HTTPError) as e:
        want(False, "a recorder reads the same channel as MPEG-TS (%s)" % e)
    want(len(got) >= 188 * 1000 and got[:188 * 1000] == sent[:188 * 1000],
         "a recorder on the same channel is sent its MPEG-TS from the start, byte for byte (%d bytes)" % len(got))
    status, body, _, _ = play(at, base)
    want(status == 200 and listed(body.decode("utf-8", "replace")) == (pieces, began),
         "asked for again, the playlist is the same: the stand-in has sent nothing more (%d)" % status)
    n = json.loads(get(provider + "/smoke/counts")[1])
    want(n["real_ts"] == 1 and n["real_m3u8"] == 0,
         "the provider was asked for that channel once, as MPEG-TS, for both devices, and never as HLS (%d and %d)" % (n["real_ts"], n["real_m3u8"]))


def check(base, hls="", provider=""):
    bad = []

    def want(ok, what):
        print(("ok    " if ok else "FAIL  ") + what, flush=True)
        if not ok:
            bad.append(what)

    for _ in range(60):  # the proxy part takes a moment to start
        try:
            get(base + "/status", 2)
            break
        except OSError:
            time.sleep(0.5)

    status, body = get("%s/player_api.php?username=%s&password=%s" % (base, EASY_USER, EASY_PASS))
    want(status == 200 and b"user_info" in body, "a player logs in with the easy login (%d)" % status)
    want(REAL_USER.encode() not in body and REAL_PASS.encode() not in body,
         "the answer to the login does not carry the provider's own login")

    status, body = get("%s/player_api.php?username=%s&password=wrong" % (base, EASY_USER))
    want(b'"auth":1' not in body.replace(b" ", b"") and REAL_USER.encode() not in body,
         "a wrong password is not let in (%d)" % status)

    status, _ = get("%s/live/%s/wrong/1.ts" % (base, EASY_USER))
    want(status != 200, "a channel asked for with a wrong password is refused (%d)" % status)

    status, body = get(base + "/status")
    want(status == 200 and len(body) > 500, "the status page opens (%d, %d bytes)" % (status, len(body)))

    # The live channel, watched for 9 s: through the opening burst, the lumps and the long pause.
    got, first, start = bytearray(), None, time.time()
    try:
        with urllib.request.urlopen("%s/live/%s/%s/1.ts" % (base, EASY_USER, EASY_PASS), timeout=6) as r:
            want(r.status == 200 and r.headers.get("Content-Type", "").startswith("video/"),
                 "the live channel opens (%d, %s)" % (r.status, r.headers.get("Content-Type")))
            while time.time() - start < 9:
                chunk = r.read1(1 << 16)
                if not chunk:
                    break
                if first is None:
                    first = time.time() - start
                got += chunk
    except (OSError, urllib.error.HTTPError) as e:
        want(False, "the live channel plays for 9 s without an error (%s)" % e)
    want(first is not None and first < 2, "its first video comes at once (%s s)" % (None if first is None else round(first, 2)))
    want(len(got) >= 4 << 20, "its video keeps coming through the lumps and the long pause (%.1f MB in 9 s)" % (len(got) / 1e6))
    whole = len(got) // 188
    want(whole > 0 and bytes(got[:whole * 188]) == packets(0, whole),
         "what the player was sent is byte for byte what the provider sent (%d packets)" % whole)

    if hls:
        check_hls(hls, provider, want)
        check_hls_from_ts(base, provider, want)

    print("smoke test: %s" % ("FAILED: " + "; ".join(bad) if bad else "all passed"), flush=True)
    return 1 if bad else 0


if __name__ == "__main__":
    if sys.argv[1:2] == ["serve"]:
        threading.Thread(target=ThreadingHTTPServer(("0.0.0.0", VIDEO_PORT), Provider).serve_forever, daemon=True).start()
        ThreadingHTTPServer(("0.0.0.0", 8080), Provider).serve_forever()
    elif sys.argv[1:2] == ["check"] and len(sys.argv) == 3:
        sys.exit(check(sys.argv[2].rstrip("/")))
    elif sys.argv[1:2] == ["check"] and len(sys.argv) == 5:
        sys.exit(check(*[a.rstrip("/") for a in sys.argv[2:5]]))
    else:
        sys.exit("usage: smoke.py serve | check http://<proxy>:<port> [http://<proxy>:<its HLS port> http://<provider>:<port>]")
