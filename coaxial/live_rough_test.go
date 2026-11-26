package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// The live MPEG-TS path, fed the roughest streams a provider sends: big lumps with long pauses between them,
// a few bytes at a time, lumps that end in the middle of a packet, a flood, bytes that are not MPEG-TS at all,
// and a second device on the channel that has stopped reading. Coaxial is a proxy, not a player, so for every
// one of them the same two things must hold for a device watching the channel:
//
//   1. it is sent exactly the bytes the provider sent, in order, with nothing added, left out or changed;
//   2. it is sent them as they come: nothing is held back, paced or waited for (AGENTS.md rules 2 and 14).
//
// A change anywhere that holds video, drops it or reorders it on this path fails here, on every push.

// roughLate: how long after the provider sent a byte a device watching the channel may be sent it.
const roughLate = 750 * time.Millisecond

// roughHeld: where the program was when video was first seen to be held in the run going on (see roughRun).
var roughHeld heldNote

func roughWhere() string {
	if s := roughHeld.Load(); s != "" {
		return "\n\n" + s
	}
	return ""
}

// roughStep: the provider waits, then sends n bytes in one write.
type roughStep struct {
	wait time.Duration
	n    int
}

type roughMark struct {
	at  time.Time
	off int // bytes sent (or read) by then, in all
}

// roughBytes: n bytes of MPEG-TS packets with no programme in them (PID 0x1fff), each one different, so a byte
// left out, repeated or moved shows.
func roughBytes(n int) []byte {
	out := make([]byte, 0, n+188)
	for i := uint64(0); len(out) < n; i++ {
		p := make([]byte, 188)
		p[0], p[1], p[2], p[3] = 0x47, 0x1f, 0xff, 0x10|byte(i&0x0f)
		binary.BigEndian.PutUint64(p[4:], i)
		for k := 12; k < 188; k++ {
			p[k] = byte(i*31 + uint64(k)*7)
		}
		out = append(out, p...)
	}
	return out[:n]
}

func roughTotal(plan []roughStep) (n int) {
	for _, s := range plan {
		n += s.n
	}
	return
}

// roughRun: one device watches a channel whose provider sends data as the plan says. Returns what the provider
// sent and when, and what the device was sent and when. other, if set, is run once the stream has started.
func roughRun(t *testing.T, id string, data []byte, plan []roughStep, other func(base string)) (sent, got []roughMark, read []byte) {
	t.Helper()
	var smu, gmu sync.Mutex
	// While the stream runs: the first time bytes the provider has sent are still not with the device after
	// the time allowed, where every part of the program is at that moment is written down (roughHeld).
	roughHeld.Store("")
	lw := startLateWatch(roughLate, &roughHeld)
	defer lw.stop()
	finished := make(chan struct{})
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/"+id+".ts" {
			return false
		}
		w.Header().Set("Content-Type", "video/mp2t")
		off := 0
		for _, s := range plan {
			select {
			case <-time.After(s.wait):
			case <-r.Context().Done():
				return true
			}
			if _, err := w.Write(data[off : off+s.n]); err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			off += s.n
			smu.Lock()
			sent = append(sent, roughMark{time.Now(), off})
			smu.Unlock()
			lw.sent(off)
		}
		close(finished)
		<-r.Context().Done() // (the connection stays open, as a live channel's does)
		return true
	})
	_, base := advFront(t, fp, "ts", advanced{})
	r := getAs(t, base+"/live/tv/easy/"+id+".ts", "192.0.2.50")
	if r.StatusCode != 200 {
		t.Fatalf("the channel: %d", r.StatusCode)
	}
	lw.got(0)
	if other != nil {
		go other(base)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256<<10)
		for {
			k, err := r.Body.Read(buf)
			if k > 0 {
				gmu.Lock()
				read = append(read, buf[:k]...)
				got = append(got, roughMark{time.Now(), len(read)})
				gmu.Unlock()
				lw.gotMore(k)
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-finished:
	case <-time.After(90 * time.Second):
		t.Fatalf("the provider never got to send everything: the channel was no longer being read%s", roughWhere())
	}
	time.Sleep(1500 * time.Millisecond) // (the last bytes are on their way)
	r.Body.Close()
	<-done
	smu.Lock()
	gmu.Lock()
	defer smu.Unlock()
	defer gmu.Unlock()
	return sent, got, read
}

