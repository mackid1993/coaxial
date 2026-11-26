package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fastFilm: a provider that sends a film as fast as it is read (a player's read ahead fills its buffers).
func fastFilm(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/movie/") {
		return false
	}
	fp.mu.Lock()
	fp.open["film"]++
	fp.most["film"] = max(fp.most["film"], fp.open["film"])
	fp.mu.Unlock()
	defer func() {
		fp.mu.Lock()
		fp.open["film"]--
		fp.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "video/mp4")
	if rg := r.Header.Get("Range"); rg == "bytes=0-1" {
		w.Header().Set("Content-Range", "bytes 0-1/1000000000")
		w.WriteHeader(206)
		w.Write([]byte{0, 0})
		return true
	}
	if r.Method == "HEAD" {
		return true
	}
	chunk := make([]byte, 64<<10)
	for r.Context().Err() == nil {
		if _, err := w.Write(chunk); err != nil {
			break
		}
	}
	return true
}

// stalled: a read of path by dev on a connection of its own that takes the answer's start and then reads no
// more (a player whose buffer is full, or one that seeks by opening the new read before closing the old,
// as ffmpeg and VLC do).
func stalled(t *testing.T, base, dev, path string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nX-Dev: %s\r\n\r\n", path, dev)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || !strings.Contains(line, " 200 ") {
		t.Fatalf("the first read: %q %v", line, err)
	}
	time.Sleep(1500 * time.Millisecond) // (the buffers on the way fill: the front door's write to it blocks)
	return c
}

// A player seeks while its older read of the film is stalled (not read from, as a player's read ahead
// leaves it). The old read is cut at once at the provider, and the new one may start: it must not wait out
// the whole grace time, at every seek.
func TestItemSeekWhileOldReadStalls(t *testing.T) {
	for _, on := range []bool{false, true} {
		fp := newFakeProv(t, false, fastFilm)
		_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerItem: on})
		old := stalled(t, base, "192.0.2.1", "/movie/tv/easy/7.mp4")
		began := time.Now()
		seek := ask(t, base, "192.0.2.1", "/movie/tv/easy/7.mp4", "Range", "bytes=500000000-")
		took := time.Since(began)
		if seek.StatusCode != 200 {
			t.Fatalf("on=%v: the seek: %d", on, seek.StatusCode)
		}
		seek.Body.Close()
		// (1.5 s where it was 1 s, for slow test machines. Not more: the fault this guards is a seek that
		// waits out the grace time, 2 s, and the limit has to stay under that to see it.)
		if took > 1500*time.Millisecond {
			t.Errorf("on=%v: the seek took %.1f s to start", on, took.Seconds())
		}
		if on {
			old.SetReadDeadline(time.Now().Add(4 * time.Second))
			if _, err := io.Copy(io.Discard, old); err != nil {
				t.Errorf("the older read was not ended: %v", err)
			}
			if n := fp.count(fp.most, "film"); n != 1 {
				t.Errorf("the provider saw the film open %d times at once, want 1", n)
			}
		}
	}
}
