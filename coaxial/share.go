// Shared list and guide downloads (the advanced option share_lists). A provider's lists (player_api.php and
// panel_api.php with an action: channels, movies, series, categories, a series' or a film's details, short
// guides) and its guide (xmltv.php) are downloaded once and every player is given the same copy for
// share_minutes. Players that ask while the download is under way follow it as it arrives. The login answer
// itself (no action) is never shared: it says how many connections are in use now.
//
// What is kept is the provider's answer as it came, unpacked. Each player is given it rewritten for itself,
// as before (pictures pointed back at the address it used, the login hidden). Only a whole answer with status
// 200 is kept; anything else is given to the players that asked for it at that moment and then let go.
// Small answers are kept in memory, larger ones in a file in the temporary folder (deleted as it is made, so
// nothing is left behind); one larger than shareMax is not kept at all.
package main

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	shareSpill  = 4 << 20  // an answer larger than this is kept in a file
	shareMemMax = 32 << 20 // answers kept in memory, all together, at most
	shareMax    = 1 << 30  // an answer larger than this is not kept (and its download is let go)
	shareKeep   = 500      // answers kept, at most
)

var (
	shareMu  sync.Mutex // shares
	shares   = map[string]*shareEntry{}
	shareMem atomic.Int64 // bytes of answers kept in memory
)

type shareEntry struct {
	key    string
	ready  chan struct{} // closed once status and header are known, or the download failed before them
	status int
	header http.Header
	err    error

	mu    sync.Mutex // everything below
	data  []byte
	file  *os.File
	size  int64
	done  bool // all of it is in, or it broke off
	whole bool // it ended as it should
	grew  chan struct{}
	until float64 // shared until then (a whole answer with status 200)
	users int
	gone  bool // no longer in the list: let go once its last reader is done
	freed bool
}

// shareKey: the key a request is shared under, "" when it is not shared.
func shareKey(f *front, r *http.Request) string {
	if f.shareFor <= 0 || r.Method != "GET" {
		return ""
	}
	low := strings.ToLower(cleanPath(r.URL.Path))
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil { // (a pair it cannot read would be left out of the key, yet the provider reads it)
		return ""
	}
	if low == "/xmltv.php" || (low == "/player_api.php" || low == "/panel_api.php") && q.Get("action") != "" {
		return f.port + " " + f.base + " " + low + "?" + q.Encode() // (two providers may be two accounts on one server)
	}
	return ""
}

// shared: the answer for key, from a download that is under way, or done and still fresh; else one is started.
// The caller closes its body.
func shared(ctx context.Context, f *front, key, target string, h http.Header) (*http.Response, error) {
	now := mono()
	shareMu.Lock()
	e := shares[key]
	if e != nil {
		e.mu.Lock()
		stale := e.done && (!e.whole || e.status != 200 || now >= e.until)
		e.mu.Unlock()
		if stale {
			dropShare(e)
			e = nil
		}
	}
	if e == nil {
		e = &shareEntry{key: key, ready: make(chan struct{}), grew: make(chan struct{})}
		shares[key] = e
		tidyShares(now)
		hd := h.Clone()
		for _, k := range []string{"If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "If-Range"} {
			hd.Del(k) // (the copy is for every player: the whole answer is asked for, never "not changed")
		}
		go e.fill(target, hd, f.shareFor)
	}
	e.mu.Lock()
	e.users++
	e.mu.Unlock()
	shareMu.Unlock()
	select {
	case <-e.ready:
	case <-ctx.Done():
		e.leave()
		return nil, ctx.Err()
	}
	if e.err != nil {
		e.leave()
		return nil, e.err
	}
	return &http.Response{StatusCode: e.status, Header: e.header.Clone(), Body: &shareReader{e: e, ctx: ctx}}, nil
}

// fill downloads the answer. It goes on when the player that started it hangs up: others may be waiting.
func (e *shareEntry) fill(target string, h http.Header, keep float64) {
	resp, err := fetch(context.Background(), target, h, opts{guardAll: true, wait: 300})
	if err != nil {
		e.err = err
		close(e.ready)
		e.finish(false, keep)
		return
	}
	defer resp.Body.Close()
	body := io.Reader(resp.Body)
	switch ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce {
	case "", "identity":
	case "gzip", "x-gzip": // (as api: some providers send their guide packed whatever was asked)
		z, err := gzip.NewReader(resp.Body)
		if err != nil {
			e.err = &refused{status: 502}
			close(e.ready)
			e.finish(false, keep)
			return
		}
		defer z.Close()
		body = z
	default:
		e.err = &refused{status: 502} // a compression this cannot read
		close(e.ready)
		e.finish(false, keep)
		return
	}
	hd := resp.Header.Clone()
	hd.Del("Content-Encoding")
	hd.Del("Content-Length")
	e.status, e.header = resp.StatusCode, hd
	close(e.ready)
	buf := make([]byte, 64<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 && !e.write(buf[:n]) {
			e.finish(false, keep)
			return
		}
		if err == io.EOF {
			e.finish(true, keep)
			return
		} else if err != nil {
			e.finish(false, keep)
			return
		}
	}
}

