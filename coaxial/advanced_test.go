package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A fake provider that counts the streams open at once, and the front door served on a real port, where a
// test names the device it plays (X-Dev) so that several devices can be told apart.
// ---------------------------------------------------------------------------

type fakeProv struct {
	srv    *httptest.Server
	mu     sync.Mutex
	open   map[string]int // streams open now, by what they are ("" for all of them)
	most   map[string]int // the most open at once, the same way
	hits   map[string]int // requests, by path and action
	agents map[string]string
	reply  func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool // (true: answered)
}

func newFakeProv(t *testing.T, public bool, reply func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool) *fakeProv {
	fp := &fakeProv{open: map[string]int{}, most: map[string]int{}, hits: map[string]int{}, agents: map[string]string{}, reply: reply}
	fp.srv = httptest.NewUnstartedServer(http.HandlerFunc(fp.serve))
	if public {
		fp.srv.Listener = publicListener(t)
	}
	fp.srv.Start()
	t.Cleanup(func() { fp.srv.CloseClientConnections(); fp.srv.Close() })
	return fp
}

func (fp *fakeProv) serve(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	if a := r.URL.Query().Get("action"); a != "" {
		key += "?" + a
	}
	fp.mu.Lock()
	fp.hits[key]++
	fp.agents[key] = r.Header.Get("User-Agent")
	fp.mu.Unlock()
	if fp.reply != nil && fp.reply(fp, w, r) {
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/live/") && strings.HasSuffix(p, ".ts"):
		fp.stream(w, r, "ch"+channelID(p), true, 0)
	case strings.HasPrefix(p, "/timeshift/"):
		fp.stream(w, r, "ch"+channelID(p), false, 0)
	case strings.HasPrefix(p, "/movie/") || strings.HasPrefix(p, "/series/"):
		fp.stream(w, r, "vod"+channelID(p), false, 0)
	default:
		http.NotFound(w, r)
	}
}

// stream sends video until size bytes are sent (0: until the reader hangs up), counted as label while it runs.
func (fp *fakeProv) stream(w http.ResponseWriter, r *http.Request, label string, ts bool, size int) {
	fp.mu.Lock()
	for _, k := range []string{label, ""} {
		fp.open[k]++
		fp.most[k] = max(fp.most[k], fp.open[k])
	}
	fp.mu.Unlock()
	defer func() {
		fp.mu.Lock()
		fp.open[label]--
		fp.open[""]--
		fp.mu.Unlock()
	}()
	chunk := make([]byte, 188*100)
	for i := 0; ts && i < len(chunk); i += 188 {
		chunk[i] = 0x47
	}
	w.Header().Set("Content-Type", map[bool]string{true: "video/mp2t", false: "video/x-matroska"}[ts])
	if size > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(size))
	}
	for sent := 0; size == 0 || sent < size; {
		n := len(chunk)
		if size > 0 {
			n = min(n, size-sent)
		}
		if _, err := w.Write(chunk[:n]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		sent += n
		if size > 0 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (fp *fakeProv) count(m map[string]int, k string) int {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return m[k]
}

// advFront: a front door for the fake provider with these advanced options, served on a port of its own.
func advFront(t *testing.T, fp *fakeProv, mode string, a advanced) (*front, string) {
	return limFront(t, fp, mode, "", a)
}

// limFront: advFront for a provider with this many connections typed in ("": none).
func limFront(t *testing.T, fp *fakeProv, mode string, connections number, a advanced) (*front, string) {
	f := newFront(provider{Name: "Adv", URL: fp.srv.URL, User: "realU", Password: "realP", Port: "18601", Connections: connections,
		LiveTV: mode, Advanced: a}, player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = r.Header.Get("X-Dev") + ":5000"
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections() // (a stream a failed test left open must not hold it up)
		srv.Close()
		mu.Lock()
		for _, ch := range channels {
			if ch.provider == f.base {
				ch.drop()
			}
		}
		for k := range sessions {
			if strings.HasPrefix(k, f.base+" ") {
				delete(sessions, k)
			}
		}
		mu.Unlock()
	})
	whereOnFail(t) // (registered last, so it looks first: before the cleanup above closes and drops everything)
	return f, srv.URL
}

var devClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// ask: a request from device dev. A stream answered 200 is read a little, then left open for the caller.
func ask(t *testing.T, base, dev, path string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("X-Dev", dev)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := devClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", dev, path, err)
	}
	if resp.StatusCode == 200 && strings.HasPrefix(resp.Header.Get("Content-Type"), "video/") {
		if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
			t.Fatalf("%s %s: no video: %v", dev, path, err)
		}
	}
	return resp
}

