package main

import (
	"net/http"
	"testing"
)

// The provider's own headers reach the player; nothing that names the provider does.
func TestPassHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("Content-Type", "video/mp2t")
	in.Set("Cache-Control", "no-store")
	in.Set("Access-Control-Allow-Origin", "*")
	in.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
	in.Set("Server", "nginx")
	in.Set("Set-Cookie", "a=b")
	in.Set("Location", "https://cdn.example/x.ts")
	in.Set("Report-To", "{}")
	in.Set("Transfer-Encoding", "chunked")
	in.Set("Connection", "keep-alive")
	out := passHeaders(in)
	for _, k := range []string{"Content-Type", "Cache-Control", "Access-Control-Allow-Origin", "Access-Control-Expose-Headers"} {
		if out.Get(k) != in.Get(k) {
			t.Errorf("%s not passed on: %q", k, out.Get(k))
		}
	}
	for _, k := range []string{"Server", "Set-Cookie", "Location", "Report-To", "Transfer-Encoding", "Connection"} {
		if out.Get(k) != "" {
			t.Errorf("%s passed on", k)
		}
	}
	html := http.Header{}
	html.Set("Content-Type", "text/html")
	if passHeaders(html).Get("Content-Type") != "" {
		t.Errorf("a non-video content type was passed on")
	}
}
