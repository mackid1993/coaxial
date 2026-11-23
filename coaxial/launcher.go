// The launcher: the container's first process. It reads settings.json, starts gluetun (the VPN) with its
// settings, starts the proxy part once the VPN is up, and serves the web interface. The proxy part is a
// second copy of this program, so it can be started again by itself: saving new provider settings does not
// drop the VPN. It never runs unless the VPN of this run has come up, and it is stopped before the VPN is.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	gluetun      = "/gluetun-entrypoint" // the VPN image's own program (its ENTRYPOINT)
	insideSocket = "@coaxial-proxy"      // where the launcher asks the proxy part what is playing (never on the network)
)

//go:embed ui
var uiFiles embed.FS

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "":
		os.Exit(runLauncher())
	case "proxy":
		stop, sig := make(chan struct{}), make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		go func() { <-sig; close(stop) }()
		os.Exit(runProxy(os.Stdin, stop))
	case "healthcheck":
		os.Exit(healthcheck())
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, "usage: coaxial [proxy | healthcheck | version]")
		os.Exit(2)
	}
}

// child: a program the launcher started.
type child struct {
	cmd     *exec.Cmd
	done    chan struct{}
	stopped bool   // stopped on purpose
	ever    bool   // (the VPN) came up at least once
	prog    string // (the VPN) the build it runs (its digest, "" for the built-in one)
}

type launcher struct {
	dir, file string
	work      sync.Mutex // one change at a time
	quit      chan int

	mu               sync.Mutex // everything below
	set              *settings  // nil: no usable settings.json yet
	problem          string     // why not
	vpn, proxy       *child
	vpnKey, proxyKey string // the settings each was started with
	vpnEnv           []string
	ctlKey           string // for gluetun's control page, made up at each start of the VPN ("" = set by the user: not ours to use)
	vpnUp            bool
	vpnUpAt          time.Time // when the VPN last came up (handed to the proxy part: see locked_to_address)
	limits           string    // the connections each provider account allows, as the proxy part learned them (see learned)
	vpnLink          int       // the index of the VPN's interface when last seen up (0: not known)
	vpnNote          string
	ui               *http.Server
	uiPort           int
	prog             program   // the VPN program in use (gluetunfetch.go)
	progAsked        time.Time // when the registry was last asked for it, for which tag, and what came of it
	progTag          string
	progDigest       string
	progOK           bool
	updDigest        string // the newest program the registry names (checked hourly), when, and what went wrong
	updAt            time.Time
	updErr           string
	updFailed        string // the newer program an update could not fetch: not tried again by itself
	pick             picked // the VPN server chosen by trying them (pick.go), when, and the one being tried now
	pickAt           time.Time
	checkMbps        float64   // the server in use, as last measured by the quiet-hour check (pick.go)
	checkAt          time.Time // when (zero: not measured since this start)
	pickTry          string
	pickLoaded       bool
	pickBusy         bool
}

func configDir() string {
	if d := os.Getenv("COAXIAL_CONFIG"); d != "" {
		return d
	}
	return "/config"
}

func runLauncher() int {
	l := &launcher{dir: configDir(), quit: make(chan int, 4), prog: builtinProgram}
	l.file = filepath.Join(l.dir, "settings.json")
	say("Coaxial %s", version)
	os.MkdirAll(l.dir, 0o755)
	s, err := loadSettings(l.file)
	switch {
	case os.IsNotExist(err):
		sample := filepath.Join(l.dir, "settings.sample.json")
		if _, e := os.Stat(sample); e != nil {
			if e := os.WriteFile(sample, []byte(sampleSettings), 0o644); e != nil {
				say("could not write %s: %v", sample, e)
			}
		}
		l.problem = "There is no settings.json yet"
		say("no settings.json yet: fill the settings in on the web interface, or copy settings.sample.json to settings.json and edit it")
	case err != nil:
		l.problem = "settings.json could not be read: " + err.Error()
		say("SETTINGS PROBLEM: %s", l.problem)
	default:
		l.set = s
		for _, e := range s.validate() {
			say("SETTINGS PROBLEM: %s: %s", e.Field, e.Message)
		}
	}
	l.uiPort = defaultUIPort
	at := s
	if at == nil {
		at = emptySettings() // (no usable settings.json: the port Docker opens for the page, see parse)
	}
	if n, ok := at.UI.Port.number(); ok && !ownPorts[n] {
		l.uiPort = n
	}
	if err := l.listenUI(l.uiPort); err != nil {
		say("the web interface could not open port %d: %v", l.uiPort, err)
		return 1
	}
	say("WEB INTERFACE: open http://<this machine's address>:%d in a web browser", l.uiPort)
	go l.apply()
	go l.watchVPN()
	go l.watchProgram()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	code := 0
	select {
	case <-sig:
		say("told to stop")
	case code = <-l.quit:
	}
	l.work.Lock() // (kept: nothing is started again)
	l.stopProxy()
	l.stopVPN()
	return code
}

