package main

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"os"
	"testing"
)

// chunks: a reader handing data on in reads of random sizes, as a provider's answer arrives.
type chunks struct {
	data []byte
	rnd  *rand.Rand
}

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), len(c.data), 1+c.rnd.Intn(40000))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func throughCatchUp(t *testing.T, data []byte, seed int64) []byte {
	t.Helper()
	out, err := io.ReadAll(&clockBody{r: &chunks{data, rand.New(rand.NewSource(seed))}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Catch-up whose archive moves on to its next file mid-answer: the clock carries on, as on a live channel.
func TestCatchUpClockFileChange(t *testing.T) {
	data := ecSynthetic()
	for seed := int64(1); seed <= 5; seed++ {
		ecCheck(t, "catch-up across archive files", data, throughCatchUp(t, data, seed))
	}
}

// A recorded episode change, read as a catch-up answer: no clock jump reaches the player.
func TestCatchUpClockReplay(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_EPISODE")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_EPISODE names no recorded episode change")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for seed := int64(1); seed <= 5; seed++ {
		ecCheck(t, "recorded episode change as catch-up", data, throughCatchUp(t, data, seed))
	}
}

// Catch-up whose clock never jumps passes byte for byte; so does anything that is not MPEG-TS.
func TestCatchUpClockUntouched(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_TS")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_TS not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for seed := int64(1); seed <= 3; seed++ {
		if out := throughCatchUp(t, data, seed); !bytes.Equal(out, data) {
			t.Fatalf("seed %d: a stream with no clock jump was changed (%d bytes in, %d out)", seed, len(data), len(out))
		}
	}
	other := bytes.Repeat([]byte("not a transport stream "), 5000)
	if out := throughCatchUp(t, other, 1); !bytes.Equal(out, other) {
		t.Fatal("an answer that is not MPEG-TS was changed")
	}
}

// Only a whole catch-up answer is touched: not a seek inside one, not a film.
func TestCatchUpClockOnlyCatchUp(t *testing.T) {
	ok := &http.Response{StatusCode: 200, Header: http.Header{}}
	get := func(rng string) *http.Request {
		r, _ := http.NewRequest("GET", "http://x/timeshift/a/b/60/2026-11-25:21-00/1.ts", nil)
		if rng != "" {
			r.Header.Set("Range", rng)
		}
		return r
	}
	ch := &hold{w: what{"ch", "1"}}
	if catchUpClock(ch, get(""), ok, nil, "tv") == nil {
		t.Error("a catch-up answer was not given the clock fix")
	}
	if catchUpClock(ch, get("bytes=100-"), ok, nil, "tv") != nil {
		t.Error("a seek inside a catch-up answer was changed")
	}
	if catchUpClock(&hold{w: what{"vod", "movie/1"}}, get(""), ok, nil, "tv") != nil {
		t.Error("a film was given the clock fix")
	}
	if catchUpClock(nil, get(""), ok, nil, "tv") != nil {
		t.Error("an unadmitted answer was given the clock fix")
	}
}
