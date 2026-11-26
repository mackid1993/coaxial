package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every way a playlist or a redirect may write an address, judged by one thing: asked through the proxy, does the
// request reach the server, the path and the query that a player straight at the provider would have reached?
// The playlists here are written the way packagers write them (line ends, a byte order mark, comments, keys, an
// fMP4 start piece, parts, separate sound and subtitles), and the test follows them as a player does.

// formsServers: a video server and a second one elsewhere; each notes what it is asked.
type formsServers struct {
	cdn, other, prov *fakeProv
	base             string
	mu               sync.Mutex
	asked            []string // "<cdn|other|prov> <method> <address>"
	last             http.Header
}

func (s *formsServers) note(who string, r *http.Request) {
	s.mu.Lock()
	s.asked = append(s.asked, who+" "+r.Method+" "+r.URL.RequestURI())
	s.last = r.Header.Clone()
	s.mu.Unlock()
}

// lastAsk: the newest ask that reached a server, and its headers.
func (s *formsServers) lastAsk() (string, http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.asked) == 0 {
		return "", nil
	}
	return s.asked[len(s.asked)-1], s.last
}

const formsMedia = "\ufeff#EXTM3U\r\n#EXT-X-VERSION:7\r\n\r\n# packaged by hand\r\n#EXT-X-TARGETDURATION:4\r\n#EXT-X-MEDIA-SEQUENCE:7\r\n" +
	"#EXT-X-DISCONTINUITY-SEQUENCE:3\r\n" +
	"#EXT-X-KEY:METHOD=AES-128,URI=\"key.php?id=1&t=2\",IV=0x0123\r\n" +
	"#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"720@0\"\r\n" +
	"#EXT-X-SESSION-KEY:METHOD=AES-128,URI=\"/keys/s.key\"\r\n" +
	"#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://abc\"\r\n" +
	"#EXT-X-KEY:METHOD=AES-128,URI=\"data:text/plain;base64,AAAA\"\r\n" +
	"#EXTINF:4,\r\nrel.ts\r\n" +
	"#EXTINF:4,\r\nsub/rel.m4s?x=1&y=a%2Fb\r\n" +
	"#EXT-X-DISCONTINUITY\r\n" +
	"#EXTINF:4,\r\n../up.ts\r\n" +
	"#EXTINF:4,\r\n/root/abs.ts?tok=1\r\n" +
	"#EXTINF:4,\r\n//OTHER/scheme/rel.ts\r\n" +
	"#EXTINF:4,\r\nhttp://OTHER/abs/seg.ts?a=b\r\n" +
	"#EXT-X-BYTERANGE:1000@2000\r\n#EXTINF:4,\r\nbr.ts\r\n" +
	"#EXTINF:4,\r\nchunk_23:00:00.ts\r\n" +
	"#EXTINF:4,\r\nwith space.ts\r\n" +
	"#EXTINF:4,\r\nenc%2Fslash/seg%20a.ts\r\n" +
	"#EXT-X-PART:DURATION=1,URI=\"part.1.m4s\"\r\n" +
	"#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"part.2.m4s\"\r\n" +
	"#EXT-X-RENDITION-REPORT:URI=\"../2/index.m3u8\",LAST-MSN=7\r\n"

// formsWant: for each address in formsMedia, in order: the server and address a player straight at the video
// server reaches ("": not an http address, left exactly as written). The playlist was asked for with ?tok=abc:
// that is the playlist's own and is carried over to nothing.
var formsWant = []string{
	"cdn /s/1/key.php?id=1&t=2", "cdn /s/1/init.mp4", "cdn /keys/s.key", "", "",
	"cdn /s/1/rel.ts", "cdn /s/1/sub/rel.m4s?x=1&y=a%2Fb", "cdn /s/up.ts", "cdn /root/abs.ts?tok=1",
	"other /scheme/rel.ts", "other /abs/seg.ts?a=b", "cdn /s/1/br.ts", "cdn /s/1/chunk_23:00:00.ts",
	"cdn /s/1/with%20space.ts", "cdn /s/1/enc%2Fslash/seg%20a.ts", "cdn /s/1/part.1.m4s", "cdn /s/1/part.2.m4s",
	"cdn /s/2/index.m3u8",
}

