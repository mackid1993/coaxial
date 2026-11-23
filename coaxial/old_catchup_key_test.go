package main

import "testing"

// The older catch_up_while_live key is never an error, whatever it holds: only "refuse" turns
// one_per_channel on.
func TestOldCatchUpKeyAnyValue(t *testing.T) {
	for v, want := range map[string]bool{`"refuse"`: true, `true`: false, `false`: false, `1`: false, `null`: false, `{}`: false} {
		s := &settings{}
		err := s.parse([]byte(`{"player":{"user":"tv","password":"easy"},"providers":[{"name":"One","url":"http://provider.example",` +
			`"user":"u","password":"p","port":8095,"live_tv":"ts","catch_up_while_live":` + v + `}]}`))
		if err != nil {
			t.Errorf("catch_up_while_live %s: %v", v, err)
			continue
		}
		if got := s.Providers[0].Advanced.OnePerChannel; got != want {
			t.Errorf("catch_up_while_live %s: one_per_channel %v, want %v", v, got, want)
		}
	}
}
