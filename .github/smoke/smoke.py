#!/usr/bin/env python3
"""Smoke test of a built Coaxial image (see smoke.sh).

serve: a stand-in provider. It knows one login and one live channel, whose MPEG-TS it sends the way live
       channels come: a burst, then lumps with pauses between them, one of them long. It also has that channel
       as HLS and one catch-up as HLS, the way providers do: its own address sends the player on to a video
       server (here its second port), every open of the live channel there is a new session, and the newest
       session is the only one that still answers.
check: a player. Through the proxy part of the image it logs in, is refused with a wrong password, opens the
       status page and watches the channel; what it is sent must be, byte for byte, what the stand-in sent.
       Then it watches the channel as HLS and opens the catch-up, following every address the way a player
       does: none may lead anywhere but the proxy, and every piece must be what the stand-in sent.
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
counts = {"opens": 0, "playlists": 0}        # opens of the live HLS channel; asks for its playlist


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
