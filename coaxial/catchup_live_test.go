package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Catch-up of a channel that is open live on the same provider: the provider allows only one of the two on an
// account, so it ended the live stream (a recording) every time catch-up of it was opened, and the live stream
// asked for again ended the catch-up, over and over. The catch-up is refused here instead, before the provider
// is asked; catch-up of any other channel is passed on as before.
func TestCatchUpOfAChannelOpenLiveIsRefused(t *testing.T) {
	f := testFront()
	f.refuseCatchUp = true
	key := channelKey("/ts", f.base, "/live/real%20user/p%40ss%261/12.ts")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mu.Lock()
	channels[key] = &channel{key: key, provider: f.base, readers: map[*reader]bool{}, ready: make(chan struct{}), ctx: ctx, cancel: cancel}
	mu.Unlock()
	defer func() { mu.Lock(); delete(channels, key); mu.Unlock() }()

	for _, c := range []struct {
		path string
		want string // the channel it is catch-up of ("" when it is not catch-up with the easy login)
	}{
		{"/timeshift/tv/easy/60/2026-01-01:10-00/12.ts", "12"},
		{"/timeshift/tv/easy/60/2026-01-01:10-00/12.m3u8", "12"},
		{"//timeshift//tv/easy/18/2026-11-23:01-38/12", "12"},
		{"/streaming/timeshift.php?username=tv&password=easy&stream=12&start=2026-01-01:10-00&duration=60", "12"},
		{"/timeshift/tv/easy/60/2026-01-01:10-00/13.ts", "13"},
		{"/timeshift/other/login/60/2026-01-01:10-00/12.ts", ""},
		{"/streaming/timeshift.php?username=tv&password=wrong&stream=12", ""},
		{"/live/tv/easy/12.ts", ""},
		{"/movie/tv/easy/12.mkv", ""},
	} {
		u, _ := url.ParseRequestURI(c.path)
		if got := f.catchUpOf(u); got != c.want {
			t.Errorf("%s: catch-up of %q, want %q", c.path, got, c.want)
		}
	}

	mu.Lock()
	open12, open13 := f.liveOpen("12"), f.liveOpen("13")
	other := newFront(provider{Name: "Two", URL: "http://other.example", User: "u", Password: "p", Port: "8096", LiveTV: "ts"}, player{User: "tv", Password: "easy"})
	openElsewhere := other.liveOpen("12")
	mu.Unlock()
	if !open12 || open13 || openElsewhere {
		t.Errorf("open live: 12 %v (want true), 13 %v, 12 on another provider %v (want false)", open12, open13, openElsewhere)
	}

	for _, p := range []string{"/timeshift/tv/easy/60/2026-01-01:10-00/12.ts", "/streaming/timeshift.php?username=tv&password=easy&stream=12&start=x&duration=60"} {
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "open live") {
			t.Errorf("%s: answered %d %q, want 503 saying the channel is open live", p, rec.Code, rec.Body.String())
		}
	}

	// With the setting left at "allow", the same catch-up is passed on to the provider, not refused.
	f.refuseCatchUp = false
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest("GET", "/timeshift/tv/easy/60/2026-01-01:10-00/12.ts", nil))
	if strings.Contains(rec.Body.String(), "open live") {
		t.Errorf("allow: catch-up of a channel open live was refused (%d %q)", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ v, want string }{{"", ""}, {"allow", ""}, {"refuse", ""}, {"Refuse", "catch_up_while_live"}, {"no", "catch_up_while_live"}} {
		p := provider{Name: "One", URL: "http://one.example", User: "u", Password: "p", Port: "8095", LiveTV: "ts", CatchUpLive: c.v}
		if got := newFront(p, player{User: "tv", Password: "easy"}).refuseCatchUp; got != (c.v == "refuse") {
			t.Errorf("catch_up_while_live %q: refuse %v", c.v, got)
		}
		s := &settings{Player: player{User: "tv", Password: "easy"}, Providers: []provider{p}}
		if msg := fmt.Sprint(s.validate()); (c.want == "") == strings.Contains(msg, "catch_up_while_live") {
			t.Errorf("catch_up_while_live %q: check said %q", c.v, msg)
		}
	}

	// A live HLS session of the channel, on a provider whose players use HLS, counts the same while it is used.
	h := newFront(provider{Name: "Three", URL: "http://hls.example", User: "u", Password: "p", Port: "8097", LiveTV: "hls"}, player{User: "tv", Password: "easy"})
	skey := h.base + " /live/u/p/44.m3u8"
	mu.Lock()
	sessions[skey] = &session{loc: "http://cdn.example/44.m3u8", final: "http://cdn.example/44.m3u8", used: mono()}
	hlsOpen := h.liveOpen("44")
	sessions[skey].used = mono() - 31
	hlsStale := h.liveOpen("44")
	delete(sessions, skey)
	mu.Unlock()
	if !hlsOpen || hlsStale {
		t.Errorf("live HLS session: open %v (want true), unused for 31 s %v (want false)", hlsOpen, hlsStale)
	}
}
