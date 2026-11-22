package main

import "testing"

// A saved password is kept only for the server it was saved for.
func TestKeepSecretsOtherServer(t *testing.T) {
	old := goodSettings()
	for url, want := range map[string]string{"https://provider.example": "p", "https://PROVIDER.example/": "p", "http://provider.example": "p",
		"http://elsewhere.example": "", "https://provider.example:8443": "", "https://provider.example.elsewhere.example": ""} {
		s := &settings{}
		s.parse([]byte(`{"providers":[{"name":"One","url":"` + url + `","user":"u","password":"","port":8095,"live_tv":"ts"}]}`))
		s.keepSecrets(old)
		if got := s.Providers[0].Password; got != want {
			t.Errorf("%s: kept password %q, want %q", url, got, want)
		}
	}
}