// gluetunEnv: gluetun's environment: the image's own defaults, two of this program's, then the settings.
func (l *launcher) gluetunEnv(s *settings) (env []string, key string, names []string) {
	set := map[string]string{
		"HTTP_CONTROL_SERVER_ADDRESS": "127.0.0.1:8000", // gluetun's own control page stays inside the container
		// gluetun's server lists, and the updates it makes to them, are kept in the settings folder
		"STORAGE_SERVERS_DIRECTORY_PATH": filepath.Join(l.dir, "gluetun", "servers") + "/",
	}
	for k, v := range s.vpnEnv() {
		set[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, mine := set[k]; !mine && !strings.HasPrefix(k, "COAXIAL_") {
			env = append(env, kv)
		}
	}
	var keys []string
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+set[k])
		key += k + "=" + set[k] + "\n"
	}
	tag, _ := s.gluetunVersion()
	key += "gluetun_version=" + tag + "\n" // (not one of gluetun's variables, but a change of it starts the VPN again)
	return env, key, names
}

// apply brings what runs in line with the settings: the VPN is started again only if its own settings
// changed; the proxy part only if the player or provider settings did.
func (l *launcher) apply() {
	l.work.Lock()
	defer l.work.Unlock()
	l.mu.Lock()
	s := l.set
	l.mu.Unlock()
	want := s != nil
	if want {
		for _, e := range s.validate() {
			want = want && !strings.HasPrefix(e.Field, "vpn.")
		}
	}
	var env, names []string
	key, pkey := "", ""
	if want {
		env, key, names = l.gluetunEnv(s)
		pkey = string(s.forProxy())
		if server := l.pickOverride(s); server != "" { // the server chosen by trying them, or the one being tried
			env = append(env, server)
			key += "picked " + server + "\n"
		}
	}
	l.mu.Lock()
	vpnSame := key == l.vpnKey && (l.vpn != nil || !want)
	proxySame := pkey == l.proxyKey
	l.mu.Unlock()
	if !vpnSame {
		if want {
			tag, _ := s.gluetunVersion()
			l.askBeforeStop(tag)
		}
		l.stopProxy()
		l.stopVPN()
		l.mu.Lock()
		l.vpnKey = key
		l.mu.Unlock()
		if want {
			tag, _ := s.gluetunVersion()
			l.startVPN(env, names, tag)
		}
	}
	if !proxySame {
		l.stopProxy()
		l.mu.Lock()
		l.proxyKey = pkey
		l.mu.Unlock()
	}
	l.startProxy()
}

func (l *launcher) startVPN(env, names []string, tag string) {
	os.MkdirAll(filepath.Join(l.dir, "gluetun"), 0o755)
	prog := l.chooseProgram(tag)
	// gluetun's control page listens inside the container only and wants a key for every request. A new key
	// is made up at each start and known only here (it is how "refresh the server list now" is asked for).
	ctlKey := ""
	mine := true
	for _, kv := range env {
		if strings.HasPrefix(kv, "HTTP_CONTROL_SERVER_AUTH_DEFAULT_ROLE=") && kv != "HTTP_CONTROL_SERVER_AUTH_DEFAULT_ROLE={}" ||
			strings.HasPrefix(kv, "HTTP_CONTROL_SERVER_ADDRESS=") && kv != "HTTP_CONTROL_SERVER_ADDRESS=127.0.0.1:8000" {
			mine = false // set otherwise in the settings: left alone
		}
	}
	if raw := make([]byte, 24); mine {
		if _, err := rand.Read(raw); err == nil {
			ctlKey = hex.EncodeToString(raw)
			role, _ := json.Marshal(map[string]string{"auth": "apikey", "apikey": ctlKey})
			var kept []string
			for _, kv := range env {
				if !strings.HasPrefix(kv, "HTTP_CONTROL_SERVER_AUTH_DEFAULT_ROLE=") {
					kept = append(kept, kv)
				}
			}
			env = append(kept, "HTTP_CONTROL_SERVER_AUTH_DEFAULT_ROLE="+string(role))
		}
	}
	cmd := exec.Command(prog.Path)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // (what it starts stays in its session: see endSession)
	say("starting the VPN (gluetun); set from settings.json: %s", strings.Join(names, " "))
	c := &child{cmd: cmd, done: make(chan struct{}), prog: prog.Digest}
	if err := cmd.Start(); err != nil {
		say("the VPN program could not be started: %v", err)
		l.mu.Lock()
		l.vpnNote = "The VPN program could not be started"
		l.mu.Unlock()
		return
	}
	l.mu.Lock()
	l.vpn, l.vpnEnv, l.vpnUp, l.vpnNote, l.ctlKey = c, env, false, "", ctlKey
	l.mu.Unlock()
	go func() {
		err := cmd.Wait()
		endSession(cmd.Process.Pid)
		close(c.done)
		l.mu.Lock()
		onPurpose, ever := c.stopped, c.ever
		if l.vpn == c {
			l.vpn, l.vpnUp = nil, false
		}
		if !onPurpose && !ever {
			l.vpnNote = "The VPN program stopped before it connected. Check the VPN settings; the container log has its reason"
		}
		l.mu.Unlock()
		switch {
		case onPurpose:
		case ever: // the VPN is gone: stop everything, so the container is started again whole
			say("the VPN program stopped (%v); stopping", err)
			l.quit <- 1
		default:
			// It never connected in this run (most often a setting gluetun refused). The web interface stays up
			// so the setting can be put right; nothing else runs. Tried again in 30 s, or when settings are saved.
			say("the VPN program stopped before it connected (%v); check the VPN settings. Trying again in 30 s", err)
			time.AfterFunc(30*time.Second, l.apply)
		}
	}()
}