// roughCheck: the two rules. late: how long after the provider sent a byte the device may be sent it.
func roughCheck(t *testing.T, data []byte, sent, got []roughMark, read []byte, late time.Duration) {
	t.Helper()
	total := sent[len(sent)-1].off
	// (a packet the provider has sent only part of is not passed on until the rest of it comes)
	if len(read) < total-187 || len(read) > total {
		t.Errorf("the provider sent %d bytes, the device was sent %d", total, len(read))
	}
	if n := min(len(read), total); !bytes.Equal(read[:n], data[:n]) {
		t.Fatalf("the device was not sent what the provider sent: %s", diffAt(data[:total], read[:n]))
	}
	worst, where, k := time.Duration(0), 0, 0
	for _, s := range sent {
		for k < len(got) && got[k].off < s.off-187 {
			k++
		}
		if k == len(got) {
			break // (counted above)
		}
		if d := got[k].at.Sub(s.at); d > worst {
			worst, where = d, s.off
		}
	}
	t.Logf("%d bytes in %d lumps; the device was at most %d ms behind the provider (at byte %d)", total, len(sent), worst.Milliseconds(), where)
	if worst > late {
		t.Errorf("video was held: bytes the provider had sent reached the device %d ms later (at byte %d); at most %d ms is allowed%s",
			worst.Milliseconds(), where, late.Milliseconds(), roughWhere())
	}
}

func TestLiveRoughFeedsPassStraightThrough(t *testing.T) {
	const late = roughLate
	ms := time.Millisecond

	lumps := []roughStep{{0, 3 << 20}} // an opening burst, then lumps with pauses, one of them long
	for i := 0; i < 8; i++ {
		w := 700 * ms
		if i == 4 {
			w = 3000 * ms
		}
		lumps = append(lumps, roughStep{w, 600 << 10})
	}

	var dribble []roughStep // a few bytes at a time: every packet comes in pieces
	for i := 0; i < 600; i++ {
		dribble = append(dribble, roughStep{4 * ms, 1 + (i*37)%211})
	}

	var torn []roughStep // every lump ends in the middle of a packet, and then nothing comes for a while
	for i := 0; i < 10; i++ {
		torn = append(torn, roughStep{500 * ms, 188*(200+i*13) + 1 + (i*53)%187})
	}

	var flood []roughStep // as fast as the provider can send
	for i := 0; i < 500; i++ {
		flood = append(flood, roughStep{0, 64 << 10})
	}

	cases := []struct {
		name  string
		plan  []roughStep
		spoil func(data []byte)
		other func(base string)
	}{
		{name: "lumps and pauses", plan: lumps},
		{name: "a few bytes at a time", plan: dribble},
		{name: "lumps cut in the middle of a packet", plan: torn},
		{name: "a flood", plan: flood},
		{name: "bytes that are not MPEG-TS", plan: lumps, spoil: func(data []byte) {
			rnd := rand.New(rand.NewSource(7))
			rnd.Read(data[1<<20 : 1<<20+70001]) // (inside the first lump, and not a whole number of packets)
			rnd.Read(data[4<<20 : 4<<20+333])
		}},
		{name: "another device on the channel has stopped reading", plan: lumps, other: func(base string) {
			u, _ := url.Parse(base)
			c, err := net.Dial("tcp", u.Host)
			if err != nil {
				return
			}
			fmt.Fprintf(c, "GET /live/tv/easy/906.ts HTTP/1.1\r\nHost: %s\r\nX-Dev: 192.0.2.51\r\n\r\n", u.Host)
			time.Sleep(20 * time.Second) // (it asks for the channel and never reads a byte of it)
			c.Close()
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := roughBytes(roughTotal(c.plan))
			if c.spoil != nil {
				c.spoil(data)
			}
			sent, got, read := roughRun(t, fmt.Sprint(901+i), data, c.plan, c.other)
			if len(sent) == 0 || len(got) == 0 {
				t.Fatalf("nothing was sent (%d lumps) or nothing arrived (%d reads)", len(sent), len(got))
			}
			roughCheck(t, data, sent, got, read, late)
		})
	}
}
