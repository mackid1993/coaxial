package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The connections a provider allows are the number the user typed in. With 2 typed in, two devices on two
// films play, a third device is refused before the provider is asked (it would have cut someone off), and
// once one of the two stops, the third gets in.
func TestTypedInLimitAcrossDevices(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := limFront(t, fp, "ts", "2", advanced{})
	a := ask(t, base, "192.0.2.1", "/movie/tv/easy/1.mkv")
	b := ask(t, base, "192.0.2.2", "/movie/tv/easy/2.mkv")
	if a.StatusCode != 200 || b.StatusCode != 200 {
		t.Fatalf("two devices under a limit of 2: %d %d", a.StatusCode, b.StatusCode)
	}
	c := ask(t, base, "192.0.2.3", "/movie/tv/easy/3.mkv")
	if text := body(c); c.StatusCode != 503 || !strings.Contains(text, "All 2 connections") {
		t.Errorf("a third device over a limit of 2: %d %q", c.StatusCode, text)
	}
	if fp.count(fp.hits, "/movie/realU/realP/3.mkv") != 0 {
		t.Errorf("the provider was asked for the refused stream")
	}
	a.Body.Close()
	eventually(t, func() bool { return fp.count(fp.open, "") == 1 }, "the first film closing")
	var again *http.Response
	eventually(t, func() bool {
		if again != nil {
			again.Body.Close()
		}
		again = ask(t, base, "192.0.2.3", "/movie/tv/easy/3.mkv")
		return again.StatusCode == 200
	}, "the third device getting in once there is room")
	again.Body.Close()
	b.Body.Close()
	if n := fp.count(fp.most, ""); n != 2 {
		t.Errorf("the provider saw %d streams at once, want at most 2", n)
	}
}

// No number typed in (an older settings file): nothing is refused for the limit, and the provider's own
// figure is never used, however often a player logs in. Turned off, a number typed in is not used either.
func TestNoNumberNoLimit(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/player_api.php" {
			io.WriteString(w, `{"user_info":{"username":"realU","password":"realP","max_connections":"1","active_cons":"0"},"server_info":{"url":"x"}}`)
			return true
		}
		return false
	})
	for _, c := range []struct {
		name        string
		connections number
		a           advanced
	}{{"no number", "", advanced{}}, {"switched off", "1", advanced{StayUnder: new(bool)}}} {
		t.Run(c.name, func(t *testing.T) {
			f, base := limFront(t, fp, "ts", c.connections, c.a)
			if r := ask(t, base, "192.0.2.1", "/player_api.php?username=tv&password=easy"); r.StatusCode != 200 {
				t.Fatalf("login: %d", r.StatusCode)
			} else {
				body(r)
			}
			var open []*http.Response
			for i, dev := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
				r := ask(t, base, dev, "/movie/tv/easy/"+string(rune('1'+i))+".mkv")
				if r.StatusCode != 200 {
					t.Errorf("device %s refused with no limit kept: %d %q", dev, r.StatusCode, body(r))
					continue
				}
				open = append(open, r)
			}
			mu.Lock()
			lim := f.limit()
			mu.Unlock()
			if lim != 0 {
				t.Errorf("limit %d, want 0 (none kept)", lim)
			}
			for _, r := range open {
				r.Body.Close()
			}
			eventually(t, func() bool { return fp.count(fp.open, "") == 0 }, "the films closing")
		})
	}
}

