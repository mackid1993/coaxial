package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// With the web page open, the page asks /api/status every 2 s and the launcher asks the proxy part's /status
// (statusData) each time. Rule 1 and incident 20: nothing asks a provider anything while a channel plays. The
// account read already waits for that (344c463); the channel-name list (player_api.php with the provider's
// login and action=get_live_streams, the whole live list) must wait the same way.
func TestStatusPageAsksNoProviderWhilePlaying(t *testing.T) {
	var hits atomic.Int32
	var last atomic.Value
	last.Store("")
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store("action=" + r.URL.Query().Get("action"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("action") == "get_live_streams" {
			io.WriteString(w, `[{"stream_id":1,"name":"One"}]`)
			return
		}
		io.WriteString(w, `{"user_info":{"auth":1,"status":"Active"}}`)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	const port = "18761"
	ch := &channel{key: "/ts bgtest 1", provider: prov.URL, readers: map[*reader]bool{{kind: "MPEG-TS", who: "192.0.2.7", port: port, id: "1"}: true}}
	mu.Lock()
	keptChannels, keptRows, keptFilms := channels, hlsRows, films // (what other tests left: not this test's business)
	channels, hlsRows, films = map[string]*channel{ch.key: ch}, map[string]*hlsRow{}, map[string]*film{}
	fronts[port] = newFront(provider{Name: "P", URL: prov.URL, User: "u", Password: "p", Port: port, LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	// The exit address and the ping are kept fresh, so this test asks nothing of the internet.
	keptWhere, keptPing := keptValues["where"], keptValues["ping"]
	keptValues["where"] = &keptValue{at: mono(), value: map[string]any{"ip": "192.0.2.1"}}
	keptValues["ping"] = &keptValue{at: mono(), value: 20.0}
	mu.Unlock()
	defer func() {
		mu.Lock()
		delete(fronts, port)
		delete(accounts, port)
		delete(channelNames, port)
		channels, hlsRows, films = keptChannels, keptRows, keptFilms
		keptValues["where"], keptValues["ping"] = keptWhere, keptPing
		mu.Unlock()
	}()
	for i := 0; i < 3; i++ { // three polls of the open page
		statusData()
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // (the provider is asked in the background)
	if n := hits.Load(); n != 0 {
		t.Errorf("a channel plays and the open status page asked the provider %d time(s) (last: %s)", n, last.Load())
	}
	// Control: with nothing playing the page does ask (so the fake was reachable and the test is not empty).
	mu.Lock()
	delete(channels, ch.key)
	delete(channelNames, port)
	mu.Unlock()
	statusData()
	for end := time.Now().Add(3 * time.Second); hits.Load() == 0 && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Errorf("control: with nothing playing, the status page asked the provider nothing (the fake was not reached)")
	}
}
