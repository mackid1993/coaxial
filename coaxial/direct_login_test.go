package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A provider that lists a live HLS channel's pieces at full or root-relative addresses on its own server: the
// player's playlist must not carry the provider's login, and each address must still lead to the piece. Both
// when the provider answers the playlist itself and when it sends the channel to another address of its own;
// the same for a film it sends on within its own server.
func TestPlaylistKeepsLoginHidden(t *testing.T) {
	var asked []string
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		switch {
		case strings.HasPrefix(r.URL.Path, "/live/realU/realP/13.m3u8"): // sent on, as Xtream servers do
			http.Redirect(w, r, "/hlsr/tok/realU/realP/13/index.m3u8", 302)
		case r.URL.Path == "/movie/realU/realP/8.mkv": // a film sent on within the provider's own server
			http.Redirect(w, r, "http://"+r.Host+"/vod/realU/realP/8.mkv?username=realU&password=realP&t=1", 302)
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			host := "http://" + r.Host
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n"+
				"#EXTINF:4,\n"+host+"/live/realU/realP/12_1.ts\n"+
				"#EXTINF:4,\n/live/realU/realP/12_2.ts\n"+
				"#EXTINF:4,\n//"+r.Host+"/live/realU/realP/12_3.ts\n"+
				"#EXTINF:4,\n12_4.ts\n"+
				"#EXTINF:4,\n/hls/12_5.ts?username=realU&password=realP\n"+
				"#EXTINF:4,\n"+host+"/hlsr/tok/realU/realP/12_6.ts\n")
		default:
			io.WriteString(w, "piece "+r.URL.Path)
		}
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f := newFront(provider{Name: "Direct", URL: prov.URL, User: "realU", Password: "realP", Port: "18501", LiveTV: "hls"},
		player{User: "tv", Password: "easy"})
	do := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "192.0.2.10:5000"
		f.ServeHTTP(w, r)
		return w
	}
	for _, open := range []string{"/live/tv/easy/12.m3u8", "/live/tv/easy/13.m3u8"} {
		w := do(open)
		for w.Code == 302 {
			w = do(w.Header().Get("Location"))
		}
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "#EXTINF") {
			t.Fatalf("%s: %d %q", open, w.Code, body)
		}
		if strings.Contains(body, "realU") || strings.Contains(body, "realP") {
			t.Errorf("%s: the provider's login is written into the player's playlist:\n%s", open, body)
		}
		// every address still leads to its piece at the provider (through the front door's login swap)
		for _, line := range strings.Split(body, "\n") {
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "/__cdn/") {
				continue
			}
			asked = nil
			if pw := do(line); pw.Code != 200 || !strings.HasPrefix(pw.Body.String(), "piece ") || len(asked) != 1 ||
				!strings.Contains(asked[0], "realP") {
				t.Errorf("%s: %d %q, provider asked %v", line, pw.Code, pw.Body.String(), asked)
			}
		}
	}
	w := do("/movie/tv/easy/8.mkv")
	loc := w.Header().Get("Location")
	if w.Code != 302 || strings.Contains(loc, "realU") || strings.Contains(loc, "realP") {
		t.Fatalf("a film sent on: %d, Location %q", w.Code, loc)
	}
	asked = nil
	if w = do(loc); w.Code != 200 || len(asked) != 1 || asked[0] != "/vod/realU/realP/8.mkv?username=realU&password=realP&t=1" {
		t.Errorf("following %s: %d, provider asked %v", loc, w.Code, asked)
	}
}

// publicListener: a listener on an address of this machine that is not a home network's (the provider's
// redirects are refused toward those); the test is skipped where there is none.
func publicListener(t *testing.T) net.Listener {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !isPrivate(n.IP.String()) {
			if ln, err := net.Listen("tcp", n.IP.String()+":0"); err == nil {
				return ln
			}
		}
	}
	t.Skip("no address here outside the home-network ranges")
	return nil
}
