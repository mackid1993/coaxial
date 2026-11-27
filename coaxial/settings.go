// Settings: one file, settings.json, on the one folder the container is given.
//
//	{
//	  "player":    {"user": "", "password": ""},            the easy login typed into every player
//	  "providers": [{"name": "", "url": "", "user": "", "password": "", "port": 8095, "connections": 2,
//	                 "live_tv": "ts", "advanced": {...}}],     connections: how many the user bought from the provider;
//	                                                          advanced: what a provider needs handled (see advanced)
//	  "ui":        {"port": 8195, "log_level": "detailed"}, the web interface and the health check; how much the
//	                                                          log tells: detailed (also when not set), normal or quiet
//	  "vpn":       {"service_provider": "", "type": "wireguard", ..., "update_hours": 0, "gluetun_version": "latest",
//	                "extra": {"ANY_GLUETUN_VARIABLE": "value"}}
//	}
//
// Under "vpn", every key but "extra", "update_hours" and "gluetun_version" is a gluetun setting written in small letters (wireguard_private_key is
// gluetun's WIREGUARD_PRIVATE_KEY; service_provider and type are VPN_SERVICE_PROVIDER and VPN_TYPE). "extra"
// holds gluetun variables under their own names; it is applied last, so it wins. What would run a program, open
// gluetun's control page or not be one line is refused, here as under "vpn" (see validate).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultUIPort = 8195
	// gluetun's own periodic update of the VPN service's server list (its UPDATER_PERIOD), in hours. Off unless
	// set: gluetun downloads the list through the VPN on its timer whatever plays (rule 1 in AGENTS.md). The
	// page's "Refresh server list" button does it when asked.
	defaultUpdateHours = 0
)

// port: a port as written in the file or sent by the web page: a number, or text that should be one.
type port string