// body: the whole answer as text.
func body(r *http.Response) string {
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20)) // (a stream that was not expected ends here too)
	return string(b)
}

// ended: whether a stream left open stops sending within a few seconds (it was cut to make room).
func ended(r *http.Response) bool {
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, r.Body); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(4 * time.Second):
		r.Body.Close()
		return false
	}
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s did not happen", what)
}

func off() *bool  { b := false; return &b }
func flag() *bool { b := true; return &b }

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// The older catch_up_while_live setting: "refuse" loads as one_per_channel on and is saved in the new form;
// "allow", any other value or none is off. An explicit one_per_channel wins. Keys this program does not know
// do not stop the file from loading. stay_under_limit is on unless it is turned off.
func TestAdvancedSettingsLoad(t *testing.T) {
	load := func(prov string) provider {
		s := &settings{}
		if err := s.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[` + prov + `],"some_new_key":1}`)); err != nil {
			t.Fatalf("%s: %v", prov, err)
		}
		return s.Providers[0]
	}
	base := `"name":"One","url":"http://provider.example","user":"u","password":"p","port":8095,"live_tv":"ts"`
	for _, c := range []struct {
		extra string
		want  bool
	}{
		{`,"catch_up_while_live":"refuse"`, true},
		{`,"catch_up_while_live":"allow"`, false},
		{`,"catch_up_while_live":"Refuse"`, false},
		{``, false},
		{`,"catch_up_while_live":"refuse","advanced":{"one_per_channel":false}`, false},
		{`,"advanced":{"one_per_channel":true,"a_later_option":"x"},"another":true`, true},
	} {
		p := load("{" + base + c.extra + "}")
		if p.Advanced.OnePerChannel != c.want || !p.Advanced.limitOn() || p.OldCatchUp != nil {
			t.Errorf("%s: one_per_channel %v (want %v), stay_under_limit %v", c.extra, p.Advanced.OnePerChannel, c.want, p.Advanced.limitOn())
		}
	}
	s := &settings{}
	s.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[{` + base + `,"catch_up_while_live":"refuse"}]}`))
	file := t.TempDir() + "/settings.json"
	if err := s.save(file); err != nil {
		t.Fatal(err)
	}
	again, err := loadSettings(file)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(again)
	if strings.Contains(string(data), "catch_up_while_live") || !strings.Contains(string(data), `"one_per_channel":true`) ||
		!strings.Contains(string(data), `"stay_under_limit":true`) || !again.Providers[0].Advanced.OnePerChannel {
		t.Errorf("saved in the old form, or the setting was lost: %s", data)
	}
	if p := load("{" + base + `,"advanced":{"stay_under_limit":false}}`); p.Advanced.limitOn() {
		t.Errorf("stay_under_limit cannot be turned off")
	}
}