// endSession: what the VPN program started and left running when it ended is stopped and collected. gluetun
// starts openvpn (in a process group of its own, so only the session finds it). When gluetun crashes or is
// killed, openvpn ran on beside the next one, started 30 s later, and once it ended it stayed a zombie for
// good: this program is the container's first process, and nothing else collects what is left to it.
func endSession(sid int) {
	for try := 0; try < 100; try++ {
		left := 0
		list, _ := os.ReadDir("/proc")
		for _, e := range list {
			pid, err := strconv.Atoi(e.Name())
			data, _ := os.ReadFile("/proc/" + e.Name() + "/stat")
			if err != nil || len(data) == 0 {
				continue
			}
			// after the name (in brackets, and it may hold anything): state, parent, group, session
			f := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
			if len(f) < 4 || f[3] != strconv.Itoa(sid) {
				continue
			}
			left++
			syscall.Kill(pid, syscall.SIGKILL)
			syscall.Wait4(pid, nil, syscall.WNOHANG, nil) // (collected here when it was left to this program)
		}
		if left == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *launcher) stop(c *child, wait time.Duration) {
	if c == nil {
		return
	}
	l.mu.Lock()
	c.stopped = true
	l.mu.Unlock()
	c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(wait):
		c.cmd.Process.Kill()
		<-c.done
	}
}

func (l *launcher) stopVPN() {
	l.mu.Lock()
	c := l.vpn
	l.mu.Unlock()
	if c != nil {
		say("stopping the VPN")
		l.stop(c, 8*time.Second)
	}
}

func (l *launcher) stopProxy() {
	l.mu.Lock()
	c := l.proxy
	l.proxy = nil
	l.mu.Unlock()
	if c != nil {
		if data, ok := askProxy("/limits", 2*time.Second); ok { // (kept for its next start)
			l.mu.Lock()
			l.limits = learned(l.limits + "," + string(data)) // (the newer value wins; the launcher runs no provider itself)
			l.mu.Unlock()
		}
		say("stopping the proxy part")
		l.stop(c, 3*time.Second)
	}
}

// startProxy starts the proxy part if it should run and does not: only while the VPN of this run is up.
// (l.work held)
func (l *launcher) startProxy() {
	l.mu.Lock()
	ready, input := l.proxy == nil && l.proxyKey != "" && l.vpnUp, l.proxyKey
	l.mu.Unlock()
	if !ready {
		return
	}
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self, "proxy")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(input), os.Stdout, os.Stderr
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GOMEMLIMIT=384MiB"} // (no VPN key, no settings: it is handed what it needs)
	l.mu.Lock()
	if !l.vpnUpAt.IsZero() {
		cmd.Env = append(cmd.Env, "COAXIAL_VPN_UP_AT="+strconv.FormatInt(l.vpnUpAt.Unix(), 10))
	}
	if l.limits != "" {
		cmd.Env = append(cmd.Env, "COAXIAL_LIMITS="+l.limits)
	}
	l.mu.Unlock()
	if tz := os.Getenv("TZ"); tz != "" {
		cmd.Env = append(cmd.Env, "TZ="+tz)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if os.Getuid() == 0 { // it needs no privileges
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 65534, Gid: 65534}
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	endTries() // (a provider's port tried by the page a moment ago)
	say("starting the proxy part")
	if err := cmd.Start(); err != nil {
		say("the proxy part could not be started: %v", err)
		return
	}
	l.mu.Lock()
	l.proxy = c
	l.mu.Unlock()
	go func() {
		err := cmd.Wait()
		close(c.done)
		l.mu.Lock()
		onPurpose := c.stopped
		if l.proxy == c {
			l.proxy = nil
		}
		l.mu.Unlock()
		if !onPurpose {
			say("the proxy part stopped by itself (%v); starting it again", err)
			time.AfterFunc(2*time.Second, func() { l.work.Lock(); defer l.work.Unlock(); l.startProxy() })
		}
	}()
}