func (p *port) UnmarshalJSON(b []byte) error {
	*p = port(strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`)))
	if *p == "null" {
		*p = ""
	}
	return nil
}

func (p port) MarshalJSON() ([]byte, error) {
	if n, ok := p.number(); ok {
		return []byte(strconv.Itoa(n)), nil
	}
	return json.Marshal(string(p))
}

func (p port) String() string { return string(p) }

var portRe = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

func (p port) number() (int, bool) {
	if !portRe.MatchString(string(p)) {
		return 0, false
	}
	n, _ := strconv.Atoi(string(p))
	return n, n <= 65535
}

type player struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

type provider struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	User     string `json:"user"`
	Password string `json:"password"`
	Port     port   `json:"port"`
	// Connections: how many connections the user bought (typed in; never asked of the provider). Required when
	// the page saves; a file without it still loads, and then no limit is kept (see missingConnections).
	Connections number `json:"connections"`
	LiveTV      string `json:"live_tv"`
	// Advanced: the advanced options (advanced.go). Every one is off unless set, but stay_under_limit.
	Advanced advanced `json:"advanced"`
	// OldCatchUp: an older form of one_per_channel, read and then dropped: "refuse" turns one_per_channel on.
	OldCatchUp any `json:"catch_up_while_live,omitempty"` // (any value loads: it is never an error)
}

type uiSettings struct {
	Port     port   `json:"port"`
	LogLevel string `json:"log_level,omitempty"` // detailed ("" too), normal or quiet (see logging.go)
}

// vpnSettings: gluetun's settings. Vars: the friendly keys (small letters). Extra: gluetun's own names.
type vpnSettings struct {
	Vars, Extra map[string]string
}

type settings struct {
	Player    player      `json:"player"`
	Providers []provider  `json:"providers"`
	UI        uiSettings  `json:"ui"`
	VPN       vpnSettings `json:"vpn"`
}

func scalar(v any) (string, bool) { // a setting's value as text
	switch v := v.(type) {
	case nil:
		return "", true
	case string:
		return v, true
	case bool:
		return map[bool]string{true: "on", false: "off"}[v], true
	case json.Number:
		return v.String(), true
	}
	return "", false
}

func (v *vpnSettings) UnmarshalJSON(b []byte) error {
	v.Vars, v.Extra = map[string]string{}, map[string]string{}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var raw map[string]any
	if err := d.Decode(&raw); err != nil {
		return err
	}
	for k, x := range raw {
		switch {
		case k == "extra":
			m, ok := x.(map[string]any)
			if !ok && x != nil {
				return fmt.Errorf("vpn.extra must be a list of names and values")
			}
			for name, val := range m {
				s, ok := scalar(val)
				if !ok {
					return fmt.Errorf("vpn.extra.%s must be a single value", name)
				}
				v.Extra[name] = s
			}
		case k == "extra_set" || strings.HasSuffix(k, "_set"): // (the web page's notes, not settings)
		default:
			s, ok := scalar(x)
			if !ok {
				return fmt.Errorf("vpn.%s must be a single value", k)
			}
			v.Vars[k] = s
		}
	}
	return nil
}

func (v vpnSettings) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, s := range v.Vars {
		m[k] = s
	}
	extra := v.Extra
	if extra == nil {
		extra = map[string]string{}
	}
	m["extra"] = extra
	return json.Marshal(m)
}

func (s *settings) parse(data []byte) error {
	*s = settings{}
	if err := json.Unmarshal(data, s); err != nil {
		return err
	}
	if s.VPN.Vars == nil {
		s.VPN.Vars, s.VPN.Extra = map[string]string{}, map[string]string{}
	}
	if s.UI.Port == "" {
		// None saved (at the first start there is no file at all): the port Docker opens for the page, UI_PORT in
		// .env, which the compose file passes on as COAXIAL_UI_PORT; else 8195.
		s.UI.Port = port(strconv.Itoa(defaultUIPort))
		if n, ok := port(os.Getenv("COAXIAL_UI_PORT")).number(); ok && !ownPorts[n] {
			s.UI.Port = port(strconv.Itoa(n))
		}
	}
	if s.Providers == nil {
		s.Providers = []provider{}
	}
	for i := range s.Providers {
		p := &s.Providers[i]
		if p.OldCatchUp == "refuse" && !p.Advanced.channelSet {
			p.Advanced.OnePerChannel = true
		}
		p.OldCatchUp = nil // (written in the new form at the next save; any other value meant "allow")
		p.moveOldConnections()
		if p.Advanced.StayUnder == nil {
			on := true
			p.Advanced.StayUnder = &on
		}
	}
	return nil
}

// moveOldConnections: older settings kept the connections under "advanced", where 0 meant "no limit". A whole
// number from 1 to 999 there becomes the provider's "connections" (unless that is already set); 0 turns
// stay_under_limit off, as it had the same effect; anything else is dropped. It is written in the new place at
// the next save.
func (p *provider) moveOldConnections() {
	old := p.Advanced.OldConnections
	p.Advanced.OldConnections = ""
	if old == "" || p.Connections != "" {
		return
	}
	if v, ok := old.value(); ok && v == 0 {
		off := false
		p.Advanced.StayUnder = &off
	} else if _, ok := old.or(0, 1, 999); ok {
		p.Connections = old
	}
}

func emptySettings() *settings {
	s := &settings{}
	s.parse([]byte(`{"vpn":{"type":"wireguard"}}`))
	return s
}

// envName: gluetun's variable for a friendly key.
func envName(key string) string {
	switch key {
	case "service_provider":
		return "VPN_SERVICE_PROVIDER"
	case "type":
		return "VPN_TYPE"
	}
	return strings.ToUpper(key)
}

// updateHours: how often gluetun refreshes its list of the VPN service's servers ("update_hours" under
// "vpn"; 0 = never). Not set: never.
func (s *settings) updateHours() (int, bool) {
	v, set := s.VPN.Vars["update_hours"]
	if !set || v == "" {
		return defaultUpdateHours, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= 0 && n <= 8760
}

var versionRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// gluetunVersion: which gluetun program runs ("gluetun_version" under "vpn"): "latest" (also when not
// set), a tag of gluetun's image such as "v3.41.3", or "builtin" for the one in this image, never downloaded.
func (s *settings) gluetunVersion() (string, bool) {
	v := strings.TrimSpace(s.VPN.Vars["gluetun_version"])
	if v == "" {
		return "latest", true
	}
	return v, versionRe.MatchString(v)
}

// vpnEnv: gluetun's variables as the settings give them: the friendly keys first, then "extra" over them.
func (s *settings) vpnEnv() map[string]string {
	env := map[string]string{}
	if n, ok := s.updateHours(); ok {
		env["UPDATER_PERIOD"] = strconv.Itoa(n) + "h"
		if n == 0 {
			env["UPDATER_PERIOD"] = "0"
		}
	}
	keys := []string{}
	for k := range s.VPN.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys) // (two keys can name one variable, "type" and "vpn_type": the same one wins every time)
	for _, k := range keys {
		if v := s.VPN.Vars[k]; v != "" && k != "update_hours" && k != "gluetun_version" {
			env[envName(k)] = v
		}
	}
	for k, v := range s.VPN.Extra {
		env[k] = v
	}
	return env
}

var secretName = regexp.MustCompile(`(?i)(password|passphrase|secret|token|private_key|preshared_key|protection_key|^openvpn_(encrypted_)?key)$`)

// isSecret: a setting whose value is never shown again once saved (by its friendly or its gluetun name).
func isSecret(name string) bool { return secretName.MatchString(name) }

type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

var (
	easyRe     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,39}$`)
	providerRe = regexp.MustCompile(`^https?://[A-Za-z0-9._-]+(:[0-9]{1,5})?/?$`)
	hasControl = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// ownPorts: used inside the VPN container itself (its name service, its control page, its health check).
var ownPorts = map[int]bool{53: true, 8000: true, 9999: true}

// validate: what is wrong with the settings, each with the field it is about ("providers[0].url"). The same
// refusals, for the same reasons, that the old start script gave for its settings file.
func (s *settings) validate() []fieldError {
	var errs []fieldError
	bad := func(field, format string, a ...any) {
		errs = append(errs, fieldError{field, fmt.Sprintf(format, a...)})
	}
	if !easyRe.MatchString(s.Player.User) {
		bad("player.user", "The player username must be letters, digits, - or _ only (it is yours to choose)")
	} else if u := s.Player.User; u == "live" || u == "movie" || u == "series" || u == "timeshift" {
		bad("player.user", "The player username cannot be live, movie, series or timeshift")
	}
	if !easyRe.MatchString(s.Player.Password) {
		bad("player.password", "The player password must be letters, digits, - or _ only (it is yours to choose)")
	}
	ui, uiOK := s.UI.Port.number()
	if !uiOK {
		bad("ui.port", "port must be a number from 1 to 65535")
	} else if ownPorts[ui] {
		bad("ui.port", "port %d is used inside the VPN container itself", ui)
	}
	if !validLogLevel(s.UI.LogLevel) {
		bad("ui.log_level", "log detail must be detailed, normal or quiet")
	}
	used, names := map[int]bool{}, map[string]bool{}
	for i, p := range s.Providers {
		at := "providers[" + strconv.Itoa(i) + "]."
		n, portOK := p.Port.number()
		switch {
		case !nameRe.MatchString(p.Name):
			bad(at+"name", "a provider needs a name: letters, digits, spaces, - . or _ (40 at most)")
		case names[strings.ToLower(p.Name)]:
			bad(at+"name", "the name %s is already used by another provider", p.Name)
		}
		names[strings.ToLower(p.Name)] = true
		for _, f := range [][2]string{{"url", p.URL}, {"port", p.Port.String()}, {"user", p.User}, {"password", p.Password}} {
			if f[1] == "" {
				bad(at+f[0], "a provider needs an address, a port, a username and a password")
			}
		}
		switch {
		case p.Port == "":
		case !portOK:
			bad(at+"port", "port must be a number from 1 to 65535")
		case ownPorts[n] || (uiOK && n == ui):
			bad(at+"port", "port %d is used inside the VPN container itself", n)
		case used[n]:
			bad(at+"port", "port %d is already used by another provider", n)
		default:
			used[n] = true
		}
		switch m := providerRe.FindStringSubmatch(p.URL); {
		case p.URL != "" && m == nil:
			bad(at+"url", "address must be http://name or https://name, optionally :port, with nothing after it")
		case m != nil && m[1] != "" && (atoi(m[1][1:]) < 1 || atoi(m[1][1:]) > 65535):
			// (Go takes :99999 or :0 in an address, and then cannot connect to it: every request would fail)
			bad(at+"url", "the port in the address must be a number from 1 to 65535")
		}
		if _, ok := p.Connections.or(0, 1, 999); !ok {
			bad(at+"connections", "connections must be a whole number from 1 to 999: how many connections you bought")
		}
		if p.LiveTV != "hls" && p.LiveTV != "ts" {
			bad(at+"live_tv", `live_tv must be "hls" if your players use HLS, or "ts" if they use MPEG-TS`)
		}
		for _, e := range p.Advanced.check() {
			bad(at+"advanced."+e.Field, "%s", e.Message)
		}
		for _, f := range [][2]string{{"user", p.User}, {"password", p.Password}} {
			if hasControl.MatchString(f[1]) {
				bad(at+f[0], "its username or password is broken over two lines")
			} else if strings.Contains(f[1], "/") {
				bad(at+f[0], "its username or password contains a slash (/), which cannot be passed on inside an address")
			}
		}
	}
	if _, ok := s.updateHours(); !ok {
		bad("vpn.update_hours", "update_hours must be a whole number of hours (0 = never)")
	}
	if _, ok := s.gluetunVersion(); !ok {
		bad("vpn.gluetun_version", `gluetun_version must be "latest", "builtin" or a version of gluetun's image such as v3.41.3`)
	}
	if s.vpnEnv()["VPN_SERVICE_PROVIDER"] == "" {
		bad("vpn.service_provider", "Pick your VPN service")
	}
	for k, v := range s.VPN.Vars {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			bad("vpn."+k, "that is not a setting's name")
		} else if strings.Contains(v, "\x00") {
			bad("vpn."+k, "that cannot be a variable's value")
		}
	}
	for k, v := range s.VPN.Extra { // (any name gluetun may know is passed on; only what cannot be a variable is refused)
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.Contains(v, "\x00") {
			bad("vpn.extra."+k, "that cannot be a variable's name")
		}
	}
	// gluetun runs the program named in a ..._COMMAND variable (VPN_UP_COMMAND and the like) as root, where
	// every password in this file can be read. The web page asks nobody who they are, so none is passed on.
	// OpenVPN can be told to run programs too, through options passed in OPENVPN_FLAGS.
	// gluetun reads a name in capitals with - as _ (VPN_UP-COMMAND is VPN_UP_COMMAND), and takes the value
	// from the file named in NAME_FILE or NAME_SECRETFILE: every spelling is refused alike.
	gluetunName := func(name string) (base string, fromFile bool) {
		base = strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		for _, suffix := range []string{"_SECRETFILE", "_FILE"} {
			if strings.HasSuffix(base, suffix) {
				return strings.TrimSuffix(base, suffix), true
			}
		}
		return base, false
	}
	scripts := map[string]bool{"--up": true, "--down": true, "--route-up": true, "--route-pre-down": true, "--ipchange": true,
		"--tls-verify": true, "--client-connect": true, "--client-disconnect": true, "--learn-address": true,
		"--auth-user-pass-verify": true, "--plugin": true, "--script-security": true, "--config": true, "--tls-crypt-v2-verify": true}
	runs := func(name, value string) bool {
		if base, fromFile := gluetunName(name); base != "OPENVPN_FLAGS" {
			return false
		} else if fromFile { // (what the file holds is not known here)
			return true
		}
		for _, f := range strings.Fields(value) {
			f, _, _ = strings.Cut(strings.ToLower(f), "=")
			if scripts[f] || scripts["--"+strings.TrimLeft(f, "-")] {
				return true
			}
		}
		return false
	}
	for k, v := range s.VPN.Vars {
		if runs(k, v) {
			bad("vpn."+k, "Coaxial does not pass on OpenVPN options that run programs (such as --up): the program would run with every password in the settings in reach")
		}
	}
	for k, v := range s.VPN.Extra {
		if runs(k, v) {
			bad("vpn.extra."+k, "Coaxial does not pass on OpenVPN options that run programs (such as --up): the program would run with every password in the settings in reach")
		}
	}
	command := func(name string) bool { base, _ := gluetunName(name); return strings.HasSuffix(base, "_COMMAND") }
	for k := range s.VPN.Vars {
		if command(k) {
			bad("vpn."+k, "Coaxial does not pass on gluetun's ..._COMMAND variables: the program named would run with every password in the settings in reach")
		}
	}
	for k := range s.VPN.Extra {
		if command(k) {
			bad("vpn.extra."+k, "Coaxial does not pass on gluetun's ..._COMMAND variables: the program named would run with every password in the settings in reach")
		}
	}
	// gluetun's control page can set those same commands and shows the VPN's key. Coaxial keeps it inside the
	// container, with a key of its own; moved to a published port or opened up, anyone could use it.
	// And gluetun writes some values (OPENVPN_AUTH, OPENVPN_CIPHERS, ...) into OpenVPN's configuration as
	// lines: a value broken over two lines would add an option of its own (script-security, up). Only the
	// certificate and key, which gluetun checks are base64, may be.
	for field, vars := range map[string]map[string]string{"vpn.": s.VPN.Vars, "vpn.extra.": s.VPN.Extra} {
		for k, v := range vars {
			switch base, _ := gluetunName(envName(k)); {
			case strings.HasPrefix(base, "HTTP_CONTROL_SERVER_") && base != "HTTP_CONTROL_SERVER_LOG":
				bad(field+k, "Coaxial uses gluetun's control page itself and keeps it inside the container: it is not set here")
			case strings.ContainsAny(v, "\r\n") && base != "OPENVPN_CERT" && base != "OPENVPN_KEY" && base != "OPENVPN_ENCRYPTED_KEY":
				bad(field+k, "a VPN setting must be on one line")
			}
		}
	}
	return errs
}

