package main

// HLS built from the shared MPEG-TS stream, for a provider whose live_tv setting is "ts".
//
// Most providers allow one session per channel, and an HLS request and an MPEG-TS request for the same
// channel count as two: each ends the other. So on a ts provider the provider only ever sees MPEG-TS. A
// player that asks for a live channel as HLS joins the channel's one shared MPEG-TS stream like any other
// reader, and the relay cuts that stream into short pieces at keyframes (timed by the video's own clock)
// and serves a normal live playlist from memory. Everyone on the channel stays on one provider connection.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	segTarget   = 4.0      // seconds of video per piece (cut at the first keyframe after this)
	segKeep     = 8        // pieces kept (about half a minute)
	segIdle     = 30.0     // seconds without a playlist request before the HLS reader leaves the channel
	segMinFirst = 2        // pieces a playlist waits for first (a provider's opening burst holds them)
	segWait     = 20       // seconds a playlist request waits for them
	segBytes    = 40 << 20 // pieces kept, at most, in bytes (a 4K channel's pieces are much larger)
)

// a live channel address that can be built from MPEG-TS: /live/U/P/<n>.m3u8 or /U/P/<n>.m3u8
var liveM3U8 = regexp.MustCompile(`^/+(live/+)?[^/]+/+[^/]+/+[0-9]+\.m3u8$`)

type segment struct {
	seq   int
	dur   float64
	disc  bool
	data  []byte
	ready time.Time
}

type segmenter struct {
	key, provider, who, port, id string
	ch                           *channel
	rd                           *reader
	segs                         []*segment
	next                         int
	asked                        float64 // last playlist or piece request
	grew                         chan struct{}
	over, started                bool
	discSeq                      int
	longest                      float64            // the longest piece so far: TARGETDURATION never goes down
	players                      map[string]float64 // device -> its last request
	waiting                      map[string]int     // device -> its playlist requests still waiting for pieces
}

var segmenters = map[string]*segmenter{} // TS channel key -> its HLS builder

