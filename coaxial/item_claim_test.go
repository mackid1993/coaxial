package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A second device that keeps asking for the film (a player retrying after its 503) while the first device
// seeks: the film stays the first device's. Its seek is not refused, and the other device is not let in in the
// moment between the old read closing and the new one opening.
func TestItemSeekNotTakenByRetryingDevice(t *testing.T) {
	fp := newFakeProv(t, false, nil)
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off(), OnePerItem: true})
	for round := 0; round < 5; round++ {
		a := ask(t, base, "192.0.2.1", "/movie/tv/easy/7.mkv", "Range", fmt.Sprintf("bytes=%d-", round))
		if a.StatusCode != 200 {
			t.Fatalf("round %d: the first device: %d", round, a.StatusCode)
		}
		stop, got := make(chan struct{}), make(chan int, 1000)
		go func() {
			defer close(got)
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest("GET", base+"/movie/tv/easy/7.mkv", nil)
				req.Header.Set("X-Dev", "192.0.2.2")
				if r, err := devClient.Do(req); err == nil {
					got <- r.StatusCode
					r.Body.Close()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		time.Sleep(30 * time.Millisecond)
		seek := ask(t, base, "192.0.2.1", "/movie/tv/easy/7.mkv", "Range", "bytes=1000000-")
		time.Sleep(30 * time.Millisecond)
		close(stop)
		in := 0
		for s := range got {
			if s == 200 {
				in++
			}
		}
		if seek.StatusCode != 200 || in > 0 {
			t.Errorf("round %d: the first device's seek: %d; the other device let in %d times", round, seek.StatusCode, in)
		}
		seek.Body.Close()
		a.Body.Close()
		eventually(t, func() bool { return fp.count(fp.open, "") == 0 }, "the film closing")
	}
}
