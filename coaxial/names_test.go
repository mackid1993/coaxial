package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// namesProvider: a stand-in provider that counts how often its whole live list is asked for.
func namesProvider(t *testing.T, at string) (lists *atomic.Int32, undo func()) {
	t.Helper()
	lists = &atomic.Int32{}
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("action") == "get_live_streams" {
			lists.Add(1)
			io.WriteString(w, `[{"stream_id":1,"name":"One"},{"stream_id":"2","name":"Two"}]`)
			return
		}
		io.WriteString(w, `{"user_info":{"auth":1,"status":"Active"}}`)
	}))
	prov.Listener = publicListener(t)
	prov.Start()
	mu.Lock()
	keptChannels, keptRows, keptFilms := channels, hlsRows, films // (what other tests left: not this test's business)
	channels, hlsRows, films = map[string]*channel{}, map[string]*hlsRow{}, map[string]*film{}
	fronts[at] = newFront(provider{Name: "P", URL: prov.URL, User: "u", Password: "p", Port: port(at), LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	delete(channelNames, at)
	mu.Unlock()
	keptFile := namesFile
	namesFile = filepath.Join(t.TempDir(), "names.json")
	return lists, func() {
		prov.Close()
		mu.Lock()
		delete(fronts, at)
		delete(accounts, at)
		delete(channelNames, at)
		channels, hlsRows, films = keptChannels, keptRows, keptFilms
		mu.Unlock()
		namesFile = keptFile
	}
}

func playOn(port string) (stop func()) {
	ch := &channel{key: "/ts namestest " + port, provider: "http://198.51.100.9", readers: map[*reader]bool{{kind: "MPEG-TS", who: "192.0.2.7", port: port, id: "1"}: true}}
	mu.Lock()
	channels[ch.key] = ch
	mu.Unlock()
	return func() { mu.Lock(); delete(channels, ch.key); mu.Unlock() }
}

// The channel names on the status page are kept in a file. A start used to lose them, and with a channel
// playing they could not be fetched again, so the page showed bare channel numbers until everything stopped.
// A port that now belongs to another provider does not get the old provider's names.
func TestChannelNamesAreKeptAcrossAStart(t *testing.T) {
	const port = "18771"
	_, undo := namesProvider(t, port)
	defer undo()
	<-fetchNames(port)
	mu.Lock()
	got := len(channelNames[port].names)
	delete(channelNames, port) // (the program starts again: nothing in memory)
	mu.Unlock()
	if got != 2 {
		t.Fatalf("fetched %d names, want 2", got)
	}
	loadNames()
	mu.Lock()
	n := channelNames[port]
	mu.Unlock()
	if n == nil || n.names["1"] != "One" || n.names["2"] != "Two" || n.saved == 0 {
		t.Fatalf("after a start the names were not read back from the file: %+v", n)
	}
	mu.Lock()
	delete(channelNames, port)
	fronts[port].name = "Q" // (the port now belongs to another provider)
	mu.Unlock()
	loadNames()
	mu.Lock()
	n = channelNames[port]
	mu.Unlock()
	if n != nil {
		t.Errorf("another provider on the same port was given the old provider's channel names")
	}
}

// A provider's whole live list is a login and a large answer. Unasked, it is fetched once a day at most, and
// only after nothing has played or recorded for an hour (shortened here): never while a channel plays, not the
// moment the last one stops, and not again once it has been fetched.
func TestChannelNamesAreFetchedOnlyAfterAnIdleHour(t *testing.T) {
	const port = "18772"
	lists, undo := namesProvider(t, port)
	defer undo()
	keptIdle, keptLook := namesIdle, namesLook
	namesIdle, namesLook = 0.6, 20*time.Millisecond
	stopPlaying := playOn(port)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { namesLoop(stop); close(done) }()
	defer func() { close(stop); <-done; namesIdle, namesLook = keptIdle, keptLook }()

	time.Sleep(1200 * time.Millisecond)
	if n := lists.Load(); n != 0 {
		t.Fatalf("a channel plays and the provider's live list was fetched %d time(s)", n)
	}
	stopPlaying()
	time.Sleep(300 * time.Millisecond)
	if n := lists.Load(); n != 0 {
		t.Errorf("0.3 s after the last channel stopped the live list was fetched %d time(s): the idle spell (0.6 s here, an hour for real) was not waited for", n)
	}
	for end := time.Now().Add(5 * time.Second); lists.Load() == 0 && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	if lists.Load() != 1 {
		t.Fatalf("control: after the idle spell the live list was fetched %d time(s), want 1", lists.Load())
	}
	time.Sleep(1500 * time.Millisecond)
	if n := lists.Load(); n != 1 {
		t.Errorf("the live list was fetched %d times: once a day is the most", n)
	}
}

// The button on a provider's card fetches its names now, whether or not something plays: the page warns first
// and the user decides. (Only what Coaxial does unasked waits for an idle hour.)
func TestNamesButtonIsNeverRefused(t *testing.T) {
	const port = "18773"
	lists, undo := namesProvider(t, port)
	defer undo()
	ask := func() (int, string) {
		w := httptest.NewRecorder()
		insideStatus(w, httptest.NewRequest("GET", "http://proxy/names-refresh?port="+port, nil))
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	stopPlaying := playOn(port)
	if code, body := ask(); code != 200 || body != `{"channels":2}` || lists.Load() != 1 {
		t.Errorf("a channel plays and the button got %d %q, with %d fetch(es); want 200 with 2 channels and one fetch", code, body, lists.Load())
	}
	stopPlaying()
	if code, body := ask(); code != 200 || body != `{"channels":2}` || lists.Load() != 2 {
		t.Errorf("nothing plays and the button got %d %q, with %d fetch(es); want 200 with 2 channels and a second fetch", code, body, lists.Load())
	}
	if code, _ := ask2(port + "9"); code != 404 {
		t.Errorf("a port no provider has: got %d, want 404", code)
	}
}

func ask2(port string) (int, string) {
	w := httptest.NewRecorder()
	insideStatus(w, httptest.NewRequest("GET", "http://proxy/names-refresh?port="+port, nil))
	return w.Code, strings.TrimSpace(w.Body.String())
}
