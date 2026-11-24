// Keeping the VPN program current. gluetun is published only as a container image, and its program is one
// file in it (/gluetun-entrypoint). Before the VPN is started, the newest image is looked up at its public
// registry, the one layer holding that file is downloaded, checked against the registry's checksums and
// kept in the settings folder. What cannot be fetched or does not check out is never run: the last good
// copy is, or else the program built into this image.
//
//	<settings folder>/gluetun/bin/<manifest digest>/gluetun-entrypoint   the program
//	<settings folder>/gluetun/bin/<manifest digest>/info.json            what it is, and its own checksum
//	<settings folder>/gluetun/bin/current.json                           {"tag","digest","fetched_at","source"}
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	programName   = "gluetun-entrypoint"
	fetchLimit    = 120 * time.Second // the whole look-up and download
	recheckAfter  = 6 * time.Hour     // a later start of the VPN asks the registry again only after this
	retryAfter    = 10 * time.Minute  // ... or this, when the last look-up failed
	maxManifest   = 4 << 20
	maxLayer      = 300 << 20
	minProgram    = 5 << 20
	maxProgram    = 200 << 20
	keepPrograms  = 2
	mediaIndex    = "application/vnd.oci.image.index.v1+json"
	mediaList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerV2 = "application/vnd.docker.distribution.manifest.v2+json"
)

// registry: where gluetun's image is published. Anyone may read it: a token is asked for without a login.
type registry struct {
	name  string // as logged and kept in current.json
	base  string // https://host
	token string // the token address, with service and scope
	repo  string
}

// Both carry the same tags with the same digests (checked). GitHub's first: Docker Hub limits how often
// an address without a login may pull.
var registries = []registry{
	{"ghcr.io/qdm12/gluetun", "https://ghcr.io", "https://ghcr.io/token?service=ghcr.io&scope=repository:qdm12/gluetun:pull", "qdm12/gluetun"},
	{"docker.io/qmcgaw/gluetun", "https://registry-1.docker.io", "https://auth.docker.io/token?service=registry.docker.io&scope=repository:qmcgaw/gluetun:pull", "qmcgaw/gluetun"},
}

// program: the VPN program chosen for a start.
type program struct {
	Path      string `json:"-"`
	Tag       string `json:"tag,omitempty"`
	Version   string `json:"version"`
	Source    string `json:"source"`     // downloaded, cached or builtin
	Digest    string `json:"digest"`     // the image's manifest digest (sha256:...), "" for the built-in one
	FetchedAt string `json:"fetched_at"` // when it was downloaded
	From      string `json:"from,omitempty"`
	Sum       string `json:"sha256,omitempty"` // of the program file itself
}

var builtinProgram = program{Path: gluetun, Version: "built into the image", Source: "builtin"}

var (
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexDirRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// elfMachine: the ELF header's class (1 = 32 bit, 2 = 64 bit) and machine number for each Go architecture.
var elfMachine = map[string][2]uint16{
	"amd64": {2, 62}, "386": {1, 3}, "arm64": {2, 183}, "arm": {1, 40}, "ppc64le": {2, 21}, "riscv64": {2, 243}, "s390x": {2, 22},
}

// tryProgram: the cheapest thing gluetun's program can be asked that proves it runs here: the help of its
// format-servers command, which prints and leaves with 0 (it has no --version; an unknown command leaves with 1).
var tryProgram = func(file string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, file, "format-servers", "-help")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("it did not answer in 10 s")
		}
		return err
	}
	return nil
}

// plain: an error without the address it happened at (a download address carries a signature).
func plain(err error) string {
	var u *url.Error
	if errors.As(err, &u) {
		host := ""
		if p, e := url.Parse(u.URL); e == nil {
			host = " " + p.Host
		}
		return u.Op + host + ": " + u.Err.Error()
	}
	return err.Error()
}

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func short(digest string) string {
	if len(digest) > 19 {
		return digest[:19] // "sha256:" and 12
	}
	return digest
}

func fetchClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DisableKeepAlives:     true,
	}}
}

// fetcher: one look-up at one registry.
type fetcher struct {
	ctx    context.Context
	client *http.Client
	reg    registry
	bearer string
	bytes  int64 // downloaded, all told
	blobs  int   // layers downloaded
}

func (f *fetcher) get(address, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(f.ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if f.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+f.bearer) // (not sent on to another host when redirected)
	}
	r, err := f.client.Do(req)
	if err != nil {
		return nil, errors.New(plain(err))
	}
	if r.StatusCode != 200 {
		r.Body.Close()
		return nil, fmt.Errorf("%s answered %d", req.URL.Host, r.StatusCode)
	}
	return r, nil
}

