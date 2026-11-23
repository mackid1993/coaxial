package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// drained: a response read to the end in the background, counting what arrived.
type drained struct {
	n    atomic.Int64
	done chan struct{}
	r    *http.Response
}

func drain(r *http.Response) *drained {
	d := &drained{done: make(chan struct{}), r: r}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			k, err := r.Body.Read(buf)
			d.n.Add(int64(k))
			if err != nil {
				close(d.done)
				return
			}
		}
	}()
	return d
}

func (d *drained) close() { d.r.Body.Close() }

func (d *drained) flowing(t *testing.T, what string, for_ time.Duration) {
	t.Helper()
	a := d.n.Load()
	time.Sleep(for_)
	if b := d.n.Load(); b <= a {
		t.Errorf("%s: no video arrived in %v (%d bytes before, %d after)", what, for_, a, b)
	}
}
