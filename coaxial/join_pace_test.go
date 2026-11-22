package main

import "testing"

// A joiner's cushion is queued in pieces the size of a read, so its pace applies to all of it: queued as one
// piece of up to 2 MB, it went at once and the joiner then got nothing for seconds (8.6 s at 230 KB a second).
func TestJoinerCushionPaced(t *testing.T) {
	mu.Lock()
	defer mu.Unlock()
	ch := &channel{key: "k", provider: "p", readers: map[*reader]bool{}, ready: make(chan struct{})}
	ch.readers[&reader{wake: make(chan struct{}, 1)}] = true
	data := make([]byte, 65424)
	for i := 0; i < len(data); i += 188 {
		data[i] = 0x47
	}
	now := mono()
	for i := 0; i < 60; i++ { // 20 s of a 200 KB a second channel
		ch.ring = append(ch.ring, item{now - 20 + float64(i)/3, data})
		ch.kept += len(data)
		ch.total += int64(len(data))
	}
	ch.marks = []mark{{now - 20, 0}, {now, ch.total}}
	rd := ch.join(false)
	if rd.pace == nil {
		t.Fatal("the joiner is not paced")
	}
	for _, d := range rd.q {
		if len(d) > 65536 {
			t.Fatalf("%d KB queued as one piece, past the pace", len(d)>>10)
		}
	}
}
