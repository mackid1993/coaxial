/*

copyFlush, flushWriter and readWithDeadline in this file are taken from ah4c
(https://github.com/sullrich/ah4c, main.go), unchanged apart from this notice. ah4c's notice:

Copyright 2023 Fancy Bits, LLC

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the “Software”), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED “AS IS”, WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

*/

package main

import (
	"context"
	"io"
	"time"
)

// flushWriter is a response that can be pushed all the way out.
type flushWriter interface {
	io.Writer
	Flush()
}

// copyFlush is io.Copy, flushed after every write.
func copyFlush(dst flushWriter, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var n int64
	for {
		r, rerr := src.Read(buf)
		if r > 0 {
			w, werr := dst.Write(buf[:r])
			n += int64(w)
			if werr != nil {
				return n, werr
			}
			dst.Flush()
		}
		if rerr != nil {
			if rerr == io.EOF {
				return n, nil
			}
			return n, rerr
		}
	}
}

// readWithDeadline does r.Read with a timeout: on expiry the body is closed,
// unblocking the blocked Read with an error. No goroutine leak, no buf race.
func readWithDeadline(r io.ReadCloser, buf []byte, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	defer context.AfterFunc(ctx, func() { r.Close() })()
	return r.Read(buf)
}
