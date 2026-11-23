package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// With no temporary folder to keep a large answer in (a full disk, a folder that cannot be written), every
// player asking for it is still given all of it, as with the option off: those following the download and
// those asking afterwards. It is downloaded again for each, never given broken.
func TestShareNoTempFolder(t *testing.T) {
	t.Setenv("TMPDIR", "/nonexistent/coaxial-share-test")
	big := bytes.Repeat([]byte(`{"name":"x","stream_icon":"http://img.example/a.png"},`), 120000) // (about 6 MB)
	slow := make(chan struct{})
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("action") != "get_vod_streams" {
			return false
		}
		<-slow
		w.Write([]byte("["))
		w.Write(big)
		w.Write([]byte(`{}]`))
		return true
	})
	f := newFront(provider{Name: "N", URL: fp.srv.URL, User: "u", Password: "p", Port: "18631", LiveTV: "ts", Advanced: advanced{Share: true}},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	list := srv.URL + "/player_api.php?username=tv&password=easy&action=get_vod_streams"
	get := func() string {
		r, err := devClient.Get(list)
		if err != nil {
			return err.Error()
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err.Error()
		}
		return string(b)
	}
	var wg sync.WaitGroup
	got := make([]string, 3)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i] = get() }()
	}
	eventually(t, func() bool { return fp.count(fp.hits, "/player_api.php?get_vod_streams") >= 1 }, "the download starting")
	close(slow)
	wg.Wait()
	got = append(got, get()) // (afterwards: nothing kept, asked again)
	for i, g := range got {
		if len(g) < len(big) || !strings.HasSuffix(g, "{}]") || !strings.Contains(g, "/__cdn/http/img.example/a.png") {
			t.Errorf("player %d was given %d bytes of the large list (%.60q)", i, len(g), g)
		}
	}
}