func (f *fetcher) small(address, accept string, limit int64) ([]byte, http.Header, error) {
	r, err := f.get(address, accept)
	if err != nil {
		return nil, nil, err
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, nil, errors.New(plain(err))
	}
	if int64(len(data)) > limit {
		return nil, nil, errors.New("the answer is too large")
	}
	f.bytes += int64(len(data))
	return data, r.Header, nil
}

func (f *fetcher) login() error {
	data, _, err := f.small(f.reg.token, "", 64<<10)
	if err != nil {
		return fmt.Errorf("no token: %v", err)
	}
	var t struct {
		Token  string `json:"token"`
		Access string `json:"access_token"`
	}
	json.Unmarshal(data, &t)
	if t.Token == "" {
		t.Token = t.Access
	}
	if t.Token == "" {
		return errors.New("no token in the registry's answer")
	}
	f.bearer = t.Token
	return nil
}

// manifest fetches a manifest by tag or digest. Its bytes must hash to the digest asked for, or, for a
// tag, to the digest the registry names for it.
func (f *fetcher) manifest(ref string) (data []byte, digest string, err error) {
	accept := strings.Join([]string{mediaIndex, mediaList, mediaManifest, mediaDockerV2}, ", ")
	data, head, err := f.small(f.reg.base+"/v2/"+f.reg.repo+"/manifests/"+ref, accept, maxManifest)
	if err != nil {
		return nil, "", err
	}
	digest = ref
	if !digestRe.MatchString(ref) {
		digest = head.Get("Docker-Content-Digest")
		if !digestRe.MatchString(digest) {
			return nil, "", errors.New("the registry named no sha256 digest for the tag")
		}
	}
	if got := "sha256:" + sumHex(data); got != digest {
		return nil, "", fmt.Errorf("the manifest does not match its digest (%s, not %s)", short(got), short(digest))
	}
	return data, digest, nil
}

// blob fetches a small blob (the image's description) and checks it against its digest.
func (f *fetcher) blob(digest string, limit int64) ([]byte, error) {
	data, _, err := f.small(f.reg.base+"/v2/"+f.reg.repo+"/blobs/"+digest, "", limit)
	if err != nil {
		return nil, err
	}
	if got := "sha256:" + sumHex(data); got != digest {
		return nil, fmt.Errorf("a blob does not match its digest (%s, not %s)", short(got), short(digest))
	}
	return data, nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type manifestDoc struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"` // an index: one image per architecture
	Config    descriptor   `json:"config"`    // one image
	Layers    []descriptor `json:"layers"`
}

// pickImage: from an image index, the image for linux on this architecture. (An index also lists
// attestations, as images for the architecture "unknown".) For arm the v7 build is taken before v6.
func pickImage(doc *manifestDoc, arch string) (descriptor, error) {
	best, rank := descriptor{}, -1
	for _, m := range doc.Manifests {
		if m.Platform.OS != "linux" || m.Platform.Architecture != arch || !digestRe.MatchString(m.Digest) {
			continue
		}
		r := 1
		if arch == "arm" {
			r = map[string]int{"v7": 3, "v6": 2}[m.Platform.Variant]
		}
		if r > rank {
			best, rank = m, r
		}
	}
	if rank < 0 {
		return best, fmt.Errorf("the image has no build for linux/%s", arch)
	}
	return best, nil
}

// checkELF: a program for this architecture begins with the ELF mark, then its class and machine number.
func checkELF(head []byte, arch string) error {
	want, known := elfMachine[arch]
	if len(head) < 20 || string(head[:4]) != "\x7fELF" {
		return errors.New("it is not a program (no ELF mark)")
	}
	if !known {
		return nil
	}
	machine := uint16(head[18]) | uint16(head[19])<<8
	if head[5] == 2 { // (big-endian)
		machine = uint16(head[19]) | uint16(head[18])<<8
	}
	if uint16(head[4]) != want[0] || machine != want[1] {
		return fmt.Errorf("it is a program for another kind of processor (ELF class %d machine %d, not %s)", head[4], machine, arch)
	}
	return nil
}

