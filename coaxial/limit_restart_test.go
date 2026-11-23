package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The connections a provider reports are learned when a player logs in, and every save restarts the proxy
// part. The next start is handed what was learned (the launcher keeps it), per account: two accounts on one
// server stay apart, and an account nobody logged in to stays unknown.
func TestLimitKeptAcrossRestart(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/player_api.php" {
			return false
		}
		n := map[string]string{"one": "2", "two": "5"}[r.URL.Query().Get("username")]
		io.WriteString(w, `{"user_info":{"username":"x","max_connections":"`+n+`","active_cons":"0"},"server_info":{"url":"x"}}`)
		return true
	})
	var ports [2]string
	for i := range ports {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		_, ports[i], _ = net.SplitHostPort(ln.Addr().String())
		ln.Close()
	}
	conf := `{"player":{"user":"tv","password":"easy"},"providers":[` +
		`{"name":"One","url":"` + fp.srv.URL + `","user":"one","password":"p","port":` + ports[0] + `,"live_tv":"ts"},` +
		`{"name":"Two","url":"` + fp.srv.URL + `","user":"two","password":"p","port":` + ports[1] + `,"live_tv":"ts"}]}`
	// start: the proxy part running; what it returns stops it and gives the limits it had then.
	start := func() func() (int, int) {
		stop, done := make(chan struct{}), make(chan struct{})
		go func() { runProxy(strings.NewReader(conf), stop); close(done) }()
		for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if c, err := net.Dial("tcp", "127.0.0.1:"+ports[1]); err == nil {
				c.Close()
				break
			}
		}
		return func() (int, int) {
			close(stop)
			<-done
			mu.Lock()
			defer mu.Unlock()
			a, b := fronts[ports[0]].limit(), fronts[ports[1]].limit()
			delete(fronts, ports[0])
			delete(fronts, ports[1])
			return a, b
		}
	}
	t.Setenv("COAXIAL_LIMITS", "")
	end := start()
	r, err := devClient.Get("http://127.0.0.1:" + ports[0] + "/player_api.php?username=tv&password=easy")
	if err != nil {
		end()
		t.Fatal(err)
	}
	body(r)
	w := httptest.NewRecorder() // (what the launcher asks before it stops the proxy part)
	insideStatus(w, httptest.NewRequest("GET", "/limits", nil))
	carried := w.Body.String()
	if a, b := end(); a != 2 || b != 0 {
		t.Fatalf("learned from the login: %d and %d, want 2 and 0", a, b)
	}
	t.Setenv("COAXIAL_LIMITS", learned("")+","+carried) // (as the launcher hands it on)
	if a, b := start()(); a != 2 || b != 0 {
		t.Errorf("after a restart: limits %d and %d, want 2 (as learned) and 0 (never reported)", a, b)
	}
	if strings.Contains(carried, "one") || strings.Contains(carried, fp.srv.URL) {
		t.Errorf("what is handed on names the account: %q", carried)
	}
	if m := learnedBefore("aa=3,bb=1," + "bb=4,cc=x,=2"); len(m) != 2 || m["aa"] != 3 || m["bb"] != 4 {
		t.Errorf("what was kept, merged with what is new: %v", m)
	}
}
