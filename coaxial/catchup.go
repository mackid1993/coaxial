// Catch-up: a channel's archive passed on as one seekable MPEG-TS file, with its clock made to run straight through.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A catch-up answer is the channel's archive as one file the player seeks in by byte (Range). Where the archive
// moves on to its next stored file (a new episode of a 24/7 channel), the file's clock starts over part way. A
// player works its scrub bar out from the clock at the start and the end of the file and seeks by it, so with the
// clock starting over the bar is wrong, a seek lands in the wrong episode, and the picture can loop. So before a
// catch-up answer is passed on, where its clock starts over is found with a few small reads of the same file, and
// every timestamp (PCR, PTS, DTS) after each such place is moved to carry on from the one before. It goes by byte
// position, so every read of the file, from any point, gets the same timestamps. Nothing is added or left out:
// the length and every other byte are the provider's own. A file whose clock never starts over is not touched.

type cuJump struct {
	pos int64 // from this byte of the file on
	off int64 // added to every timestamp, modulo 2^33
}

type cuMap struct {
	at    float64 // when it was decided (kept cuKeep seconds; never changed meanwhile)
	size  int64
	align int64 // a packet starts at every byte position that is align modulo 188
	jumps []cuJump
}

var (
	cuMu    sync.Mutex
	cuMaps  = map[string]*cuMap{} // the catch-up file's address -> its map (an hour)
	cuBuild = map[string]chan struct{}{}
)

const (
	cuProbeSize = 64 << 10
	cuKeep      = 6 * 3600 // seconds a file's map is kept: a viewing session never sees it change
	cuBuildWait = 10       // seconds the reads may take; past that the file is passed on as it is (and stays so)
)

// cuProbe reads n bytes of the file at pos with a Range request like the player's own. It returns the bytes and
// the file's whole length.
func cuProbe(ctx context.Context, out *http.Request, pos, n int64) ([]byte, int64, error) {
	pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req := out.Clone(pctx)
	req.Header = out.Header.Clone()
	req.Header.Del("If-Range")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, pos+n-1))
	resp, err := sharedTransport.RoundTrip(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		return nil, 0, fmt.Errorf("catch-up read: status %d", resp.StatusCode)
	}
	total := int64(-1)
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			total, _ = strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64)
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, n))
	return data, total, err
}

// tsSync: where packets start in b (-1: none found).
func tsSync(b []byte) int {
	for i := 0; i < 188 && i+376 < len(b); i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 && b[i+376] == 0x47 {
			return i
		}
	}
	return -1
}

// pcrOf: the PCR in packet p (-1: none), on pid (any pid if pid < 0), and the packet's pid.
func pcrOf(p []byte, pid int) (int64, int) {
	id := int(p[1]&0x1f)<<8 | int(p[2])
	if p[3]&0x20 == 0 || p[4] < 7 || p[5]&0x10 == 0 || (pid >= 0 && id != pid) {
		return -1, id
	}
	return int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7, id
}

// firstPCR: the first PCR on pid in b, which starts at byte base of the file.
func firstPCR(b []byte, base int64, pid int) (int64, int64, int, bool) {
	s := tsSync(b)
	for i := s; s >= 0 && i+188 <= len(b); i += 188 {
		if b[i] != 0x47 {
			return 0, 0, 0, false
		}
		if v, id := pcrOf(b[i:i+188], pid); v >= 0 {
			return base + int64(i), v, id, true
		}
	}
	return 0, 0, 0, false
}

type cuPoint struct {
	pos int64 // the packet carrying the PCR
	pcr int64
}