// Each option is checked only when it is on, and reported under its own key; the page is given every option
// with the numbers that apply filled in.
func TestAdvancedSettingsCheck(t *testing.T) {
	check := func(adv string) string {
		s := &settings{}
		s.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[{"name":"One","url":"http://provider.example","user":"u",` +
			`"password":"p","port":8095,"live_tv":"ts","advanced":` + adv + `}],"vpn":{"service_provider":"x"}}`))
		var out []string
		for _, e := range s.validate() {
			out = append(out, e.Field)
		}
		return strings.Join(out, " ")
	}
	for adv, want := range map[string]string{
		`{}`:                     "",
		`{"connections":"3"}`:    "", // (the older place for the number: moved, see TestOldConnectionsMoved)
		`{"connections":"lots"}`: "",
		`{"stay_under_limit":false,"connections":"lots"}`:      "",
		`{"locked_to_address":true}`:                           "",
		`{"locked_to_address":true,"address_wait_seconds":5}`:  "providers[0].advanced.address_wait_seconds",
		`{"locked_to_address":false,"address_wait_seconds":5}`: "",
		`{"send_app_name":true}`:                               "providers[0].advanced.app_name",
		`{"send_app_name":true,"app_name":"Player/1.0 (TV)"}`:  "",
		`{"send_app_name":true,"app_name":"two\nlines"}`:       "providers[0].advanced.app_name",
		`{"send_app_name":false,"app_name":"two\nlines"}`:      "",
		`{"share_lists":true}`:                                 "",
		`{"share_lists":true,"share_minutes":0}`:               "providers[0].advanced.share_minutes",
		`{"limit_movie_speed":true}`:                           "",
		`{"limit_movie_speed":true,"movie_mbps":"fast"}`:       "providers[0].advanced.movie_mbps",
		`{"limit_movie_speed":true,"movie_mbps":40}`:           "",
	} {
		if got := check(adv); got != want {
			t.Errorf("%s: problems %q, want %q", adv, got, want)
		}
	}
	s := &settings{}
	s.parse([]byte(`{"providers":[{"name":"One","connections":2,"advanced":{"share_lists":true}}]}`))
	shown, _ := json.Marshal(s.masked())
	for _, want := range []string{`"stay_under_limit":true`, `"connections":2,`, `"share_lists":true`, `"share_minutes":10`,
		`"address_wait_seconds":90`, `"movie_mbps":100`, `"one_per_channel":false`, `"app_name":""`} {
		if !strings.Contains(string(shown), want) {
			t.Errorf("the page is not given %s: %s", want, shown)
		}
	}
}

// What the page is given for a provider's advanced options comes back as it is when the page saves: it loads,
// passes the checks, keeps every value, and the provider's password is kept as before.
func TestAdvancedSettingsRoundTrip(t *testing.T) {
	old := &settings{}
	old.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[{"name":"One","url":"http://provider.example","user":"u",` +
		`"password":"p","port":8095,"connections":2,"live_tv":"ts","advanced":{"one_per_item":true,"send_app_name":true,"app_name":"App/1",` +
		`"limit_movie_speed":true,"movie_mbps":"30"}},{"name":"Two","url":"http://two.example","user":"u","password":"p","port":8096,` +
		`"connections":1,"live_tv":"hls"}],"vpn":{"service_provider":"x"}}`))
	shown, _ := json.Marshal(old.masked())
	var page map[string]any
	json.Unmarshal(shown, &page)
	for i, p := range page["providers"].([]any) {
		a, _ := p.(map[string]any)["advanced"].(map[string]any)
		if _, ok := a["connections"]; ok || p.(map[string]any)["connections"] == nil {
			t.Errorf("provider %d: the connections are not given on the provider itself: %v", i, p)
		}
		for _, k := range []string{"stay_under_limit", "one_per_channel", "one_per_item", "locked_to_address", "address_wait_seconds",
			"send_app_name", "app_name", "share_lists", "share_minutes", "limit_movie_speed", "movie_mbps"} {
			if _, ok := a[k]; !ok {
				t.Errorf("provider %d: the page is not given %s: %v", i, k, a)
			}
		}
		if a["stay_under_limit"] != true {
			t.Errorf("provider %d: stay_under_limit left out must show as on: %v", i, a["stay_under_limit"])
		}
	}
	s := &settings{}
	if err := s.parse(shown); err != nil {
		t.Fatal(err)
	}
	s.keepSecrets(old)
	if errs := append(s.validate(), s.missingConnections()...); len(errs) != 0 {
		t.Fatalf("the page's own settings are refused: %v", errs)
	}
	a := s.Providers[0].Advanced
	if v, _ := s.Providers[0].Connections.value(); v != 2 || !a.OnePerItem || a.AppName != "App/1" || a.MovieMbps != "30" || !a.limitOn() || s.Providers[0].Password != "p" {
		t.Errorf("a value was lost on the way back: %+v", a)
	}
	if v, _ := s.Providers[1].Advanced.LockWait.value(); v != 90 || s.Providers[1].Connections != "1" {
		t.Errorf("the defaults shown came back otherwise: %+v", s.Providers[1].Advanced)
	}
}

// ---------------------------------------------------------------------------
// Stay under my connection limit
// ---------------------------------------------------------------------------

// At the limit, a new stream from another device is refused before the provider is asked (it would have cut
// someone off); the same device asking for something else replaces its own stream, after it has closed.
func TestLimitRefusesAndReplaces(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := limFront(t, fp, "ts", "1", advanced{})
	a := ask(t, base, "192.0.2.1", "/movie/tv/easy/1.mkv")
	if a.StatusCode != 200 {
		t.Fatalf("the first film: %d", a.StatusCode)
	}
	b := ask(t, base, "192.0.2.2", "/movie/tv/easy/2.mkv")
	if text := body(b); b.StatusCode != 503 || !strings.Contains(text, "connection your provider allows is in use") {
		t.Errorf("a second device over the limit: %d %q", b.StatusCode, text)
	}
	if fp.count(fp.hits, "/movie/realU/realP/2.mkv") != 0 {
		t.Errorf("the provider was asked for the refused stream")
	}
	a2 := ask(t, base, "192.0.2.1", "/movie/tv/easy/2.mkv")
	if a2.StatusCode != 200 {
		t.Fatalf("the same device, another film: %d %q", a2.StatusCode, body(a2))
	}
	if !ended(a) {
		t.Errorf("the device's first film was not cut to make room")
	}
	a2.Body.Close()
	if n := fp.count(fp.most, ""); n != 1 {
		t.Errorf("the provider saw %d streams at once, want 1", n)
	}
}