// missingConnections: every provider with no number of connections typed in. The page does not save one (the
// limit could not be kept); a file that lacks it still loads, so these are not part of validate.
func (s *settings) missingConnections() []fieldError {
	var errs []fieldError
	for i, p := range s.Providers {
		if p.Connections == "" {
			errs = append(errs, fieldError{"providers[" + strconv.Itoa(i) + "].connections", "Enter how many connections you bought"})
		}
	}
	return errs
}

// masked: the settings for the web page. Every password and key is left out (""), with a note next to it
// saying whether one is saved ("password_set": true).
func (s *settings) masked() map[string]any {
	provs := []any{}
	for _, p := range s.Providers {
		provs = append(provs, map[string]any{"name": p.Name, "url": p.URL, "user": p.User, "password": "", "password_set": p.Password != "",
			"port": p.Port, "connections": p.Connections, "live_tv": p.LiveTV, "advanced": p.Advanced.view()})
	}
	hours, _ := s.updateHours()
	tag, _ := s.gluetunVersion()
	vpn := map[string]any{"wireguard_private_key": "", "wireguard_private_key_set": false, "update_hours": hours, "gluetun_version": tag}
	for k, v := range s.VPN.Vars {
		if k == "update_hours" || k == "gluetun_version" {
			continue
		}
		if isSecret(k) {
			vpn[k], vpn[k+"_set"] = "", v != ""
		} else {
			vpn[k] = v
		}
	}
	extra, extraSet := map[string]any{}, map[string]any{}
	for k, v := range s.VPN.Extra {
		if isSecret(k) {
			extra[k], extraSet[k] = "", v != ""
		} else {
			extra[k] = v
		}
	}
	vpn["extra"], vpn["extra_set"] = extra, extraSet
	return map[string]any{
		"player":    map[string]any{"user": s.Player.User, "password": "", "password_set": s.Player.Password != ""},
		"providers": provs,
		"ui":        map[string]any{"port": s.UI.Port, "log_level": or(s.UI.LogLevel, logDetailed)},
		"vpn":       vpn,
	}
}

