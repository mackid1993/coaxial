package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The launcher asks the proxy part whether anything plays every 20 s, day and night (the watch loop), and
// before each server it tries. That must not ask a provider anything: its account and its channel list are
// for the page, while someone looks at it.
func TestAskingWhatPlaysAsksNoProvider(t *testing.T) {
	var hits atomic.Int32
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"user_info":{"auth":1,"status":"Active"}}`)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	ln, err := net.Listen("unix", insideSocket)
	if err != nil {
		t.Skip("the proxy part's socket is in use here")
	}
	srv := &http.Server{Handler: http.HandlerFunc(insideStatus)}
	go srv.Serve(ln)
	defer srv.Close()
	mu.Lock()
	keptChannels, keptRows, keptFilms := channels, hlsRows, films // (what other tests left: not this test's business)
	channels, hlsRows, films = map[string]*channel{}, map[string]*hlsRow{}, map[string]*film{}
	fronts["18533"] = newFront(provider{Name: "P", URL: prov.URL, User: "u", Password: "p", Port: "18533", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	mu.Unlock()
	defer func() {
		mu.Lock()
		delete(fronts, "18533")
		delete(accounts, "18533")
		delete(channelNames, "18533")
		channels, hlsRows, films = keptChannels, keptRows, keptFilms
		mu.Unlock()
	}()
	for i := 0; i < 3; i++ {
		if !nothingPlaying() || someonePlaying() {
			t.Fatal("nothing plays, and that was not what the answer said")
		}
	}
	time.Sleep(2 * time.Second) // (a provider is asked in the background)
	if n := hits.Load(); n != 0 {
		t.Errorf("asking what plays asked the provider %d times", n)
	}
	ch := &channel{key: "/ts test 1", readers: map[*reader]bool{{kind: "MPEG-TS", who: "192.0.2.7"}: true}}
	mu.Lock()
	channels[ch.key] = ch
	mu.Unlock()
	if nothingPlaying() || !someonePlaying() {
		t.Error("a reader on a channel was not seen")
	}
}