// The limit is the number typed in, whatever the provider's login answer says (it is passed on, never read
// for this); a device joining a live channel that is open costs nothing; a shared channel is never cut for
// another device.
func TestLimitTypedInAndLiveChannels(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/player_api.php" {
			io.WriteString(w, `{"user_info":{"username":"realU","password":"realP","max_connections":"5","active_cons":"0"},"server_info":{"url":"x"}}`)
			return true
		}
		return false
	})
	f, base := limFront(t, fp, "ts", "1", advanced{})
	a := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts")
	if a.StatusCode != 200 {
		t.Fatalf("the first channel: %d", a.StatusCode)
	}
	if r := ask(t, base, "192.0.2.2", "/player_api.php?username=tv&password=easy"); r.StatusCode != 200 || strings.Contains(body(r), "realP") {
		t.Fatalf("login")
	}
	mu.Lock()
	lim := f.limit()
	mu.Unlock()
	if lim != 1 {
		t.Fatalf("after a login answer that says 5: limit %d, want the 1 typed in", lim)
	}
	join := ask(t, base, "192.0.2.2", "/live/tv/easy/12.ts")
	if join.StatusCode != 200 {
		t.Errorf("joining an open channel at the limit: %d", join.StatusCode)
	}
	other := ask(t, base, "192.0.2.2", "/live/tv/easy/14.ts")
	if text := body(other); other.StatusCode != 503 {
		t.Errorf("a new channel while the only one open is shared: %d %q", other.StatusCode, text)
	}
	join.Body.Close()
	eventually(t, func() bool { // (the second device is off channel 12)
		mu.Lock()
		defer mu.Unlock()
		c := f.conns()
		return len(c) == 1 && len(c[0].who) == 1
	}, "the second device leaving")
	zap := ask(t, base, "192.0.2.1", "/live/tv/easy/14.ts") // alone on 12 now: changing channel works
	if zap.StatusCode != 200 {
		t.Fatalf("changing channel at the limit: %d %q", zap.StatusCode, body(zap))
	}
	if !ended(a) {
		t.Errorf("the old channel was not let go")
	}
	zap.Body.Close()
	if n := fp.count(fp.most, "ch14"); n != 1 {
		t.Errorf("channel 14 opened %d times at once", n)
	}
}

// ---------------------------------------------------------------------------
// One connection per channel
// ---------------------------------------------------------------------------

func TestOnePerChannel(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	live := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts")
	if live.StatusCode != 200 {
		t.Fatalf("live: %d", live.StatusCode)
	}
	for _, p := range []string{"/timeshift/tv/easy/60/2026-01-01:10-00/12.ts", "/streaming/timeshift.php?username=tv&password=easy&stream=12&start=2026-01-01:10-00&duration=60",
		"/timeshift/tv/easy/60/2026-01-01:10-00/12.m3u8"} {
		r := ask(t, base, "192.0.2.2", p)
		if text := body(r); r.StatusCode != 503 || !strings.Contains(text, "one stream per channel") {
			t.Errorf("another device, catch-up of a channel open live (%s): %d %q", p, r.StatusCode, text)
		}
	}
	other := ask(t, base, "192.0.2.2", "/timeshift/tv/easy/60/2026-01-01:10-00/13.ts")
	if other.StatusCode != 200 {
		t.Errorf("catch-up of another channel: %d", other.StatusCode)
	}
	other.Body.Close()
	// The device watching it rewinds: its catch-up replaces its own live stream.
	back := ask(t, base, "192.0.2.1", "/timeshift/tv/easy/60/2026-01-01:10-00/12.ts")
	if back.StatusCode != 200 {
		t.Fatalf("the device watching rewinds: %d %q", back.StatusCode, body(back))
	}
	if !ended(live) {
		t.Errorf("the live stream was not let go for the device's own catch-up")
	}
	// Catch-up of the same channel at another time: refused for another device, a replacement for this one.
	if r := ask(t, base, "192.0.2.2", "/timeshift/tv/easy/30/2026-01-01:09-00/12.ts"); r.StatusCode != 503 {
		t.Errorf("another device, a second catch-up of the channel: %d", r.StatusCode)
		r.Body.Close()
	}
	skip := ask(t, base, "192.0.2.1", "/timeshift/tv/easy/30/2026-01-01:09-30/12.ts")
	if skip.StatusCode != 200 {
		t.Fatalf("the same device skips: %d", skip.StatusCode)
	}
	if !ended(back) {
		t.Errorf("the device's older catch-up was not let go")
	}
	skip.Body.Close()
	if n := fp.count(fp.most, "ch12"); n != 1 {
		t.Errorf("the provider saw channel 12 open %d times at once, want 1", n)
	}
}

