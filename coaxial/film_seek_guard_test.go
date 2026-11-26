package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A film or an episode is passed on as the provider answers it, so that seeking, resuming and a player's
// look at the file before it plays work as they do straight at the provider: a part asked for with Range is
// answered 206 with its place in the whole, If-Range reaches the provider, a part past the end is answered 416
// with the file's length, several parts in one request get the provider's own answer, and a HEAD is asked as a
// HEAD and answered with the length and no body. The provider's Content-Type goes on as it is, and none is
// made up when the provider sends none.
func TestFilmSeekAnswersPassUnchanged(t *testing.T) {
	film := strings.Repeat("0123456789", 1000)
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var seen []string
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Header.Get("Range")+" "+r.Header.Get("If-Range"))
		mu.Unlock()
		switch r.URL.Path {
		case "/movie/realU/realP/5.mkv":
			w.Header().Set("Content-Type", "application/octet-stream")
		case "/series/realU/realP/6.mp4":
			w.Header()["Content-Type"] = nil // (none sent)
		default:
			return false
		}
		if strings.Contains(r.Header.Get("Range"), ",") { // several parts: one multipart answer
			w.Header().Set("Content-Type", "multipart/byteranges; boundary=cut")
			w.WriteHeader(206)
			io.WriteString(w, "--cut\r\nContent-Range: bytes 0-9/10000\r\n\r\n0123456789\r\n--cut\r\nContent-Range: bytes 20-29/10000\r\n\r\n0123456789\r\n--cut--\r\n")
			return true
		}
		http.ServeContent(w, r, "", when, strings.NewReader(film))
		return true
	})
	_, base := advFront(t, fp, "ts", advanced{StayUnder: off()})
	get := func(method, path string, hdr ...string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, nil)
		req.Header.Set("X-Dev", "192.0.2.1")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := rawClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return seen[len(seen)-1]
	}
	stamp := when.Format(http.TimeFormat)
	for _, c := range []struct {
		method, path string
		hdr          []string
		status       int
		length, part string // Content-Length and Content-Range the player must get
		text         string
		asked        string // what the provider must be asked
	}{
		{"GET", "/movie/tv/easy/5.mkv", nil, 200, "10000", "", film, "GET  "},
		{"GET", "/movie/tv/easy/5.mkv", []string{"Range", "bytes=0-"}, 206, "10000", "bytes 0-9999/10000", film, "GET bytes=0- "},
		{"GET", "/movie/tv/easy/5.mkv", []string{"Range", "bytes=9990-"}, 206, "10", "bytes 9990-9999/10000", film[9990:], "GET bytes=9990- "},
		{"GET", "/movie/tv/easy/5.mkv", []string{"Range", "bytes=-5"}, 206, "5", "bytes 9995-9999/10000", film[9995:], "GET bytes=-5 "},
		{"GET", "/movie/tv/easy/5.mkv", []string{"Range", "bytes=5-9", "If-Range", stamp}, 206, "5", "bytes 5-9/10000", film[5:10], "GET bytes=5-9 " + stamp},
		{"GET", "/movie/tv/easy/5.mkv", []string{"Range", "bytes=5-9", "If-Range", `"another"`}, 200, "10000", "", film, `GET bytes=5-9 "another"`},
		{"GET", "/movie/tv/easy/5.mkv", []string{"Range", "bytes=20000-"}, 416, "", "bytes */10000", "", "GET bytes=20000- "},
		{"HEAD", "/movie/tv/easy/5.mkv", nil, 200, "10000", "", "", "HEAD  "},
		{"HEAD", "/movie/tv/easy/5.mkv", []string{"Range", "bytes=100-"}, 206, "9900", "bytes 100-9999/10000", "", "HEAD bytes=100- "},
		{"GET", "/series/tv/easy/6.mp4", []string{"Range", "bytes=100-199"}, 206, "100", "bytes 100-199/10000", film[100:200], "GET bytes=100-199 "},
	} {
		what := fmt.Sprintf("%s %s %v", c.method, c.path, c.hdr)
		resp, text := get(c.method, c.path, c.hdr...)
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", what, resp.StatusCode, c.status)
			continue
		}
		if got := resp.Header.Get("Content-Range"); got != c.part {
			t.Errorf("%s: Content-Range %q, want %q", what, got, c.part)
		}
		if got := resp.Header.Get("Content-Length"); c.length != "" && got != c.length {
			t.Errorf("%s: Content-Length %q, want %q", what, got, c.length)
		}
		if c.status != 416 && text != c.text {
			t.Errorf("%s: %d bytes of body, want %d", what, len(text), len(c.text))
		}
		if c.status < 300 && resp.Header.Get("Accept-Ranges") != "bytes" {
			t.Errorf("%s: Accept-Ranges %q, want the provider's (bytes): players seek only when it is there", what, resp.Header.Get("Accept-Ranges"))
		}
		if got := last(); got != c.asked {
			t.Errorf("%s: the provider was asked %q, want %q", what, got, c.asked)
		}
		want := map[bool][]string{true: {"application/octet-stream"}, false: nil}[strings.HasPrefix(c.path, "/movie/")]
		if got := resp.Header.Values("Content-Type"); c.status < 300 && strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: Content-Type %q, want %q as the provider sent it", what, got, want)
		}
	}
	// several parts in one request: the provider's own answer (here, the parts in one multipart answer)
	resp, text := get("GET", "/movie/tv/easy/5.mkv", "Range", "bytes=0-9,20-29")
	if resp.StatusCode != 206 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "multipart/byteranges") ||
		!strings.Contains(text, "bytes 0-9/10000") || !strings.Contains(text, "bytes 20-29/10000") {
		t.Errorf("two parts in one request: %d %q %q, want the provider's multipart answer", resp.StatusCode, resp.Header.Get("Content-Type"), text)
	}
}
