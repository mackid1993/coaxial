package main

// MPEG-TS made from a provider's HLS (hlsSource, the "/hls" bridge a recorder gets on a
// provider whose live_tv is "hls") across a clock that starts over inside the stream (a 24/7 channel's next
// episode). The pieces are the provider's stream cut at packet starts and served as a live playlist that moves
// on one piece per look, as a provider's does.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// hlsWindow: a live playlist of size pieces that moves on one piece each time it is read, ENDLIST at the end.
type hlsWindow struct {
	mu     sync.Mutex
	pieces [][]byte
	k      int
	size   int
}

func (w *hlsWindow) RoundTrip(r *http.Request) (*http.Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	status, body := 200, []byte(nil)
	if r.URL.Path == "/p.m3u8" {
		end := min(len(w.pieces), w.k+w.size)
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", w.k)
		for i := w.k; i < end; i++ {
			fmt.Fprintf(&b, "#EXTINF:0.5,\n%d.ts\n", i)
		}
		if end == len(w.pieces) {
			b.WriteString("#EXT-X-ENDLIST\n")
		} else {
			w.k++
		}
		body = []byte(b.String())
	} else if i := 0; func() bool { _, err := fmt.Sscanf(r.URL.Path, "/%d.ts", &i); return err == nil }() && i < len(w.pieces) {
		body = w.pieces[i]
	} else {
		status = 404
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

// hlsCut: data cut into pieces of about size bytes, each starting at a packet start (three 0x47 bytes 188
// apart), and also at cut (a new file starts a new piece; -1: no such cut).
func hlsCut(data []byte, size, cut int) [][]byte {
	isStart := func(p int) bool {
		return p+376 < len(data) && data[p] == 0x47 && data[p+188] == 0x47 && data[p+376] == 0x47
	}
	var out [][]byte
	for from := 0; from < len(data); {
		to := from + size
		for to < len(data) && !isStart(to) {
			to++
		}
		if cut > from && cut < to {
			to = cut
		}
		to = min(to, len(data))
		out = append(out, data[from:to])
		from = to
	}
	return out
}

func throughHLSBridge(t *testing.T, pieces [][]byte) []byte {
	t.Helper()
	src := hlsSource("http://hls.test/p.m3u8", "", &hlsWindow{pieces: pieces, size: 3})
	var out []byte
	if err := src(context.Background(), &channel{id: "audit"}, func(d []byte) bool { out = append(out, d...); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

// A 24/7 channel's next episode, read as HLS: the recorder's MPEG-TS clock carries on.
func TestHLSBridgeEpisodeChangeSynthetic(t *testing.T) {
	data := ecSynthetic()
	jump, _, _ := ecFirstJump(ecStamps(data))
	ecCheck(t, "synthetic 24/7 stream as HLS, new file = new piece", data, throughHLSBridge(t, hlsCut(data, 188*800, jump)))
	ecCheck(t, "synthetic 24/7 stream as HLS, new file inside a piece", data, throughHLSBridge(t, hlsCut(data, 188*800, -1)))
}

func TestHLSBridgeEpisodeChangeReplay(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_EPISODE")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_EPISODE names no recorded episode change")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jump, _, _ := ecFirstJump(ecStamps(data[syncAt(data, 0):]))
	jump += syncAt(data, 0)
	ecCheck(t, "recorded 24/7 stream as HLS, new file = new piece", data, throughHLSBridge(t, hlsCut(data, 2<<20, jump)))
	ecCheck(t, "recorded 24/7 stream as HLS, new file inside a piece", data, throughHLSBridge(t, hlsCut(data, 2<<20, -1)))
}

// A stream whose clock never starts over comes out of the bridge byte for byte as the pieces went in.
func TestHLSBridgeUntouched(t *testing.T) {
	data := ecSynthetic()
	jump, _, _ := ecFirstJump(ecStamps(data))
	data = data[:jump] // (the first episode only)
	if out := throughHLSBridge(t, hlsCut(data, 188*800, -1)); !bytes.Equal(out, data) {
		t.Fatalf("synthetic: a stream with no clock jump was changed (%d bytes in, %d out): %s", len(data), len(out), diffAt(data, out))
	}
	path := os.Getenv("COAXIAL_REPLAY_TS")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_TS not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out := throughHLSBridge(t, hlsCut(data, 4<<20, -1)); !bytes.Equal(out, data) {
		t.Fatalf("recorded: a stream with no clock jump was changed (%d bytes in, %d out): %s", len(data), len(out), diffAt(data, out))
	}
}