// watchVPN asks gluetun's own health check whether the tunnel is up, and starts the proxy part when it is.
func (l *launcher) watchVPN() {
	for {
		l.mu.Lock()
		c, env, prog := l.vpn, l.vpnEnv, l.prog.Path
		l.mu.Unlock()
		up := false
		if c != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cmd := exec.CommandContext(ctx, prog, "healthcheck") // (the same program that runs)
			cmd.Env = env
			up = cmd.Run() == nil
			cancel()
		}
		link := 0
		if up {
			link = tunnelLink()
		}
		if again := l.sawVPN(c, up, link); again { // (back up under the proxy part: its address may be new. A proxy part started now is told at its start)
			askProxy("/vpn-changed", 3*time.Second)
		}
		if up {
			l.work.Lock()
			l.startProxy()
			l.work.Unlock()
			time.Sleep(8 * time.Second)
		}
		time.Sleep(2 * time.Second)
	}
}

// sawVPN notes one look at the VPN of child c: up, and the index of its interface (0: not known). It returns
// whether the proxy part must be told the VPN came back up, its address perhaps new. gluetun makes the interface
// anew each time it connects, also when it reconnects by itself (its own health check failed, and with no
// server named it takes another one) within the few seconds between two looks here: a new index is that.
func (l *launcher) sawVPN(c *child, up bool, link int) (again bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.vpn != c {
		return false
	}
	if up && !l.vpnUp {
		say("the VPN is up")
		l.vpnUpAt, again = time.Now(), l.proxy != nil
	} else if up && link != 0 && l.vpnLink != 0 && link != l.vpnLink {
		l.vpnUpAt, again = time.Now(), l.proxy != nil
	} else if !up && l.vpnUp {
		say("the VPN is down; gluetun lets nothing out until it is back")
	}
	l.vpnUp = up
	if up {
		l.vpnLink = link
		if c != nil {
			c.ever = true
		}
	}
	return again
}

// tunnelLink: the index of the VPN's own interface (a point-to-point one that is up), 0 while there is none.
func tunnelLink() int {
	list, _ := net.Interfaces()
	for _, i := range list {
		if i.Flags&net.FlagPointToPoint != 0 && i.Flags&net.FlagUp != 0 {
			return i.Index
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// The web interface: the page (embedded files) and its API
// ---------------------------------------------------------------------------

func (l *launcher) listenUI(n int) error {
	endTries() // (the page tries its new port just before it saves)
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(n))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", l.healthz)
	mux.HandleFunc("/api/settings", l.apiSettings)
	mux.HandleFunc("/api/status", l.apiStatus)
	mux.HandleFunc("/api/vpn/options", l.apiOptions)
	mux.HandleFunc("/api/vpn/servers", l.apiServers)
	mux.HandleFunc("/api/vpn/server/pick", l.apiPick)
	mux.HandleFunc("/api/vpn/refresh", l.apiRefresh)
	mux.HandleFunc("/api/vpn/program/check", l.apiProgramCheck)
	mux.HandleFunc("/api/vpn/program/update", l.apiProgramUpdate)
	mux.HandleFunc("/api/port/try", apiPortTry)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 404, map[string]any{"ok": false}) })
	page, _ := fs.Sub(uiFiles, "ui")
	files := http.FileServer(http.FS(page))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: sameSite(mux), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	l.mu.Lock()
	old := l.ui
	l.ui, l.uiPort = srv, n
	l.mu.Unlock()
	go srv.Serve(ln)
	if old != nil {
		time.AfterFunc(time.Second, func() { old.Close() })
	}
	return nil
}

