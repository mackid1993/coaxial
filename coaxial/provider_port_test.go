package main

import (
	"context"
	"strings"
	"testing"
)

// A provider address whose port no connection can use (over 65535, or 0) is refused when it is saved: it was
// accepted, and then every request to that provider failed with no word why.
func TestProviderURLPort(t *testing.T) {
	for url, ok := range map[string]bool{"http://provider.example:99999": false, "http://provider.example:0": false,
		"https://provider.example:65536/": false, "http://provider.example:8080": true, "https://provider.example:443/": true,
		"http://provider.example": true} {
		s := goodSettings()
		s.Providers[0].URL = url
		bad := false
		for _, e := range s.validate() {
			bad = bad || e.Field == "providers[0].url"
		}
		if bad == ok {
			t.Errorf("%s: refused %v, want %v", url, bad, !ok)
		}
	}
	// what such an address did once saved: every request to the provider fails
	if _, err := fetch(context.Background(), "http://192.0.2.1:99999/player_api.php", nil, opts{wait: 1}); err == nil || !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("fetching from port 99999 gave %v", err)
	}
}
