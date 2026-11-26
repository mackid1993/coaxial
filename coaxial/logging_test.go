package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf: a log written by several goroutines at once.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog: the log from here to the end of the test, at the detailed level, as the program writes it.
func captureLog(t *testing.T) *syncBuf {
	b := &syncBuf{}
	logTo(b, true)
	was := currentLogLevel()
	setLogLevel(logDetailed)
	t.Cleanup(func() {
		setLogLevel(was)
		logTo(os.Stdout, false)
	})
	t.Cleanup(func() {
		if os.Getenv("COAXIAL_SHOW_LOG") != "" { // (to read what a test logged)
			t.Log("\n" + b.String())
		}
	})
	return b
}

// linesWith: the log lines that hold every one of parts.
func linesWith(log string, parts ...string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		ok := line != ""
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

func getAs(t *testing.T, url, dev string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Dev", dev)
	r, err := devClient.Do(req)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return r
}

// The format: a UTC time, a short level, "coaxial:", the message, then the facts as key=value.
func TestLogLineFormat(t *testing.T) {
	b := captureLog(t)
	(&askLog{kind: "stream", id: "1234", device: "198.51.100.7", provider: "Main"}).answered(403, 210*time.Millisecond)
	line := strings.TrimSpace(b.String())
	re := regexp.MustCompile(`^20\d\d-\d\d-\d\dT\d\d:\d\d:\d\dZ WRN coaxial: the provider answered 403 \(forbidden\) ` +
		`channel=1234 device=198.51.100.7 kind=stream ms=210 provider=Main status=403$`)
	if !re.MatchString(line) {
		t.Errorf("line not as it should be:\n%s", line)
	}
	if strings.ContainsRune(line, 0x1b) {
		t.Errorf("color codes in the log")
	}
}

// A provider that answers 403 to a live channel and to a list: each gives a WRN line naming the status, what was
// asked (kind and number), the device and the provider. The player is still given the provider's answer.
func TestLog403IsAWarning(t *testing.T) {
	b := captureLog(t)
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "Forbidden", 403)
		return true
	})
	_, base := advFront(t, fp, "ts", advanced{})
	r := getAs(t, base+"/live/tv/easy/4031.ts", "198.51.100.7")
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Errorf("the player was given %d, want the provider's 403", r.StatusCode)
	}
	r = getAs(t, base+"/player_api.php?username=tv&password=easy&action=get_live_streams", "198.51.100.8")
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Errorf("the list: the player was given %d, want 403", r.StatusCode)
	}
	time.Sleep(200 * time.Millisecond)
	log := b.String()
	if got := linesWith(log, " WRN ", "the provider answered 403", "kind=stream", "channel=4031", "device=198.51.100.7", "provider=Adv", "status=403", "ms="); len(got) != 1 {
		t.Errorf("stream: want one WRN line with status=403, got %d:\n%s", len(got), log)
	}
	if got := linesWith(log, " WRN ", "the provider answered 403", "kind=player_api", "action=get_live_streams", "device=198.51.100.8", "status=403"); len(got) != 1 {
		t.Errorf("player_api: want one WRN line with status=403, got %d:\n%s", len(got), log)
	}
}

// A provider that cannot be reached at all is a WRN line too (here: nothing listens on its port any more).
func TestLogUnreachableProvider(t *testing.T) {
	b := captureLog(t)
	fp := newFakeProv(t, true, nil)
	_, base := advFront(t, fp, "ts", advanced{})
	fp.srv.Close()
	r := getAs(t, base+"/movie/tv/easy/77.mkv", "198.51.100.9")
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	time.Sleep(200 * time.Millisecond)
	if got := linesWith(b.String(), " WRN ", "the provider could not be reached", "kind=movie", "item=77", "device=198.51.100.9", "reason="); len(got) != 1 {
		t.Errorf("want one WRN line for the unreachable provider:\n%s", b.String())
	}
}

