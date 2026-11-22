package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// One save that moves the web interface to another port and a provider onto the port it had: the proxy part
// starts while the old web interface still holds that port (it is let go a second later). The provider was
// then never started ("NOT STARTED: address already in use") and stayed down until the next save or restart.
func TestProviderTakesThePortTheWebInterfaceLeft(t *testing.T) {
	held, err := net.Listen("tcp", ":0") // the web interface, still on its old port
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(held.Addr().String())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runProxy(strings.NewReader(`{"player":{"user":"tv","password":"easy"},"providers":[{"name":"P","url":"http://prov.example",`+
			`"user":"u","password":"p","port":`+port+`,"live_tv":"ts"}]}`), stop)
		close(done)
	}()
	defer func() {
		close(stop)
		<-done
		mu.Lock()
		delete(fronts, port)
		mu.Unlock()
	}()
	time.Sleep(time.Second)
	held.Close() // (as listenUI lets the old one go, a second after the move)
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
			c.Close()
			return
		}
	}
	t.Fatalf("the provider never took port %s after the web interface let it go", port)
}