// sameSite: what changes something (any request but GET and HEAD) is taken from this page only. A browser
// names the site a request comes from, so another web site open in a browser on the network cannot press
// the page's buttons (restart the VPN, try servers, open ports). Programs such as curl name none and pass.
func sameSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refuse := false
		switch site := r.Header.Get("Sec-Fetch-Site"); {
		case r.Method == "GET" || r.Method == "HEAD" || site == "same-origin" || site == "none":
		case site != "":
			refuse = true // cross-site or same-site
		default: // (an older browser says only where the page is: Origin)
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				refuse = err != nil || u.Host != r.Host
			}
		}
		if refuse {
			writeJSON(w, 403, map[string]any{"ok": false, "message": "Only Coaxial's own page can change it"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	w.Write(data)
}

// askProxy asks the proxy part (inside the container, over its own socket).
func askProxy(path string, wait time.Duration) ([]byte, bool) {
	client := &http.Client{Timeout: wait, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", insideSocket)
		}}}
	r, err := client.Get("http://proxy" + path)
	if err != nil {
		return nil, false
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	return data, err == nil && r.StatusCode == 200
}

func (l *launcher) healthz(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	why := ""
	switch {
	case l.set == nil:
		why = "not set up"
	case l.vpn == nil || !l.vpnUp:
		why = "the VPN is not up"
	case l.proxy == nil:
		why = "the proxy part is not running"
	}
	l.mu.Unlock()
	if why == "" {
		if data, ok := askProxy("/healthz", 4*time.Second); !ok {
			why = "the proxy part does not answer"
		} else if strings.TrimSpace(string(data)) != "ok" {
			why = strings.TrimSpace(string(data))
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	if why != "" {
		w.WriteHeader(503)
		io.WriteString(w, why+"\n")
		return
	}
	io.WriteString(w, "ok\n")
}

// healthcheck: the image's HEALTHCHECK. Healthy = set up, the VPN up, the proxy part answering.
func healthcheck() int {
	n := defaultUIPort
	s, err := loadSettings(filepath.Join(configDir(), "settings.json"))
	if err != nil {
		s = emptySettings() // (as runLauncher)
	}
	if p, ok := s.UI.Port.number(); ok && !ownPorts[p] {
		n = p
	}
	client := &http.Client{Timeout: 8 * time.Second}
	r, err := client.Get("http://127.0.0.1:" + strconv.Itoa(n) + "/healthz")
	if err != nil {
		fmt.Println("the web interface does not answer")
		return 1
	}
	defer r.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
	fmt.Print(string(data))
	if r.StatusCode != 200 {
		return 1
	}
	return 0
}

func (l *launcher) apiSettings(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	old := l.set
	l.mu.Unlock()
	switch r.Method {
	case "GET", "HEAD":
		m := emptySettings().masked()
		if old != nil {
			m = old.masked()
		} else if _, err := os.Stat(l.file); err == nil { // a settings.json is there that could not be read: saving replaces it
			l.mu.Lock()
			m["problem"] = l.problem
			l.mu.Unlock()
		}
		writeJSON(w, 200, m)
	case "PUT":
		fail := func(status int, errs ...fieldError) {
			writeJSON(w, status, map[string]any{"ok": false, "errors": errs})
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s := &settings{}
		if err == nil {
			err = s.parse(data)
		}
		if err != nil {
			fail(400, fieldError{"", "The settings could not be read: " + err.Error()})
			return
		}
		s.keepSecrets(old)
		if errs := s.validate(); len(errs) > 0 {
			s.whyNoPassword(old, errs)
			fail(400, errs...)
			return
		}
		l.work.Lock() // (not in the middle of a change)
		_, key, _ := l.gluetunEnv(s)
		l.mu.Lock()
		vpnToo := key != l.vpnKey
		proxyToo := vpnToo || string(s.forProxy()) != l.proxyKey
		l.mu.Unlock()
		err = s.save(l.file)
		if err == nil {
			l.mu.Lock()
			l.set, l.problem = s, ""
			l.mu.Unlock()
		}
		l.work.Unlock()
		if err != nil {
			say("settings.json could not be written: %v", err)
			fail(500, fieldError{"", "The settings could not be written to the settings folder"})
			return
		}
		say("settings saved from the web interface")
		writeJSON(w, 200, map[string]any{"ok": true, "restarting": map[string]bool{"proxy": proxyToo, "vpn": vpnToo}})
		go func() {
			l.apply()
			l.mu.Lock()
			now := l.uiPort
			l.mu.Unlock()
			if n, _ := s.UI.Port.number(); n != now {
				if err := l.listenUI(n); err != nil {
					say("the web interface could not move to port %d: %v", n, err)
				} else {
					say("WEB INTERFACE: now on port %d", n)
				}
			}
		}()
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, 405, map[string]any{"ok": false})
	}
}

func (l *launcher) apiStatus(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	s, up, note, running, problem, prog, started := l.set, l.vpnUp, l.vpnNote, l.proxy != nil, l.problem, l.prog, l.vpn != nil
	l.mu.Unlock()
	if note == "" && !up && !started && s != nil {
		for _, e := range s.validate() {
			if strings.HasPrefix(e.Field, "vpn.") { // (apply does not start the VPN with these)
				note = "The VPN is not started: the VPN settings have a problem (" + e.Field + ": " + e.Message + ")"
				break
			}
		}
	}
	vpn := map[string]any{"up": up, "city": "", "country": "", "ip": "", "network": "", "ping_ms": 0, "program": map[string]string{
		"version": prog.Version, "source": prog.Source, "digest": short(prog.Digest), "fetched_at": prog.FetchedAt}}
	vpn["update"] = l.programUpdate()
	if p := l.pickStatus(); p != nil {
		vpn["server"] = p
	}
	if note != "" {
		vpn["note"] = note
	}
	out := map[string]any{"vpn": vpn, "providers": []any{}, "playing": []any{}, "version": version}
	if problem != "" {
		out["problem"] = problem
	}
	var in struct {
		Providers, Playing []any
		City, Country, IP  string
		Network            string
		Ping               int `json:"ping_ms"`
	}
	if data, ok := askProxy("/status", 8*time.Second); running && ok && json.Unmarshal(data, &in) == nil {
		if up {
			vpn["city"], vpn["country"], vpn["ip"], vpn["ping_ms"] = in.City, in.Country, in.IP, in.Ping
			vpn["network"] = in.Network
		}
		if in.Providers != nil {
			out["providers"] = in.Providers
		}
		if in.Playing != nil {
			out["playing"] = in.Playing
		}
	} else if s != nil { // the proxy part is not running: the providers as set up, none of them up
		list := []any{}
		for _, p := range s.Providers {
			list = append(list, map[string]any{"name": p.Name, "port": p.Port, "up": false, "live_tv": p.LiveTV, "channels_open": 0})
		}
		out["providers"] = list
	}
	writeJSON(w, 200, out)
}

// ---------------------------------------------------------------------------
// What the page offers for the VPN: read when asked from gluetun's own server lists, so the choices follow
// gluetun's updates. The lists are one file per VPN service in gluetun's data folder, named in its
// manifest.json: {"version":N,"timestamp":N,"servers":[{"vpn":"wireguard","country":"","region":"","city":"",...}]}.
// Before gluetun has written them (it does at its first start), its own program is asked for the lists
// built into it.
// ---------------------------------------------------------------------------

type vpnOptions struct {
	Providers []string            `json:"providers"`
	Types     []string            `json:"types"`
	Countries map[string][]string `json:"countries"`
	Regions   map[string][]string `json:"regions"`
	Cities    map[string][]string `json:"cities"`
	Updated   map[string]string   `json:"updated"`    // each service's list: when its data was made (the list's own stamp)
	UpdatedAt string              `json:"updated_at"` // the same for the service in use, or else the newest
}

var (
	optionsMu   sync.Mutex
	optionsKey  string
	optionsKept *vpnOptions
)

type serverEntry struct {
	Country    string `json:"country"`
	Region     string `json:"region"`
	City       string `json:"city"`
	ServerName string `json:"server_name"`
	Hostname   string `json:"hostname"`
	// what picking a server goes by (pick.go)
	VPN         string `json:"vpn"`
	SecureCore  bool   `json:"secure_core"`
	Tor         bool   `json:"tor"`
	Free        bool   `json:"free"`
	MultiHop    bool   `json:"multihop"`
	Stream      bool   `json:"stream"`
	PortForward bool   `json:"port_forward"`
	Owned       bool   `json:"owned"`
}

// optionsServers: every service's servers as last read (set whole by readOptions, never changed after).
var optionsServers map[string][]serverEntry

func (l *launcher) serversDir() string {
	dir := filepath.Join(l.dir, "gluetun", "servers")
	l.mu.Lock()
	if l.set != nil {
		if d := l.set.vpnEnv()["STORAGE_SERVERS_DIRECTORY_PATH"]; d != "" {
			dir = d
		}
	}
	l.mu.Unlock()
	return dir
}

func (l *launcher) apiOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, l.optionsNow())
}

// apiServers: one service's servers by name and by hostname, for the page's lists; narrowed to the
// countries, regions and cities picked so far (there can be thousands otherwise).
func (l *launcher) apiServers(w http.ResponseWriter, r *http.Request) {
	l.optionsNow()
	q := r.URL.Query()
	picked := func(key string) map[string]bool {
		m := map[string]bool{}
		for _, s := range strings.Split(q.Get(key), ",") {
			if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
				m[s] = true
			}
		}
		return m
	}
	countries, regions, cities := picked("countries"), picked("regions"), picked("cities")
	optionsMu.Lock()
	list := optionsServers[strings.ToLower(strings.TrimSpace(q.Get("provider")))]
	optionsMu.Unlock()
	sets := [2]map[string]bool{{}, {}}
	for _, s := range list {
		if len(countries) > 0 && !countries[strings.ToLower(s.Country)] || len(regions) > 0 && !regions[strings.ToLower(s.Region)] ||
			len(cities) > 0 && !cities[strings.ToLower(s.City)] {
			continue
		}
		for i, v := range [2]string{s.ServerName, s.Hostname} {
			if v != "" {
				sets[i][v] = true
			}
		}
	}
	out := [2][]string{{}, {}}
	for i := range sets {
		for v := range sets[i] {
			out[i] = append(out[i], v)
		}
		sort.Strings(out[i])
	}
	writeJSON(w, 200, map[string]any{"names": out[0], "hostnames": out[1]})
}

