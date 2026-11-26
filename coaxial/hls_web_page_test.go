package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A player inside a web page. Its browser lets it read an answer from another address only if the answer carries
// Access-Control-Allow-Origin, and that goes for every redirect on the way as well: one redirect without the
// header and the player is told nothing but "failed". A provider that allows web pages sends the header on all
// its answers, and the proxy passes those on. The redirects the proxy makes itself (to a channel's folder) must
// not be the one answer on the way that stops such a player.

var errPageBlocked = errors.New("an answer without Access-Control-Allow-Origin: the browser stops the page's player here")

// pageGet: one ask by a player in a web page, the way a browser makes it: the page is named (Origin), redirects
// are followed only while each one allows the page, and the last answer must allow it too.
func pageGet(base, path string) (status int, data []byte, err error) {
	client := &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.Response.Header.Get("Access-Control-Allow-Origin") == "" {
				return fmt.Errorf("status %d for %s: %w", req.Response.StatusCode, via[len(via)-1].URL.Path, errPageBlocked)
			}
			return nil
		}}
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Origin", "http://page.example")
	req.Header.Set("X-Dev", "192.0.2.31")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") == "" {
		return resp.StatusCode, nil, fmt.Errorf("status %d for %s: %w", resp.StatusCode, resp.Request.URL.Path, errPageBlocked)
	}
	data, err = io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// pagePlays: the page's player tunes the channel: its playlist, then the first piece the playlist names.
func pagePlays(t *testing.T, base, channel string) {
	t.Helper()
	status, list, err := pageGet(base, channel)
	if err != nil || status != 200 || !strings.HasPrefix(string(list), "#EXTM3U") {
		t.Fatalf("a player in a web page asked for the channel and got no playlist: status %d, %v", status, err)
	}
	piece := ""
	for _, line := range strings.Split(string(list), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") && piece == "" {
			piece = line
		}
	}
	if !strings.HasPrefix(piece, "/") { // (named next to the playlist: worked out as a browser does)
		piece = "/__ch" + channel + "/" + piece
	}
	if status, data, err := pageGet(base, piece); err != nil || status != 200 || len(data) == 0 {
		t.Fatalf("the page's player got the playlist but not its first piece: status %d, %d bytes, %v", status, len(data), err)
	}
}

// plainRedirect: a player that is not in a web page (it names no page) gets the proxy's redirect as it always did.
func plainRedirect(t *testing.T, base, channel string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", base+channel, nil)
	req.Header.Set("X-Dev", "192.0.2.32")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/__ch"+channel+"/__index.m3u8" {
		t.Fatalf("an ordinary player: status %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, ok := resp.Header["Access-Control-Allow-Origin"]; ok {
		t.Errorf("an ordinary player's redirect changed: it now carries Access-Control-Allow-Origin")
	}
}

// The provider's live TV is HLS: the channel's address is sent on to a video server (the usual shape).
func TestHLSChannelForAPlayerInAWebPage(t *testing.T) {
	cdn := newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/v/tok/") {
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", "*") // (a provider that allows web pages: on every answer)
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:4,\np7.ts\n#EXTINF:4,\np8.ts\n")
			return true
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(make([]byte, 1880))
		return true
	})
	prov := newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/31.m3u8" {
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Redirect(w, r, cdn.srv.URL+"/v/tok/index.m3u8", 302)
		return true
	})
	_, base := limFront(t, prov, "hls", "", advanced{})
	pagePlays(t, base, "/live/tv/easy/31.m3u8")
	plainRedirect(t, base, "/live/tv/easy/31.m3u8")
	// (a piece asked for next to the playlist is sent on to where it really is: that redirect too)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", base+"/__ch/live/tv/easy/31.m3u8/p7.ts", nil)
	req.Header.Set("Origin", "http://page.example")
	req.Header.Set("X-Dev", "192.0.2.31")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Errorf("a piece asked for next to the playlist: status %d, Access-Control-Allow-Origin %q; the page's player may not follow it",
			resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

// The provider's live TV is MPEG-TS and the playlist is made here (tshls.go). The provider's own headers for the
// stream are already put on the playlist and its pieces; the redirect to the channel's folder comes before them.
func TestTSChannelAsHLSForAPlayerInAWebPage(t *testing.T) {
	p := &tsHLSProv{s: wreckBuild(11, wreckPart{secs: 30, clock: 1000}), burst: 12,
		asks: map[string]int{}, open: map[string]int{}, closed: map[string]time.Time{}}
	fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/live/realU/realP/") || !strings.HasSuffix(r.URL.Path, ".ts") {
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", "*") // (a provider that allows web pages)
		p.serve(w, r)
		return true
	})
	_, base := limFront(t, fp, "ts", "", advanced{})
	pagePlays(t, base, "/live/tv/easy/7.m3u8")
	plainRedirect(t, base, "/live/tv/easy/7.m3u8")
}