// cuMapFor finds where the file's clock starts over. nil: it never does, or it could not be read; either way the
// answer is decided once per file and every read of it gets the same (passed on as is, or moved by one map).
// The reads go one at a time over one kept connection, after the player's own answer has started.
func cuMapFor(ctx context.Context, out *http.Request, total int64) *cuMap {
	key := out.URL.Scheme + "://" + out.URL.Host + out.URL.EscapedPath() + "#" + strconv.FormatInt(total, 10)
	cuMu.Lock()
	for k, m := range cuMaps {
		if mono()-m.at > cuKeep {
			delete(cuMaps, k)
		}
	}
	if m, ok := cuMaps[key]; ok {
		cuMu.Unlock()
		if len(m.jumps) == 0 {
			return nil
		}
		return m
	}
	if ch := cuBuild[key]; ch != nil { // another read of the same file is finding it already
		cuMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil
		}
		cuMu.Lock()
		m := cuMaps[key]
		cuMu.Unlock()
		if m == nil || len(m.jumps) == 0 {
			return nil
		}
		return m
	}
	ch := make(chan struct{})
	cuBuild[key] = ch
	cuMu.Unlock()
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cuBuildWait*time.Second) // (not the player's: others wait on it too)
	t0 := mono()
	m := cuFind(bctx, out, total)
	cancel()
	if m == nil { // could not be read: passed on as it is, and stays so (never one read fixed and the next not)
		m = &cuMap{at: mono()}
		lg.Warn().Float64("seconds", round1(mono()-t0)).Msg("a catch-up file could not be checked for clock restarts; passed on as it is")
	} else {
		lg.Info().Float64("seconds", round1(mono()-t0)).Int("restarts", len(m.jumps)).Msg("a catch-up file was checked for clock restarts")
	}
	cuMu.Lock()
	cuMaps[key] = m
	delete(cuBuild, key)
	close(ch)
	cuMu.Unlock()
	if m == nil || len(m.jumps) == 0 {
		return nil
	}
	return m
}

