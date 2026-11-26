package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A provider whose address is https, with a certificate no list of trusted ones knows (made by the provider
// itself, expired, or for another name): common, and players ask such servers without a word. The proxy does
// not check certificates either (see newTransport); a library default would, and every request to such a
// provider would fail before it was sent. No stand-in provider in the tests spoke https. This one does, on
// every path a player uses: the login, live MPEG-TS, a film read in parts, and live HLS, whose playlist and
// pieces come back by way of /__cdn/https/.
func TestProviderOnHTTPSWithACertificateOfItsOwn(t *testing.T) {
	live := roughBytes(1 << 20)
	film := make([]byte, 300000)
	for i := range film {
		film[i] = byte(i*13 + i>>8)
	}
	piece := roughBytes(188 * 400)
	// (its video server for live HLS, https with a certificate of its own as well)
	cdn := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hls/tok1/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:77\n#EXTINF:4,\np77.ts\n")
		case "/hls/tok1/p77.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(piece)
		default:
			http.NotFound(w, r)
		}
	}))
	cdn.Listener = publicListener(t)
	cdn.StartTLS()
	t.Cleanup(func() { cdn.CloseClientConnections(); cdn.Close() })
	prov := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/player_api.php":
			if q := r.URL.Query(); q.Get("username") != "realU" || q.Get("password") != "realP" {
				http.Error(w, "no", 403)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"user_info":{"username":"realU","password":"realP","auth":1,"status":"Active"},"server_info":{"url":"provider.invalid","port":"80","https_port":"443","server_protocol":"https"}}`)
		case "/live/realU/realP/5.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(live)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/movie/realU/realP/7.mkv":
			w.Header().Set("Content-Type", "video/x-matroska")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(film))
		case "/live/realU/realP/44.m3u8":
			http.Redirect(w, r, cdn.URL+"/hls/tok1/index.m3u8", 302)
		default:
			http.NotFound(w, r)
		}
	}))
	prov.Listener = publicListener(t)
	prov.StartTLS()
	t.Cleanup(func() { prov.CloseClientConnections(); prov.Close() })
	if !strings.HasPrefix(prov.URL, "https://") {
		t.Fatalf("the stand-in provider is at %s", prov.URL)
	}
	_, ts := limFront(t, &fakeProv{srv: prov}, "ts", "", advanced{})
	_, hls := limFront(t, &fakeProv{srv: prov}, "hls", "", advanced{})

	r := getAs(t, ts+"/player_api.php?username=tv&password=easy", "192.0.2.1")
	if text := body(r); r.StatusCode != 200 || !strings.Contains(text, `"auth":1`) || strings.Contains(text, "realU") {
		t.Errorf("the login: %d %q", r.StatusCode, text)
	}
	r = getAs(t, ts+"/live/tv/easy/5.ts", "192.0.2.1")
	got := make([]byte, len(live))
	if _, err := io.ReadFull(r.Body, got); r.StatusCode != 200 || err != nil || !bytes.Equal(got, live) {
		t.Errorf("a live channel as MPEG-TS: %d, %v", r.StatusCode, err)
	}
	r.Body.Close()
	r = ask(t, ts, "192.0.2.1", "/movie/tv/easy/7.mkv", "Range", "bytes=100000-199999")
	if data := []byte(body(r)); r.StatusCode != 206 || r.Header.Get("Content-Range") != fmt.Sprintf("bytes 100000-199999/%d", len(film)) || !bytes.Equal(data, film[100000:200000]) {
		t.Errorf("a part of a film: %d, Content-Range %q, %d bytes", r.StatusCode, r.Header.Get("Content-Range"), len(data))
	}
	status, list, _, at := tsHLSGet(t, "192.0.2.1", hls+"/live/tv/easy/44.m3u8")
	_, names := tsHLSList(string(list))
	if status != 200 || len(names) != 1 || !strings.HasPrefix(names[0], "/__cdn/https/") {
		t.Fatalf("a live channel as HLS: %d %q; want its piece named under /__cdn/https/", status, list)
	}
	listAt, _ := url.Parse(at)
	u, _ := listAt.Parse(names[0])
	if st, data, _, _ := tsHLSGet(t, "192.0.2.1", u.String()); st != 200 || !bytes.Equal(data, piece) {
		t.Errorf("its piece: %d, %d bytes, want the provider's %d", st, len(data), len(piece))
	}
}