// A live provider's lumps 10 s apart are normal and never logged; a 17 s silence is, once, as a WRN line with
// its length; the stream's end is logged with how long it ran and how much came.
func TestLogProviderSilence(t *testing.T) {
	b := captureLog(t)
	pkt := make([]byte, 188*200)
	for i := 0; i < len(pkt); i += 188 {
		pkt[i], pkt[i+1], pkt[i+2], pkt[i+3] = 0x47, 0x1f, 0xff, 0x10
	}
	start := time.Now()
	// lumps at 0, 10, 20 s (10 s apart), then nothing until 37 s (17 s: past gapWarn, short of liveQuiet, after
	// which the channel would be asked for again), then lumps every second to 44 s
	plan := []float64{0, 10, 20, 37, 38, 39, 40, 41, 42, 43, 44}
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "video/mp2t")
		for _, at := range plan {
			select {
			case <-time.After(time.Until(start.Add(time.Duration(at * float64(time.Second))))):
			case <-r.Context().Done():
				return true
			}
			if _, err := w.Write(pkt); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
		return true
	})
	_, base := advFront(t, fp, "ts", advanced{})
	r := getAs(t, base+"/live/tv/easy/4500.ts", "198.51.100.10")
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, r.Body); close(done) }()
	time.Sleep(time.Until(start.Add(45 * time.Second)))
	r.Body.Close()
	<-done
	time.Sleep(500 * time.Millisecond)
	log := b.String()
	gaps := linesWith(log, " WRN ", "the provider sent nothing for", "channel=4500")
	if len(gaps) != 1 {
		t.Fatalf("want exactly one silence warning (the 17 s one, none for the 10 s lumps), got %d:\n%s", len(gaps), log)
	}
	m := regexp.MustCompile(`seconds=([0-9.]+)`).FindStringSubmatch(gaps[0])
	if s, _ := strconv.ParseFloat(m[1], 64); s < 16 || s > 19 {
		t.Errorf("the silence was logged as %.1f s, want about 17:\n%s", s, gaps[0])
	}
	if !strings.Contains(gaps[0], "the provider sent nothing for 17 s on channel 4500") {
		t.Errorf("the line should say it plainly: %s", gaps[0])
	}
	if got := linesWith(log, "a channel's stream from the provider ended", "channel=4500", "longest_gap=17", "mb=", "seconds=4"); len(got) != 1 {
		t.Errorf("want one end line with its length, MB and the longest gap:\n%s", log)
	}
}

// The once-in-a-while line: not before statEvery seconds of playing, then one.
func TestLogStillPlayingIsRare(t *testing.T) {
	b := captureLog(t)
	st := newStreamStats(&askLog{kind: "stream", id: "77", provider: "P"})
	for i := 0; i < 1000; i++ {
		st.got(188 * 7)
	}
	if strings.Contains(b.String(), "still playing") {
		t.Errorf("a still-playing line in the first seconds")
	}
	st.since -= statEvery
	st.got(188)
	if got := linesWith(b.String(), " DBG ", "a channel is still playing", "channel=77", "mbit_s=", "longest_gap="); len(got) != 1 {
		t.Errorf("want one still-playing line after %d s:\n%s", statEvery, b.String())
	}
}

// What Coaxial asks on its own is tagged background: at the detailed level when it went well, WRN when not.
func TestLogBackgroundTag(t *testing.T) {
	b := captureLog(t)
	a := &askLog{kind: "provider account read for the status page", provider: "Main", background: true}
	a.answered(200, time.Millisecond)
	a.answered(429, time.Millisecond)
	if got := linesWith(b.String(), " DBG ", "background: provider account read for the status page answered 200"); len(got) != 1 {
		t.Errorf("no background line at the detailed level:\n%s", b.String())
	}
	if got := linesWith(b.String(), " WRN ", "background: provider account read for the status page answered 429", "status=429"); len(got) != 1 {
		t.Errorf("no WRN line for a background 429:\n%s", b.String())
	}
}

