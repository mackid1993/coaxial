package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// cuServe serves data as a catch-up file that takes Range, like an archive server.
func cuServe(data []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		http.ServeContent(w, r, "x.ts", time.Time{}, bytes.NewReader(data))
	}))
}

// cuRead reads the file from byte start the way a player through the proxy gets it.
func cuRead(t *testing.T, url string, start int64) []byte {
	t.Helper()
	out, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	out.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	resp, err := sharedTransport.RoundTrip(out)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var src io.Reader = resp.Body
	if r := catchUpClock(&hold{w: what{"ch", "1"}}, out, resp, src); r != nil {
		src = r
	}
	b, err := io.ReadAll(src)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cuCheck(t *testing.T, what string, data []byte) {
	t.Helper()
	srv := cuServe(data)
	defer srv.Close()
	whole := cuRead(t, srv.URL+"/a.ts", 0)
	ecCheck(t, what, data, whole) // the clock runs straight through; length and every other byte the provider's
	// a seek anywhere gets exactly the bytes a read from the start has there
	for _, f := range []float64{0.1, 0.33, 0.5, 0.71, 0.97} {
		at := int64(float64(len(data)) * f)
		if got := cuRead(t, srv.URL+"/a.ts", at); !bytes.Equal(got, whole[at:]) {
			t.Errorf("%s: a seek to byte %d got other timestamps than a read from the start", what, at)
		}
	}
}

// A catch-up file spanning an episode change (the archive's next stored file): one clock, seekable.
func TestCatchUpClockFileChange(t *testing.T) {
	cuCheck(t, "catch-up across an episode change", ecSynthetic())
}

func TestCatchUpClockReplay(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_EPISODE")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_EPISODE names no recorded episode change")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cuCheck(t, "recorded episode change as catch-up", data)
}

// A catch-up file whose clock never starts over passes byte for byte, read from anywhere.
func TestCatchUpClockUntouched(t *testing.T) {
	path := os.Getenv("COAXIAL_REPLAY_TS")
	if path == "" {
		t.Skip("COAXIAL_REPLAY_TS not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := cuServe(data)
	defer srv.Close()
	for _, at := range []int64{0, int64(len(data) / 3)} {
		if got := cuRead(t, srv.URL+"/b.ts", at); !bytes.Equal(got, data[at:]) {
			t.Fatalf("a file with no clock restart was changed (read from %d)", at)
		}
	}
}

// Films and answers outside catch-up are not touched.
func TestCatchUpClockOnlyCatchUp(t *testing.T) {
	ok := &http.Response{StatusCode: 200, Header: http.Header{}}
	r, _ := http.NewRequest("GET", "http://x/movie/a/b/1.ts", nil)
	if catchUpClock(&hold{w: what{"vod", "movie/1"}}, r, ok, nil) != nil || catchUpClock(nil, r, ok, nil) != nil {
		t.Fatal("a film or an unadmitted answer was given the catch-up clock fix")
	}
}
