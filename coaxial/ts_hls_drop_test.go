package main

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Live TV watched as HLS on a provider set to MPEG-TS (tshls.go), when the provider ends the channel's
// connection. ts_hls_live_test.go follows such a channel while the provider behaves; live_wreck_test.go drops
// the connection under a device that reads MPEG-TS. Nothing did both: a device on the HLS made here sees a
// drop only through its playlist, and what it must see is nothing at all.
//
// The provider is live_wreck_test's: a real channel sent as an opening burst and lumps; it ends the connection
// 33 s in, and a connection opened after that starts, as providers' do, 12 s of video back, in the middle of a
// frame. In one scene it serves the channel again at once, in one it answers 404 for 7 s first (it is
// restarting the channel: incident 19), in one it ends the connection between two lumps, on a whole packet.
// A player asks for the playlist four times a second (far more often than a real one, to see when each piece
// appears; this costs the provider nothing) and fetches every piece it names. What must hold:
//
//   1. the playlist is answered 200, at once, every time, the outage included, and never says the channel has
//      ended; its numbering runs on across the drop: it never goes back and never leaves a hole;
//   2. the pieces joined end to end are the provider's video and sound packet for packet, in order, nothing
//      twice, and nothing missing but what the reconnect itself loses: the packet the drop cut short, and the
//      video between it and the first keyframe the new connection sent past it (the stream asked for again is
//      joined on at a keyframe, see spliceAt);
//   3. nothing is held: a piece is in the playlist within tsDropLate of the provider sending the keyframe
//      that ends it, before the drop and after it. A player is never kept waiting by the proxy for anywhere
//      near its own 8 s;
//   4. the provider is asked for the channel exactly as often as for a device that reads MPEG-TS: once, and
//      once more after the drop (after 2, 3 and 5 s more while it answers 404).

// tsDropLate: how long after the provider sent the keyframe that ends a piece the piece may be missing from
// the playlist (real: about the 250 ms between two asks).
const tsDropLate = 3 * time.Second

// tsDropAnswer: how long a playlist may take to be answered once the channel plays.
const tsDropAnswer = 2 * time.Second

type tsDropScene struct {
	name string
	end  float64 // seconds watched
	set  func(p *wreckProv)
	asks int // times the provider is asked for the channel
	lost int // places where video is missing (a drop on a whole packet, with no new video yet, loses none)
}

func TestLiveHLSFromTSThroughAProviderDrop(t *testing.T) {
	scenes := []tsDropScene{
		{"served again at once", 52, func(p *wreckProv) { p.drops = []wreckDrop{{at: 33}} }, 2, 1},
		{"404 for 7 s, then served again", 58, func(p *wreckProv) { p.drops, p.refuse, p.refuseFor = []wreckDrop{{at: 33}}, 404, 7 }, 5, 1},
		{"dropped between two lumps, on a whole packet", 52, func(p *wreckProv) { p.drops = []wreckDrop{{at: 34.5, clean: true}} }, 2, 0},
	}
	for _, sc := range scenes {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			tsDropWatch(t, sc)
		})
	}
}