func newFormsServers(t *testing.T) *formsServers {
	s := &formsServers{}
	s.other = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		s.note("other", r)
		io.WriteString(w, "other "+r.URL.RequestURI())
		return true
	})
	elsewhere := s.other.srv.Listener.Addr().String()
	s.cdn = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		s.note("cdn", r)
		p := r.URL.Path
		switch {
		case p == "/s/1/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, strings.ReplaceAll(formsMedia, "OTHER", elsewhere))
		case p == "/s/2/index.m3u8": // qualities, with sound and subtitles of their own
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"en\",URI=\"audio/en.m3u8?t=1\"\n"+
				"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"s\",NAME=\"en\",URI=\"subs/en.m3u8\"\n"+
				"#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=1000,URI=\"iframe.m3u8\"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=2000,AUDIO=\"a\",SUBTITLES=\"s\"\nv/720.m3u8?t=9\n")
		case strings.HasPrefix(p, "/s/2/") && strings.HasSuffix(p, ".m3u8"):
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\nseg1.ts\n")
		case p == "/s/9/index.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\na.ts\n")
		case strings.HasPrefix(p, "/redir/") && !strings.HasSuffix(p, "/next.ts"):
			var code int
			var kind string
			fmt.Sscanf(p, "/redir/%d/%s", &code, &kind)
			w.Header().Set("Location", map[string]string{"rel.ts": "next.ts?z=1", "root.ts": "/final/root.ts?k=v",
				"host.ts": "http://" + elsewhere + "/final/host.ts?k=v", "scheme.ts": "//" + elsewhere + "/final/scheme.ts",
				"chain.ts": "/redir/307/root.ts"}[kind])
			w.WriteHeader(code)
		case strings.HasPrefix(p, "/err/"):
			var code int
			fmt.Sscanf(p, "/err/%d.ts", &code)
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(code)
			fmt.Fprintf(w, "<h1>%d</h1>", code)
		case r.Header.Get("Range") != "": // a part of a file
			w.Header().Set("Content-Range", "bytes 2000-2999/5000")
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(206)
			w.Write(make([]byte, 1000))
		case strings.HasPrefix(p, "/kinds/"): // sent in bits, with no length, typed as its server types it
			w.Header().Set("Content-Type", map[string]string{"m4s": "video/iso.segment", "mp4": "video/mp4", "aac": "audio/aac",
				"vtt": "text/vtt", "ts": "video/mp2t"}[p[strings.LastIndexByte(p, '.')+1:]])
			for i := 0; i < 3; i++ {
				w.Write(make([]byte, 1880))
				w.(http.Flusher).Flush()
			}
		default:
			w.Header().Set("Content-Type", "video/mp2t")
			io.WriteString(w, "piece "+r.URL.RequestURI())
		}
		return true
	})
	s.prov = newFakeProv(t, true, func(_ *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		s.note("prov", r)
		var n int
		switch _, err := fmt.Sscanf(r.URL.Path, "/live/realU/realP/%d.m3u8", &n); {
		case err == nil && n == 9: // sent on within the provider's own server first, by path alone
			w.Header().Set("Location", "/lb/9.m3u8?s=1")
			w.WriteHeader(301)
		case err == nil:
			http.Redirect(w, r, fmt.Sprintf("%s/s/%d/index.m3u8?tok=abc", s.cdn.srv.URL, n), 302)
		case r.URL.Path == "/lb/9.m3u8": // and from there to the video server
			w.Header().Set("Location", s.cdn.srv.URL+"/s/9/index.m3u8?tok=abc")
			w.WriteHeader(307)
		default:
			return false
		}
		return true
	})
	_, s.base = limFront(t, s.prov, "hls", "", advanced{})
	return s
}

// formsAsk: one ask as a player makes it, redirects not followed.
func formsAsk(t *testing.T, method, addr string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(method, addr, nil)
	if err != nil {
		t.Fatalf("%s: %v", addr, err)
	}
	req.Header.Set("X-Dev", "192.0.2.41")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", addr, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// formsFollow: asks, following redirects the way a player does: each new address is worked out from the one just
// asked. Returns the last answer; an address that leaves the proxy fails the test.
func formsFollow(t *testing.T, s *formsServers, addr string, hdr ...string) (*http.Response, []byte, string) {
	t.Helper()
	for hop := 0; hop < 8; hop++ {
		resp, data := formsAsk(t, "GET", addr, hdr...)
		if !isRedirect(resp.StatusCode) {
			return resp, data, addr
		}
		at, _ := url.Parse(addr)
		next, err := at.Parse(resp.Header.Get("Location"))
		if err != nil || !strings.HasPrefix(next.String(), s.base+"/") {
			t.Fatalf("%s was sent on to %q, which is not the proxy's", addr, resp.Header.Get("Location"))
		}
		addr = next.String()
	}
	t.Fatalf("%s: sent on without end", addr)
	return nil, nil, ""
}

// addressesOf: the addresses a playlist names, in order: lines of their own, and URI="..." inside tags.
func addressesOf(text string) (out []string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		switch {
		case line == "":
		case !strings.HasPrefix(line, "#"):
			out = append(out, line)
		default:
			for _, m := range uriInTag.FindAllStringSubmatch(line, -1) {
				out = append(out, m[2])
			}
		}
	}
	return out
}

