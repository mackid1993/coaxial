package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A player is sent to this proxy by another server (a guide server that redirects every stream here), asks
// for a catch-up as HLS, and before playing it checks that the first piece the playlist names can be had. It
// works that piece's address out from the address it first asked, the other server's. A provider's own
// catch-up playlist names its pieces in full, so the check finds them wherever the player started from. The
// playlist this proxy hands out must do the same: every piece is named in full, at the proxy's own address.
// Named by path alone ("/__cdn/..."), the piece was looked for on the other server, was not there, and the
// catch-up was taken for missing.
func TestCatchUpPlaylistNamesPiecesInFull(t *testing.T) {
	const cu = "/timeshift/realU/realP/60/2026-01-01:10-00/7.m3u8"
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case cu:
			http.Redirect(w, r, "/arch/tok7/7.m3u8", 302)
		case "/arch/tok7/7.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\npiece1.ts\n#EXTINF:10,\npiece2.ts\n#EXT-X-ENDLIST\n")
		case "/arch/tok7/piece1.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(make([]byte, 188*50))
		case "/live/realU/realP/8.m3u8":
			http.Redirect(w, r, "/hls/tok8/8.m3u8", 302)
		case "/hls/tok8/8.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:5\n#EXTINF:10,\nlive5.ts\n")
		default:
			return false
		}
		return true
	})
	_, base := advFront(t, fp, "hls", advanced{})
	proxy, _ := url.Parse(base)
	// The player asks the address the other server sent it to, and is sent on to the playlist.
	r := ask(t, base, "192.0.2.1", strings.Replace(cu, "realU/realP", "tv/easy", 1))
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, "192.0.2.1", loc)
	}
	text := body(r)
	if r.StatusCode != 200 || !strings.Contains(text, "#EXT-X-ENDLIST") {
		t.Fatalf("the catch-up's playlist: %d %q", r.StatusCode, text)
	}
	var first string
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") && first == "" {
			first = line
		}
	}
	// The player's check: the first piece's address, worked out from the address it first asked.
	guide, _ := url.Parse("http://guide.invalid:8001" + strings.Replace(cu, "realU/realP", "tv/easy", 1))
	piece, err := guide.Parse(first)
	if err != nil {
		t.Fatalf("the first piece %q: %v", first, err)
	}
	if piece.Host != proxy.Host {
		t.Fatalf("the playlist names its first piece %q: worked out from the address the player first asked, that is on %q, not on this proxy (%q)",
			first, piece.Host, proxy.Host)
	}
	if strings.Contains(text, "realU") || strings.Contains(text, "realP") {
		t.Errorf("the playlist carries the provider's login: %q", text)
	}
	p := ask(t, base, "192.0.2.1", piece.RequestURI(), "Range", "bytes=0-2047")
	p.Body.Close()
	if p.StatusCode != 200 && p.StatusCode != 206 {
		t.Errorf("the first piece, at the address the playlist gives: %d", p.StatusCode)
	}
	// A live playlist is handed out as before.
	l := ask(t, base, "192.0.2.2", "/live/tv/easy/8.m3u8")
	for l.StatusCode == 302 {
		loc := l.Header.Get("Location")
		l.Body.Close()
		l = ask(t, base, "192.0.2.2", loc)
	}
	live := body(l)
	if !strings.Contains(live, "#EXTINF") || strings.Contains(live, "http://") {
		t.Errorf("a live playlist should be as it was (addresses by path): %q", live)
	}
}