// A live channel shared with another device (a recorder) keeps playing: catch-up of it is refused even for
// one of the devices on it. Off, catch-up of a channel open live is passed on as before.
func TestOnePerChannelSharedAndOff(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerChannel: true})
	tv := ask(t, base, "192.0.2.1", "/live/tv/easy/12.ts")
	rec := ask(t, base, "192.0.2.9", "/live/tv/easy/12.ts")
	defer tv.Body.Close()
	defer rec.Body.Close()
	if r := ask(t, base, "192.0.2.1", "/timeshift/tv/easy/60/2026-01-01:10-00/12.ts"); r.StatusCode != 503 {
		t.Errorf("catch-up of a channel a recorder is on: %d", r.StatusCode)
		r.Body.Close()
	}
	fp2 := newFakeProv(t, false, nil)
	_, base2 := advFront(t, fp2, "ts", advanced{})
	l := ask(t, base2, "192.0.2.1", "/live/tv/easy/12.ts")
	defer l.Body.Close()
	c := ask(t, base2, "192.0.2.2", "/timeshift/tv/easy/60/2026-01-01:10-00/12.ts")
	if c.StatusCode != 200 {
		t.Errorf("with the option off, catch-up of a channel open live: %d", c.StatusCode)
	}
	c.Body.Close()
}

// On a provider whose players use HLS, a live session of the channel counts the same.
func TestOnePerChannelHLS(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/44.m3u8" { // a session of its own, as Xtream servers do
			http.Redirect(w, r, "/hlsr/tok44/realU/realP/44/index.m3u8", 302)
			return true
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\n44_1.ts\n")
			return true
		}
		return false
	})
	_, base := advFront(t, fp, "hls", advanced{StayUnder: off(), OnePerChannel: true})
	r := ask(t, base, "192.0.2.1", "/live/tv/easy/44.m3u8")
	for r.StatusCode == 302 {
		loc := r.Header.Get("Location")
		r.Body.Close()
		r = ask(t, base, "192.0.2.1", loc)
	}
	if text := body(r); r.StatusCode != 200 || !strings.Contains(text, "#EXTINF") {
		t.Fatalf("live HLS: %d %q", r.StatusCode, text)
	}
	if c := ask(t, base, "192.0.2.2", "/timeshift/tv/easy/60/2026-01-01:10-00/44.ts"); c.StatusCode != 503 {
		t.Errorf("catch-up of a channel open as live HLS: %d", c.StatusCode)
		c.Body.Close()
	}
	c := ask(t, base, "192.0.2.1", "/timeshift/tv/easy/60/2026-01-01:10-00/44.ts")
	if c.StatusCode != 200 {
		t.Errorf("the device on the live session rewinds: %d", c.StatusCode)
	}
	c.Body.Close()
}

// ---------------------------------------------------------------------------
// One connection per movie or episode
// ---------------------------------------------------------------------------

func TestOnePerItem(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerItem: true})
	a := ask(t, base, "192.0.2.1", "/movie/tv/easy/7.mkv")
	if a.StatusCode != 200 {
		t.Fatalf("a film: %d", a.StatusCode)
	}
	b := ask(t, base, "192.0.2.2", "/movie/tv/easy/7.mkv")
	if text := body(b); b.StatusCode != 503 || !strings.Contains(text, "another device") {
		t.Errorf("a second device on the same film: %d %q", b.StatusCode, text)
	}
	other := ask(t, base, "192.0.2.2", "/series/tv/easy/8.mp4")
	if other.StatusCode != 200 {
		t.Errorf("another item: %d", other.StatusCode)
	}
	other.Body.Close()
	seek := ask(t, base, "192.0.2.1", "/movie/tv/easy/7.mkv", "Range", "bytes=1000000-")
	if seek.StatusCode != 200 {
		t.Fatalf("the same device seeks: %d %q", seek.StatusCode, body(seek))
	}
	if !ended(a) {
		t.Errorf("the device's first read of the film was not let go")
	}
	seek.Body.Close()
	if n := fp.count(fp.most, "vod7"); n != 1 {
		t.Errorf("the provider saw the film open %d times at once, want 1", n)
	}
	// Off: two devices on one film, as before.
	fp2 := newFakeProv(t, false, nil)
	_, base2 := advFront(t, fp2, "ts", advanced{})
	x, y := ask(t, base2, "192.0.2.1", "/movie/tv/easy/7.mkv"), ask(t, base2, "192.0.2.2", "/movie/tv/easy/7.mkv")
	if x.StatusCode != 200 || y.StatusCode != 200 {
		t.Errorf("with the option off: %d %d", x.StatusCode, y.StatusCode)
	}
	x.Body.Close()
	y.Body.Close()
}

