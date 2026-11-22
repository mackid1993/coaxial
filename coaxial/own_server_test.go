package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The provider's own server written another way than in the settings (the port 80 or 443 written out or
// left out, capitals, a final dot) is still its own server: its addresses come back with the easy login,
// never with the real one under /__cdn/.
func TestOwnServerHoweverWritten(t *testing.T) {
	for _, c := range []struct{ setting, written string }{
		{"http://prov.example", "http://prov.example:80"},
		{"http://prov.example:80", "http://prov.example"},
		{"https://prov.example:443", "https://prov.example"},
		{"https://prov.example", "https://PROV.example.:443"},
		{"http://prov.example:8080", "http://Prov.Example.:8080"},
	} {
		f := newFront(provider{Name: "P", URL: c.setting, User: "realU", Password: "realP", Port: "18502", LiveTV: "hls"},
			player{User: "tv", Password: "easy"})
		text := "#EXTM3U\n#EXTINF:4,\n" + c.written + "/live/realU/realP/12_1.ts\n#EXTINF:4,\n" + c.written + "/hlsr/tok/realU/realP/12_2.ts\n"
		out, err := rewritePlaylist(text, c.setting+"/live/realU/realP/12.m3u8", f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "realP") || !strings.Contains(out, "\n/live/tv/easy/12_1.ts\n") || !strings.Contains(out, "\n/hlsr/tok/tv/easy/12_2.ts\n") {
			t.Errorf("provider %s, playlist writes %s:\n%s", c.setting, c.written, out)
		}
	}
	// another port, another scheme or another name is another server: left under /__cdn/ as before
	f := newFront(provider{Name: "P", URL: "http://prov.example", User: "realU", Password: "realP", Port: "18502", LiveTV: "hls"},
		player{User: "tv", Password: "easy"})
	for _, other := range []string{"http://prov.example:8080", "https://prov.example", "http://cdn.prov.example"} {
		out, _ := rewritePlaylist("#EXTM3U\n"+other+"/x/1.ts\n", "http://prov.example/live/realU/realP/12.m3u8", f)
		if !strings.Contains(out, "/__cdn/") {
			t.Errorf("%s taken for the provider's own server: %q", other, out)
		}
	}

	// A redirect to the provider's own server written with a final dot.
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/movie/realU/realP/8.mkv" {
			host, port, _ := strings.Cut(r.Host, ":")
			http.Redirect(w, r, "http://"+host+".:"+port+"/vod/realU/realP/8.mkv", 302)
			return
		}
		io.WriteString(w, "film")
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	f = newFront(provider{Name: "P", URL: prov.URL, User: "realU", Password: "realP", Port: "18503", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/movie/tv/easy/8.mkv", nil)
	r.RemoteAddr = "192.0.2.10:5000"
	f.ServeHTTP(w, r)
	if loc := w.Header().Get("Location"); w.Code != 302 || loc != "/vod/tv/easy/8.mkv" {
		t.Errorf("redirect within the provider's own server: %d, Location %q", w.Code, loc)
	}
}
