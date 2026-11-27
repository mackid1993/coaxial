package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A film, an episode or a catch-up watched as HLS: the playlist is complete and asked for once, then the player
// reads piece after piece for the rest of the programme. Its pieces must keep the device counted as playing
// whatever their server types them as (a web server left at its defaults types a .ts file as text), or about
// 30 s after the playlist nothing counted it any more, and a VPN server move or an update, which wait for
// nothing to play, could run under the film.

// fhProv: a provider whose film 77 is watched as HLS, in the way given: "sent on" (to a video server, as most
// do), "answered itself" (the playlist at the film's own address, pieces on the provider's own server),
// "quality levels" (sent on to a list of qualities, one of which the player reads). Its catch-up of channel 12
// as HLS is sent on to its own server. Every piece is typed as text.
func fhProv(t *testing.T, how string) *fakeProv {
	list := func(w http.ResponseWriter, name string) {
		text := "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n"
		for i := 0; i < 30; i++ {
			text += fmt.Sprintf("#EXTINF:10.0,\n%s%d.ts\n", name, i)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(text + "#EXT-X-ENDLIST\n"))
	}
	piece := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(tsFill(2000))
	}
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch p := r.URL.Path; {
		case p == "/vod/tok1/master.m3u8":
			w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nv1/index.m3u8\n"))
		case strings.HasSuffix(p, ".m3u8"):
			list(w, "p")
		default:
			piece(w)
		}
		return true
	})
	return newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch p := r.URL.Path; {
		case p == "/movie/realU/realP/77.m3u8" && how == "sent on":
			http.Redirect(w, r, cdn.srv.URL+"/vod/tok1/index.m3u8", 302)
		case p == "/movie/realU/realP/77.m3u8" && how == "quality levels":
			http.Redirect(w, r, cdn.srv.URL+"/vod/tok1/master.m3u8", 302)
		case p == "/movie/realU/realP/77.m3u8":
			list(w, "/hls/f77/p")
		case strings.HasPrefix(p, "/timeshift/"):
			http.Redirect(w, r, fp.srv.URL+"/arch/tok1/index.m3u8", 302)
		case p == "/arch/tok1/index.m3u8":
			list(w, "piece")
		case strings.HasPrefix(p, "/hls/") || strings.HasPrefix(p, "/arch/"):
			piece(w)
		default:
			return false
		}
		return true
	})
}

// fhOpen: device dev opens a playlist the way a player does (redirects followed, a list of qualities read on to
// its first one) and returns the pieces as the paths the player asks this proxy for.
func fhOpen(t *testing.T, base, dev, path string) []string {
	t.Helper()
	for {
		st, data, err := devGet(base, dev, path, 10*time.Second)
		if err != nil || st != 200 {
			t.Fatalf("%s: status %d, %v, %q", path, st, err, data)
		}
		var lines []string
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				lines = append(lines, strings.TrimPrefix(l, base))
			}
		}
		if len(lines) == 0 {
			t.Fatalf("%s names nothing: %q", path, data)
		}
		if !strings.Contains(string(data), "#EXT-X-STREAM-INF") {
			return lines
		}
		if path = lines[0]; !strings.HasPrefix(path, "/") {
			t.Fatalf("a quality level is not pointed back here: %q", path)
		}
	}
}

// fhLater: the playlist was asked for more than 30 s ago (a player asks for a complete one once): what counted
// the device by its playlist requests is over, as it is half a minute into the film.
func fhLater(dev string) {
	mu.Lock()
	for _, row := range hlsRows {
		if row.who == dev {
			row.first, row.last = row.first-31, row.last-31
		}
	}
	delete(films, dev)
	mu.Unlock()
}

func TestFilmWatchedAsHLSCountsAsPlayingWhateverThePiecesType(t *testing.T) {
	for i, how := range []string{"sent on", "answered itself", "quality levels"} {
		t.Run(how, func(t *testing.T) {
			dev := fmt.Sprintf("192.0.2.%d", 71+i)
			_, base := cuHLSFront(t, fhProv(t, how), "", advanced{})
			pieces := fhOpen(t, base, dev, "/movie/tv/easy/77.m3u8")
			fhLater(dev)
			for _, p := range pieces[:2] {
				if st, data, err := devGet(base, dev, p, 10*time.Second); err != nil || st != 200 || len(data) == 0 {
					t.Fatalf("a piece: status %d, %v, %d bytes", st, err, len(data))
				}
			}
			if !playsNow(dev) {
				t.Errorf("a film watched as HLS (%s) whose pieces are being read does not count as playing", how)
			}
		})
	}
}

// The same for a catch-up watched as HLS that the provider sends on to its own server: the player asks this
// proxy for it at the provider's port, not under /__cdn/.
func TestCatchUpHLSOnTheProvidersOwnServerCountsAsPlaying(t *testing.T) {
	const dev = "192.0.2.75"
	_, base := cuHLSFront(t, fhProv(t, "sent on"), "", advanced{})
	pieces := fhOpen(t, base, dev, cuHLS12)
	fhLater(dev)
	for _, p := range pieces[:2] {
		if st, data, err := devGet(base, dev, p, 10*time.Second); err != nil || st != 200 || len(data) == 0 {
			t.Fatalf("a piece: status %d, %v, %d bytes", st, err, len(data))
		}
	}
	if !playsNow(dev) {
		t.Errorf("a catch-up watched as HLS on the provider's own server, its pieces being read, does not count as playing")
	}
}

// The other direction. A live HLS channel whose pieces are typed as text counts as playing by its playlist
// reloads, as before (30 s after the last one), not as a film: its pieces add no minute on top. And a film stops
// counting a minute after its last piece, so the background work that waits for nothing to play runs once the
// film is over.
func TestFilmPiecesCountOnlyWhileAFilmIsWatched(t *testing.T) {
	p := newRealProv(t, realCfg{target: 4, gaps: []float64{6.3}, piece: func(tok, n int, w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(tsFill(2000))
		return true
	}})
	const live, film = "192.0.2.76", "192.0.2.77"
	list := tuneIn(t, p, live)
	if st, _, err := devGet(p.base, live, pieceOf(t, list), 10*time.Second); err != nil || st != 200 {
		t.Fatalf("a live piece: status %d, %v", st, err)
	}
	mu.Lock()
	_, counted := films[live]
	mu.Unlock()
	if counted {
		t.Errorf("a piece of a live HLS channel was counted as a film")
	}
	_, base := cuHLSFront(t, fhProv(t, "sent on"), "", advanced{})
	pieces := fhOpen(t, base, film, "/movie/tv/easy/77.m3u8")
	fhLater(film)
	devGet(base, film, pieces[0], 10*time.Second)
	if !playsNow(film) {
		t.Fatalf("a film whose piece was just read does not count as playing")
	}
	mu.Lock()
	if f := films[film]; f != nil {
		f.last -= 61
	}
	mu.Unlock()
	if playsNow(film) {
		t.Errorf("a film whose last piece was read over a minute ago still counts as playing: nothing in the background would ever run")
	}
}
