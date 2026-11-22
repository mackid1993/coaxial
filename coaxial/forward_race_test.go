package main

import (
	"net"
	"net/http/httptest"
	"testing"
)

// The pass-through's wait runs out just as the server fails for another reason: the timer and the request
// both look at whether it ran out (go test -race).
func TestForwardWaitRace(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now: refused at once
	f := newFront(provider{Name: "Gone", URL: "http://" + addr, User: "u", Password: "p", Port: "18503", LiveTV: "ts"},
		player{User: "tv", Password: "easy"})
	for i := 0; i < 1000; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/movie/tv/easy/1.mkv", nil)
		f.forward(&counted{ResponseWriter: w}, r, "http://"+addr+"/x", "http", addr, 1e-9*float64(i%20), false)
	}
}
