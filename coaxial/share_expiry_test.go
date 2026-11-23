package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A copy is let go when its time is up, even when no player asks for anything new.
func TestShareLetGoOnTime(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("action") == "get_vod_categories" {
			io.WriteString(w, `[{"category_id":"1"}]`)
			return true
		}
		return false
	})
	f := newFront(provider{Name: "X", URL: fp.srv.URL, User: "u", Password: "p", Port: "18626", LiveTV: "ts", Advanced: advanced{Share: true}},
		player{User: "tv", Password: "easy"})
	f.shareFor = 0.3
	srv := httptest.NewServer(f)
	defer srv.Close()
	held := func() (n int) {
		shareMu.Lock()
		defer shareMu.Unlock()
		for k := range shares {
			if strings.Contains(k, fp.srv.URL+" ") {
				n++
			}
		}
		return n
	}
	r, err := devClient.Get(srv.URL + "/player_api.php?username=tv&password=easy&action=get_vod_categories")
	if err != nil {
		t.Fatal(err)
	}
	body(r)
	if held() != 1 {
		t.Fatalf("the list was not kept")
	}
	eventually(t, func() bool { return held() == 0 }, "letting the expired copy go")
}