func (e *shareEntry) write(p []byte) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.size+int64(len(p)) > shareMax {
		return false
	}
	if e.file == nil && (len(e.data)+len(p) > shareSpill || shareMem.Load()+int64(len(p)) > shareMemMax) {
		f, err := os.CreateTemp("", "coaxial-share-*")
		if err != nil {
			return false
		}
		os.Remove(f.Name()) // (open, it stays readable; closed, it is gone: nothing is ever left behind)
		if _, err := f.Write(e.data); err != nil {
			f.Close()
			return false
		}
		shareMem.Add(-int64(len(e.data)))
		e.data, e.file = nil, f
	}
	if e.file != nil {
		if _, err := e.file.Write(p); err != nil {
			return false
		}
	} else {
		e.data = append(e.data, p...)
		shareMem.Add(int64(len(p)))
	}
	e.size += int64(len(p))
	close(e.grew)
	e.grew = make(chan struct{})
	return true
}

// finish: the download is over. Only a whole answer with status 200 is kept.
func (e *shareEntry) finish(whole bool, keep float64) {
	e.mu.Lock()
	e.done, e.whole = true, whole && e.err == nil
	kept := e.whole && e.status == 200 && e.size > 0 // (an empty answer is never a list or a guide: asked again)
	if kept {
		e.until = mono() + keep
	}
	close(e.grew)
	e.grew = make(chan struct{})
	orphan := e.gone && e.users == 0 // (taken off the list while it downloaded, and nobody reads it)
	e.mu.Unlock()
	if kept { // let go on time, even when nothing new is asked for (a guide may be 100 MB or more)
		time.AfterFunc(secs(keep), func() {
			shareMu.Lock()
			if shares[e.key] == e {
				dropShare(e)
			}
			shareMu.Unlock()
		})
	}
	if orphan {
		e.free()
	}
	if !kept {
		shareMu.Lock()
		if shares[e.key] == e {
			dropShare(e)
		}
		shareMu.Unlock()
	}
}

// dropShare takes e off the list; it is let go once nobody reads it. (shareMu held)
func dropShare(e *shareEntry) {
	if shares[e.key] == e {
		delete(shares, e.key)
	}
	e.mu.Lock()
	e.gone = true
	free := e.users == 0
	e.mu.Unlock()
	if free {
		e.free()
	}
}

func (e *shareEntry) leave() {
	e.mu.Lock()
	e.users--
	free := e.gone && e.users == 0
	e.mu.Unlock()
	if free {
		e.free()
	}
}

func (e *shareEntry) free() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.freed || !e.done { // (a download under way lets go when it is done)
		return
	}
	e.freed = true
	shareMem.Add(-int64(len(e.data)))
	e.data = nil
	if e.file != nil {
		e.file.Close()
		e.file = nil
	}
}

// tidyShares: expired answers go; past shareKeep, the oldest finished ones too. (shareMu held)
func tidyShares(now float64) {
	var oldest *shareEntry
	for _, e := range shares {
		e.mu.Lock()
		done, until := e.done, e.until
		e.mu.Unlock()
		if done && now >= until {
			dropShare(e)
		} else if done && (oldest == nil || until < oldest.until) {
			oldest = e
		}
	}
	if len(shares) > shareKeep && oldest != nil {
		dropShare(oldest)
	}
}

// endShares lets every kept answer go (the proxy part is stopping).
func endShares() {
	shareMu.Lock()
	defer shareMu.Unlock()
	for _, e := range shares {
		dropShare(e)
	}
}

// shareReader follows a shared answer from its start, waiting for what has not arrived yet.
type shareReader struct {
	e      *shareEntry
	ctx    context.Context
	off    int64
	closed bool
}

func (r *shareReader) Read(p []byte) (int, error) {
	e := r.e
	for {
		e.mu.Lock()
		if r.off < e.size {
			n, err := 0, error(nil)
			if want := min(int64(len(p)), e.size-r.off); e.file != nil {
				n, err = e.file.ReadAt(p[:want], r.off)
			} else {
				n = copy(p[:want], e.data[r.off:])
			}
			e.mu.Unlock()
			r.off += int64(n)
			if err == io.EOF {
				err = nil
			}
			return n, err
		}
		if e.done {
			whole := e.whole
			e.mu.Unlock()
			if whole {
				return 0, io.EOF
			}
			return 0, io.ErrUnexpectedEOF
		}
		grew := e.grew
		e.mu.Unlock()
		select {
		case <-grew:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
}

func (r *shareReader) Close() error {
	if !r.closed {
		r.closed = true
		r.e.leave()
	}
	return nil
}
