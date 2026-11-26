#!/usr/bin/env python3
"""Smoke test of a built Coaxial image (see smoke.sh).

serve: a stand-in provider. It knows one login and one live channel, whose MPEG-TS it sends the way live
       channels come: a burst, then lumps with pauses between them, one of them long.
check: a player. Through the proxy part of the image it logs in, is refused with a wrong password, opens the
       status page and watches the channel; what it is sent must be, byte for byte, what the stand-in sent.
"""
import json
import sys
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

REAL_USER, REAL_PASS = "realU", "realP"      # the stand-in provider's login (the proxy's settings carry it)
EASY_USER, EASY_PASS = "tv", "easy"          # the login players use at the proxy


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

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
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


def check(base):
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

    print("smoke test: %s" % ("FAILED: " + "; ".join(bad) if bad else "all passed"), flush=True)
    return 1 if bad else 0


if __name__ == "__main__":
    if sys.argv[1:2] == ["serve"]:
        ThreadingHTTPServer(("0.0.0.0", 8080), Provider).serve_forever()
    elif sys.argv[1:2] == ["check"] and len(sys.argv) == 3:
        sys.exit(check(sys.argv[2].rstrip("/")))
    else:
        sys.exit("usage: smoke.py serve | check http://<proxy>:<port>")
