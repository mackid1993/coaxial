package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"
)

// saClient: a device's own connection to the proxy, with a small socket buffer (a TV's player holds what it
// has read in its own buffer, not in the network stack), so what the proxy still holds for it is what the
// player has not read yet.
func saClient() *http.Client {
	d := &net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 256<<10) })
	}}
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: d.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// A viewer tunes a 20 Mbit/s channel, and a few seconds in its user presses pause for 16 s, then plays on: the
// player reads nothing while paused and then reads at playing speed. It is now 12 to 18 s of video behind the
// provider (the low end just before each lump, the high end just after), which is more than 16 MB and more
// than 5 s of this channel's video, but under 15 s of it at each lump's start: a player that paused and plays
// on is not a reader that fell behind, and it is never cut. A second device on the same channel that stopped
// reading for good (its app hung) is cut, and the viewer is not held up by it.
// Guards behindVideo (on a high-rate channel the allowance is 15 s of its video, not 16 MB) and farBehind.
func TestLiveTSRealisticPausedViewerOnHighRateChannelKept(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(50, 1000, 240) // (about 20 Mbit/s)
	rate := float64(len(feed.data)) / 50
	const burst, pauseFrom, pauseTo, end = 6.0, 2.0, 18.0, 34.0
	p.add("9", &rpChannel{feed: feed, lumps: rpLumps(burst, 6, 12, 18, 24, 30), back: 8})
	log := captureLog(t)
	base := rpFront(t, p, "ts", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/live/tv/easy/9.ts", nil)
	req.Header.Set("X-Dev", "192.0.2.16")
	resp, err := saClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the channel: %d", resp.StatusCode)
	}
	u, _ := url.Parse(base) // a second device asks for the channel and, after the first moment, never reads again
	hung, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer hung.Close()
	fmt.Fprintf(hung, "GET /live/tv/easy/9.ts HTTP/1.1\r\nHost: %s\r\nX-Dev: 192.0.2.17\r\n\r\n", u.Host)

	got, cut := 0, 0.0
	buf := make([]byte, 256<<10)
	read := func(most int) bool {
		k, err := resp.Body.Read(buf[:most])
		if k > 0 && !bytes.Equal(buf[:k], feed.data[got:got+k]) {
			t.Fatalf("the viewer was sent bytes the provider did not send, at byte %d", got)
		}
		got += k
		if err != nil && ctx.Err() == nil {
			cut = p.wall()
		}
		return err == nil
	}
	for p.wall() < pauseFrom && read(len(buf)) { // it plays from the start, reading all it is sent...
	} // (its last read waits for the provider's next lump, and then it stops: paused just after the opening burst)
	if cut == 0 && p.until(ctx, pauseTo) { // ...is paused, then plays on: it reads as fast as the video plays
		resumed := got
		for cut == 0 && ctx.Err() == nil {
			owe := resumed + int((p.wall()-pauseTo)*rate) - got
			if owe <= 0 {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if !read(min(owe, len(buf))) {
				break
			}
		}
	}
	behind := (float64(feed.upTo(30+burst)) - float64(got)) / rate
	t.Logf("the viewer: %.1f s of video read, %.1f s behind the provider at the end", float64(got)/rate, behind)
	if cut > 0 {
		t.Errorf("a viewer that paused for %.0f s and played on was cut at %.1f s (%.1f s of video behind)", pauseTo-pauseFrom, cut, behind)
	}
	if n := len(linesWith(log.String(), "too slow to keep up", "192.0.2.16")); n != 0 {
		t.Errorf("the viewer was taken for too slow %d times", n)
	}
	if n := len(linesWith(log.String(), "too slow to keep up", "192.0.2.17")); n != 1 {
		t.Errorf("the device that stopped reading was cut %d times, want once", n)
	}
	if n := p.asked("9"); n != 1 {
		t.Errorf("the provider was asked %d times, want once", n)
	}
}