// sameServer: two provider addresses that name the same server (whatever the scheme, the case or a final slash).
func sameServer(a, b string) bool {
	host := func(u string) string {
		if i := strings.Index(u, "://"); i >= 0 {
			u = u[i+3:]
		}
		return strings.ToLower(strings.TrimRight(u, "/"))
	}
	return host(a) == host(b)
}

// keepSecrets: a password or key sent empty means "keep the one that is saved". A provider is matched to
// its saved self by name, or else by address and username (it may have been renamed or removed). A saved
// password is kept only for the server it was saved for: with another address it has to be typed again
// (the web interface asks nobody who they are, so otherwise anyone could have a saved password sent to a
// server of their own by changing the address and leaving the password empty).
func (s *settings) keepSecrets(old *settings) {
	if old == nil {
		return
	}
	if s.Player.Password == "" {
		s.Player.Password = old.Player.Password
	}
	for i := range s.Providers {
		p := &s.Providers[i]
		if p.Password != "" {
			continue
		}
		for _, o := range old.Providers {
			if strings.EqualFold(o.Name, p.Name) && sameServer(o.URL, p.URL) {
				p.Password = o.Password
			}
		}
		for _, o := range old.Providers {
			if p.Password == "" && sameServer(o.URL, p.URL) && o.User == p.User {
				p.Password = o.Password
			}
		}
	}
	for k, v := range s.VPN.Vars {
		if v == "" && isSecret(k) {
			s.VPN.Vars[k] = old.VPN.Vars[k]
		}
	}
	// A VPN setting the page does not know and so did not send (written into the file by hand) is kept too.
	// Sent empty, it is cleared.
	for k, v := range old.VPN.Vars {
		if _, sent := s.VPN.Vars[k]; !sent {
			s.VPN.Vars[k] = v
		}
	}
	for k, v := range s.VPN.Extra {
		if v == "" && isSecret(k) && old.VPN.Extra[k] != "" {
			s.VPN.Extra[k] = old.VPN.Extra[k]
		}
	}
}

