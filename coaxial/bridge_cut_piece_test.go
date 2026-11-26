package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A recorder on a provider set to HLS is sent MPEG-TS made from the channel's HLS pieces (hlsSource), which are
// fetched inside the program, through the front door's own handlers. A piece whose server goes quiet part way
// (through a VPN tunnel that happens) is given up after pieceWait. It must then be asked for again and handed
// on whole, as the bridge does for a piece that sent nothing at all. Handed on cut short as if that were all of
// it, the recording lost the rest of the piece (seconds of video) and went on in the middle of a packet.
func TestBridgePieceStoppedPartWayIsAskedAgain(t *testing.T) {
	packet := append([]byte{0x47}, bytes.Repeat([]byte{0xa5}, 187)...)
	whole := 1 << 20 / 188 * 188
	_, _, base := quietHLS(t, func(path string, n int, w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(path, ".ts") || n != 1 {
			return false
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", fmt.Sprint(whole))
		w.Write(bytes.Repeat(packet, 1500))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // (quiet from here on, never closed)
		return true
	})
	ctx, cancel := context.WithTimeout(context.Background(), secs(2*pieceWait+8))
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/live/tv/easy/44.ts", nil)
	req.Header.Set("X-Dev", "192.0.2.9")
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("the recorder got no answer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the recorder was answered %d", resp.StatusCode)
	}
	began := time.Now()
	var got []byte
	buf := make([]byte, 65536)
	for len(got) < whole {
		n, err := resp.Body.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if len(got) != whole {
		t.Fatalf("the recorder was sent %d bytes of the piece (%d packets and %d bytes) in %.1f s, want all %d: the piece was handed on cut short",
			len(got), len(got)/188, len(got)%188, time.Since(began).Seconds(), whole)
	}
	if d := diffAt(bytes.Repeat(packet, whole/188), got); d != "" {
		t.Errorf("the piece the recorder was sent is not the provider's: %s", d)
	}
}
