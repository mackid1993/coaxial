package main

import "testing"

// gluetun runs the program named in VPN_UP_COMMAND (and the other ..._COMMAND variables) as root: from the
// web page, that would hand anyone on the network the settings file with every password in it.
func TestNoCommandVariables(t *testing.T) {
	for field, change := range map[string]func(*settings){
		"vpn.extra.VPN_UP_COMMAND":                   func(s *settings) { s.VPN.Extra["VPN_UP_COMMAND"] = "/bin/sh -c id" },
		"vpn.extra.VPN_DOWN_COMMAND":                 func(s *settings) { s.VPN.Extra["VPN_DOWN_COMMAND"] = "/bin/sh -c id" },
		"vpn.extra.VPN_PORT_FORWARDING_UP_COMMAND":   func(s *settings) { s.VPN.Extra["VPN_PORT_FORWARDING_UP_COMMAND"] = "/bin/sh -c id" },
		"vpn.extra.vpn_port_forwarding_down_command": func(s *settings) { s.VPN.Extra["vpn_port_forwarding_down_command"] = "/bin/sh -c id" },
		"vpn.vpn_up_command":                         func(s *settings) { s.VPN.Vars["vpn_up_command"] = "/bin/sh -c id" },
	} {
		s := goodSettings()
		change(s)
		if errs := s.validate(); len(errs) != 1 || errs[0].Field != field {
			t.Errorf("%s: got %v", field, errs)
		}
	}
}

// OpenVPN options that run a program are refused too; others pass.
func TestNoOpenVPNScripts(t *testing.T) {
	for _, flags := range []string{"--up /bin/sh", "--mute-replay-warnings --script-security 2", "--route-up=/x", "-up /x", "--CONFIG /tmp/x"} {
		s := goodSettings()
		s.VPN.Extra["OPENVPN_FLAGS"] = flags
		if errs := s.validate(); len(errs) != 1 || errs[0].Field != "vpn.extra.OPENVPN_FLAGS" {
			t.Errorf("%q: got %v", flags, errs)
		}
	}
	s := goodSettings()
	s.VPN.Extra["OPENVPN_FLAGS"] = "--mute-replay-warnings --sndbuf 0"
	if errs := s.validate(); len(errs) != 0 {
		t.Errorf("harmless flags were refused: %v", errs)
	}
}

// gluetun reads VPN_UP-COMMAND as VPN_UP_COMMAND, and VPN_UP_COMMAND_FILE from a file: each spelling is refused.
// So are a value on two lines (gluetun writes OPENVPN_AUTH and others into OpenVPN's configuration, where the
// second line is an option of its own) and moving gluetun's control page (it can set the commands too).
func TestNoCommandsOtherwiseSpelled(t *testing.T) {
	for field, change := range map[string]func(*settings){
		"vpn.extra.VPN_DOWN-COMMAND":                          func(s *settings) { s.VPN.Extra["VPN_DOWN-COMMAND"] = "/bin/sh -c id" },
		"vpn.extra.vpn-up-command":                            func(s *settings) { s.VPN.Extra["vpn-up-command"] = "/bin/sh -c id" },
		"vpn.vpn_up-command":                                  func(s *settings) { s.VPN.Vars["vpn_up-command"] = "/bin/sh -c id" },
		"vpn.extra.VPN_DOWN_COMMAND_FILE":                     func(s *settings) { s.VPN.Extra["VPN_DOWN_COMMAND_FILE"] = "/etc/openvpn/auth.conf" },
		"vpn.extra.VPN_PORT_FORWARDING_UP_COMMAND_SECRETFILE": func(s *settings) { s.VPN.Extra["VPN_PORT_FORWARDING_UP_COMMAND_SECRETFILE"] = "/x" },
		"vpn.extra.OPENVPN-FLAGS":                             func(s *settings) { s.VPN.Extra["OPENVPN-FLAGS"] = "--script-security 2 --up /bin/sh" },
		"vpn.extra.OPENVPN_FLAGS_FILE":                        func(s *settings) { s.VPN.Extra["OPENVPN_FLAGS_FILE"] = "/config/flags" },
		"vpn.extra.OPENVPN_AUTH":                              func(s *settings) { s.VPN.Extra["OPENVPN_AUTH"] = "sha256\nscript-security 2\nup /bin/sh" },
		"vpn.openvpn_user":                                    func(s *settings) { s.VPN.Vars["openvpn_user"] = "me\rup /bin/sh" },
		"vpn.extra.HTTP_CONTROL_SERVER_ADDRESS":               func(s *settings) { s.VPN.Extra["HTTP_CONTROL_SERVER_ADDRESS"] = ":8096" },
		"vpn.extra.http-control-server-auth-default-role":     func(s *settings) { s.VPN.Extra["http-control-server-auth-default-role"] = `{"auth":"none"}` },
		"vpn.extra.HTTP_CONTROL_SERVER_AUTH_CONFIG_FILEPATH":  func(s *settings) { s.VPN.Extra["HTTP_CONTROL_SERVER_AUTH_CONFIG_FILEPATH"] = "/x.toml" },
	} {
		s := goodSettings()
		change(s)
		if errs := s.validate(); len(errs) != 1 || errs[0].Field != field {
			t.Errorf("%s: got %v", field, errs)
		}
	}
	s := goodSettings() // what stays allowed: a certificate pasted over several lines, the control page's log
	s.VPN.Extra["OPENVPN_CERT"] = "MIIBszCCAVmgAwIBAgIU\nZm9vYmFy\n"
	s.VPN.Extra["HTTP_CONTROL_SERVER_LOG"] = "off"
	s.VPN.Extra["PUBLICIP_FILE"] = "/tmp/ip"
	if errs := s.validate(); len(errs) != 0 {
		t.Errorf("harmless settings were refused: %v", errs)
	}
}