// optionsNow: the server data as it is on disk now (read again only when a file changed).
func (l *launcher) optionsNow() *vpnOptions {
	dir := l.serversDir()
	key := "built in"
	if st, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
		key = dir + " " + st.ModTime().String()
		if list, err := os.ReadDir(dir); err == nil { // (an update rewrites one service's file)
			for _, e := range list {
				if info, err := e.Info(); err == nil {
					key += " " + strconv.FormatInt(info.ModTime().UnixNano(), 36)
				}
			}
		}
	}
	return l.options(dir, key)
}

func (l *launcher) options(dir, key string) *vpnOptions {
	optionsMu.Lock()
	defer optionsMu.Unlock()
	l.mu.Lock()
	prog := l.prog.Path
	l.mu.Unlock()
	if optionsKept == nil || optionsKey != key {
		optionsKept, optionsKey = readOptions(dir, key != "built in", prog), key
	}
	o := *optionsKept
	l.mu.Lock()
	using := ""
	if l.set != nil {
		using = strings.ToLower(l.set.vpnEnv()["VPN_SERVICE_PROVIDER"])
	}
	l.mu.Unlock()
	o.UpdatedAt = o.Updated[using]
	for _, t := range o.Updated {
		if o.Updated[using] == "" && t > o.UpdatedAt {
			o.UpdatedAt = t
		}
	}
	return &o
}

