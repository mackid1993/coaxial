package main

import (
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// A recording runs for hours, and so does a TV left on. Everything a live channel keeps (the video kept for
// joiners, what waits for each reader, the marks its speed is measured from) must stay the same size however
// much video has passed, or the proxy part grows until it is killed (the launcher runs it with a memory limit
// of 384 MB) and every stream ends with it. Hours cannot be waited for, but the amount of video can be had at
// in a few seconds: 800 MB, a recording of about three hours, is sent through one channel in ten seconds (long
// enough for the channel's speed to be measured, which sets how much it keeps), to a device that reads it all
// and beside one that asked for the channel and never reads a byte. What is in use afterwards, with the
// channel still open, may not have grown by more than the channel is meant to keep.
func TestLiveChannelKeepsMemoryBoundedOverALongStream(t *testing.T) {
	const total, grow = 800 << 20, 200 << 20
	block := roughBytes(4 << 20)
	var sent atomic.Int64
	finished := make(chan struct{})
	fp := newFakeProv(t, false, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/live/realU/realP/930.ts" {
			return false
		}
		w.Header().Set("Content-Type", "video/mp2t")
		for sent.Load() < total {
			n, err := w.Write(block)
			sent.Add(int64(n))
			if err != nil {
				return true
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return true
			case <-time.After(50 * time.Millisecond):
			}
		}
		close(finished)
		<-r.Context().Done()
		return true
	})
	_, base := advFront(t, fp, "ts", advanced{})
	inUse := func() int64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	before := inUse()
	r := getAs(t, base+"/live/tv/easy/930.ts", "192.0.2.50")
	if r.StatusCode != 200 {
		t.Fatalf("the channel: %d", r.StatusCode)
	}
	defer r.Body.Close()
	idle := getAs(t, base+"/live/tv/easy/930.ts", "192.0.2.51") // (asks, and never reads)
	defer idle.Body.Close()
	var got atomic.Int64
	go func() {
		buf := make([]byte, 1<<20)
		for {
			n, err := r.Body.Read(buf)
			got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	began := time.Now()
	select {
	case <-finished:
	case <-time.After(4 * time.Minute):
		t.Fatalf("after 4 minutes the provider has sent %d MB of %d: the channel is not being read", sent.Load()>>20, total>>20)
	}
	for end := time.Now().Add(20 * time.Second); got.Load() < sent.Load()-188 && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
	}
	if got.Load() < sent.Load()-188 {
		t.Errorf("the provider sent %d MB, the device that reads was sent %d MB", sent.Load()>>20, got.Load()>>20)
	}
	after := inUse()
	mu.Lock()
	var kept, marks, waiting int
	for _, ch := range channels {
		if ch.id == "930" {
			kept, marks = ch.kept, len(ch.marks)
			for rd := range ch.readers {
				waiting += rd.n
			}
		}
	}
	mu.Unlock()
	t.Logf("%d MB in %.1f s; memory in use went from %d MB to %d MB; the channel keeps %d MB for joiners, %d MB wait for its readers, %d speed marks",
		sent.Load()>>20, time.Since(began).Seconds(), before>>20, after>>20, kept>>20, waiting>>20, marks)
	if after-before > grow {
		t.Errorf("after %d MB of video, %d MB more memory is in use than before the channel was opened; a channel keeps at most %d MB for joiners and as much again for a reader that is behind, so at most %d MB is allowed",
			sent.Load()>>20, (after-before)>>20, ringMax>>20, grow>>20)
	}
	if kept > ringMax+len(block) || marks > 62 {
		t.Errorf("the channel keeps %d MB for joiners (at most %d) and %d speed marks (at most 61)", kept>>20, ringMax>>20, marks)
	}
}