func TestHLSAddressFormsReachTheirServer(t *testing.T) {
	s := newFormsServers(t)
	resp, data, at := formsFollow(t, s, s.base+"/live/tv/easy/1.m3u8", "User-Agent", "Player/1", "Referer", "http://app.example/", "Cookie", "c=1",
		"Accept-Encoding", "gzip")
	if resp.StatusCode != 200 {
		t.Fatalf("the playlist: status %d", resp.StatusCode)
	}
	if ask, h := s.lastAsk(); ask != "cdn GET /s/1/index.m3u8?tok=abc" || h.Get("User-Agent") != "Player/1" || h.Get("Referer") != "http://app.example/" ||
		h.Get("Cookie") != "c=1" || h.Get("Accept-Encoding") != "" {
		t.Errorf("the playlist was asked for as %q with %v; want the video server's own address, the player's app name, referer and cookie, and no packing asked for", ask, h)
	}
	text := string(data)
	// what is not an address comes out as it went in: the mark at the start, the line ends, the empty line, the
	// comment, every tag
	orig := strings.Split(formsMedia, "\r\n")
	lines := strings.Split(text, "\r\n")
	if len(lines) != len(orig) {
		t.Fatalf("the playlist has %d lines with its own line ends, the provider's has %d:\n%q", len(lines), len(orig), text)
	}
	for i := range orig {
		if o := orig[i]; (strings.HasPrefix(o, "#") || o == "" || i == 0) && !strings.Contains(o, "URI=") && lines[i] != o {
			t.Errorf("line %d changed: %q, the provider wrote %q", i+1, lines[i], o)
		}
	}
	for _, keep := range []string{`,IV=0x0123`, `,BYTERANGE="720@0"`, `METHOD=SAMPLE-AES,URI="skd://abc"`, `URI="data:text/plain;base64,AAAA"`, `,LAST-MSN=7`,
		"#EXT-X-PART:DURATION=1,URI=", "#EXT-X-PRELOAD-HINT:TYPE=PART,URI="} {
		if !strings.Contains(text, keep) {
			t.Errorf("the playlist lost %q", keep)
		}
	}
	got := addressesOf(text)
	if len(got) != len(formsWant) {
		t.Fatalf("the playlist names %d addresses, want %d: %q", len(got), len(formsWant), got)
	}
	was := addressesOf(formsMedia)
	here, _ := url.Parse(at)
	for i, a := range got {
		if formsWant[i] == "" {
			if a != was[i] {
				t.Errorf("%q is no http address and was changed to %q", was[i], a)
			}
			continue
		}
		u, err := here.Parse(a) // (worked out as a player does, from the playlist's own address)
		if err != nil || !strings.HasPrefix(u.String(), s.base+"/") {
			t.Errorf("%q became %q, which does not lead to the proxy", was[i], a)
			continue
		}
		var hdr []string
		if strings.Contains(a, "br.ts") || strings.Contains(a, "init.mp4") {
			hdr = []string{"Range", "bytes=2000-2999"}
		}
		resp, body := formsAsk(t, "GET", u.String(), hdr...)
		server, addr, _ := strings.Cut(formsWant[i], " ")
		ask, h := s.lastAsk()
		if ask != server+" GET "+addr {
			t.Errorf("%q in the playlist: asked through the proxy it reached %q, a player straight at the provider reaches %q", was[i], ask, server+" GET "+addr)
			continue
		}
		if hdr != nil && (h.Get("Range") != hdr[1] || resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 2000-2999/5000" || len(body) != 1000) {
			t.Errorf("%q asked for in part: the server was asked %q and the player got status %d, %q, %d bytes", was[i], h.Get("Range"),
				resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
		}
	}
}

func TestHLSQualitiesSoundAndSubtitlesReachTheirServer(t *testing.T) {
	s := newFormsServers(t)
	resp, data, at := formsFollow(t, s, s.base+"/live/tv/easy/2.m3u8")
	if resp.StatusCode != 200 {
		t.Fatalf("the playlist: status %d", resp.StatusCode)
	}
	here, _ := url.Parse(at)
	want := []string{"/s/2/audio/en.m3u8?t=1", "/s/2/subs/en.m3u8", "/s/2/iframe.m3u8", "/s/2/v/720.m3u8?t=9"}
	got := addressesOf(string(data))
	if len(got) != len(want) {
		t.Fatalf("the list of qualities names %d addresses, want %d: %q", len(got), len(want), got)
	}
	for i, a := range got {
		u, err := here.Parse(a)
		if err != nil || !strings.HasPrefix(u.String(), s.base+"/") {
			t.Fatalf("%q does not lead to the proxy", a)
		}
		resp, list := formsAsk(t, "GET", u.String())
		if ask, _ := s.lastAsk(); resp.StatusCode != 200 || ask != "cdn GET "+want[i] {
			t.Errorf("playlist %d of the list: status %d, the video server was asked %q, want %q", i+1, resp.StatusCode, ask, "cdn GET "+want[i])
			continue
		}
		// its own pieces are worked out from where IT is, and its query (?t=...) is carried over to none
		dir := want[i][:strings.LastIndexByte(want[i], '/')+1]
		pieces := addressesOf(string(list))
		if len(pieces) != 1 {
			t.Fatalf("playlist %d names %q", i+1, pieces)
		}
		pu, _ := u.Parse(pieces[0])
		formsAsk(t, "GET", pu.String())
		if ask, _ := s.lastAsk(); ask != "cdn GET "+dir+"seg1.ts" {
			t.Errorf("the piece of playlist %d reached %q, want %q", i+1, ask, "cdn GET "+dir+"seg1.ts")
		}
	}
}

func TestHLSRedirectFormsReachTheirServer(t *testing.T) {
	s := newFormsServers(t)
	// a playlist behind two redirects (301 by path alone on the provider's own server, then 307 to another
	// server): its pieces are worked out from where the playlist really was
	resp, data, at := formsFollow(t, s, s.base+"/live/tv/easy/9.m3u8")
	if resp.StatusCode != 200 || len(addressesOf(string(data))) != 1 {
		t.Fatalf("the playlist behind two redirects: status %d, %q", resp.StatusCode, data)
	}
	here, _ := url.Parse(at)
	pu, _ := here.Parse(addressesOf(string(data))[0])
	formsAsk(t, "GET", pu.String())
	if ask, _ := s.lastAsk(); ask != "cdn GET /s/9/a.ts" {
		t.Errorf("the piece of a playlist behind two redirects reached %q, want %q", ask, "cdn GET /s/9/a.ts")
	}
	// a piece that is sent on, with every status and every way of writing where to
	cdn := s.base + "/__cdn/http/" + s.cdn.srv.Listener.Addr().String()
	for _, code := range []int{301, 302, 303, 307, 308} {
		for kind, want := range map[string]string{"rel.ts": fmt.Sprintf("cdn GET /redir/%d/next.ts?z=1", code), "root.ts": "cdn GET /final/root.ts?k=v",
			"host.ts": "other GET /final/host.ts?k=v", "scheme.ts": "other GET /final/scheme.ts", "chain.ts": "cdn GET /final/root.ts?k=v"} {
			first, _ := formsAsk(t, "GET", fmt.Sprintf("%s/redir/%d/%s", cdn, code, kind))
			if first.StatusCode != code {
				t.Errorf("a piece sent on with %d (%s): the player got status %d", code, kind, first.StatusCode)
				continue
			}
			resp, _, _ := formsFollow(t, s, fmt.Sprintf("%s/redir/%d/%s", cdn, code, kind))
			if ask, _ := s.lastAsk(); resp.StatusCode != 200 || ask != want {
				t.Errorf("a piece sent on with %d (%s): followed as a player follows it, status %d and it reached %q, want %q", code, kind, resp.StatusCode, ask, want)
			}
		}
	}
}

func TestHLSPieceAnswersPassedOnAsTheyAre(t *testing.T) {
	s := newFormsServers(t)
	cdn := s.base + "/__cdn/http/" + s.cdn.srv.Listener.Addr().String()
	for _, code := range []int{403, 404, 500, 503} { // what the server refuses, the player sees refused the same way
		resp, body := formsAsk(t, "GET", fmt.Sprintf("%s/err/%d.ts", cdn, code))
		if resp.StatusCode != code || string(body) != fmt.Sprintf("<h1>%d</h1>", code) || resp.Header.Get("Content-Type") != "text/html" {
			t.Errorf("a piece the server answers %d: the player got %d, %q, %q", code, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	for ext, kind := range map[string]string{"m4s": "video/iso.segment", "mp4": "video/mp4", "aac": "audio/aac", "vtt": "text/vtt", "ts": "video/mp2t"} {
		resp, body := formsAsk(t, "GET", cdn+"/kinds/x."+ext)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != kind || len(body) != 3*1880 {
			t.Errorf("a .%s piece sent in bits: status %d, type %q, %d bytes; want 200, %q, %d", ext, resp.StatusCode, resp.Header.Get("Content-Type"), len(body), kind, 3*1880)
		}
	}
	resp, body := formsAsk(t, "HEAD", cdn+"/s/1/rel.ts") // asked about, not for: nothing is downloaded to answer
	if ask, _ := s.lastAsk(); resp.StatusCode != 200 || len(body) != 0 || ask != "cdn HEAD /s/1/rel.ts" || resp.Header.Get("Content-Type") != "video/mp2t" {
		t.Errorf("HEAD for a piece: status %d, %d bytes, the server was asked %q", resp.StatusCode, len(body), ask)
	}
}
