package main

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// keepProvider: a front on a fake provider whose account answer gives max (or a 503 for the first `fails` asks).
func keepProvider(t *testing.T, name, pt, max string, fails int) (*front, *fakeProv) {
	n := 0
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/player_api.php" {
			return false
		}
		fp.mu.Lock()
		n++
		k := n
		fp.mu.Unlock()
		if k <= fails {
			http.Error(w, "busy", 503)
			return true
		}
		io.WriteString(w, `{"user_info":{"auth":1,"status":"Active","max_connections":"`+max+`","active_cons":"0"},"server_info":{}}`)
		return true
	})
	f := newFront(provider{Name: name, URL: fp.srv.URL, User: "u", Password: "p", Port: port(pt), LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	mu.Lock()
	fronts[f.port] = f
	delete(accounts, f.port)
	mu.Unlock()
	return f, fp
}

func waitLimit(t *testing.T, f *front, fp *fakeProv, want int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		got := f.limit()
		mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(end) {
			fp.mu.Lock()
			asked := fp.hits["/player_api.php"]
			fp.mu.Unlock()
			t.Fatalf("limit %d, want %d (provider asked %d times)", got, want, asked)
		}
	}
}

// The connection limit is learned in the background when Coaxial starts, before anyone opens the status page or tunes,
// with one request.
func TestKeepAccountsLearnsLimit(t *testing.T) {
	f, fp := keepProvider(t, "Keep", "18631", "3", 0)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { keepAccounts(stop); close(done) }()
	waitLimit(t, f, fp, 3)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-done
	fp.mu.Lock()
	asked := fp.hits["/player_api.php"]
	fp.mu.Unlock()
	if asked != 1 {
		t.Errorf("the provider was asked %d times, want 1", asked)
	}
	time.Sleep(100 * time.Millisecond) // (the answer's own goroutine finishes)
	mu.Lock()
	delete(fronts, f.port)
	delete(accounts, f.port)
	mu.Unlock()
}

// A provider that does not answer at first is asked again soon (not in 10 minutes), and the limit is learned then.
func TestKeepAccountsRetriesAtStart(t *testing.T) {
	oldR := accountRetry
	accountRetry = 100 * time.Millisecond
	defer func() { accountRetry = oldR }()
	f, fp := keepProvider(t, "Keep2", "18632", "2", 1)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { keepAccounts(stop); close(done) }()
	// account() waits a minute after a failed answer before it asks again; let this test skip that wait
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		st := accounts[f.port]
		failed := st != nil && st.task == nil && st.fails > 0
		if failed {
			st.tried = -1e9
		}
		mu.Unlock()
		if failed {
			break
		}
	}
	waitLimit(t, f, fp, 2)
	close(stop)
	<-done
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	delete(fronts, f.port)
	delete(accounts, f.port)
	mu.Unlock()
}