// askGluetun asks gluetun's own control page (inside the container, with this start's key).
func (l *launcher) askGluetun(method, path, body string) (string, error) {
	l.mu.Lock()
	key, running := l.ctlKey, l.vpn != nil
	l.mu.Unlock()
	if !running {
		return "", errors.New("The VPN is not running")
	}
	if key == "" {
		return "", errors.New("gluetun's control page was set up differently in the extra VPN variables, so Coaxial cannot use it")
	}
	req, _ := http.NewRequest(method, "http://127.0.0.1:8000"+path, strings.NewReader(body))
	req.Header.Set("X-API-Key", key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", errors.New("gluetun's control page does not answer")
	}
	defer r.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if r.StatusCode != 200 {
		return "", fmt.Errorf("gluetun's control page answered %d", r.StatusCode)
	}
	var v struct{ Status, Outcome string }
	json.Unmarshal(data, &v)
	return v.Status + v.Outcome, nil
}

// apiRefresh: "refresh the server list now", done by gluetun's own updater (PUT /v1/updater/status on its
// control page, the way its documentation gives). It fetches the list through the VPN, so the VPN must be up.
func (l *launcher) apiRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		writeJSON(w, 405, map[string]any{"ok": false, "message": "Use POST"})
		return
	}
	fail := func(msg string) { writeJSON(w, 200, map[string]any{"ok": false, "message": msg}) }
	if _, err := l.askGluetun("PUT", "/v1/updater/status", `{"status":"running"}`); err != nil {
		fail(err.Error())
		return
	}
	for end := time.Now().Add(90 * time.Second); ; {
		time.Sleep(time.Second)
		state, err := l.askGluetun("GET", "/v1/updater/status", "")
		switch {
		case err != nil:
			fail(err.Error())
		case state == "completed":
			dir := l.serversDir()
			st, _ := os.Stat(filepath.Join(dir, "manifest.json"))
			key := dir + " refreshed " + time.Now().String()
			if st == nil {
				key = "built in"
			}
			writeJSON(w, 200, map[string]any{"ok": true, "updated_at": l.options(dir, key).UpdatedAt})
		case state == "running" && time.Now().Before(end) && r.Context().Err() == nil:
			continue
		case state == "running":
			fail("gluetun is still updating its server list; look again in a minute")
		default:
			fail("gluetun could not update its server list (" + state + "); its reason is in the container log")
		}
		return
	}
}

