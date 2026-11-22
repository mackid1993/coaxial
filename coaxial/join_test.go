package main

import "testing"

// A joiner on a channel in its first seconds (no speed measured yet) is not paced by a guess.
func TestEarlyJoinerNotPaced(t *testing.T) {
	mu.Lock()
	defer mu.Unlock()
	ch := &channel{key: "k", provider: "p", readers: map[*reader]bool{}, ready: make(chan struct{})}
	ch.readers[&reader{wake: make(chan struct{}, 1)}] = true
	data := make([]byte, 188*300)
	for i := 0; i < len(data); i += 188 {
		data[i] = 0x47
	}
	for i := 0; i < 20; i++ {
		ch.feed(data)
	}
	if rd := ch.join(false); rd.pace != nil || rd.n == 0 || rd.given != rd.n {
		t.Errorf("an early joiner: pace %v, %d bytes waiting, %d counted as given", rd.pace, rd.n, rd.given)
	}
}
