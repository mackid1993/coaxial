package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A provider's account is read only when the page is open and nothing plays (a login during a stream is what a
// provider reads as abuse). So after a start under a stream there is no account answer yet, and the page said
// "Down" for a provider whose channel was playing at that moment. A provider that is sending video is up: the
// page says so, and still asks the provider nothing while the channel plays.
func TestProviderWithAChannelPlayingIsShownUp(t *testing.T) {
	var hits atomic.Int32
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"user_info":{"auth":1,"status":"Active"}}`)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	const at = "18781"
	ch := &channel{key: "/ts " + prov.URL + " uptest 1", provider: prov.URL, readers: map[*reader]bool{{kind: "MPEG-TS", who: "192.0.2.7", port: at, id: "1"}: true}}
	mu.Lock()
	keptChannels, keptRows, keptFilms := channels, hlsRows, films // (what other tests left: not this test's business)
	channels, hlsRows, films = map[string]*channel{ch.key: ch}, map[string]*hlsRow{}, map[string]*film{}
	fronts[at] = newFront(provider{Name: "P", URL: prov.URL, User: "u", Password: "p", Port: at, LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	delete(accounts, at)
	// The exit address and the ping are kept fresh, so this test asks nothing of the internet.
	keptWhere, keptPing := keptValues["where"], keptValues["ping"]
	keptValues["where"] = &keptValue{at: mono(), value: map[string]any{"ip": "192.0.2.1"}}
	keptValues["ping"] = &keptValue{at: mono(), value: 20.0}
	mu.Unlock()
	defer func() {
		mu.Lock()
		delete(fronts, at)
		delete(accounts, at)
		delete(channelNames, at)
		channels, hlsRows, films = keptChannels, keptRows, keptFilms
		keptValues["where"], keptValues["ping"] = keptWhere, keptPing
		mu.Unlock()
	}()
	var up, found bool
	for _, p := range statusData()["providers"].([]any) {
		if m := p.(map[string]any); m["port"] == 18781 {
			up, found = m["up"] == true, true
		}
	}
	if !found {
		t.Fatalf("the provider is not in the status data")
	}
	if !up {
		t.Errorf("a channel of the provider is playing and the page is told the provider is down")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a channel plays and the provider was asked %d time(s) to find that out", n)
	}
}