// ---------------------------------------------------------------------------
// My provider locks my account to one address
// ---------------------------------------------------------------------------

func TestLockWait(t *testing.T) {
	for _, c := range []struct {
		locked                     bool
		wait, changed, now, expect float64
	}{
		{true, 90, 1000, 1000, 90}, {true, 90, 1000, 1060, 30}, {true, 90, 1000, 1090, 0}, {true, 90, 1000, 1200, 0},
		{false, 90, 1000, 1000, 0}, {true, 90, 0, 1000, 0},
	} {
		if got := lockLeft(c.locked, c.wait, c.changed, c.now); got != c.expect {
			t.Errorf("%+v: %v", c, got)
		}
	}
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), Locked: true})
	mu.Lock()
	was := vpnChanged
	vpnChanged = mono()
	mu.Unlock()
	defer func() { mu.Lock(); vpnChanged = was; mu.Unlock() }()
	r := ask(t, base, "192.0.2.1", "/movie/tv/easy/1.mkv")
	if text := body(r); r.StatusCode != 503 || r.Header.Get("Retry-After") == "" || !strings.Contains(text, "locks your account to one address") {
		t.Errorf("a stream right after the VPN changed: %d %q (Retry-After %q)", r.StatusCode, text, r.Header.Get("Retry-After"))
	}
	if r := ask(t, base, "192.0.2.1", "/player_api.php?username=tv&password=easy&action=get_live_streams"); r.StatusCode == 503 {
		t.Errorf("a list was held back too")
	}
	mu.Lock()
	vpnChanged = mono() - 91
	mu.Unlock()
	if r := ask(t, base, "192.0.2.1", "/movie/tv/easy/1.mkv"); r.StatusCode != 200 {
		t.Errorf("after the wait: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	w := httptest.NewRecorder() // the launcher says the VPN is back up
	insideStatus(w, httptest.NewRequest("GET", "/vpn-changed", nil))
	mu.Lock()
	left := lockLeft(true, 90, vpnChanged, mono())
	mu.Unlock()
	if left < 85 {
		t.Errorf("the VPN coming back up did not start the wait: %.0f s left", left)
	}
}

// With a provider locked to one address, a server once chosen is never changed on Coaxial's own, even when a
// better one is found; the page's button still can (it is not run here: it starts the VPN).
func TestLockedProviderKeepsServer(t *testing.T) {
	l := &launcher{dir: t.TempDir(), quit: make(chan int, 4), prog: builtinProgram, pickLoaded: true}
	l.set = &settings{}
	l.set.parse([]byte(`{"providers":[{"name":"One","advanced":{"locked_to_address":true}}],` +
		`"vpn":{"service_provider":"fakevpn","type":"wireguard","server_cities":"New York","gluetun_version":"builtin"}}`))
	key := pickKey(l.set.vpnEnv())
	l.pick = picked{Key: key, Env: "SERVER_NAMES=NY#2", Name: "NY#2"} // (chosen before)
	l.probeKey = key
	l.better, l.betterWhy, l.betterRuns = &probeResult{Name: "NY#1", env: "SERVER_NAMES=NY#1", Median: 3}, "a test", 5
	l.switchServer(false)
	_, err := os.Stat(pickFile(l.dir))
	if l.pickTry != "" || l.pick.Env != "SERVER_NAMES=NY#2" || !l.pickAt.IsZero() || err == nil {
		t.Errorf("the server was changed on Coaxial's own: trying %q, chose %q at %v", l.pickTry, l.pick.Env, l.pickAt)
	}
	if st := l.pickStatus(); st["locked"] != true {
		t.Errorf("the status does not say a provider is locked: %v", st)
	}
}

// ---------------------------------------------------------------------------
// Send this app name to my provider
// ---------------------------------------------------------------------------

func TestAppNameSent(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/player_api.php" || r.URL.Path == "/xmltv.php":
			io.WriteString(w, `{"user_info":{},"server_info":{}}`)
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n1.ts\n")
		default:
			return false
		}
		return true
	})
	for _, c := range []struct {
		a    advanced
		want string
	}{{advanced{SendApp: true, AppName: "Allowed App/3.1"}, "Allowed App/3.1"}, {advanced{SendApp: false, AppName: "Allowed App/3.1"}, "Player/1"}} {
		f, base := advFront(t, fp, "ts", c.a)
		for _, p := range []string{"/player_api.php?username=tv&password=easy", "/player_api.php?username=tv&password=easy&action=get_vod_streams",
			"/xmltv.php?username=tv&password=easy", "/movie/tv/easy/3.mkv", "/live/tv/easy/21.ts", "/timeshift/tv/easy/9/2026-01-01:10-00/22.ts",
			"/timeshift/tv/easy/9/2026-01-01:10-00/23.m3u8"} {
			r := ask(t, base, "192.0.2.1", p, "User-Agent", "Player/1")
			r.Body.Close()
		}
		mu.Lock() // (the account shown on the status page, asked by Coaxial itself)
		fronts[f.port] = f
		mu.Unlock()
		playerAPI(context.Background(), f.port, "&action=account_page")
		mu.Lock()
		delete(fronts, f.port)
		mu.Unlock()
		eventually(t, func() bool { return fp.count(fp.hits, "/live/realU/realP/21.ts") > 0 }, "the live channel opening")
		fp.mu.Lock()
		for path, ua := range fp.agents {
			want := c.want
			if strings.Contains(path, "account_page") && !c.a.SendApp {
				want = "" // (Coaxial's own request names no app, as before)
			}
			if ua != want {
				t.Errorf("%s was sent as %q, want %q", path, ua, want)
			}
		}
		if c.want != "Player/1" && len(fp.agents) != 8 { // (every kind of request reached the provider)
			t.Errorf("the provider was asked %d kinds of request, want 8: %v", len(fp.agents), fp.agents)
		}
		fp.agents = map[string]string{}
		fp.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Share list and guide downloads
// ---------------------------------------------------------------------------

func TestShareLists(t *testing.T) {
	slow := make(chan struct{})
	big := bytes.Repeat([]byte(`{"name":"x","stream_icon":"http://img.example/a.png"},`), 120000) // (about 6 MB: kept in a file)
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Query().Get("action") {
		case "get_live_streams":
			io.WriteString(w, `[{"name":"One","stream_icon":"http://img.example/1.png","username":"realU"}]`)
		case "get_vod_streams": // slow to start: players that ask meanwhile share the one download
			<-slow
			w.Write([]byte("["))
			w.Write(big)
			w.Write([]byte(`{}]`))
		case "get_series":
			w.WriteHeader(500)
			io.WriteString(w, "busy")
		case "":
			if r.URL.Path == "/xmltv.php" {
				w.Header().Set("Content-Encoding", "gzip")
				z := gzip.NewWriter(w)
				io.WriteString(z, `<tv><channel id="1"><icon src="http://img.example/g.png"/></channel></tv>`)
				z.Close()
			} else {
				io.WriteString(w, `{"user_info":{"username":"realU","password":"realP"},"server_info":{"url":"provider.example"}}`)
			}
		default:
			return false
		}
		return true
	})
	f, base := advFront(t, fp, "ts", advanced{Share: true})
	f.shareFor = 0.6 // (minutes in the settings; a moment here)
	get := func(host, q string) (int, string) {
		req, _ := http.NewRequest("GET", base+q, nil)
		req.Host = host
		req.Header.Set("X-Dev", "192.0.2.1")
		r, err := devClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r.StatusCode, body(r)
	}
	list := "/player_api.php?username=tv&password=easy&action=get_live_streams"
	s1, b1 := get("tv-a.lan:18601", list)
	s2, b2 := get("tv-b.lan:18601", list)
	if s1 != 200 || s2 != 200 || !strings.Contains(b1, "http://tv-a.lan:18601/__cdn/http/img.example/1.png") ||
		!strings.Contains(b2, "http://tv-b.lan:18601/__cdn/http/img.example/1.png") || strings.Contains(b1+b2, "realU") {
		t.Errorf("each player is not given the list rewritten for itself:\n%s\n%s", b1, b2)
	}
	if n := fp.count(fp.hits, "/player_api.php?get_live_streams"); n != 1 {
		t.Errorf("the list was downloaded %d times, want 1", n)
	}
	for i := 0; i < 2; i++ {
		get("tv-a.lan:18601", "/player_api.php?username=tv&password=easy")
		get("tv-a.lan:18601", "/player_api.php?username=tv&password=easy&action=get_series")
		if _, g := get("tv-a.lan:18601", "/xmltv.php?username=tv&password=easy"); !strings.Contains(g, "http://tv-a.lan:18601/__cdn/http/img.example/g.png") {
			t.Errorf("the guide: %q", g)
		}
	}
	if a, b, c := fp.count(fp.hits, "/player_api.php"), fp.count(fp.hits, "/player_api.php?get_series"), fp.count(fp.hits, "/xmltv.php"); a != 2 || b != 2 || c != 1 {
		t.Errorf("the login answer is never shared (asked %d times, want 2), nor an error (%d, want 2); the guide is (%d, want 1)", a, b, c)
	}
	time.Sleep(700 * time.Millisecond)
	get("tv-a.lan:18601", list)
	if n := fp.count(fp.hits, "/player_api.php?get_live_streams"); n != 2 {
		t.Errorf("an expired copy was used: downloaded %d times, want 2", n)
	}
	// Several players at once, while the provider is slow: one download, and every one gets all of it.
	var wg sync.WaitGroup
	got := make([]string, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, got[i] = get("tv.lan", "/player_api.php?username=tv&password=easy&action=get_vod_streams")
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(slow)
	wg.Wait()
	for i, g := range got {
		if len(g) < len(big) || !strings.HasSuffix(g, "{}]") || !strings.Contains(g, "http://tv.lan/__cdn/http/img.example/a.png") {
			t.Errorf("player %d was given %d bytes of the large list", i, len(g))
		}
	}
	if n := fp.count(fp.hits, "/player_api.php?get_vod_streams"); n != 1 {
		t.Errorf("players asking at once: downloaded %d times, want 1", n)
	}
	// Off: every request goes to the provider.
	f2, base2 := advFront(t, fp, "ts", advanced{})
	_ = f2
	for i := 0; i < 2; i++ {
		r := ask(t, base2, "192.0.2.1", "/player_api.php?username=tv&password=easy&action=get_live_streams")
		r.Body.Close()
	}
	if n := fp.count(fp.hits, "/player_api.php?get_live_streams"); n != 4 {
		t.Errorf("with the option off, the list was downloaded %d times in all, want 4", n)
	}
}

