package main

import (
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The page tries a new port (POST /api/port/try) just before it saves. The save starts the proxy part, or
// moves the page, at once: the port must be free for it by then, not held for 8 more seconds.
func TestPortTryLetsGo(t *testing.T) {
	n := freePort(t)
	w := httptest.NewRecorder()
	apiPortTry(w, httptest.NewRequest("POST", "/api/port/try?port="+strconv.Itoa(n), nil))
	if !strings.Contains(w.Body.String(), "for 8 seconds") {
		t.Fatalf("the port was not tried: %s", w.Body)
	}
	l := &launcher{quit: make(chan int, 4)}
	if err := l.listenUI(n); err != nil {
		t.Fatalf("the page could not move to the port just tried: %v", err)
	}
	l.ui.Close()
	m := freePort(t)
	apiPortTry(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/port/try?port="+strconv.Itoa(m), nil))
	endTries() // (what startProxy does before the proxy part opens the providers' ports)
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(m))
	if err != nil {
		t.Fatalf("a provider could not open the port just tried: %v", err)
	}
	ln.Close()
}