// whyNoPassword: a provider still without a password after keepSecrets because its address changed is told
// so, instead of only being asked for one (the page shows the saved password as "Saved").
func (s *settings) whyNoPassword(old *settings, errs []fieldError) {
	if old == nil {
		return
	}
	for i, p := range s.Providers {
		if p.Password != "" {
			continue
		}
		for _, o := range old.Providers {
			if o.Password != "" && !sameServer(o.URL, p.URL) && (strings.EqualFold(o.Name, p.Name) || o.User == p.User) {
				for j := range errs {
					if errs[j].Field == "providers["+strconv.Itoa(i)+"].password" {
						errs[j].Message = "the address changed, so the saved password is not sent to it: type the password again"
					}
				}
			}
		}
	}
}

// addressLocked: whether any provider locks its account to one address (locked_to_address): the VPN server
// is then never changed on Coaxial's own.
func (s *settings) addressLocked() bool {
	for _, p := range s.Providers {
		if p.Advanced.Locked {
			return true
		}
	}
	return false
}

// forProxy: what the proxy part is given: no VPN setting at all. The log detail is left out here, so a change of it
// alone never starts the proxy part again (it is applied while it runs; see proxyInput).
func (s *settings) forProxy() []byte {
	c := *s
	c.VPN = vpnSettings{}
	c.UI.LogLevel = ""
	data, _ := json.Marshal(&c)
	return data
}

