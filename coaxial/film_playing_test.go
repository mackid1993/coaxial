package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A film, an episode or catch-up being watched counts as playing (the VPN is not restarted under it for an
// update or a server check), and for a minute after its last answer, while the player asks for the next part.
// A poster does not.
func TestFilmCountsAsPlaying(t *testing.T) {
	release := make(chan struct{})
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/logo.png" {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("PNG"))
			return
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.WriteHeader(206)
		w.Write(make([]byte, 65536))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer prov.Close()
	defer func() { mu.Lock(); delete(films, "192.0.2.31"); delete(films, "192.0.2.32"); mu.Unlock() }()
	f := newFront(provider{Name: "P", URL: prov.URL, User: "realU7", Password: "realP7-long", Port: "18514", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	plays := func(who string) bool {
		for _, p := range playingNow() {
			if p.(map[string]any)["device"] == who {
				return true
			}
		}
		return false
	}
	ask := func(target, who string) {
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr = who + ":5000"
		f.ServeHTTP(httptest.NewRecorder(), r)
	}
	ask("/logo.png", "192.0.2.31")
	if plays("192.0.2.31") {
		t.Errorf("a poster counts as playing")
	}
	done := make(chan struct{})
	go func() { ask("/movie/tv/easy/77.mkv", "192.0.2.32"); close(done) }()
	for end := time.Now().Add(5 * time.Second); !plays("192.0.2.32") && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	if !plays("192.0.2.32") {
		t.Errorf("a film being watched does not count as playing")
	}
	close(release)
	<-done
	if !plays("192.0.2.32") {
		t.Errorf("a film just watched does not count as playing for the next part")
	}
}