// The proxy part as it runs: a provider with a number typed in keeps it, one without keeps none, and the
// account the status page reads (which names other numbers) changes neither. The status data and the start
// notes flag the provider without a number, so the page can ask for it.
func TestTypedInLimitWithAccountReads(t *testing.T) {
	asked := 0
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/player_api.php" {
			return false
		}
		fp.mu.Lock()
		asked++
		fp.mu.Unlock()
		io.WriteString(w, `{"user_info":{"username":"x","auth":1,"status":"Active","max_connections":"5","active_cons":"0"},"server_info":{"url":"x"}}`)
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
		`{"name":"One","url":"` + fp.srv.URL + `","user":"one","password":"p","port":` + ports[0] + `,"connections":2,"live_tv":"ts"},` +
		`{"name":"Two","url":"` + fp.srv.URL + `","user":"two","password":"p","port":` + ports[1] + `,"live_tv":"ts"}]}`
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { runProxy(strings.NewReader(conf), stop); close(done) }()
	defer func() {
		close(stop)
		<-done
		mu.Lock()
		delete(fronts, ports[0])
		delete(fronts, ports[1])
		delete(accounts, ports[0])
		delete(accounts, ports[1])
		mu.Unlock()
	}()
	for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("tcp", "127.0.0.1:"+ports[1]); err == nil {
			c.Close()
			break
		}
	}
	time.Sleep(300 * time.Millisecond)
	fp.mu.Lock()
	before := asked
	fp.mu.Unlock()
	if before != 0 {
		t.Errorf("the provider's account was read %d times with nobody looking: it must never be polled", before)
	}
	if r, err := devClient.Get("http://127.0.0.1:" + ports[0] + "/player_api.php?username=tv&password=easy"); err == nil {
		body(r)
	}
	w := httptest.NewRecorder() // (what the web page shows: it reads each account)
	insideStatus(w, httptest.NewRequest("GET", "/status", nil))
	var st struct {
		Providers []struct {
			Name    string
			Missing bool `json:"connections_missing"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || len(st.Providers) != 2 {
		t.Fatalf("status: %v %s", err, w.Body.String())
	}
	for _, p := range st.Providers {
		if p.Missing != (p.Name == "Two") {
			t.Errorf("status: %s flagged as missing its number: %v", p.Name, p.Missing)
		}
	}
	mu.Lock()
	a, b := fronts[ports[0]].limit(), fronts[ports[1]].limit()
	mu.Unlock()
	if a != 2 || b != 0 {
		t.Errorf("limits %d and %d after a login and the account reads that say 5, want 2 (typed in) and 0 (none)", a, b)
	}
	notes, _ := json.Marshal(startNotes)
	if !strings.Contains(string(notes), `"connections_missing":true,"mode":"ts","name":"Two"`) ||
		!strings.Contains(string(notes), `"connections_missing":false,"mode":"ts","name":"One"`) {
		t.Errorf("start notes: %s", notes)
	}
}

// Older settings kept the number under "advanced". It is moved to the provider and written there at the next
// save; 0 there meant "no limit" and turns stay_under_limit off; a number written in both places keeps the
// provider's own; anything else there is dropped without stopping the provider.
func TestOldConnectionsMoved(t *testing.T) {
	base := `"name":"One","url":"http://provider.example","user":"u","password":"p","port":8095,"live_tv":"ts"`
	load := func(extra string) provider {
		s := &settings{}
		if err := s.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[{` + base + extra + `}],"vpn":{"service_provider":"x"}}`)); err != nil {
			t.Fatal(err)
		}
		if errs := s.validate(); len(errs) != 0 {
			t.Errorf("%s: refused: %v", extra, errs)
		}
		return s.Providers[0]
	}
	for _, c := range []struct {
		extra, want string
		on          bool
	}{
		{`,"advanced":{"connections":3}`, "3", true},
		{`,"advanced":{"connections":"4","stay_under_limit":false}`, "4", false},
		{`,"advanced":{"connections":0}`, "", false},
		{`,"advanced":{"connections":""}`, "", true},
		{`,"advanced":{"connections":"lots"}`, "", true},
		{`,"advanced":{"connections":1000}`, "", true},
		{`,"connections":2,"advanced":{"connections":5}`, "2", true},
		{`,"connections":2`, "2", true},
	} {
		p := load(c.extra)
		if string(p.Connections) != c.want || p.Advanced.limitOn() != c.on || p.Advanced.OldConnections != "" {
			t.Errorf("%s: connections %q stay_under_limit %v, want %q %v", c.extra, p.Connections, p.Advanced.limitOn(), c.want, c.on)
		}
	}
	s := &settings{}
	s.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[{` + base + `,"advanced":{"connections":3}}]}`))
	file := t.TempDir() + "/settings.json"
	if err := s.save(file); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	var saved struct {
		Providers []map[string]any
	}
	json.Unmarshal(data, &saved)
	adv, _ := saved.Providers[0]["advanced"].(map[string]any)
	if _, old := adv["connections"]; old || saved.Providers[0]["connections"] != 3.0 {
		t.Errorf("not saved in the new place: %s", data)
	}
	f := newFront(s.Providers[0], s.Player)
	mu.Lock()
	lim := f.limit()
	mu.Unlock()
	if lim != 3 {
		t.Errorf("the moved number is not the limit: %d", lim)
	}
}

// Saving from the page needs a whole number of connections from 1 to 999 on every provider; a file without one
// still loads and runs (the provider starts, with no limit kept).
func TestSaveNeedsConnections(t *testing.T) {
	for _, c := range []struct {
		value any
		code  int
	}{{nil, 400}, {"", 400}, {0, 400}, {"two", 400}, {1000, 400}, {1, 200}, {"4", 200}} {
		_, code, got := put(t, pageSaved(goodSettings()), func(m map[string]any) {
			m["providers"].([]any)[1].(map[string]any)["connections"] = c.value
		})
		if code != c.code {
			t.Errorf("connections %v: saved with %d (%v), want %d", c.value, code, got, c.code)
			continue
		}
		if errs, _ := got["errors"].([]any); code == 400 && (len(errs) != 1 || errs[0].(map[string]any)["field"] != "providers[1].connections") {
			t.Errorf("connections %v: %v", c.value, got)
		}
	}
	s := goodSettings()
	s.Providers[1].Connections = ""
	good, notes := checkProviders(s)
	if len(good) != 2 {
		t.Errorf("a provider without a number did not start: %v", notes)
	}
}

// A number of connections that is written must be a whole number from 1 to 999; none written still loads
// (only a save from the page asks for it, see TestSaveNeedsConnections).
func TestValidateConnections(t *testing.T) {
	for value, want := range map[number]string{"0": "providers[1].connections", "1000": "providers[1].connections",
		"two": "providers[1].connections", "-1": "providers[1].connections", "": "", "1": "", "999": ""} {
		s := goodSettings()
		s.Providers[1].Connections = value
		var got []string
		for _, e := range s.validate() {
			got = append(got, e.Field)
		}
		if strings.Join(got, " ") != want {
			t.Errorf("connections %q: problems %v, want %q", value, got, want)
		}
	}
}