func cuFind(ctx context.Context, out *http.Request, total int64) *cuMap {
	head, size, err := cuProbe(ctx, out, 0, cuProbeSize)
	if err != nil || size <= 0 || size != total {
		return nil
	}
	none := &cuMap{at: mono()}
	s := tsSync(head)
	if s < 0 {
		return none // not MPEG-TS
	}
	_, _, pid, ok := firstPCR(head, 0, -1)
	if !ok {
		return none
	}
	at := func(pos int64) (cuPoint, bool) { // the first PCR from pos on (read on until one comes, up to 2 MB)
		for k := int64(0); k < 8; k++ {
			from := pos + k*cuProbeSize*4
			if from >= size {
				break
			}
			b, _, err := cuProbe(ctx, out, from, min(cuProbeSize*4, size-from))
			if err != nil {
				return cuPoint{}, false
			}
			if p, v, _, ok := firstPCR(b, from, pid); ok {
				return cuPoint{p, v}, true
			}
		}
		return cuPoint{}, false
	}

	// the clock at evenly spread places, a few reads at a time
	const n = 16
	pts := make([]cuPoint, n+1)
	oks := make([]bool, n+1)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 1) // (one at a time: one connection beside the player's)
	for i := 0; i <= n; i++ {
		pos := size * int64(i) / n
		if i == n {
			pos = max(0, size-cuProbeSize)
		}
		wg.Add(1)
		go func(i int, pos int64) {
			defer wg.Done()
			sem <- struct{}{}
			pts[i], oks[i] = at(pos)
			<-sem
		}(i, pos)
	}
	wg.Wait()
	var good []cuPoint
	for i := range pts {
		if oks[i] && (len(good) == 0 || pts[i].pos > good[len(good)-1].pos) {
			good = append(good, pts[i])
		}
	}
	if len(good) < 2 || ctx.Err() != nil {
		return nil
	}
	// seconds of clock per byte where it runs straight (the median of the pieces that go forward)
	var rates []float64
	for i := 1; i < len(good); i++ {
		if d := tsDiff(good[i].pcr, good[i-1].pcr); d > 0 {
			rates = append(rates, float64(d)/float64(good[i].pos-good[i-1].pos))
		}
	}
	if len(rates) == 0 {
		return none
	}
	sort.Float64s(rates)
	rate := rates[len(rates)/2]
	clean := func(a, b cuPoint) bool { // the clock ran straight from a to b (a restart is a step back, or a leap)
		d := tsDiff(b.pcr, a.pcr)
		return d >= 0 && float64(d) <= 3*rate*float64(b.pos-a.pos)+10*90000
	}
	var restarts [][2]int64 // stretches of the file (from one PCR packet to the next point) a restart is in
	var bisect func(a, b cuPoint, depth int)
	bisect = func(a, b cuPoint, depth int) {
		if ctx.Err() != nil || clean(a, b) {
			return
		}
		if b.pos-a.pos <= 1<<20 || depth > 40 {
			restarts = append(restarts, [2]int64{a.pos, b.pos})
			return
		}
		m, ok := at(a.pos + (b.pos-a.pos)/2)
		if !ok || m.pos <= a.pos || m.pos >= b.pos {
			restarts = append(restarts, [2]int64{a.pos, b.pos}) // (no clock to split it by: read the stretch whole)
			return
		}
		bisect(a, m, depth+1)
		bisect(m, b, depth+1)
	}
	for i := 1; i < len(good); i++ {
		bisect(good[i-1], good[i], 0)
	}
	if len(restarts) == 0 {
		return none
	}
	// each stretch read whole, from its PCR packet to past the next point: the exact packet where the clock starts
	// over, and how far to move what follows
	m := &cuMap{at: mono(), size: size, align: int64(s)}
	off := int64(0)
	const chunk = 188 * 11155 // (about 2 MB, whole packets)
	for _, st := range restarts {
		a, end := st[0], min(size, st[1]+188*64)
		last := int64(-1) // the newest PCR in this stretch (moved by off)
		lastPTS := map[int]int64{}
		first := int64(-1) // the first packet of the new file (its sound or pictures may restart before its PCR)
		for from := a; from < end; from += chunk {
			if ctx.Err() != nil {
				return nil
			}
			b, _, err := cuProbe(ctx, out, from, min(chunk, end-from))
			if err != nil {
				return nil
			}
			for i := 0; i+188 <= len(b) && b[i] == 0x47; i += 188 {
				p := b[i : i+188]
				at := from + int64(i)
				if v, _ := pcrOf(p, pid); v >= 0 {
					vv := (v + off) % tsWrap
					if last >= 0 && cuStep(vv, last) && (len(m.jumps) == 0 || at > m.jumps[len(m.jumps)-1].pos) {
						if first < 0 {
							first = at
						}
						off = ((off+tsDiff(last, vv)+3600)%tsWrap + tsWrap) % tsWrap // 40 ms after the last one
						m.jumps = append(m.jumps, cuJump{first, off})
						vv = (v + off) % tsWrap
						lastPTS = map[int]int64{}
					}
					first = -1 // (a sound or picture restart the PCR did not follow is left as it is)
					last = vv
					continue
				}
				if pts, id, ok := cuPESPTS(p); ok {
					vv := (pts + off) % tsWrap
					if q, seen := lastPTS[id]; seen && first < 0 && last >= 0 && cuStep(vv, q) {
						first = at
					}
					if first < 0 {
						lastPTS[id] = vv
					}
				}
			}
		}
	}
	if len(m.jumps) == 0 {
		return none
	}
	lg.Info().Int("restarts", len(m.jumps)).Int64("bytes", size).
		Msgf("a catch-up file's clock starts over %d times; its timestamps are made to run straight through", len(m.jumps))
	return m
}

// cuStep: v after last is a restart (back more than 3 s or ahead more than 30 s), as clockFix counts one.
func cuStep(v, last int64) bool {
	d := tsDiff(v, last)
	return d < -3*90000 || d > 30*90000
}

// cuPESPTS: the PTS a packet starting an audio or video PES carries, and its pid.
func cuPESPTS(p []byte) (int64, int, bool) {
	pos := 4
	if p[3]&0x20 != 0 {
		pos = 5 + int(p[4])
	}
	if p[1]&0x40 == 0 || p[3]&0x10 == 0 || pos+14 > 188 {
		return 0, 0, false
	}
	e := p[pos:]
	if e[0] != 0 || e[1] != 0 || e[2] != 1 || !(e[3] >= 0xc0 && e[3] <= 0xef || e[3] == 0xbd) || e[6]&0xc0 != 0x80 || e[7]>>6&2 == 0 {
		return 0, 0, false
	}
	return readTS(e[9:14]), int(p[1]&0x1f)<<8 | int(p[2]), true
}

