package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every piece of a live HLS channel gets a live piece's wait (pieceWait), however a player asks for it and
// however its server answers. The fakes go quiet without closing, as a connection through a VPN tunnel can.

// askPiece: a player's request for a piece with these headers; the status, the whole body and how long it
// took. The player gives up after 20 s (longer than any player waits).
func askPiece(base, path string, hdr ...string) (int, []byte, time.Duration) {
	began := time.Now()
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("X-Dev", "192.0.2.1")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, time.Since(began)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return -1, data, time.Since(began)
	}
	return resp.StatusCode, data, time.Since(began)
}

const pieceSize = 1 << 20 / 188 * 188 // (quietHLS's piece)

// Players built on FFmpeg ask for every piece with "Range: bytes=0-" (libavformat/http.c sends it whenever
// seeking is not ruled out, and hls.c leaves that open), and servers answer 206. Such a piece that stops part
// way is carried on from where it stopped, like one asked for without a Range: the player gets the whole piece
// on its one answer after pieceWait, not a broken one after 60 s.
func TestHLSPieceAskedWithRangeStoppedPartWay(t *testing.T) {
	var lock sync.Mutex
	var ranges []string
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(path, ".ts") {
			return false
		}
		lock.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		lock.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Content-Length", fmt.Sprint(pieceSize))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", pieceSize-1, pieceSize))
			w.WriteHeader(206)
			w.Write(bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 1500))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return true
		}
		return false
	})
	_, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	status, data, took := askPiece(base, pieceOf(t, list), "Range", "bytes=0-")
	if status != 206 || len(data) != pieceSize {
		t.Errorf("piece: status %d, %d bytes after %.1f s, want the whole piece (%d bytes) on one 206 answer", status, len(data), took.Seconds(), pieceSize)
	}
	if took > secs(pieceWait+4) {
		t.Errorf("the piece took %.1f s; a server that goes quiet must be given up after %d s", took.Seconds(), pieceWait)
	}
	lock.Lock()
	defer lock.Unlock()
	if want := fmt.Sprintf("bytes=%d-%d", 1500*188, pieceSize-1); len(ranges) != 2 || ranges[0] != "bytes=0-" || ranges[1] != want {
		t.Errorf("the server was asked with Range %q, want [\"bytes=0-\" %q]", ranges, want)
	}
}

// A piece sent without a Content-Length (some servers send pieces in chunks) that goes quiet part way.
func TestHLSPieceWithoutLengthStoppedPartWay(t *testing.T) {
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(path, ".ts") || n != 1 {
			return false
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 1500))
		w.(http.Flusher).Flush() // (no length: sent in chunks)
		<-r.Context().Done()
		return true
	})
	_, list, _ := playerGet(t, base, "/live/tv/easy/44.m3u8")
	status, data, took := askPiece(base, pieceOf(t, list))
	if status != 200 || len(data) != pieceSize {
		t.Errorf("piece: status %d, %d bytes after %.1f s, want the whole piece (%d bytes)", status, len(data), took.Seconds(), pieceSize)
	}
	if took > secs(pieceWait+4) {
		t.Errorf("the piece took %.1f s; a server that goes quiet must be given up after %d s", took.Seconds(), pieceWait)
	}
}

// A channel whose playlist lists quality levels (a master playlist): the player reads the variant's own
// playlist, and the pieces that one names are live pieces too. One whose server takes the request and says
// nothing is asked again on a new connection after pieceWait.
func TestHLSVariantPieceQuietServerAskedAgain(t *testing.T) {
	piece := bytes.Repeat(append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...), 2000)
	var lock sync.Mutex
	asks := 0
	cdn := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/hlsr/tok46/index.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nv1/mono.m3u8\n")
		case "/hlsr/tok46/v1/mono.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\np1.ts\n")
		case "/hlsr/tok46/v1/p1.ts":
			lock.Lock()
			asks++
			first := asks == 1
			lock.Unlock()
			if first {
				<-r.Context().Done()
				return true
			}
			w.Header().Set("Content-Type", "video/mp2t")
			http.ServeContent(w, r, "p1.ts", time.Time{}, bytes.NewReader(piece))
		default:
			return false
		}
		return true
	})
	prov := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/live/realU/realP/46.m3u8" {
			http.Redirect(w, r, cdn.srv.URL+"/hlsr/tok46/index.m3u8", 302)
			return true
		}
		return false
	})
	_, base := limFront(t, prov, "hls", "", advanced{})
	status, master, _ := playerGet(t, base, "/live/tv/easy/46.m3u8")
	if status != 200 || !bytes.Contains(master, []byte("mono.m3u8")) {
		t.Fatalf("master playlist: %d %q", status, master)
	}
	status, list, _ := playerGet(t, base, pieceOf(t, master))
	if status != 200 || !bytes.Contains(list, []byte("#EXTINF")) {
		t.Fatalf("variant playlist: %d %q", status, list)
	}
	status, data, took := askPiece(base, pieceOf(t, list))
	if status != 200 || len(data) != len(piece) {
		t.Errorf("piece: status %d, %d bytes after %.1f s, want the whole piece (%d bytes)", status, len(data), took.Seconds(), len(piece))
	}
	if took > secs(pieceWait+4) {
		t.Errorf("the piece took %.1f s; a quiet server must be asked again after %d s", took.Seconds(), pieceWait)
	}
}