// ---------------------------------------------------------------------------
// Limit movie download speed
// ---------------------------------------------------------------------------

func TestMovieSpeedLimit(t *testing.T) {
	const size = 2500000
	fp := newFakeProv(t, false, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/movie/") || strings.HasPrefix(r.URL.Path, "/timeshift/") {
			fp.stream(w, r, "x", false, size)
			return true
		}
		return false
	})
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), SlowMovies: true, MovieMbps: "8"}) // (1 MB a second)
	took := func(p string) float64 {
		began := time.Now()
		r := ask(t, base, "192.0.2.1", p)
		if n := len(body(r)); n != size-4096 {
			t.Fatalf("%s: %d bytes", p, n)
		}
		return time.Since(began).Seconds()
	}
	if s := took("/movie/tv/easy/4.mkv"); s < 1.2 || s > 4 {
		t.Errorf("2.5 MB of a film at 8 Mbit/s took %.2f s, want about 1.5", s)
	}
	if s := took("/timeshift/tv/easy/9/2026-01-01:10-00/22.ts"); s > 0.8 {
		t.Errorf("catch-up is slowed too: %.2f s", s)
	}
	_, fast := advFront(t, fp, "ts", advanced{})
	began := time.Now()
	body(ask(t, fast, "192.0.2.1", "/movie/tv/easy/4.mkv"))
	if s := time.Since(began).Seconds(); s > 0.8 {
		t.Errorf("with the option off, a film is slowed: %.2f s", s)
	}
}

// A player that pauses (or reads slower than the limit for a while) and then reads ahead as fast as it can is
// still held to the limit: the time it did not read is not saved up as a burst.
func TestMovieSpeedAfterPause(t *testing.T) {
	th := &throttle{r: endless{}, ctx: context.Background(), p: &speed{rate: 1e6}}
	buf := make([]byte, 32768)
	read := func(n int) float64 {
		began := time.Now()
		for got := 0; got < n; {
			k, _ := th.Read(buf[:min(len(buf), n-got)])
			got += k
		}
		return time.Since(began).Seconds()
	}
	read(1500000) // (the first second's worth, and half a second more)
	time.Sleep(3 * time.Second)
	if s := read(3000000); s < 1.7 {
		t.Errorf("after a 3 s pause, 3 MB at 1 MB a second came in %.2f s, want about 2 (one second's worth ahead)", s)
	}
}

type endless struct{}

func (endless) Read(p []byte) (int, error) { return len(p), nil }