func tsDropWatch(t *testing.T, sc tsDropScene) {
	s := wreckShared()
	p := &wreckProv{fd: wreckLive(s, 12, 16, wreckLumps(sc.end-2)), back: 12}
	sc.set(p)
	base := wreckFront(t, p)
	p.start = time.Now()
	since := func(at time.Time) float64 { return at.Sub(p.start).Seconds() }
	const dev = "192.0.2.71"
	status, data, _, at := tsHLSGet(t, dev, base+"/live/tv/easy/7.m3u8")
	if status != 200 || !bytes.Contains(data, []byte("#EXTINF")) {
		t.Fatalf("the channel asked for as HLS: %d %q", status, data)
	}
	listAt, _ := url.Parse(at)
	pieces := map[int][]byte{}
	seen := map[int]time.Time{}
	firstSeq, lastSeq, lists, discSeq := -1, -1, 0, -1
	tuned := -1 // the last piece of the first playlist
	var slowest time.Duration
	slowestAt := 0.0
	for time.Since(p.start).Seconds() < sc.end {
		if lists > 0 {
			time.Sleep(250 * time.Millisecond)
			began := time.Now()
			status, data, _, _ = tsHLSGet(t, dev, at)
			if took := time.Since(began); took > slowest {
				slowest, slowestAt = took, since(began)
			}
			if status != 200 {
				t.Fatalf("playlist %d, %.1f s in: the player was answered %d %q (the provider's drops so far, in seconds: %.1f)", lists+1, since(began), status, data, tsDropDrops(p))
			}
		}
		lists++
		now := time.Now()
		text := string(data)
		seq, names := tsHLSList(text)
		if seq < 0 || len(names) == 0 {
			t.Fatalf("playlist %d, %.1f s in, names no pieces or has no MEDIA-SEQUENCE:\n%s", lists, since(now), text)
		}
		if strings.Contains(text, "#EXT-X-ENDLIST") {
			t.Fatalf("playlist %d, %.1f s in, says the channel has ended (#EXT-X-ENDLIST); the provider dropped its connection (at %.1f s) and serves the channel again: a player stops for good at that line",
				lists, since(now), tsDropDrops(p))
		}
		if firstSeq < 0 {
			firstSeq = seq
		}
		if lastSeq >= 0 && (seq+len(names)-1 < lastSeq || seq > lastSeq+1) {
			t.Fatalf("playlist %d, %.1f s in, names pieces %d to %d; the one before it ended at piece %d: a player is left with a hole, or sent back (the provider's drops, in seconds: %.1f)",
				lists, since(now), seq, seq+len(names)-1, lastSeq, tsDropDrops(p))
		}
		lastSeq = seq + len(names) - 1
		if tuned < 0 {
			tuned = lastSeq
		}
		for _, line := range strings.Split(text, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "#EXT-X-DISCONTINUITY-SEQUENCE:"); ok {
				n, _ := strconv.Atoi(v)
				if n < discSeq {
					t.Fatalf("playlist %d: EXT-X-DISCONTINUITY-SEQUENCE went back from %d to %d", lists, discSeq, n)
				}
				discSeq = n
			}
		}
		for i, name := range names {
			n := seq + i
			if _, have := pieces[n]; have {
				continue
			}
			u, err := listAt.Parse(name)
			if err != nil {
				t.Fatalf("playlist %d names a piece at an address the player cannot read: %q", lists, name)
			}
			st, piece, kind, _ := tsHLSGet(t, dev, u.String())
			if st != 200 || !strings.HasPrefix(kind, "video/") || len(piece) == 0 || len(piece)%188 != 0 {
				t.Fatalf("piece %d, named in playlist %d (%.1f s in): status %d, type %q, %d bytes (want 200, video, whole packets)", n, lists, since(now), st, kind, len(piece))
			}
			pieces[n], seen[n] = piece, now
		}
	}
	p.mu.Lock()
	asks, sends, dropped := append([]float64(nil), p.asks...), append([]wreckSend(nil), p.sends...), append([]float64(nil), p.dropped...)
	p.mu.Unlock()
	if len(dropped) != 1 {
		t.Fatalf("the stand-in provider dropped the connection %d times, the scene wants one drop", len(dropped))
	}
	if len(asks) != sc.asks {
		t.Errorf("the provider was asked for the channel %d times (at %.1f s), want %d: the HLS made here costs the provider what a device reading MPEG-TS costs it", len(asks), asks, sc.asks)
	}
	if slowest > tsDropAnswer {
		t.Errorf("a playlist took %.1f s to be answered, %.1f s in (the drop came at %.1f s); once the channel plays it is answered from what is in hand, at most %.1f s is allowed",
			slowest.Seconds(), slowestAt, dropped[0], tsDropAnswer.Seconds())
	}
	// The provider's video and sound packets, and where each one is in its stream.
	var src []byte
	var raw []int
	for i := 0; i+188 <= len(s.data); i += 188 {
		if pid := int(s.data[i+1]&0x1f)<<8 | int(s.data[i+2]); pid != 0 && pid != 0x1000 {
			src, raw = append(src, s.data[i:i+188]...), append(raw, i)
		}
	}
	srcN := len(raw)
	pkt := func(k int) []byte { return src[k*188 : (k+1)*188] }
	// What the first connection had sent when it ended, and what the connection that carried on began with.
	oldEnd, newConn, newFrom := 0, 0, 0
	for _, sd := range sends {
		if sd.conn == 1 {
			oldEnd = max(oldEnd, sd.to)
		} else if newConn == 0 {
			newConn, newFrom = sd.conn, sd.from
		}
	}
	if newConn == 0 {
		t.Fatalf("the provider never sent video again after the drop at %.1f s (asked at %.1f s)", dropped[0], asks)
	}
	// The pieces, in order, followed through the provider's packets.
	type hole struct{ piece, from, to int }
	var holes []hole
	starts := map[int]int{} // where each piece starts among the provider's packets
	pos, fills, total := -1, 0, 0
	for n := firstSeq; n <= lastSeq; n++ {
		piece, ok := pieces[n]
		if !ok {
			t.Fatalf("piece %d was never named in a playlist the player was given (pieces %d to %d were)", n, firstSeq, lastSeq)
		}
		pid := func(i int) int { return int(piece[i*188+1]&0x1f)<<8 | int(piece[i*188+2]) }
		if len(piece) < 3*188 || pid(0) != 0 || pid(1) != 0x1000 {
			t.Fatalf("piece %d does not start with the program tables (PAT, then PMT): a player cannot begin at it", n)
		}
		if first := piece[2*188:]; pid(2) != 0x100 || first[3]&0x20 == 0 || first[5]&0x40 == 0 {
			t.Errorf("piece %d does not start at a keyframe (its first video packet is on PID %#x and carries no keyframe mark)", n, pid(2))
		}
		av := tsHLSAV(piece)
		total += len(av)
		for i := 0; i+188 <= len(av); i += 188 {
			q := av[i : i+188]
			if pos < 0 {
				at := bytes.Index(src, q)
				if at < 0 || at%188 != 0 {
					t.Fatalf("the first piece does not begin with a packet the provider sent")
				}
				if pos = at / 188; pos != 0 {
					t.Errorf("the first piece begins %d packets into the provider's video and sound; the stream began at a keyframe, so nothing before it should be missing", pos)
				}
			}
			if pos < srcN && bytes.Equal(pkt(pos), q) {
				if i == 0 {
					starts[n] = pos
				}
				pos++
				continue
			}
			// The packet the drop cut short: what came of it, filled out to a whole packet (see tsSource).
			if pos < srcN && raw[pos] < oldEnd && raw[pos]+188 > oldEnd {
				k := oldEnd - raw[pos]
				if bytes.Equal(q[:k], pkt(pos)[:k]) && bytes.Count(q[k:], []byte{0xff}) == 188-k {
					fills++
					pos++
					continue
				}
			}
			next := -1
			for j := pos + 1; j < srcN; j++ {
				if bytes.Equal(pkt(j), q) {
					next = j
					break
				}
			}
			if next < 0 {
				for j := 0; j < pos && j < srcN; j++ {
					if bytes.Equal(pkt(j), q) {
						t.Fatalf("video was sent twice: in piece %d, after the provider's packet %d (%.1f s into the channel) came its packet %d (%.1f s) again",
							n, pos-1, float64(s.frameOf(raw[pos-1]))/wreckFPS, j, float64(s.frameOf(raw[j]))/wreckFPS)
					}
				}
				t.Fatalf("piece %d holds a packet the provider never sent, %d packets into the piece: % x ...", n, i/188, q[:16])
			}
			holes = append(holes, hole{n, pos, next})
			if i == 0 {
				starts[n] = next
			}
			pos = next + 1
		}
	}
	// What is missing: only what the reconnect lost.
	if len(holes) != sc.lost {
		t.Errorf("video is missing in %d places (%v, as piece, first packet missing, next packet there); this drop loses video in %d", len(holes), holes, sc.lost)
	}
	if fills > 1 {
		t.Errorf("%d packets were filled out; one drop cuts one packet short", fills)
	}
	key := s.nextKey(s.frameOf(max(newFrom, oldEnd)-1) + 1) // the first keyframe the new connection sent past what was sent before
	for key < len(s.frames) && s.frames[key] < newFrom {
		key = s.nextKey(key + 1)
	}
	for _, h := range holes {
		lostFrom, lostTo := float64(s.frameOf(raw[h.from]))/wreckFPS, float64(s.frameOf(raw[h.to]))/wreckFPS
		t.Logf("the drop at %.1f s: the first connection had sent the channel up to %.2f s; the next one began %.2f s into it; piece %d carries on from %.2f s to %.2f s (%.2f s of video lost with the reconnect)",
			dropped[0], float64(s.frameOf(oldEnd-1))/wreckFPS, float64(s.frameOf(newFrom))/wreckFPS, h.piece, lostFrom, lostTo, lostTo-lostFrom)
		if raw[h.from]+188 <= oldEnd {
			t.Errorf("piece %d leaves out video the provider had sent before the drop: its packet %d (%.2f s into the channel) was sent whole on the first connection and is missing",
				h.piece, h.from, lostFrom)
		}
		if key < len(s.frames) && raw[h.to] != s.stamped(key) {
			t.Errorf("piece %d carries on at %.2f s into the channel; the first keyframe the new connection sent past the drop is at %.2f s: video the provider did send is missing, or the stream was not joined on at a keyframe",
				h.piece, lostTo, float64(key)/wreckFPS)
		}
	}
	t.Logf("%d playlists in %.0f s; pieces %d to %d (%d), %.1f MB; the provider was asked at %.1f s; the slowest playlist answer took %d ms",
		lists, sc.end, firstSeq, lastSeq, len(pieces), float64(total)/1e6, asks, slowest.Milliseconds())
	// Held? A piece is whole once the provider has sent the first video packet of the next one.
	sentAt := func(k int) time.Time { // when the provider first sent its packet k whole
		var at time.Time
		for _, sd := range sends {
			if sd.from <= raw[k] && raw[k]+188 <= sd.to && (at.IsZero() || sd.at.Before(at)) {
				at = sd.at
			}
		}
		return at
	}
	// (The pieces of the very first playlist are not judged here: that playlist waits for the provider to have
	// sent two pieces, and ts_hls_live_test.go holds the tune to its own limit.)
	worst, worstOf, after := time.Duration(0), firstSeq, 0
	for n := firstSeq; n <= lastSeq; n++ {
		if since(seen[n]) > dropped[0] {
			after++
		}
		next, ok := starts[n+1]
		if !ok || n <= tuned {
			continue
		}
		ended := sentAt(next)
		if ended.IsZero() {
			continue
		}
		if late := seen[n].Sub(ended); late > worst {
			worst, worstOf = late, n
		}
	}
	t.Logf("a piece was in the playlist at most %d ms after the provider sent the keyframe that ends it (piece %d, which the provider ended %.1f s in); %d pieces came into the playlist after the drop",
		worst.Milliseconds(), worstOf, since(sentAt(starts[worstOf+1])), after)
	if worst > tsDropLate {
		t.Errorf("video was held: piece %d was in the playlist %d ms after the provider had sent the keyframe that ends it (%.1f s in; the drop came at %.1f s); at most %d ms is allowed",
			worstOf, worst.Milliseconds(), since(sentAt(starts[worstOf+1])), dropped[0], tsDropLate.Milliseconds())
	}
	if after < 2 {
		t.Errorf("only %d pieces came into the playlist after the drop at %.1f s, in %.0f s of watching: the playlist did not carry on", after, dropped[0], sc.end)
	}
}

func tsDropDrops(p *wreckProv) []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]float64(nil), p.dropped...)
}