func readOptions(dir string, stored bool, gluetun string) *vpnOptions {
	o := &vpnOptions{Providers: []string{}, Types: []string{"wireguard", "openvpn"}, Countries: map[string][]string{},
		Regions: map[string][]string{}, Cities: map[string][]string{}, Updated: map[string]string{}}
	servers := map[string][]serverEntry{}
	if stored {
		var manifest map[string]json.RawMessage
		data, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
		json.Unmarshal(data, &manifest)
		for name, raw := range manifest {
			var m struct {
				Filepath string `json:"filepath"`
			}
			if json.Unmarshal(raw, &m) != nil || m.Filepath == "" {
				continue // ("version")
			}
			var file struct {
				Timestamp int64         `json:"timestamp"`
				Servers   []serverEntry `json:"servers"`
			}
			data, _ := os.ReadFile(m.Filepath)
			json.Unmarshal(data, &file)
			servers[name] = file.Servers
			if file.Timestamp > 0 {
				o.Updated[name] = time.Unix(file.Timestamp, 0).UTC().Format(time.RFC3339)
			}
		}
	} else { // gluetun's own program lists the services it knows, and each one's servers
		help, _ := exec.Command(gluetun, "format-servers", "-help").CombinedOutput()
		for _, line := range strings.Split(string(help), "\n") {
			flag := strings.TrimSpace(line)
			if !strings.HasPrefix(line, "  -") || flag == "-format string" || flag == "-output string" {
				continue
			}
			var list []serverEntry
			var out bytes.Buffer
			cmd := exec.Command(gluetun, "format-servers", "-format", "json", flag)
			cmd.Stdout = &out
			if cmd.Run() == nil {
				json.Unmarshal(out.Bytes(), &list)
			}
			servers[strings.ReplaceAll(strings.TrimPrefix(flag, "-"), "-", " ")] = list
		}
	}
	optionsServers = servers // (optionsMu held by the caller)
	for name, list := range servers {
		o.Providers = append(o.Providers, name)
		sets := [3]map[string]bool{{}, {}, {}}
		for _, s := range list {
			for i, v := range [3]string{s.Country, s.Region, s.City} {
				if v != "" {
					sets[i][v] = true
				}
			}
		}
		for i, into := range [3]map[string][]string{o.Countries, o.Regions, o.Cities} {
			vals := []string{}
			for v := range sets[i] {
				vals = append(vals, v)
			}
			sort.Strings(vals)
			into[name] = vals
		}
	}
	sort.Strings(o.Providers)
	o.Providers = append(o.Providers, "custom") // (your own WireGuard or OpenVPN settings: gluetun keeps no list for it)
	for _, into := range [3]map[string][]string{o.Countries, o.Regions, o.Cities} {
		into["custom"] = []string{}
	}
	return o
}

// The ports apiPortTry listens on. Each is let go as soon as the proxy part starts or the page moves: the
// save that follows a try comes within its 8 seconds, and the port is wanted then.
var (
	triesMu sync.Mutex
	tries   = map[*http.Server]net.Listener{}
)

// endTries closes every port opened by apiPortTry.
func endTries() {
	triesMu.Lock()
	defer triesMu.Unlock()
	for srv, ln := range tries {
		ln.Close() // (itself too: a server that has not begun serving yet does not close it)
		srv.Close()
		delete(tries, srv)
	}
}

// apiPortTry: so the web page can tell, before saving, whether a port typed for a provider is one Docker
// publishes: this program cannot see Docker's settings, but it can listen on the port for a few seconds and
// let the page try to reach it from the browser. A port already in use here (a provider's own) is left alone.
func apiPortTry(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("port"))
	if r.Method != "POST" || err != nil || n < 1 || n > 65535 {
		writeJSON(w, 400, map[string]any{"ok": false})
		return
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(n))
	if err != nil { // (in use: something of ours already answers there)
		writeJSON(w, 200, map[string]any{"ok": true, "listening": "already"})
		return
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(204)
	})}
	triesMu.Lock()
	tries[srv] = ln
	triesMu.Unlock()
	go srv.Serve(ln)
	time.AfterFunc(8*time.Second, func() {
		triesMu.Lock()
		delete(tries, srv)
		triesMu.Unlock()
		srv.Close()
	})
	writeJSON(w, 200, map[string]any{"ok": true, "listening": "for 8 seconds"})
}