// Routine asks (playlist reloads, video pieces) are not logged when they go well, only when they do not; and an
// access line for a video piece is written at no level the page offers.
func TestLogRoutineQuiet(t *testing.T) {
	b := captureLog(t)
	a := &askLog{kind: "piece", provider: "P", routine: true}
	a.answered(200, time.Millisecond)
	a.answered(404, time.Millisecond)
	if strings.Contains(b.String(), "answered 200") || len(linesWith(b.String(), " WRN ", "answered 404", "kind=piece")) != 1 {
		t.Errorf("routine answers logged wrongly:\n%s", b.String())
	}
	fp := newFakeProv(t, true, nil) // (answers 404 to a piece)
	_, base := advFront(t, fp, "ts", advanced{})
	host := strings.TrimPrefix(fp.srv.URL, "http://")
	for _, p := range []string{"/__cdn/http/" + host + "/x.ts", "/healthz"} {
		r := getAs(t, base+p, "198.51.100.12")
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(b.String(), "kind=cdn") || !strings.Contains(b.String(), "kind=health") ||
		len(linesWith(b.String(), " WRN ", "answered 404", "kind=piece", "device=198.51.100.12")) != 1 {
		t.Errorf("kind=cdn access lines must stay out of the log, others in:\n%s", b.String())
	}
}

// The levels: normal leaves the detailed lines out, quiet everything but problems; applied at once.
func TestLogLevels(t *testing.T) {
	b := captureLog(t)
	write := func(tag string) {
		lg.Debug().Msg("d-" + tag)
		lg.Info().Msg("i-" + tag)
		lg.Warn().Msg("w-" + tag)
	}
	write("detailed")
	setLogLevel(logNormal)
	write("normal")
	setLogLevel(logQuiet)
	write("quiet")
	setLogLevel(logDetailed)
	log := b.String()
	for want, in := range map[string]bool{"d-detailed": true, "i-detailed": true, "w-detailed": true, "d-normal": false, "i-normal": true,
		"w-normal": true, "d-quiet": false, "i-quiet": false, "w-quiet": true} {
		if strings.Contains(log, want+"\n") != in {
			t.Errorf("%s: in the log %v, want %v", want, !in, in)
		}
	}
}

// The log detail set on the page: saved in settings.json, shown again when the page loads, a bad value refused,
// applied at once in the launcher and told to the proxy part, which is NOT started again for it.
func TestLogLevelSetting(t *testing.T) {
	captureLog(t)
	if m := goodSettings().masked(); m["ui"].(map[string]any)["log_level"] != logDetailed {
		t.Errorf("not set: the page should show detailed, got %v", m["ui"])
	}
	l, code, got := put(t, pageSaved(goodSettings()), func(m map[string]any) { m["ui"].(map[string]any)["log_level"] = "quiet" })
	if code != 200 {
		t.Fatalf("save: %d %v", code, got)
	}
	if r, _ := got["restarting"].(map[string]any); r["vpn"] != false || r["proxy"] != false {
		t.Errorf("a new log detail restarts %v; it must restart nothing", r)
	}
	if currentLogLevel() != logQuiet {
		t.Errorf("the launcher's log detail is %q, want quiet at once", currentLogLevel())
	}
	data, _ := os.ReadFile(l.file)
	var onDisk map[string]any
	json.Unmarshal(data, &onDisk)
	if onDisk["ui"].(map[string]any)["log_level"] != "quiet" {
		t.Errorf("settings.json: %s", data)
	}
	back, err := loadSettings(l.file)
	if err != nil || back.masked()["ui"].(map[string]any)["log_level"] != "quiet" {
		t.Errorf("loaded again: %v %v", err, back)
	}
	time.Sleep(100 * time.Millisecond) // (what the save starts in the background)
	l.mu.Lock()
	if !strings.Contains(l.proxyIn, `"log_level":"quiet"`) || strings.Contains(l.proxyKey, "log_level") {
		t.Errorf("the proxy part's next start must get the level (%s) without it counting as a change (%s)", l.proxyIn, l.proxyKey)
	}
	l.mu.Unlock()
	if _, code, got = put(t, goodSettings(), func(m map[string]any) { m["ui"].(map[string]any)["log_level"] = "loud" }); code != 400 ||
		!strings.Contains(strings.ToLower(jsonText(got)), "ui.log_level") {
		t.Errorf("a bad level was not refused: %d %v", code, got)
	}
	// the proxy part: told over its socket, applied at once
	rec := httptest.NewRecorder()
	insideStatus(rec, httptest.NewRequest("GET", "/log-level?to=normal", nil))
	if rec.Code != 200 || currentLogLevel() != logNormal {
		t.Errorf("the proxy part did not take the level: %d %q", rec.Code, currentLogLevel())
	}
	rec = httptest.NewRecorder()
	insideStatus(rec, httptest.NewRequest("GET", "/log-level?to=loud", nil))
	if rec.Code != 400 || currentLogLevel() != logNormal {
		t.Errorf("a bad level was taken: %d %q", rec.Code, currentLogLevel())
	}
	// and the proxy part started with it
	s := goodSettings()
	s.UI.LogLevel = logQuiet
	in := &settings{}
	if err := in.parse(s.proxyInput()); err != nil || in.UI.LogLevel != logQuiet || string(s.forProxy()) != string(goodSettings().forProxy()) {
		t.Errorf("proxy input: %v %q", err, in.UI.LogLevel)
	}
}

func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

// Never a login, a password or an address with a login in it, whatever happens: logins and passwords made up
// for this test are searched for in everything logged while a provider plays, refuses, breaks off, sends its own
// error page with the address in it, and is read in the background.
func TestLogNeverShowsSecrets(t *testing.T) {
	b := captureLog(t)
	const user, pass, pu, pp = "zqSecretUser41", "zqSecretPass42", "zqPlayerUser43", "zqPlayerPass44"
	var mu2 sync.Mutex
	n := 0
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		mu2.Lock()
		n++
		k := n
		mu2.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/movie/"):
			http.Error(w, "The requested URL "+r.URL.String()+" was not found", 404) // the address, login and all
		case r.URL.Path == "/player_api.php" && r.URL.Query().Get("action") == "":
			w.Write([]byte(`{"user_info":{"username":"` + user + `","password":"` + pass + `","status":"Active"},"server_info":{}}`))
		case r.URL.Path == "/player_api.php":
			http.Error(w, "slow down "+user, 429)
		case strings.HasPrefix(r.URL.Path, "/timeshift/"):
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close() // broken off
		case strings.Contains(r.URL.Path, "/9001.ts") && k%2 == 0:
			http.Error(w, "no", 403)
		default:
			return false
		}
		return true
	})
	f := newFront(provider{Name: "Hidden", URL: fp.srv.URL, User: user, Password: pass, Port: "18691", LiveTV: "ts"}, player{User: pu, Password: pp})
	mu.Lock()
	fronts[f.port] = f
	mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = r.Header.Get("X-Dev") + ":5000"
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		mu.Lock()
		delete(fronts, f.port)
		delete(accounts, f.port)
		for _, ch := range channels {
			if ch.provider == f.base {
				ch.drop()
			}
		}
		mu.Unlock()
	})
	for _, p := range []string{"/live/" + pu + "/" + pp + "/9001.ts", "/" + pu + "/" + pp + "/9001.ts",
		"/movie/" + pu + "/" + pp + "/55.mkv", "/timeshift/" + pu + "/" + pp + "/60/2026-11-24:20-00/9002.ts",
		"/player_api.php?username=" + pu + "&password=" + pp, "/player_api.php?username=" + pu + "&password=" + pp + "&action=get_vod_streams",
		"/xmltv.php?username=" + pu + "&password=" + pp, "/get.php?username=" + pu + "&password=" + pp} {
		r := getAs(t, srv.URL+p, "198.51.100.11")
		buf := make([]byte, 64<<10)
		r.Body.Read(buf)
		r.Body.Close()
	}
	playerAPI(t.Context(), f.port, "") // the status page's own read (background)
	time.Sleep(500 * time.Millisecond)
	log := b.String()
	if !strings.Contains(log, "provider=Hidden") || !strings.Contains(log, " WRN ") {
		t.Fatalf("nothing was logged; the test proves nothing:\n%s", log)
	}
	for _, secret := range []string{user, pass, pu, pp, "password=", "username="} {
		if strings.Contains(log, secret) {
			t.Errorf("%q is in the log:\n%s", secret, log)
		}
	}
}