func checkProgramFile(file, arch string) (sum string, err error) {
	st, err := os.Lstat(file)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() < minProgram || st.Size() > maxProgram {
		return "", fmt.Errorf("it is not a plain file of a plausible size (%d bytes)", st.Size())
	}
	fh, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	head := make([]byte, 20)
	if _, err := io.ReadFull(fh, head); err != nil {
		return "", err
	}
	if err := checkELF(head, arch); err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(head)
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// layer downloads one layer, hashing all of it as it comes, and writes the program to a new file in dir if
// the layer holds it. found = "" when it does not. The file is kept only if the whole layer matched its digest.
func (f *fetcher) layer(l descriptor, dir string) (found string, err error) {
	gz := strings.HasSuffix(l.MediaType, "+gzip") || strings.HasSuffix(l.MediaType, ".gzip")
	if !gz && !strings.HasSuffix(l.MediaType, ".tar") {
		return "", fmt.Errorf("a layer is packed in a way this program cannot read (%s)", l.MediaType)
	}
	if !digestRe.MatchString(l.Digest) || l.Size <= 0 || l.Size > maxLayer {
		return "", fmt.Errorf("a layer is larger than %d MB, or has no size or digest", maxLayer>>20)
	}
	r, err := f.get(f.reg.base+"/v2/"+f.reg.repo+"/blobs/"+l.Digest, "")
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	f.blobs++
	h := sha256.New()
	n := int64(0)
	raw := io.TeeReader(io.LimitReader(r.Body, l.Size+1), countWriter{h, &n})
	defer func() {
		f.bytes += n
		if err != nil && found != "" {
			os.Remove(found)
			found = ""
		}
	}()
	found, err = unpackProgram(raw, gz, dir)
	if _, e := io.Copy(io.Discard, raw); e != nil { // the rest of the layer: all of it is hashed
		return found, fmt.Errorf("the download broke off: %s", plain(e))
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); n != l.Size || got != l.Digest {
		return found, fmt.Errorf("a downloaded layer does not match its digest (%d bytes %s, not %d bytes %s)", n, short(got), l.Size, short(l.Digest))
	}
	return found, err
}

// unpackProgram reads a layer (a tar file, most often gzipped) as far as the program, and writes it to a
// new file in dir. found = "" when the layer does not hold it.
func unpackProgram(raw io.Reader, gz bool, dir string) (found string, err error) {
	files := raw
	if gz {
		z, err := gzip.NewReader(raw)
		if err != nil {
			return "", fmt.Errorf("a layer could not be unpacked: %s", plain(err))
		}
		files = z
	}
	for t := tar.NewReader(files); ; {
		hd, err := t.Next()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("a layer could not be unpacked: %s", plain(err))
		}
		name := strings.TrimPrefix(path.Clean("/"+hd.Name), "/")
		if name == ".wh."+programName {
			return "", errors.New("the image removes its own program")
		}
		if name != programName {
			continue
		}
		if hd.Typeflag != tar.TypeReg || hd.Size < minProgram || hd.Size > maxProgram {
			return "", fmt.Errorf("the program in the image is not a plain file of a plausible size (%d bytes)", hd.Size)
		}
		tmp, err := os.CreateTemp(dir, ".fetch-*.tmp")
		if err != nil {
			return "", err
		}
		n, err := io.Copy(tmp, io.LimitReader(t, hd.Size))
		if err == nil && n != hd.Size {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			err = tmp.Chmod(0o755)
		}
		if err == nil {
			err = tmp.Sync()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return tmp.Name(), fmt.Errorf("the program could not be unpacked: %s", plain(err))
		}
		return tmp.Name(), nil
	}
}

type countWriter struct {
	w io.Writer
	n *int64
}

func (c countWriter) Write(p []byte) (int, error) {
	*c.n += int64(len(p))
	return c.w.Write(p)
}

// binDir: where the downloaded programs are kept.
func binDir(dir string) string { return filepath.Join(dir, "gluetun", "bin") }

