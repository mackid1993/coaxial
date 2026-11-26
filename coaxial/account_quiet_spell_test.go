package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// With the web page open, the provider's account is read again when a read is due (every 30 minutes) and
// nothing plays. "Nothing plays right now" was the whole test: a viewer changing channel on MPEG-TS has no
// channel open for a moment, and one whose player dropped out is on its way back. The page asks every 2 s, so
// the login to the provider went out in that gap, just as the viewer tuned again (rule 1; the ping round and
// the update check already wait for a quiet spell for the same reason). The first read after a start is still
// made at once when nothing plays.
func TestAccountReadWaitsForAQuietSpell(t *testing.T) {
	var logins atomic.Int32
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") == "" {
			logins.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"user_info":{"auth":1,"status":"Active"}}`)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	defer prov.Close()
	const port = "18775"
	ch := &channel{key: "/ts quiettest 1", provider: prov.URL, readers: map[*reader]bool{{kind: "MPEG-TS", who: "192.0.2.7", port: port, id: "1"}: true}}
	keptEvery, keptQuiet := accountEvery, quietSpell
	accountEvery, quietSpell = 0.2, 3*time.Second // (30 minutes and 5 minutes, shortened)
	mu.Lock()
	keptChannels, keptRows, keptFilms := channels, hlsRows, films // (what other tests left: not this test's business)
	channels, hlsRows, films = map[string]*channel{}, map[string]*hlsRow{}, map[string]*film{}
	fronts[port] = newFront(provider{Name: "P", URL: prov.URL, User: "u", Password: "p", Port: port, LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	delete(accounts, port)
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
		accountEvery, quietSpell = keptEvery, keptQuiet
	}()
	polls := func(d time.Duration) { // the open page, asking as it does (faster here)
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			statusData()
		}
	}

	polls(time.Second)
	if n := logins.Load(); n == 0 {
		t.Fatalf("nothing plays and the page is opened for the first time: the account was not read")
	}
	mu.Lock()
	channels[ch.key] = ch // a viewer tunes in
	mu.Unlock()
	polls(time.Second)
	time.Sleep(500 * time.Millisecond) // (a read started just before the viewer came is over)
	before := logins.Load()
	polls(time.Second) // watching, with a read long due
	if n := logins.Load() - before; n != 0 {
		t.Fatalf("a channel plays and the account was read %d time(s)", n)
	}
	mu.Lock()
	delete(channels, ch.key) // the viewer changes channel, or its player drops out
	mu.Unlock()
	polls(1500 * time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	if n := logins.Load() - before; n != 0 {
		t.Errorf("the last channel closed and within 1.5 s the provider was logged in to %d time(s): the quiet spell (%v here, 5 minutes for real) was not waited for", n, quietSpell)
	}
	polls(4 * time.Second)
	if logins.Load() == before {
		t.Errorf("control: nothing has played for a quiet spell and the account was not read again")
	}
}