// cuShift moves every timestamp in packet p by off.
func cuShift(p []byte, off int64) {
	pos := 4
	if p[3]&0x20 != 0 {
		pos = 5 + int(p[4])
		if p[4] >= 7 && p[5]&0x10 != 0 {
			v := (int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7 + off) % tsWrap
			p[6], p[7], p[8], p[9] = byte(v>>25), byte(v>>17), byte(v>>9), byte(v>>1)
			p[10] = p[10]&0x7f | byte(v<<7)&0x80
		}
	}
	if p[1]&0x40 == 0 || p[3]&0x10 == 0 || pos+14 > 188 {
		return
	}
	e := p[pos:]
	if e[0] != 0 || e[1] != 0 || e[2] != 1 || !(e[3] >= 0xc0 && e[3] <= 0xef || e[3] == 0xbd) || e[6]&0xc0 != 0x80 {
		return
	}
	flags := e[7] >> 6
	if flags&2 == 0 || pos+19 > 188 && flags == 3 {
		return
	}
	writeTS(e[9:14], readTS(e[9:14])+off)
	if flags == 3 {
		writeTS(e[14:19], readTS(e[14:19])+off)
	}
}

func (m *cuMap) offAt(pos int64) int64 {
	i := sort.Search(len(m.jumps), func(i int) bool { return m.jumps[i].pos > pos })
	if i == 0 {
		return 0
	}
	return m.jumps[i-1].off
}

// cuBody passes a catch-up answer on, starting at byte start of the file, with its timestamps moved by m.
// Only whole packets wait (up to 187 bytes, until the rest comes).
type cuBody struct {
	r     io.Reader
	m     *cuMap
	pos   int64 // the file position of the first byte of carry
	carry []byte
	out   []byte
	err   error
}

func (b *cuBody) Read(p []byte) (int, error) {
	for len(b.out) == 0 {
		if b.err != nil {
			if len(b.carry) > 0 {
				b.out, b.carry = b.carry, nil
				break
			}
			return 0, b.err
		}
		buf := make([]byte, 64<<10)
		n, err := b.r.Read(buf)
		b.err = err
		data := append(b.carry, buf[:n]...)
		// the first packet start in data
		i := int(((b.m.align-b.pos)%188 + 188) % 188)
		end := i
		for ; i+188 <= len(data); i += 188 {
			if data[i] == 0x47 {
				if off := b.m.offAt(b.pos + int64(i)); off != 0 {
					cuShift(data[i:i+188], off)
				}
			}
			end = i + 188
		}
		if end > len(data) {
			end = len(data)
		}
		b.out, b.carry = data[:end], append([]byte(nil), data[end:]...)
		b.pos += int64(end)
	}
	n := copy(p, b.out)
	b.out = b.out[n:]
	return n, nil
}

// cuSpan: where an answer starts in the file and the file's whole length, from the provider's own answer (so every
// Range form a player sends is covered: from a byte, a span, the last N bytes). ok false: not one whole file.
func cuSpan(resp *http.Response) (start, total int64, ok bool) {
	if resp.StatusCode == 200 {
		return 0, resp.ContentLength, resp.ContentLength > 0
	}
	cr := strings.TrimSpace(resp.Header.Get("Content-Range"))
	if resp.StatusCode != 206 || !strings.HasPrefix(cr, "bytes ") {
		return 0, 0, false
	}
	span, size, found := strings.Cut(cr[6:], "/")
	a, _, found2 := strings.Cut(span, "-")
	start, e1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	total, e2 := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
	if !found || !found2 || e1 != nil || e2 != nil || total <= 0 || start < 0 {
		return 0, 0, false
	}
	return start, total, true
}
