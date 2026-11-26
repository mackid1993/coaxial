package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// A live playlist is reloaded every few seconds over a connection that is already open. The reload must not
// depend on the resolver answering at that moment: a home router's resolver fails or is slow now and then.

// fakeResolver stands in for the resolver the program asks (net.DefaultResolver): a small DNS server that
// answers every name with the address in lead, fails every lookup while mode is 1 (SERVFAIL, as a resolver
// that cannot reach its own servers does), or says nothing while mode is 2.
type fakeResolver struct {
	lead  atomic.Value // net.IP
	mode  atomic.Int32
	asked atomic.Int32
}

func newFakeResolver(t *testing.T, lead net.IP) *fakeResolver {
	fr := &fakeResolver{}
	fr.lead.Store(lead.To4())
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			end := 12 // the one question: a name, a type and a class
			for end < n && q[end] != 0 {
				end += int(q[end]) + 1
			}
			end += 5
			if n < 12 || end > n {
				continue
			}
			fr.asked.Add(1)
			mode := fr.mode.Load()
			if mode == 2 {
				continue
			}
			a := append([]byte{}, q[:end]...)
			a[2], a[3] = 0x81, 0x80 // an answer, no error
			a[6], a[7], a[8], a[9], a[10], a[11] = 0, 0, 0, 0, 0, 0
			if mode == 1 {
				a[3] = 0x82 // the server failed
			} else if q[end-4] == 0 && q[end-3] == 1 { // an A record asked for
				a[7] = 1
				a = append(a, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4)
				a = append(a, fr.lead.Load().(net.IP)...)
			}
			pc.WriteTo(a, from)
		}
	}()
	old := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
	t.Cleanup(func() { net.DefaultResolver = old; pc.Close() })
	return fr
}

// namedHLS: newestHLS with the video server reached by name (video.example), as real ones are.
func namedHLS(t *testing.T) (h *newestHLS, fr *fakeResolver, port string) {
	h = newNewestHLS(t)
	u, _ := url.Parse(h.cdn.srv.URL)
	port = u.Port()
	fr = newFakeResolver(t, net.ParseIP(u.Hostname()))
	h.frontHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		h.latest.Store(int32(n))
		http.Redirect(w, r, fmt.Sprintf("http://video.example:%s/hlsr/tok%d/index.m3u8", port, n), 302)
		return true
	}
	return h, fr, port
}

// aMinuteLater moves the program's clock on 61 s, so that what it remembers for a minute is old; the HLS
// sessions are kept as they were (a player reloading every few seconds keeps its session fresh).
func aMinuteLater() {
	mu.Lock()
	defer mu.Unlock()
	clockStart = clockStart.Add(-61 * time.Second)
	for _, s := range sessions {
		s.used, s.opened = s.used+61, s.opened+61
		for d := range s.who {
			s.who[d] += 61
		}
	}
}

const reloadPath = "/__ch/live/tv/easy/44.m3u8/__index.m3u8"

// The resolver fails while a channel plays. The reload goes out on the connection kept open to the video
// server and the player gets its playlist; the channel is not opened again at the provider.
func TestHLSReloadWhenLookupFails(t *testing.T) {
	for _, c := range []struct {
		name string
		mode int32
		aged bool // the name was last looked up over a minute ago
	}{{"lookups fail", 1, false}, {"lookups fail, the name looked up a while ago", 1, true}, {"lookups get no answer", 2, true}} {
		t.Run(c.name, func(t *testing.T) {
			h, fr, _ := namedHLS(t)
			if st, _, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 10*time.Second); err != nil || st != 200 {
				t.Fatalf("tune: %d %v", st, err)
			}
			if fr.asked.Load() == 0 {
				t.Fatalf("the fake resolver was never asked")
			}
			if c.aged {
				aMinuteLater()
			}
			fr.mode.Store(c.mode)
			began := time.Now()
			st, list, err := devGet(h.base, "192.0.2.1", reloadPath, 10*time.Second)
			fr.mode.Store(0)
			if err != nil || st != 200 || len(list) == 0 {
				t.Errorf("the reload while the resolver was down: status %d, %v, want the playlist", st, err)
			}
			if took := time.Since(began); took > 5*time.Second {
				t.Errorf("the reload took %.1f s", took.Seconds())
			}
			if n := h.opens.Load(); n != 1 {
				t.Errorf("the channel was opened at the provider %d times, want 1", n)
			}
		})
	}
}

// What the lookup is for still holds: a name that comes to lead to this machine or a home network is never
// connected to, whether the name's last answer is still remembered or not.
func TestHLSReloadNameTurnsHome(t *testing.T) {
	for _, aged := range []bool{false, true} {
		h, fr, port := namedHLS(t)
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Skipf("the video server's port is taken on this machine: %v", err)
		}
		var reached atomic.Int32
		home := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached.Add(1)
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4,\nhome.ts\n")
		})}
		go home.Serve(ln)
		if st, _, err := devGet(h.base, "192.0.2.1", "/live/tv/easy/44.m3u8", 10*time.Second); err != nil || st != 200 {
			t.Fatalf("tune: %d %v", st, err)
		}
		fr.lead.Store(net.ParseIP("127.0.0.1").To4()) // the name now leads home
		sharedTransport.CloseIdleConnections()        // (and the connection kept to the real server is gone)
		if aged {
			aMinuteLater()
		}
		st, list, _ := devGet(h.base, "192.0.2.1", reloadPath, 10*time.Second)
		if st != 403 || reached.Load() != 0 {
			t.Errorf("aged %v: a name that leads home: status %d %q, the server at home was asked %d times, want it refused (403)", aged, st, list, reached.Load())
		}
		home.Close()
	}
}
