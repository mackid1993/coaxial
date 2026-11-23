package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Two providers on one server with two accounts: each is given its own account's list, never the other's (which
// names the other account's login, and only each provider's own login is hidden from its players).
func TestShareKeptPerProvider(t *testing.T) {
	fp := newFakeProv(t, true, func(fp *fakeProv, w http.ResponseWriter, r *http.Request) bool {
		if q := r.URL.Query(); q.Get("action") == "get_live_streams" {
			io.WriteString(w, `[{"name":"for `+q.Get("username")+`","username":"`+q.Get("username")+`"}]`)
			return true
		}
		return false
	})
	got := map[string]string{}
	for _, acct := range []string{"acctA", "acctB"} {
		f := newFront(provider{Name: acct, URL: fp.srv.URL, User: acct, Password: "pw" + acct, Port: port(map[string]string{"acctA": "18621", "acctB": "18622"}[acct]),
			LiveTV: "ts", Advanced: advanced{Share: true}}, player{User: "tv", Password: "easy"})
		srv := httptest.NewServer(f)
		r, err := devClient.Get(srv.URL + "/player_api.php?username=tv&password=easy&action=get_live_streams")
		if err != nil {
			t.Fatal(err)
		}
		got[acct] = body(r)
		srv.Close()
	}
	if !strings.Contains(got["acctA"], "for acctA") || !strings.Contains(got["acctB"], "for acctB") || strings.Contains(got["acctB"], "acctA") {
		t.Errorf("each provider must be given its own account's list:\nA: %s\nB: %s", got["acctA"], got["acctB"])
	}
	if n := fp.count(fp.hits, "/player_api.php?get_live_streams"); n != 2 {
		t.Errorf("downloaded %d times, want once per provider", n)
	}
}