// proxyInput: forProxy with the log detail, as the proxy part is started with it.
func (s *settings) proxyInput() []byte {
	c := *s
	c.VPN = vpnSettings{}
	data, _ := json.Marshal(&c)
	return data
}

func loadSettings(file string) (*settings, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	s := &settings{}
	if err := s.parse(data); err != nil {
		return nil, err
	}
	return s, nil
}

// save writes the file whole or not at all: a new file beside it, then renamed over it. Only its owner may
// read it (it holds logins and the VPN's key).
func (s *settings) save(file string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // (a password is written as it is, so the file can be read and edited by hand)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	data := buf.Bytes()
	tmp, err := os.CreateTemp(filepath.Dir(file), ".settings-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// The sample written next to a missing settings.json. Every value in it is made up.
const sampleSettings = `{
  "player": {
    "user": "tv",
    "password": "choose-your-own"
  },
  "providers": [
    {
      "name": "Example",
      "url": "https://provider.example",
      "user": "the-username-your-provider-gave-you",
      "password": "the-password-your-provider-gave-you",
      "port": 8095,
      "connections": 1,
      "live_tv": "ts"
    }
  ],
  "ui": {
    "port": 8195
  },
  "vpn": {
    "service_provider": "mullvad",
    "type": "wireguard",
    "wireguard_private_key": "the-private-key-from-your-vpn-account",
    "wireguard_addresses": "10.64.0.2/32",
    "server_countries": "",
    "server_cities": "",
    "update_hours": 0,
    "gluetun_version": "latest",
    "extra": {
      "TZ": "America/New_York"
    }
  }
}
`

// or: a, or b when a is empty.
func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
