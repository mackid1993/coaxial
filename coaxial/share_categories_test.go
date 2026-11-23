package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With shared downloads on, apps that load the channel list one category at a time and apps that load the
// whole list each get the list they asked for; devices asking the same thing share one download.
func TestShareKeptPerCategory(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if q := r.URL.Query(); q.Get("action") == "get_live_streams" {
			switch q.Get("category_id") {
			case "":
				io.WriteString(w, `[{"name":"News One","category_id":"3"},{"name":"Sports One","category_id":"7"}]`)
			case "3":
				io.WriteString(w, `[{"name":"News One","category_id":"3"}]`)
			case "7":
				io.WriteString(w, `[{"name":"Sports One","category_id":"7"}]`)
			}
			return true
		}
		return false
	})
	f := newFront(provider{Name: "Cat", URL: fp.srv.URL, User: "u", Password: "p", Port: "18624", LiveTV: "ts", Advanced: advanced{Share: true}},
		player{User: "tv", Password: "easy"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	list := srv.URL + "/player_api.php?username=tv&password=easy&action=get_live_streams"
	for i, c := range []struct{ query, want, notWant string }{
		{"&category_id=3", "News One", "Sports One"},
		{"", "Sports One", ""},
		{"&category_id=7", "Sports One", "News One"},
		{"&category_id=3", "News One", "Sports One"}, // (another device, same category)
		{"", "News One", ""},
	} {
		r, err := devClient.Get(list + c.query)
		if err != nil {
			t.Fatal(err)
		}
		g := body(r)
		if r.StatusCode != 200 || !strings.Contains(g, c.want) || c.notWant != "" && strings.Contains(g, c.notWant) {
			t.Errorf("request %d (%q): %d %s", i, c.query, r.StatusCode, g)
		}
	}
	if n := fp.count(fp.hits, "/player_api.php?get_live_streams"); n != 3 {
		t.Errorf("the provider was asked %d times, want once per different list (3)", n)
	}
}
