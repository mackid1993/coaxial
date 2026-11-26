package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A playlist its server sends packed with gzip although it was asked for plain text. For a playlist the player's
// Accept-Encoding is not passed on, so a server that packs only when asked sends plain text. Some pack all the
// same (a playlist kept packed on disk, a server set to pack everything), and a player asking such a server
// itself unpacks what it gets. Through the proxy the player must get a playlist it can read: plain text, with
// every address in it pointed back at the proxy.
func TestHLSPlaylistSentPacked(t *testing.T) {
	packed := func(w http.ResponseWriter, status int, text string) { // (packed whatever was asked for)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(status)
		z := gzip.NewWriter(w)
		io.WriteString(z, text)
		z.Close()
	}
	const media = "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:4,\np7.ts\n#EXTINF:4,\np8.ts\n"
	cdn := newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v/tok/index.m3u8": // a list of qualities
			packed(w, 200, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000\nhd/index.m3u8\n")
		case "/v/tok/hd/index.m3u8":
			packed(w, 200, media)
		default:
			return false
		}
		return true
	})
	prov := newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/live/realU/realP/21.m3u8": // sent on to a video server
			http.Redirect(w, r, cdn.srv.URL+"/v/tok/index.m3u8", 302)
		case "/live/realU/realP/22.m3u8": // answered by the provider itself
			packed(w, 200, media)
		case "/live/realU/realP/23.m3u8": // refused, with a page that names the address asked for
			packed(w, 403, "No access to /live/realU/realP/23.m3u8\n")
		default:
			return false
		}
		return true
	})
	_, base := limFront(t, prov, "hls", "", advanced{})
	at := "/__cdn/http/" + cdn.srv.Listener.Addr().String()
	for _, c := range []struct{ what, path, want string }{
		{"a channel the provider sends on to a video server", "/live/tv/easy/21.m3u8", at + "/v/tok/hd/index.m3u8\n"},
		{"the quality that one lists", at + "/v/tok/hd/index.m3u8", at + "/v/tok/hd/p7.ts\n"},
		{"a channel the provider answers itself", "/live/tv/easy/22.m3u8", "\n/live/tv/easy/p7.ts\n"},
	} {
		status, data, err := devGet(base, "192.0.2.21", c.path, 5*time.Second)
		if err != nil || status != 200 {
			t.Fatalf("%s: status %d, %v", c.what, status, err)
		}
		if text := string(data); !strings.HasPrefix(text, "#EXTM3U") || !strings.Contains(text, c.want) || strings.Contains(text, "realU") {
			t.Errorf("%s: its playlist came packed, and the player was given %d bytes it cannot read as a playlist "+
				"(want plain text naming %q): %q", c.what, len(data), strings.TrimSpace(c.want), text[:min(len(text), 60)])
		}
	}
	// (a refusal that came packed is passed on with its status, readable, and without the provider's login)
	status, data, err := devGet(base, "192.0.2.21", "/live/tv/easy/23.m3u8", 5*time.Second)
	if err != nil || status != 403 || string(data) != "No access to /live/tv/easy/23.m3u8\n" {
		t.Errorf("a refusal that came packed: status %d, %q, %v; want 403 and the page in plain text with the easy login", status, data, err)
	}
}
