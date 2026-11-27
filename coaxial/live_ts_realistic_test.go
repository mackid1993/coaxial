package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Live MPEG-TS through the stand-in provider of provider_realistic_test.go, in the shapes healthy playback
// takes at home: lumps and long pauses, reads ending in a packet, a recorder and a viewer on one channel, a
// device joining during a pause, channel changes, two channels from one address. None of the program's waits
// and limits may act on any of it.

// rpWaitAll: wait for the readers to finish (their context has ended).
func rpWaitAll(rds ...*rpReader) {
	for _, rd := range rds {
		<-rd.done
	}
}

// rpExact: the reader was sent exactly the provider's bytes from..to, as the provider sent them.
func rpExact(t *testing.T, who string, rd *rpReader, feed []byte, from, to int) {
	t.Helper()
	got := rd.bytes()
	if at := rpIsPart(feed, got); at != from {
		t.Errorf("%s: what it was sent is not the provider's stream from byte %d (found at %d, %d bytes)", who, from, at, len(got))
	}
	if len(got) != to-from {
		t.Errorf("%s: was sent %d bytes, the provider had sent %d", who, len(got), to-from)
	}
}

// A viewer and a recorder on one channel from the start, and a third device that tunes in halfway through a
// 17 s provider pause (the longest healthy pause seen, at peak load). The provider is asked once; the viewer and
// the recorder get exactly what it sent, as it sent it; the device that joined in the pause is handed enough
// video at once to play through the rest of the pause without running out.
// Guards liveQuiet (it must stay above a healthy pause) and the joiner's cushion (joinSeconds).
func TestLiveTSRealisticPauseWithJoiner(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(60, 1000, 27)
	const pauseFrom, pauseTo, end = 6.0, 23.0, 30.5 // (17 s with nothing from the provider)
	p.add("7", &rpChannel{feed: feed, lumps: rpLumps(18, pauseFrom, pauseTo, 29), back: 8})
	base := rpFront(t, p, "ts", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()
	viewer := rpRead(ctx, p, base, "192.0.2.10", "/live/tv/easy/7.ts", 0, 0)
	time.Sleep(300 * time.Millisecond)
	recorder := rpRead(ctx, p, base, "192.0.2.11", "/live/tv/easy/7.ts", 0, 0)
	const joinAt = (pauseFrom + pauseTo) / 2
	p.until(ctx, joinAt)
	joiner := rpRead(ctx, p, base, "192.0.2.12", "/tv/easy/7.ts", 0, 0) // (the address form a media server uses)
	rpWaitAll(viewer, recorder, joiner)
	if n := p.asked("7"); n != 1 {
		t.Errorf("the provider was asked for the channel %d times, want once (a 17 s pause is its own pace)", n)
	}
	want := feed.upTo(29 + 18)
	rpExact(t, "the viewer", viewer, feed.data, 0, want)
	if _, _, stalls, stalled := viewer.view(0).play(2, 1, end); stalls > 0 {
		t.Errorf("the viewer ran out of video %d times (%.1f s); the provider's lead covers its pause", stalls, stalled)
	}
	rpExact(t, "the recorder", recorder, feed.data, 0, want)
	for who, rd := range map[string]*rpReader{"the viewer": viewer, "the recorder": recorder, "the device that joined": joiner} {
		if rd.ended > 0 {
			t.Errorf("%s: its answer ended at %.1f s", who, rd.ended)
		}
	}
	v := joiner.view(joinAt)
	first, least, stalls, stalled := v.play(2, 1, end)
	t.Logf("the device that joined %.1f s into the pause: playing %.1f s after asking, fewest seconds in hand %.1f, %d stalls (%.1f s)",
		joinAt-pauseFrom, first, least, stalls, stalled)
	if first < 0 || first > 3 {
		t.Errorf("the device that joined in the pause played %.1f s after asking, want within 3 s", first)
	}
	if stalls > 0 {
		t.Errorf("the device that joined in the pause ran out of video %d times (%.1f s) before the provider's next lump", stalls, stalled)
	}
	if v.back > 0 || len(v.skips) > 0 {
		t.Errorf("the device that joined: the clock went backwards %d times, jumped %v", v.back, v.skips)
	}
	if at := rpIsPart(feed.data, joiner.bytes()); at < 0 {
		t.Errorf("the device that joined was sent bytes that are not one stretch of the provider's stream")
	}
}

// A player with a small buffer on a 4K channel: it reads as fast as it plays from its first moment on, so most of
// the provider's opening burst (18 s of video, about 40 MB) waits in the proxy for it the whole time. That is a
// healthy player, not one that fell behind: it is never cut, and gets every byte in order.
// Guards the slow-reader rule (behind, behindSeconds, behindVideo, farBehind).
func TestLiveTSRealisticSmallBufferPlayerKept(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(48, 1000, 240) // (about 18 Mbit/s)
	rate := float64(len(feed.data)) / 48
	const end = 27.0
	p.add("9", &rpChannel{feed: feed, lumps: rpLumps(18, 6, 12, 18, 24), back: 8})
	base := rpFront(t, p, "ts", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()
	rd := rpRead(ctx, p, base, "192.0.2.15", "/live/tv/easy/9.ts", rate, 0.1)
	rpWaitAll(rd)
	if rd.ended > 0 {
		t.Errorf("a player reading as fast as it plays was cut at %.1f s", rd.ended)
	}
	got := rd.bytes()
	if at := rpIsPart(feed.data, got); at != 0 {
		t.Errorf("the player was not sent the provider's stream in order from its start (found at %d)", at)
	}
	if secs := float64(len(got)) / rate; secs < end-3 {
		t.Errorf("the player was sent %.1f s of video in %.0f s", secs, end)
	}
	if n := p.asked("9"); n != 1 {
		t.Errorf("the provider was asked %d times, want once", n)
	}
}

// One device address (a TV with a built-in recorder) records channel 7 and shows it too. The viewer goes to
// channel 8 for a while and comes back to 7. On an account with two connections, channel 7 is never let go: the
// provider is asked once for each channel, the recording gets every byte, and the viewer back on 7 plays at once.
// Guards the channel-change rule in live (only channels nobody reads go when another is opened).
func TestLiveTSRealisticViewerZapsAwayFromRecording(t *testing.T) {
	p := newTSRealProv(t, 1, 2)
	f7, f8 := rpBuild(50, 1000, 27), rpBuild(50, 5000, 27)
	p.add("7", &rpChannel{feed: f7, lumps: rpLumps(14, 6, 12, 18, 24), back: 8})
	p.add("8", &rpChannel{feed: f8, lumps: rpLumps(14, 6, 12, 18, 24), back: 8})
	base := rpFront(t, p, "ts", "2")
	const dev, end = "192.0.2.20", 26.0
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()
	recorder := rpRead(ctx, p, base, dev, "/live/tv/easy/7.ts", 0, 0)
	time.Sleep(500 * time.Millisecond)
	tv1ctx, tv1end := context.WithCancel(ctx)
	tv := rpRead(tv1ctx, p, base, dev, "/live/tv/easy/7.ts", 0, 0)
	p.until(ctx, 8)
	tv1end() // the viewer changes channel: its hang-up and the next request close together
	rpWaitAll(tv)
	tv2ctx, tv2end := context.WithCancel(ctx)
	tv = rpRead(tv2ctx, p, base, dev, "/live/tv/easy/8.ts", 0, 0)
	p.until(ctx, 16)
	tv2end()
	rpWaitAll(tv)
	back := p.wall()
	tv = rpRead(ctx, p, base, dev, "/live/tv/easy/7.ts", 0, 0)
	rpWaitAll(recorder, tv)
	if a7, a8 := p.asked("7"), p.asked("8"); a7 != 1 || a8 != 1 {
		t.Errorf("the provider was asked for channel 7 %d times and channel 8 %d times, want once each", a7, a8)
	}
	rpExact(t, "the recording", recorder, f7.data, 0, f7.upTo(24+14))
	p.mu.Lock()
	kicked, most := len(p.kicked), p.most
	p.mu.Unlock()
	if kicked > 0 || most > 2 {
		t.Errorf("the provider ended %d connections to make room (%d open at most, the account has 2)", kicked, most)
	}
	if recorder.ended > 0 {
		t.Errorf("the recording's answer ended at %.1f s", recorder.ended)
	}
	v := tv.view(back)
	if first, _, _, _ := v.play(2, 1, end); first < 0 || first > 3 {
		t.Errorf("the viewer back on channel 7 played %.1f s after asking, want within 3 s", first)
	}
	if v.back > 0 {
		t.Errorf("the viewer back on channel 7: the clock went backwards %d times", v.back)
	}
}

// One device address records channel 7 as MPEG-TS while it shows channel 8 as HLS built from the MPEG-TS (a TV
// recording one channel and showing another), on an account with two connections. The recording starting is
// not the viewer leaving channel 8: its builder and provider connection stay, and its playlist carries on.
// Guards releaseFor (one address may read two channels on an account with more than one connection) where
// live MPEG-TS calls it.
func TestLiveTSRealisticRecordingBesideHLSViewerOneAddress(t *testing.T) {
	p := newTSRealProv(t, 1, 2)
	f7, f8 := rpBuild(40, 1000, 27), rpBuild(40, 5000, 27)
	p.add("7", &rpChannel{feed: f7, lumps: rpLumps(14, 6, 12, 18), back: 8})
	p.add("8", &rpChannel{feed: f8, lumps: rpLumps(14, 6, 12, 18), back: 8})
	base := rpFront(t, p, "ts", "2")
	const dev, end = "192.0.2.30", 20.0
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()
	pl := rpPlay(ctx, t, p, base, dev, "/live/tv/easy/8.m3u8", 2)
	p.until(ctx, 4)
	recorder := rpRead(ctx, p, base, dev, "/live/tv/easy/7.ts", 0, 0)
	rpWaitAll(recorder)
	time.Sleep(200 * time.Millisecond)
	if a7, a8 := p.asked("7"), p.asked("8"); a7 != 1 || a8 != 1 {
		t.Errorf("the provider was asked for channel 7 %d times and channel 8 %d times, want once each", a7, a8)
	}
	rpExact(t, "the recording", recorder, f7.data, f7.upTo(14-8), f7.upTo(18+14)) // (opened at 4 s: its burst starts 8 s back)
	pl.mu.Lock()
	defer pl.mu.Unlock()
	rpSteady(t, "the HLS viewer", pl)
}

// rpSteady: an HLS player's playlists carried on as one: no failed request, no end, the media sequence never
// going back, and no piece marked as not following on after the first one it was given (a builder made anew
// marks its first piece so; the discontinuity sequence itself steps up when that piece leaves the list). (pl.mu
// held)
func rpSteady(t *testing.T, who string, pl *rpPlayer) {
	t.Helper()
	if len(pl.fails) > 0 {
		t.Errorf("%s: %v", who, pl.fails)
	}
	if pl.ended {
		t.Errorf("%s: a playlist ended the stream (#EXT-X-ENDLIST)", who)
	}
	if pl.lists < 3 {
		t.Errorf("%s: only %d playlists", who, pl.lists)
	}
	for i := 1; i < len(pl.seqs); i++ {
		if pl.seqs[i] < pl.seqs[i-1] || pl.discs[i] < pl.discs[i-1] {
			t.Errorf("%s: playlist %d went back: media sequence %v, discontinuity sequence %v", who, i, pl.seqs, pl.discs)
			break
		}
	}
	for n := range pl.discAt {
		if len(pl.seqs) > 0 && n > pl.seqs[0] {
			t.Errorf("%s: piece %d was marked as not following on (the first playlist started at %d): the stream started over", who, n, pl.seqs[0])
		}
	}
}

// A device watches a channel as MPEG-TS; a second device asks for it as HLS halfway through a 17 s provider
// pause. The builder starts from the joiner's cushion: the HLS player gets its first playlist at once (a player
// waits 8 s for an answer before it gives up), then rides out the rest of the pause on the same playlist, which
// carries on when the provider does. The provider is asked once.
// Guards segMinFirst/segWait (the first playlist waits only when it must) and liveQuiet on the shared stream.
func TestHLSFromTSRealisticJoinInPause(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(60, 1000, 27)
	const pauseFrom, pauseTo, end = 6.0, 23.0, 31.0
	p.add("7", &rpChannel{feed: feed, lumps: rpLumps(18, pauseFrom, pauseTo, 29), back: 8})
	base := rpFront(t, p, "ts", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()
	viewer := rpRead(ctx, p, base, "192.0.2.40", "/live/tv/easy/7.ts", 0, 0)
	p.until(ctx, 13)
	pl := rpPlay(ctx, t, p, base, "192.0.2.41", "/live/tv/easy/7.m3u8", 2)
	rpWaitAll(viewer)
	time.Sleep(200 * time.Millisecond)
	if n := p.asked("7"); n != 1 {
		t.Errorf("the provider was asked %d times, want once", n)
	}
	rpExact(t, "the MPEG-TS viewer", viewer, feed.data, 0, feed.upTo(29+18))
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.firstList < 0 || pl.firstList > 3 {
		t.Errorf("the HLS player got its first playlist %.1f s after asking, want within 3 s", pl.firstList)
	}
	rpSteady(t, "the HLS player", pl)
	after := 0
	for _, at := range pl.pieceAt {
		if at > pauseTo {
			after++
		}
	}
	if after < 2 {
		t.Errorf("the HLS player got %d pieces after the pause (pieces at %v)", after, pl.pieceAt)
	}
}

// A media server tunes a channel the way FFmpeg-based ones do: it opens it, reads a second of it to learn what
// is in it, hangs up, and asks again a moment later to play it. That is one tune: the provider is asked once,
// and the second ask is handed everything kept so far at once, from the start, as the first was.
// Guards linger (a channel in its first seconds is kept a moment after its last reader) and the re-ask with
// nobody else on the channel getting the full start.
func TestLiveTSRealisticProbeThenPlay(t *testing.T) {
	p := newTSRealProv(t, 1, 0)
	feed := rpBuild(40, 1000, 27)
	p.add("7", &rpChannel{feed: feed, lumps: rpLumps(14, 6), back: 8})
	base := rpFront(t, p, "ts", "")
	const dev, end = "192.0.2.25", 8.0
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(p.start.Add(secs(end))))
	defer cancel()
	pctx, probed := context.WithCancel(ctx)
	probe := rpRead(pctx, p, base, dev, "/live/tv/easy/7.ts", 0, 0)
	time.Sleep(1200 * time.Millisecond)
	probed()
	rpWaitAll(probe)
	time.Sleep(150 * time.Millisecond)
	play := rpRead(ctx, p, base, dev, "/live/tv/easy/7.ts", 0, 0)
	rpWaitAll(play)
	if n := p.asked("7"); n != 1 {
		t.Errorf("a probe and the play right after it had the provider asked %d times, want once", n)
	}
	rpExact(t, "the play after the probe", play, feed.data, 0, feed.upTo(6+14))
}

// On a recorded feed at its recorded read timing (skips without COAXIAL_REPLAY_TS): a device watches a channel as
// MPEG-TS and a second device asks for it as HLS built from the MPEG-TS. The builder starts from the joiner's
// cushion (joinSeconds of video) and cuts pieces only at keyframes the stream marks; the cushion must already
// hold segMinFirst pieces, so the first playlist is answered at once. Otherwise it waits for the provider's next
// lumps (up to segWait, 20 s), and a player gives up on an answer after 8 s.
// The HLS device comes at three moments, each on a channel of its own watched from the start: early, while the
// provider's opening burst still counts in the channel's measured rate (the cushion then holds more video than
// joinSeconds), and twice later, when the cushion is about joinSeconds of video and holds about two pieces.
func TestHLSFromTSRealisticJoinerFirstPlaylistReplay(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_TS")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_TS names no recorded feed")
	}
	fd := replayFeed(t, path)
	joins := map[string]float64{"7": 12, "8": 30, "9": 45} // channel: when the HLS device asks for it
	start := time.Now()
	asked := map[string]*atomic.Int32{}
	for id := range joins {
		asked[id] = &atomic.Int32{}
	}
	fp := newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/live/realU/realP/"), ".ts")
		if n := asked[id]; n != nil && r.URL.Path == "/live/realU/realP/"+id+".ts" {
			leadProvider(fd, start, 1e9, n)(w, r) // (every channel plays the same recording from the same moment)
			return true
		}
		return false
	})
	_, base := limFront(t, fp, "ts", "", advanced{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p := &tsRealProv{start: start, scale: 1}
	var viewers []*rpReader
	var wg sync.WaitGroup
	k := 0
	for id, joinAt := range joins {
		k++
		viewers = append(viewers, rpRead(ctx, p, base, fmt.Sprintf("192.0.2.%d", 61+2*k), "/live/tv/easy/"+id+".ts", 0, 0))
		wg.Add(1)
		go func(dev string) {
			defer wg.Done()
			p.until(ctx, joinAt)
			from := p.wall()
			st, list, _ := rpGet(t, base, dev, "/live/tv/easy/"+id+".m3u8")
			took := p.wall() - from
			pieces, _, _, _, _ := rpList(list)
			t.Logf("HLS player joining at %.0f s: first playlist %d with %d pieces after %.1f s", joinAt, st, len(pieces), took)
			if st != 200 || took > 1.5 { // (segmenterFor: the joiner's cushion makes the first pieces ready straight away)
				t.Errorf("HLS player joining a channel watched as MPEG-TS at %.0f s: first playlist answered %d after %.1f s, want at once", joinAt, st, took)
			}
		}(fmt.Sprintf("192.0.2.%d", 62+2*k))
	}
	wg.Wait()
	cancel()
	rpWaitAll(viewers...)
	for id, n := range asked {
		if n.Load() != 1 {
			t.Errorf("channel %s: the provider was asked %d times, want once", id, n.Load())
		}
	}
}
