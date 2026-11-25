package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The login older apps use (panel_api.php) answers with the account first and then every channel: often
// several megabytes. It is passed on whole, and what it says about the account's connections changes nothing:
// the limit is the number typed in.
func TestLargePanelLoginPassedOn(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/panel_api.php" {
			io.WriteString(w, `{"user_info":{"username":"realU","password":"realP","max_connections":"2","active_cons":"0"},`+
				`"server_info":{"url":"x"},"available_channels":{`)
			for i := 0; i < 20000; i++ {
				if i > 0 {
					io.WriteString(w, ",")
				}
				io.WriteString(w, `"`+strings.Repeat("1", 5)+`":{"name":"A channel with a long enough name","stream_icon":"","epg_channel_id":null}`)
			}
			io.WriteString(w, "}}")
			return true
		}
		return false
	})
	f, base := limFront(t, fp, "ts", "3", advanced{})
	r := ask(t, base, "192.0.2.1", "/panel_api.php?username=tv&password=easy")
	if r.StatusCode != 200 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	if n := len(body(r)); n < 1<<20 {
		t.Fatalf("the answer is only %d bytes", n)
	}
	mu.Lock()
	lim := f.limit()
	mu.Unlock()
	if lim != 3 {
		t.Errorf("after a login answer that says 2: limit %d, want the 3 typed in", lim)
	}
}