// hlsFromTS answers a playlist or piece request for a live channel of a ts provider. ok=false: not a live
// channel address (catch-up and the like), handled the usual way.
func hlsFromTS(w http.ResponseWriter, r *http.Request, who, provider, uri, opath, q, port string, f *front) (bool, error) {
	chURI := strings.SplitN(uri, "?", 2)[0]
	if !liveM3U8.MatchString(chURI) {
		return false, nil
	}
	m := chRe.FindStringSubmatch(opath)
	id := channelID(chURI)
	if m == nil { // the player opened the channel: give it the channel's folder (nothing asked of the provider yet)
		mu.Lock()
		t, said := hlsWho[who+" ts "+provider+" "+id]
		hlsWho[who+" ts "+provider+" "+id] = mono()
		mu.Unlock()
		if !said || mono()-t > 60 { // (once a minute: some players ask the channel's own address at each refresh)
			say("%s opened channel %s as HLS (%s); it is built from the shared MPEG-TS stream", who, id, hostOf(provider))
		}
		w.Header().Set("Location", "/__ch"+opath+"/__index.m3u8"+q)
		answer(w, 302, "", "")
		return true, nil
	}
	tsURI := strings.TrimSuffix(chURI, ".m3u8") + ".ts"
	key := channelKey("/ts", provider, tsURI)
	var sg *segmenter
	if m[2] != "__index.m3u8" { // a piece comes only from a running builder: asking for one never opens the channel
		mu.Lock()
		sg = segmenters[key]
		mu.Unlock()
		if sg == nil {
			answer(w, 404, "", "")
			return true, nil
		}
	} else if s, err := segmenterFor(r.Context(), f, key, tsURI, who, id, r.Header); err != nil {
		return true, err
	} else {
		sg = s
	}
	mu.Lock()
	sg.asked = mono()
	sg.players[who] = mono()
	if rk := who + " " + provider + " " + id; who != "127.0.0.1" && who != "::1" {
		if row := hlsRows[rk]; row != nil && mono()-row.last <= 30 {
			row.last = mono()
		} else {
			hlsRows[rk] = &hlsRow{mono(), mono(), who, id, provider, port}
		}
	}
	mu.Unlock()
	if name := m[2]; name != "__index.m3u8" { // a piece
		n, perr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg_"), ".ts"))
		mu.Lock()
		var data []byte
		for _, s := range sg.segs {
			if perr == nil && s.seq == n {
				data = s.data
			}
		}
		mu.Unlock()
		if data == nil {
			answer(w, 404, "", "")
			return true, nil
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		w.Write(data)
		return true, nil
	}
	deadline := time.After(segWait * time.Second) // the playlist: wait for the first pieces
	mu.Lock()
	sg.waiting[who]++ // (a device waiting here is not quiet: releaseFor leaves its builder alone)
	mu.Unlock()
	defer func() {
		mu.Lock()
		if sg.waiting[who]--; sg.waiting[who] <= 0 {
			delete(sg.waiting, who)
		}
		sg.players[who] = mono()
		mu.Unlock()
	}()
wait:
	for {
		mu.Lock()
		n, over, grew := len(sg.segs), sg.over, sg.grew
		mu.Unlock()
		switch {
		case n >= segMinFirst || (over && n > 0):
			break wait
		case over: // the provider's own refusal is passed on, as for MPEG-TS
			mu.Lock()
			e := sg.ch.refusal
			mu.Unlock()
			if e != nil && e.status >= 400 && e.status < 600 {
				answer(w, e.status, e.kind, string(f.hideLogin(e.text)))
			} else {
				answer(w, 502, "", "")
			}
			return true, nil
		}
		select {
		case <-grew:
		case <-deadline:
			if n == 0 {
				answer(w, 504, "", "")
				return true, nil
			}
			break wait
		case <-r.Context().Done():
			return true, nil
		}
	}
	mu.Lock()
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n", int(sg.target()))
	if len(sg.segs) > 0 {
		fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", sg.segs[0].seq, sg.discSeq)
	}
	for _, s := range sg.segs {
		if s.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg_%d.ts%s\n", s.dur, s.seq, q)
	}
	if sg.over {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	answer(w, 200, "application/vnd.apple.mpegurl", b.String())
	return true, nil
}

// segmenterFor finds the channel's HLS builder, or joins the channel's shared MPEG-TS stream (opening it at
// the provider if nobody is on it, by the rules the provider has on: see advanced.go) and starts one.
func segmenterFor(ctx context.Context, f *front, key, tsURI, who, id string, rh http.Header) (*segmenter, error) {
	provider, port := f.base, f.port
	mu.Lock()
	if sg := segmenters[key]; sg != nil && !sg.over {
		if sg.players[who] == 0 { // a device new to this builder has left any builder of its own
			releaseFor(provider, who, key)
		}
		mu.Unlock()
		return sg, nil
	}
	ch := channels[key]
	first := ch == nil
	if !first { // moving onto a channel already open: an HLS builder this device left goes too
		releaseFor(provider, who, key)
	}
	if first {
		was := lastOpen[provider]
		lastOpen[provider] = opened{mono(), key}
		releaseFor(provider, who, key)
		for _, c := range channels { // channels of this provider that nobody reads go first (changing channel)
			if c.provider == provider {
				c.dropIfEmpty()
			}
		}
		mu.Unlock()
		want := what{"ch", id}
		h, why := f.admit(ctx, who, want, false, key, nil)
		if h == nil {
			unOpen(provider, key, was)
			if why == "" {
				return nil, &refused{status: 504} // (the player hung up while it waited)
			}
			return nil, f.refusal(who, want, why)
		}
		defer h.release()
		mu.Lock()
		wait := lastDrop[provider] + dropGap - mono()
		mu.Unlock()
		time.Sleep(secs(math.Max(0, wait)))
		mu.Lock()
		// (from here on it is counted as the channel, or it joined one)
		delete(holds, h)
		if sg := segmenters[key]; sg != nil && !sg.over { // another request got here first
			mu.Unlock()
			return sg, nil
		}
		if ch = channels[key]; ch == nil {
			sent := http.Header{}
			for k, vs := range rh {
				if !skipRe.MatchString(k) {
					for _, v := range vs {
						sent.Add(k, notPrintable.ReplaceAllString(v, ""))
					}
				}
			}
			ch = newChannel(key, tsSource(provider+tsURI, f.agent(sent)), provider)
			ch.owner = f
			channels[key] = ch
		} else {
			first = false
		}
	}
	rd := ch.join(first)
	rd.pace = nil // the builder takes the joiner's cushion at once: the first pieces are ready straight away
	rd.who, rd.id, rd.port, rd.came, rd.kind = who, id, port, mono(), "HLS built from MPEG-TS"
	// Piece numbers keep growing from one builder to the next (a new one starting again at 0 left players
	// frozen after a pause), and a new builder's first piece is marked as not following on from the last.
	now := int(time.Now().Unix())
	sg := &segmenter{key: key, provider: provider, who: who, port: port, id: id, ch: ch, rd: rd, asked: mono(),
		grew: make(chan struct{}), next: now, discSeq: now, players: map[string]float64{who: mono()},
		waiting: map[string]int{}}
	segmenters[key] = sg
	mu.Unlock()
	if first {
		say("%s started channel %s (%s) for HLS from MPEG-TS", who, id, hostOf(provider))
	} else {
		say("%s joined channel %s (%s) for HLS from MPEG-TS", who, id, hostOf(provider))
	}
	go sg.run()
	return sg, nil
}

// target: the playlist's TARGETDURATION, in whole seconds. (mu held)
func (sg *segmenter) target() float64 { return math.Ceil(math.Max(segTarget, sg.longest)) }

// patPMT: the PMT PID of a PAT section's first program (program 0 is the network table, not a program).
func patPMT(p []byte) int {
	if len(p) < 12 {
		return -1
	}
	end := min(len(p)-4, 3+(int(p[1]&0x0f)<<8|int(p[2]))-4) // (minus the CRC)
	for i := 8; i+4 <= end; i += 4 {
		if p[i] != 0 || p[i+1] != 0 {
			return int(p[i+2]&0x1f)<<8 | int(p[i+3])
		}
	}
	return -1
}

// run reads the channel's MPEG-TS as it arrives and cuts it into pieces.
func (sg *segmenter) run() {
	var cut tsCutter
	start := mono()
	defer func() {
		mu.Lock()
		sg.over = true
		close(sg.grew)
		sg.grew = make(chan struct{})
		sg.ch.leave(sg.rd)
		if segmenters[sg.key] == sg {
			delete(segmenters, sg.key)
		}
		written := sg.rd.written.Load()
		mu.Unlock()
		say("%s left channel %s (HLS from MPEG-TS) after %s, %.1f MB cut into pieces", sg.who, sg.id, hms(mono()-start), float64(written)/1048576)
	}()
	for {
		mu.Lock()
		if sg.rd.dropped || (mono()-sg.asked > segIdle) {
			mu.Unlock()
			return
		}
		if len(sg.rd.q) == 0 {
			if sg.rd.over {
				mu.Unlock()
				if seg := cut.flush(); seg != nil {
					sg.add(seg)
				}
				return
			}
			mu.Unlock()
			select {
			case <-sg.rd.wake:
			case <-time.After(5 * time.Second): // (to notice a player that stopped asking)
			}
			continue
		}
		data := sg.rd.q[0]
		sg.rd.q[0], sg.rd.q = nil, sg.rd.q[1:]
		sg.rd.n -= len(data)
		sg.rd.given = max(0, sg.rd.given-len(data))
		mu.Unlock()
		sg.rd.written.Add(int64(len(data)))
		for _, seg := range cut.feed(data) {
			sg.add(seg)
		}
	}
}

func (sg *segmenter) add(s *segment) {
	mu.Lock()
	defer mu.Unlock()
	s.seq = sg.next
	sg.next++
	sg.longest = math.Max(sg.longest, s.dur)
	if !sg.started {
		s.disc, sg.started = true, true
	}
	sg.segs = append(sg.segs, s)
	total := 0
	for _, x := range sg.segs {
		total += len(x.data)
	}
	for len(sg.segs) > segKeep || (len(sg.segs) > 4 && total > segBytes) { // (a playlist keeps 3 target durations and the piece being fetched)
		total -= len(sg.segs[0].data)
		if sg.segs[0].disc {
			sg.discSeq++
		}
		sg.segs[0] = nil
		sg.segs = sg.segs[1:]
	}
	close(sg.grew)
	sg.grew = make(chan struct{})
}

// ---------------------------------------------------------------------------
// Cutting MPEG-TS into pieces
// ---------------------------------------------------------------------------
// Each piece starts with the stream's program tables (PAT and PMT) and, when the stream marks them, at a
// keyframe of the video, so a player can start playing any piece. Durations come from the video's own
// timestamps (PTS). A jump in the timestamps (the provider's stream was asked for again) starts a new piece
// marked as a discontinuity.
type tsCutter struct {
	buf           []byte
	pmtPID, avPID int
	pat, pmt      []byte
	cur           []byte  // the piece being built
	curStart      float64 // PTS (seconds) of its first frame
	lastPTS       float64 // PTS of the newest frame start; -1 = none yet
	firstPTS      float64 // the first frame seen; -1 = none yet
	sawKey        bool    // the stream marks keyframes
	nextDis       bool
}

func (c *tsCutter) feed(data []byte) []*segment {
	if c.avPID == 0 { // (first call)
		c.pmtPID, c.avPID, c.lastPTS, c.firstPTS = -1, -1, -1, -1
	}
	c.buf = append(c.buf, data...)
	var out []*segment
	for len(c.buf) >= 188 {
		if c.buf[0] != 0x47 { // lost the packet boundary: find it again
			i := 1
			for ; i+376 < len(c.buf); i++ {
				if c.buf[i] == 0x47 && c.buf[i+188] == 0x47 && c.buf[i+376] == 0x47 {
					break
				}
			}
			if i+376 >= len(c.buf) {
				c.buf = append([]byte(nil), c.buf[len(c.buf)-min(len(c.buf), 376):]...)
				return out
			}
			c.buf = c.buf[i:]
			continue
		}
		if seg := c.packet(c.buf[:188:188]); seg != nil {
			out = append(out, seg)
		}
		c.buf = c.buf[188:]
	}
	c.buf = append([]byte(nil), c.buf...) // (keep only the partial packet)
	return out
}

func (c *tsCutter) packet(pkt []byte) *segment {
	pid := int(pkt[1]&0x1f)<<8 | int(pkt[2])
	pusi := pkt[1]&0x40 != 0
	afc := (pkt[3] >> 4) & 3
	payload, rai := 4, false
	if afc&2 != 0 {
		alen := int(pkt[4])
		rai = alen > 0 && pkt[5]&0x40 != 0
		payload = 5 + alen
	}
	if afc&1 == 0 || payload >= 188 {
		payload = 188
	}
	body := pkt[payload:]
	switch {
	case pid == 0 && pusi:
		c.pat = append([]byte(nil), pkt...)
		if pmt := patPMT(tableStart(body)); pmt >= 0 {
			c.pmtPID = pmt // the first program
		}
		return nil // (every piece starts with its own copy)
	case pid == c.pmtPID && pusi:
		c.pmt = append([]byte(nil), pkt...)
		if s := pickStream(tableStart(body)); s >= 0 { // (read at every PMT: a stream asked for again may use new PIDs)
			c.avPID = s
		}
		return nil
	case pid == 0 || pid == c.pmtPID:
		return nil
	}
	var seg *segment
	if pid == c.avPID && pusi && c.pat != nil && c.pmt != nil {
		if pts, ok := pesPTS(body); ok {
			c.sawKey = c.sawKey || rai
			if c.firstPTS < 0 {
				// (no piece yet: the wait for a frame it may start at is counted from here, not from 0, so
				// the first piece starts at a keyframe too)
				c.firstPTS, c.curStart = pts, pts
			}
			jump := c.lastPTS >= 0 && (pts < c.lastPTS-1 || pts > c.lastPTS+10)
			if jump {
				c.firstPTS = pts // (counted again from here, for a stream that marks no keyframes)
			}
			// a frame a piece may start at: a keyframe, or any frame if the stream marks none (after a while)
			// (a piece is also cut when keyframe marks stop coming, so it cannot grow without end)
			startable := rai || (!c.sawKey && pts-c.firstPTS >= 2*segTarget) || jump || pts-c.curStart >= 5*segTarget
			if c.cur != nil && startable && (pts-c.curStart >= segTarget || jump) {
				dur := pts - c.curStart
				if jump {
					dur = c.lastPTS - c.curStart + 0.04
				}
				seg = &segment{dur: math.Max(dur, 0.1), disc: c.nextDis, data: c.cur}
				c.cur, c.nextDis = nil, jump
			}
			if c.cur == nil && startable {
				c.cur = append(append(make([]byte, 0, 1<<20), c.pat...), c.pmt...)
				c.curStart = pts
			}
			c.lastPTS = pts
		}
	}
	if c.cur != nil {
		c.cur = append(c.cur, pkt...)
		// No frame to cut at for this long (the video stopped while the stream went on, with only padding
		// or audio): the piece is let go rather than kept growing until memory runs out. The next piece
		// starts at the next frame it may start at, marked as not following on.
		if len(c.cur) > segBytes {
			c.cur, c.nextDis = nil, true
		}
	}
	return seg
}

// flush: what is left when the stream ends.
func (c *tsCutter) flush() *segment {
	if c.cur == nil || c.lastPTS <= c.curStart {
		return nil
	}
	s := &segment{dur: c.lastPTS - c.curStart, disc: c.nextDis, data: c.cur}
	c.cur = nil
	return s
}

// tableStart: the section after a PSI pointer field.
func tableStart(body []byte) []byte {
	if len(body) < 1 || int(body[0])+1 > len(body) {
		return nil
	}
	return body[1+int(body[0]):]
}

// pickStream: the video stream of a PMT section, or else its first audio stream.
func pickStream(s []byte) int {
	pid, _ := pickType(s)
	return pid
}

// pickType: pickStream, and its stream type.
func pickType(s []byte) (int, byte) {
	if len(s) < 12 {
		return -1, 0
	}
	end := min(len(s)-4, 3+(int(s[1]&0x0f)<<8|int(s[2]))-4) // (minus the CRC)
	i := 12 + (int(s[10]&0x0f)<<8 | int(s[11]))
	audio, audioType := -1, byte(0)
	for i+5 <= end {
		typ, pid, info := s[i], int(s[i+1]&0x1f)<<8|int(s[i+2]), int(s[i+3]&0x0f)<<8|int(s[i+4])
		switch typ {
		case 0x01, 0x02, 0x10, 0x1b, 0x24, 0x42, 0xea:
			return pid, typ
		case 0x03, 0x04, 0x0f, 0x11, 0x81, 0x87:
			if audio < 0 {
				audio, audioType = pid, typ
			}
		}
		i += 5 + info
	}
	return audio, audioType
}

// pesPTS: the presentation time, in seconds, of a PES packet that starts in this payload.
func pesPTS(b []byte) (float64, bool) {
	if len(b) < 14 || b[0] != 0 || b[1] != 0 || b[2] != 1 || b[7]&0x80 == 0 {
		return 0, false
	}
	p := int64(b[9]&0x0e)<<29 | int64(b[10])<<22 | int64(b[11]&0xfe)<<14 | int64(b[12])<<7 | int64(b[13])>>1
	return float64(p) / 90000, true
}

// releaseFor: a device that opens another channel of a provider has left its old one. A builder only that
// device was using, or one every device has stopped asking for, lets go now, so its provider connection is
// free at once (one-connection accounts). (mu held)
func releaseFor(provider, who, key string) {
	for k, sg := range segmenters {
		if sg.provider != provider || k == key {
			continue
		}
		alone := true
		for p, t := range sg.players { // (a player asks about once per target duration)
			alone = alone && (p == who || (mono()-t > 3*sg.target() && sg.waiting[p] == 0))
		}
		if alone {
			delete(segmenters, k)
			sg.asked = 0       // (its loop sees this and ends)
			sg.ch.leave(sg.rd) // let go now: the provider gets dropGap to see it before the next channel
			sg.rd.signal()
		}
	}
}