func writeWhole(file string, v any) error {
	data, _ := json.MarshalIndent(v, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(file), ".fetch-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), file)
}

// cached: the copy kept for a manifest digest, if it is there and still what was downloaded: its
// checksum is the one noted when it was verified, and it is a program for this processor.
func cached(bin, digest, arch string) (program, error) {
	dir := filepath.Join(bin, strings.TrimPrefix(digest, "sha256:"))
	var p program
	data, err := os.ReadFile(filepath.Join(dir, "info.json"))
	if err != nil {
		return p, err
	}
	if json.Unmarshal(data, &p) != nil || p.Digest != digest || p.Sum == "" {
		return p, errors.New("its info.json is damaged")
	}
	p.Path = filepath.Join(dir, programName)
	sum, err := checkProgramFile(p.Path, arch)
	if err != nil {
		return p, err
	}
	if sum != p.Sum {
		return p, errors.New("the file is not the one that was downloaded (its checksum changed)")
	}
	return p, nil
}

// keptPrograms: the manifest digests with a copy in the cache, the most recently used first.
func keptPrograms(bin string) []string {
	type entry struct {
		digest string
		at     time.Time
	}
	var list []entry
	all, _ := os.ReadDir(bin)
	for _, e := range all {
		if info, err := e.Info(); err == nil && e.IsDir() && hexDirRe.MatchString(e.Name()) {
			list = append(list, entry{"sha256:" + e.Name(), info.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.After(list[j].at) })
	var out []string
	for _, e := range list {
		out = append(out, e.digest)
	}
	return out
}

// tidy: the copy in use and the one before it stay; older ones, and files left by a broken download, go.
// The one before is the build the VPN last connected with, when that is known: a newer build it never
// connected with is undone to that one, and must not be the copy kept in its place.
func tidy(bin, using string) {
	n, good := 1, connected(bin)
	if _, err := os.Stat(filepath.Join(bin, strings.TrimPrefix(good, "sha256:"))); good == "" || good == using || err != nil {
		good = ""
	} else {
		n++
	}
	for _, d := range keptPrograms(bin) {
		if d == using || d == good {
			continue
		}
		if n++; n > keepPrograms {
			os.RemoveAll(filepath.Join(bin, strings.TrimPrefix(d, "sha256:")))
		}
	}
	old, _ := filepath.Glob(filepath.Join(bin, ".fetch-*.tmp"))
	for _, f := range old {
		os.Remove(f)
	}
}

// fetchProgram asks one registry what the tag is now, and returns the program for it: the cached copy if
// that manifest digest is already kept (nothing more is downloaded), or else a fresh, verified download.
func fetchProgram(ctx context.Context, client *http.Client, reg registry, tag, arch, bin string) (program, *fetcher, error) {
	f := &fetcher{ctx: ctx, client: client, reg: reg}
	var none program
	if err := f.login(); err != nil {
		return none, f, err
	}
	data, digest, err := f.manifest(tag)
	if err != nil {
		return none, f, err
	}
	if digest == skipped(bin) { // (left alone after an update it did not connect with: not downloaded again)
		return none, f, errSkipped
	}
	if p, err := cached(bin, digest, arch); err == nil {
		p.Source, p.Tag = "cached", tag
		return p, f, nil
	} else if !os.IsNotExist(err) {
		say("VPN program: the cached copy %s is not used: %s", short(digest), plain(err))
		os.RemoveAll(filepath.Join(bin, strings.TrimPrefix(digest, "sha256:")))
	}
	var doc manifestDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return none, f, errors.New("the manifest could not be read")
	}
	if len(doc.Manifests) > 0 { // an index: the image for this architecture is a manifest of its own
		m, err := pickImage(&doc, arch)
		if err != nil {
			return none, f, err
		}
		data, _, err = f.manifest(m.Digest)
		if err != nil {
			return none, f, err
		}
		doc = manifestDoc{}
		if err := json.Unmarshal(data, &doc); err != nil {
			return none, f, errors.New("the manifest could not be read")
		}
	}
	if !digestRe.MatchString(doc.Config.Digest) || len(doc.Layers) == 0 {
		return none, f, errors.New("the manifest names no image")
	}
	raw, err := f.blob(doc.Config.Digest, 1<<20)
	if err != nil {
		return none, f, err
	}
	var about struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	json.Unmarshal(raw, &about)
	if about.OS != "linux" || about.Architecture != arch {
		return none, f, fmt.Errorf("the image is for %s/%s, not linux/%s", about.OS, about.Architecture, arch)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return none, f, err
	}
	// The program is in the image's last layer (a COPY at the end of its build); the layers are looked
	// through from the last one back, so the copy a container would see is the one found.
	file := ""
	for i := len(doc.Layers) - 1; i >= 0 && file == ""; i-- {
		if file, err = f.layer(doc.Layers[i], bin); err != nil {
			return none, f, err
		}
	}
	if file == "" {
		return none, f, errors.New("no layer of the image holds /" + programName)
	}
	defer os.Remove(file)
	sum, err := checkProgramFile(file, arch)
	if err != nil {
		return none, f, fmt.Errorf("the downloaded program is refused: %s", plain(err))
	}
	version := about.Config.Labels["org.opencontainers.image.version"]
	if version == "" {
		version = tag
	}
	if rev := about.Config.Labels["org.opencontainers.image.revision"]; rev != "" {
		version += " (" + rev[:min(len(rev), 7)] + ")"
	}
	p := program{Tag: tag, Version: version, Source: "downloaded", Digest: digest, From: reg.name, Sum: sum,
		FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	dir := filepath.Join(bin, strings.TrimPrefix(digest, "sha256:"))
	p.Path = filepath.Join(dir, programName)
	if err = os.MkdirAll(dir, 0o755); err == nil {
		err = os.Rename(file, p.Path)
	}
	if err == nil {
		err = writeWhole(filepath.Join(dir, "info.json"), p)
	}
	if err != nil {
		os.RemoveAll(dir)
		return none, f, fmt.Errorf("the program could not be kept: %s", plain(err))
	}
	return p, f, nil
}

// noteCurrent writes current.json: what the tag was last found to be.
func noteCurrent(bin string, p program) {
	writeWhole(filepath.Join(bin, "current.json"), map[string]string{"tag": p.Tag, "digest": p.Digest, "fetched_at": p.FetchedAt, "source": p.From})
}

// chooseProgram picks the VPN program for a start of the VPN and logs one line saying which and why:
// the fresh or cached copy of what the registry has now; else the most recently used copy; else the one
// built into the image. A downloaded copy is run only after it answered a harmless command. (l.work held)
func (l *launcher) chooseProgram(tag string) program {
	pick := func(p program, why string) program {
		l.mu.Lock()
		l.prog = p
		l.mu.Unlock()
		what := "the one built into the image"
		if p.Source != "builtin" {
			what = fmt.Sprintf("gluetun %s, %s (%s, fetched %s)", p.Version, p.Source, short(p.Digest), p.FetchedAt)
		}
		say("VPN program: %s: %s", what, why)
		return p
	}
	if tag == "builtin" {
		return pick(builtinProgram, `gluetun_version is "builtin"`)
	}
	bin, arch := binDir(l.dir), runtime.GOARCH
	skip := skipped(bin)
	usable := func(p program) bool {
		if skip != "" && p.Digest == skip {
			say("VPN program: the copy %s is left alone: the VPN did not connect with it after an update", short(p.Digest))
			return false
		}
		if err := tryProgram(p.Path); err != nil {
			say("VPN program: the copy %s does not run here and is not used: %s", short(p.Digest), plain(err))
			return false
		}
		return true
	}
	why := ""
	l.mu.Lock()
	last, lastTag, lastOK, lastDigest := l.progAsked, l.progTag, l.progOK, l.progDigest
	l.mu.Unlock()
	switch age := time.Since(last); {
	case lastTag == tag && lastOK && age < recheckAfter:
		if p, err := cached(bin, lastDigest, arch); err == nil && usable(p) {
			p.Source, p.Tag = "cached", tag
			os.Chtimes(filepath.Dir(p.Path), time.Now(), time.Now())
			return pick(p, fmt.Sprintf("the registry was asked %d minutes ago (it is asked again after %d hours)", int(age.Minutes()), int(recheckAfter.Hours())))
		}
		why = "the copy checked earlier is no longer usable"
	case lastTag == tag && !lastOK && age < retryAfter:
		why = fmt.Sprintf("the last look-up failed %d minutes ago (tried again after %d)", int(age.Minutes()), int(retryAfter.Minutes()))
	}
	if why == "" || lastOK {
		got, fetched := l.askRegistries(tag, usable)
		if got.Path != "" {
			os.Chtimes(filepath.Dir(got.Path), time.Now(), time.Now())
			tidy(bin, got.Digest)
			return pick(got, fetched)
		}
		why = fetched
	}
	var good []program // the copies kept, the most recently used first; one fetched for this tag before the others
	for _, d := range keptPrograms(bin) {
		p, err := cached(bin, d, arch)
		if err != nil {
			say("VPN program: the cached copy %s is not used: %s", short(d), plain(err))
			continue
		}
		p.Source = "cached"
		good = append(good, p)
	}
	sort.SliceStable(good, func(i, j int) bool { return good[i].Tag == tag && good[j].Tag != tag })
	for _, p := range good {
		if usable(p) {
			return pick(p, why)
		}
	}
	return pick(builtinProgram, why+" and no usable copy is cached")
}

// askRegistries asks the registries what the tag is now and returns its program (cached, or downloaded and
// checked) with why; got.Path is "" when none could be had. What came of it is noted for chooseProgram.
func (l *launcher) askRegistries(tag string, usable func(program) bool) (got program, why string) {
	bin, arch := binDir(l.dir), runtime.GOARCH
	ctx, cancel := context.WithTimeout(context.Background(), fetchLimit)
	defer cancel()
	client, start := fetchClient(), time.Now()
	var fails []string
	for _, reg := range registries {
		p, f, err := fetchProgram(ctx, client, reg, tag, arch, bin)
		if err == nil && !usable(p) {
			os.RemoveAll(filepath.Dir(p.Path))
			err = errors.New("its program does not run here")
		}
		if errors.Is(err, errSkipped) { // the other registry would name the same build
			fails = append(fails, reg.name+": "+plain(err))
			break
		}
		if err != nil {
			fails = append(fails, reg.name+": "+plain(err))
			continue
		}
		got = p
		if p.Source == "downloaded" {
			why = fmt.Sprintf("%s at %s is new here; %.1f MB downloaded in %.1f s and checked against the registry's checksums",
				tag, reg.name, float64(f.bytes)/1e6, time.Since(start).Seconds())
			noteCurrent(bin, p)
		} else {
			why = fmt.Sprintf("%s at %s is still this one; nothing downloaded", tag, reg.name)
		}
		break
	}
	l.mu.Lock()
	l.progAsked, l.progTag, l.progOK, l.progDigest = time.Now(), tag, got.Path != "", got.Digest
	if got.Path != "" { // (what the registry names now is the newest: an older answer would offer the build left behind)
		l.updDigest, l.updAt, l.updErr = got.Digest, time.Now(), ""
	}
	l.mu.Unlock()
	if got.Path == "" {
		why = "the newest could not be fetched (" + strings.Join(fails, "; ") + ")"
	}
	return got, why
}

// ---------------------------------------------------------------------------
// Is there a newer VPN program? Asked once an hour and when the page asks. Taking it means starting the VPN
// again, which cuts what is playing. So it is installed by itself only after nothing has played for a while,
// and if the VPN does not connect with it, the one before is put back and that build is left alone.
// ---------------------------------------------------------------------------

const (
	updateEvery = time.Hour
	idleMinutes = 10 // nothing playing for this long before an update is installed unasked
)

var updateWait = 2 * time.Minute // the VPN must be up again within this, or the update is undone (a variable for the tests)

// errSkipped: the newest build is the one left alone (see skipped).
var errSkipped = errors.New("the newest build is the one left alone after an update it did not connect with")

// skipped: the build that did not connect after an update ("" if none). Kept in a file, so a restart of the
// container does not walk into it again.
func skipped(bin string) string { return noted(bin, "skip.json") }

// connected: the build the VPN last connected with ("" if none yet), kept in a file for the same reason:
// after a restart, a newer build the VPN never comes up with can still be told from one it did.
func connected(bin string) string { return noted(bin, "good.json") }

func noted(bin, file string) string {
	var note struct {
		Digest string `json:"digest"`
	}
	data, _ := os.ReadFile(filepath.Join(bin, file))
	json.Unmarshal(data, &note)
	if !digestRe.MatchString(note.Digest) {
		return ""
	}
	return note.Digest
}

// nothingPlaying: the proxy part answers and says no one is watching or recording.
func nothingPlaying() bool {
	var in struct{ Playing []any }
	data, ok := askProxy("/playing", 8*time.Second)
	return ok && json.Unmarshal(data, &in) == nil && len(in.Playing) == 0
}

// restartForProgram starts the VPN again so that it takes whichever program chooseProgram now picks. The
// registry is asked first, through the VPN that still runs: gluetun leaves its firewall and its name server
// (127.0.0.1) behind when it stops, so nothing can be fetched from the moment the VPN is stopped until it is up
// again. The start that follows takes what was found from the cache.
func (l *launcher) restartForProgram(why string) { l.restartIf(why, false) }

// restartIf is restartForProgram; with onlyNewer ("Update now"), the VPN is left running, and false returned, when
// the registry gave no build other than the one in use: starting it again would cut what plays for nothing.
func (l *launcher) restartIf(why string, onlyNewer bool) bool {
	l.mu.Lock()
	s := l.set
	l.mu.Unlock()
	if s != nil {
		if tag, _ := s.gluetunVersion(); tag != "builtin" {
			l.work.Lock() // (not while a start is choosing one)
			got, found := l.askRegistries(tag, func(p program) bool { return tryProgram(p.Path) == nil })
			if got.Path != "" { // (as a start does: the newest, the one the VPN last connected with, and no third)
				tidy(binDir(l.dir), got.Digest)
			}
			l.mu.Lock()
			same := got.Path == "" || got.Digest == l.prog.Digest
			l.mu.Unlock()
			l.work.Unlock()
			say("VPN program: asked before the VPN is stopped: %s", found)
			if onlyNewer && same {
				say("VPN program: no newer build could be had; the VPN is left as it is")
				return false
			}
		}
	}
	l.mu.Lock()
	l.vpnKey = why
	l.mu.Unlock()
	l.apply()
	return true
}

// askBeforeStop: a start of the VPN that will want the registry (another tag, or the last look-up is old) asks
// it now, while the VPN still runs, for the reason given at restartForProgram; the start then takes the answer
// from the cache. Without it, a gluetun_version changed in the settings was never fetched. (l.work held)
func (l *launcher) askBeforeStop(tag string) {
	l.mu.Lock()
	up, last, lastTag, lastOK := l.vpnUp, l.progAsked, l.progTag, l.progOK
	l.mu.Unlock()
	age := time.Since(last)
	if !up || tag == "builtin" || lastTag == tag && (lastOK && age < recheckAfter || !lastOK && age < retryAfter) {
		return
	}
	got, found := l.askRegistries(tag, func(p program) bool { return tryProgram(p.Path) == nil })
	if got.Path != "" {
		tidy(binDir(l.dir), got.Digest)
	}
	say("VPN program: asked before the VPN is stopped: %s", found)
}

// autoUpdate installs the newer program found, watches the VPN come back, and undoes it if it does not.
func (l *launcher) autoUpdate() {
	bin := binDir(l.dir)
	l.mu.Lock()
	want, had := l.updDigest, l.prog.Digest
	l.mu.Unlock()
	say("VPN program: a newer gluetun (%s) is out and nothing has played for %d minutes; updating now", short(want), idleMinutes)
	l.restartForProgram("(update)")
	up := false
	for end := time.Now().Add(updateWait); time.Now().Before(end) && !up; {
		time.Sleep(3 * time.Second)
		l.mu.Lock()
		up = l.vpnUp && l.vpn != nil && l.vpn.ever
		l.mu.Unlock()
	}
	l.mu.Lock()
	got := l.prog.Digest
	if got != want { // (not fetched: tried again when a newer one is named, or from the web page)
		l.updFailed = want
	}
	l.mu.Unlock()
	if up {
		if got == want {
			say("VPN program: updated to gluetun %s; the VPN is back up", short(got))
		} else {
			say("VPN program: the update could not be fetched; the VPN is back up on %s", short(got))
		}
		return
	}
	if got == had {
		say("VPN program: the VPN did not come back after an update attempt, on the same program as before; nothing to undo")
		return
	}
	writeWhole(filepath.Join(bin, "skip.json"), map[string]string{"digest": got})
	say("VPN program: the VPN did not connect within %d minutes on gluetun %s; going back to the one before and leaving that build alone", int(updateWait.Minutes()), short(got))
	l.restartForProgram("(update undone)")
}

// newest asks the registries which image the tag names now. Only a manifest is fetched: a few KB.
func newest(tag string) (digest, from string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := fetchClient()
	var fails []string
	for _, reg := range registries {
		f := &fetcher{ctx: ctx, client: client, reg: reg}
		if err := f.login(); err != nil {
			fails = append(fails, reg.name+": "+plain(err))
			continue
		}
		_, d, err := f.manifest(tag)
		if err != nil {
			fails = append(fails, reg.name+": "+plain(err))
			continue
		}
		return d, reg.name, nil
	}
	return "", "", errors.New(strings.Join(fails, "; "))
}

// checkProgram looks for a newer VPN program and remembers the answer for the status page.
func (l *launcher) checkProgram(who string) map[string]any {
	l.mu.Lock()
	s := l.set
	l.mu.Unlock()
	tag := "latest"
	if s != nil {
		tag, _ = s.gluetunVersion()
	}
	if tag == "builtin" {
		l.mu.Lock()
		l.updDigest, l.updErr, l.updAt = "", "", time.Now()
		l.mu.Unlock()
		return l.programUpdate()
	}
	began := time.Now()
	d, from, err := newest(tag)
	l.mu.Lock()
	l.updAt = time.Now()
	if err != nil {
		l.updErr = plain(err)
		say("VPN program: update check (%s) failed: %s", who, l.updErr)
	} else {
		verdict := "the one running"
		if d != l.prog.Digest {
			verdict = "NEWER than the one running (" + short(l.prog.Digest) + ")"
		}
		say("VPN program: update check (%s): asked %s in %d ms; %q is %s, %s", who, from, time.Since(began).Milliseconds(), tag, short(d), verdict)
		if d != l.updDigest && d != l.prog.Digest {
			say("VPN program: a newer gluetun (%s) is published for %q; it is installed once nothing has played for %d minutes, or from the web page", short(d), tag, idleMinutes)
		}
		l.updDigest, l.updErr = d, ""
	}
	l.mu.Unlock()
	return l.programUpdate()
}

// programUpdate: what the status page shows about it.
func (l *launcher) programUpdate() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]any{"available": l.updDigest != "" && l.updDigest != l.prog.Digest, "newest": short(l.updDigest), "checked_at": "", "error": l.updErr}
	if !l.updAt.IsZero() {
		out["checked_at"] = l.updAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (l *launcher) watchProgram() {
	time.Sleep(20 * time.Second)
	var asked, quietSince, downSince time.Time
	bin := binDir(l.dir)
	good := connected(bin)
	for ; ; time.Sleep(20 * time.Second) {
		l.mu.Lock()
		up, trying, works := l.vpnUp, l.pickBusy, "" // works: the build the VPN is connected with
		if up && l.vpn != nil && l.vpn.ever {
			works = l.vpn.prog
		}
		l.mu.Unlock()
		if trying { // servers are being tried: the tunnel comes and goes, and nothing here should read that as trouble
			downSince, quietSince = time.Time{}, time.Time{}
			continue
		}
		if up && time.Since(asked) >= updateEvery {
			l.checkProgram("hourly")
			asked = time.Now()
		}
		l.mu.Lock()
		pending := l.updDigest != "" && l.updDigest != l.prog.Digest && l.updDigest != l.updFailed
		want, named := l.updDigest, l.pick.Env != "" || l.pickTry != ""
		l.mu.Unlock()
		if !up {
			// A server chosen earlier that no longer connects is dropped: gluetun picks for itself again. And a
			// build the VPN never connected with here, taken at a start (a newer one was published, or the
			// container was restarted in the middle of an update), is undone like a failed update.
			if downSince.IsZero() {
				downSince = time.Now()
			} else if l.undoBuild(good, time.Since(downSince)) {
				downSince = time.Time{}
				continue
			} else if named && time.Since(downSince) > 2*time.Minute {
				say("VPN server: the VPN has not connected for 2 minutes on the server chosen; letting gluetun choose")
				l.mu.Lock()
				l.pick, l.pickTry = picked{}, ""
				l.mu.Unlock()
				os.Remove(pickFile(l.dir))
				l.apply()
				downSince = time.Time{}
			}
			quietSince = time.Time{}
			continue
		}
		downSince = time.Time{}
		if works != "" && works != good {
			writeWhole(filepath.Join(bin, "good.json"), map[string]string{"digest": works})
			good = works
		}
		// The VPN servers are pinged every few minutes while nothing plays (probeTick), and the VPN is
		// moved to a better one once nothing has played for a while (probe.go).
		playing := !nothingPlaying()
		quiet := time.Duration(0)
		if !playing && !quietSince.IsZero() {
			quiet = time.Since(quietSince)
		}
		l.probeTick(quiet)
		if playing {
			quietSince = time.Time{}
			continue
		}
		if quietSince.IsZero() {
			quietSince = time.Now()
		}
		if pending && want != skipped(binDir(l.dir)) && time.Since(quietSince) >= idleMinutes*time.Minute {
			quietSince = time.Time{}
			l.autoUpdate()
			continue
		}
	}
}

// undoBuild: the VPN has been down this long on the build in use. If that is a build it never connected with
// here (good: the one it last did), and for updateWait, the build is left alone (skip.json) and the VPN started
// again, on the one before. (watchProgram)
func (l *launcher) undoBuild(good string, down time.Duration) bool {
	l.mu.Lock()
	running := l.prog.Digest
	l.mu.Unlock()
	if running == "" || good == "" || running == good || down <= updateWait {
		return false
	}
	writeWhole(filepath.Join(binDir(l.dir), "skip.json"), map[string]string{"digest": running})
	say("VPN program: the VPN has not connected for %d minutes on gluetun %s, which it never connected with here; going back to %s and leaving that build alone",
		int(updateWait.Minutes()), short(running), short(good))
	l.restartForProgram("(build undone)")
	return true
}

// apiProgramCheck: "check now".
func (l *launcher) apiProgramCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]any{"ok": false})
		return
	}
	l.mu.Lock()
	trying := l.pickBusy
	l.mu.Unlock()
	if trying {
		writeJSON(w, 200, map[string]any{"ok": true, "update": l.programUpdate()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "update": l.checkProgram("button, from " + r.RemoteAddr)})
}

// apiProgramUpdate: "update now": the VPN is started again and takes the newest program on its way up.
func (l *launcher) apiProgramUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]any{"ok": false})
		return
	}
	say("VPN program: an update was asked for from the web interface; starting the VPN again")
	os.Remove(filepath.Join(binDir(l.dir), "skip.json")) // (asked for by hand: a build left alone before is tried again)
	done := l.restartIf("(update)", true)
	l.mu.Lock()
	p := l.prog
	l.mu.Unlock()
	if !done {
		writeJSON(w, 200, map[string]any{"ok": false, "message": "The newer VPN client could not be downloaded, so nothing was changed and the VPN stayed connected. The container log has the reason.",
			"program": map[string]string{"version": p.Version, "source": p.Source, "digest": short(p.Digest)}, "update": l.programUpdate()})
		return
	}
	// (the registry was asked just before the VPN stopped; it cannot be asked again until the VPN is back up)
	writeJSON(w, 200, map[string]any{"ok": true, "program": map[string]string{"version": p.Version, "source": p.Source, "digest": short(p.Digest)}, "update": l.programUpdate()})
}
