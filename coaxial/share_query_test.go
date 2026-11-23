package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A query with a pair Go cannot read (category_id=3; as PHP reads it, category 3) is not shared: left out of the
// key, it would hand one category's list to every player asking for the whole list.
func TestShareUnreadableQuery(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("action") == "get_live_streams" {
			if strings.Contains(r.URL.RawQuery, "category_id=3") {
				io.WriteString(w, `[{"name":"three"}]`)
			} else {
				io.WriteString(w, `[{"name":"all"},{"name":"three"}]`)
			}
			return true
		}
		return false
	})
	f := newFront(provider{Name: "Q", URL: fp.srv.URL, User: "u", Password: "p", Port: "18623", LiveTV: "ts", Advanced: advanced{Share: true}},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	list := srv.URL + "/player_api.php?username=tv&password=easy&action=get_live_streams"
	for _, u := range []string{list + "&category_id=3;", list} {
		r, err := devClient.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		if g := body(r); u == list && !strings.Contains(g, `"all"`) {
			t.Errorf("the whole list was answered with one category's: %s", g)
		}
	}
}
