package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProgram: what passes for a program of the given ELF class and machine: the header, then filler.
func fakeProgram(class byte, machine uint16, size int) []byte {
	p := make([]byte, size)
	copy(p, []byte{0x7f, 'E', 'L', 'F', class, 1, 1, 0})
	p[16], p[18], p[19] = 2, byte(machine), byte(machine>>8)
	return p
}

func tarGz(files map[string][]byte) []byte {
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	t := tar.NewWriter(z)
	for name, data := range files {
		t.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg})
		t.Write(data)
	}
	t.Close()
	z.Close()
	return buf.Bytes()
}

// fakeRegistry: a registry with one repository, r/g. Tags "latest" (an index of amd64, arm64 and an
// attestation) and "single" (the amd64 image by itself). What it serves can be spoiled on the way out.
type fakeRegistry struct {
	*httptest.Server
	mu        sync.Mutex
	tags      map[string]string // tag -> digest
	manifests map[string][]byte // digest -> bytes
	blobs     map[string][]byte
	spoil     string         // a digest: its bytes are served with one changed
	asked     map[string]int // "token", "manifest", "blob"
	layer     string         // the digest of the layer with the program
	index     string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{tags: map[string]string{}, manifests: map[string][]byte{}, blobs: map[string][]byte{}, asked: map[string]int{}}
	put := func(into map[string][]byte, data []byte) descriptor {
		d := "sha256:" + sumHex(data)
		into[d] = data
		return descriptor{Digest: d, Size: int64(len(data))}
	}
	image := func(arch string, class byte, machine uint16) string {
		base := put(f.blobs, tarGz(map[string][]byte{"etc/os-release": []byte("x " + arch)}))
		prog := put(f.blobs, tarGz(map[string][]byte{programName: fakeProgram(class, machine, minProgram+100)}))
		config := put(f.blobs, []byte(`{"architecture":"`+arch+`","os":"linux","config":{"Labels":{"org.opencontainers.image.version":"v9.9.9","org.opencontainers.image.revision":"abcdef0123"}}}`))
		base.MediaType, prog.MediaType = "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip"
		if arch == "amd64" {
			f.layer = prog.Digest
		}
		m, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaManifest, "config": config, "layers": []descriptor{base, prog}})
		return put(f.manifests, m).Digest
	}
	amd, arm := image("amd64", 2, 62), image("arm64", 2, 183)
	entry := func(digest, arch string) map[string]any {
		return map[string]any{"mediaType": mediaManifest, "digest": digest, "size": 1, "platform": map[string]string{"architecture": arch, "os": map[bool]string{true: "unknown", false: "linux"}[arch == "unknown"]}}
	}
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mediaIndex, "manifests": []any{entry(arm, "unknown"), entry(arm, "arm64"), entry(amd, "amd64")}})
	f.index = put(f.manifests, idx).Digest
	f.tags["latest"], f.tags["single"] = f.index, amd
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		kind, ref := "", r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var data []byte
		switch {
		case r.URL.Path == "/token":
			f.asked["token"]++
			w.Write([]byte(`{"token":"anyone"}`))
			return
		case r.Header.Get("Authorization") != "Bearer anyone":
			w.WriteHeader(401)
			return
		case strings.HasPrefix(r.URL.Path, "/v2/r/g/manifests/"):
			kind = "manifest"
			if d, ok := f.tags[ref]; ok {
				ref = d
			}
			data = f.manifests[ref]
			w.Header().Set("Docker-Content-Digest", ref)
		case strings.HasPrefix(r.URL.Path, "/v2/r/g/blobs/"):
			kind, data = "blob", f.blobs[ref]
		}
		if data == nil {
			w.WriteHeader(404)
			return
		}
		f.asked[kind]++
		if ref == f.spoil {
			data = append([]byte{}, data...)
			data[len(data)/2] ^= 1
		}
		w.Write(data)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRegistry) reg() registry {
	return registry{"fake/r/g", f.URL, f.URL + "/token", "r/g"}
}

func (f *fakeRegistry) set(spoil string) {
	f.mu.Lock()
	f.spoil, f.asked = spoil, map[string]int{}
	f.mu.Unlock()
}

func leftovers(bin string) []string {
	all, _ := filepath.Glob(filepath.Join(bin, ".fetch-*"))
	return all
}

func TestPickImage(t *testing.T) {
	var doc manifestDoc
	json.Unmarshal([]byte(`{"manifests":[
		{"digest":"sha256:`+strings.Repeat("0", 64)+`","platform":{"architecture":"unknown","os":"unknown"}},
		{"digest":"sha256:`+strings.Repeat("a", 64)+`","platform":{"architecture":"amd64","os":"linux"}},
		{"digest":"sha256:`+strings.Repeat("b", 64)+`","platform":{"architecture":"amd64","os":"windows"}},
		{"digest":"sha256:`+strings.Repeat("6", 64)+`","platform":{"architecture":"arm","os":"linux","variant":"v6"}},
		{"digest":"sha256:`+strings.Repeat("7", 64)+`","platform":{"architecture":"arm","os":"linux","variant":"v7"}},
		{"digest":"not a digest","platform":{"architecture":"386","os":"linux"}}]}`), &doc)
	for arch, want := range map[string]string{"amd64": "a", "arm": "7", "arm64": "", "386": "", "unknown": ""} {
		m, err := pickImage(&doc, arch)
		if want == "" && err == nil || want != "" && m.Digest != "sha256:"+strings.Repeat(want, 64) {
			t.Errorf("%s: got %q, %v", arch, m.Digest, err)
		}
	}
}

func TestCheckELF(t *testing.T) {
	if err := checkELF(fakeProgram(2, 62, 64), "amd64"); err != nil {
		t.Error(err)
	}
	if err := checkELF(fakeProgram(1, 40, 64), "arm"); err != nil {
		t.Error(err)
	}
	for name, head := range map[string][]byte{"another processor": fakeProgram(2, 183, 64), "32 bit": fakeProgram(1, 62, 64),
		"a script": []byte("#!/bin/sh\necho hello, this is no program"), "short": {0x7f, 'E', 'L', 'F'}} {
		if checkELF(head, "amd64") == nil {
			t.Errorf("%s was taken for an amd64 program", name)
		}
	}
}

func TestFetchProgram(t *testing.T) {
	f := newFakeRegistry(t)
	bin := filepath.Join(t.TempDir(), "gluetun", "bin")
	fetch := func(tag, arch string) (program, *fetcher, error) {
		return fetchProgram(context.Background(), f.Client(), f.reg(), tag, arch, bin)
	}
	p, got, err := fetch("latest", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p.Path)
	if p.Source != "downloaded" || p.Digest != f.index || p.Version != "v9.9.9 (abcdef0)" || got.blobs != 1 || // (only the layer with the program)
		p.Path != filepath.Join(bin, strings.TrimPrefix(f.index, "sha256:"), programName) || st == nil || st.Mode().Perm() != 0o755 || len(leftovers(bin)) != 0 {
		t.Fatalf("the download: %+v, %d layers, %v", p, got.blobs, st)
	}
	f.set("")
	p, got, err = fetch("latest", "amd64") // the digest is already kept: the tag is looked up and nothing else
	if err != nil || p.Source != "cached" || got.blobs != 0 || f.asked["blob"] != 0 || f.asked["manifest"] != 1 {
		t.Fatalf("the second time: %+v, %v, asked %v", p, err, f.asked)
	}
	if p, _, err = fetch("single", "amd64"); err != nil || p.Digest != f.tags["single"] { // a tag that is one image, no index
		t.Fatalf("one image by itself: %+v, %v", p, err)
	}
	if _, _, err = fetch("single", "arm64"); err == nil || !strings.Contains(err.Error(), "not linux/arm64") {
		t.Errorf("an image for another processor: %v", err)
	}
	if _, _, err = fetch("latest", "riscv64"); err == nil {
		t.Errorf("an index without this processor was accepted")
	}
	if _, _, err = fetch("nosuchtag", "amd64"); err == nil {
		t.Errorf("a tag that does not exist was accepted")
	}

	// the copy in the cache is changed by one byte: it is not used, and is fetched again
	if p, _, err = fetch("single", "amd64"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.Path)
	data[len(data)-1] ^= 1
	os.WriteFile(p.Path, data, 0o755)
	if _, err := cached(bin, p.Digest, "amd64"); err == nil || !strings.Contains(err.Error(), "checksum changed") {
		t.Errorf("a changed copy in the cache: %v", err)
	}
	if p, _, err = fetch("single", "amd64"); err != nil || p.Source != "downloaded" {
		t.Errorf("after a changed copy: %+v, %v", p, err)
	}
}

func TestFetchSpoiled(t *testing.T) {
	f := newFakeRegistry(t)
	for what, c := range map[string][2]string{
		"the layer":          {f.layer, "a downloaded layer does not match its digest"},
		"the index":          {f.index, "the manifest does not match its digest"},
		"the image manifest": {f.tags["single"], "the manifest does not match its digest"},
	} {
		bin := filepath.Join(t.TempDir(), "bin")
		f.set(c[0])
		p, _, err := fetchProgram(context.Background(), f.Client(), f.reg(), "latest", "amd64", bin)
		if err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Errorf("%s with one byte changed: got %+v, %v", what, p, err)
		}
		if all, _ := os.ReadDir(bin); len(all) != 0 {
			t.Errorf("%s with one byte changed: %d files were left behind", what, len(all))
		}
	}
}

func TestChooseProgram(t *testing.T) {
	f := newFakeRegistry(t)
	oldRegs, oldTry := registries, tryProgram
	defer func() { registries, tryProgram = oldRegs, oldTry }()
	registries, tryProgram = []registry{f.reg()}, func(string) error { return nil }
	if runtime.GOARCH != "amd64" {
		t.Skip("the canned image is for amd64")
	}
	l := &launcher{dir: t.TempDir(), prog: builtinProgram}
	bin := binDir(l.dir)
	if p := l.chooseProgram("builtin"); p.Path != gluetun || p.Source != "builtin" || f.asked["token"] != 0 {
		t.Fatalf("builtin: %+v, asked %v", p, f.asked)
	}
	if p := l.chooseProgram("latest"); p.Source != "downloaded" || p.Digest != f.index {
		t.Fatalf("the first start: %+v", p)
	}
	var now map[string]string
	data, _ := os.ReadFile(filepath.Join(bin, "current.json"))
	json.Unmarshal(data, &now)
	if now["tag"] != "latest" || now["digest"] != f.index || now["source"] != "fake/r/g" || now["fetched_at"] == "" || len(now) != 4 {
		t.Errorf("current.json: %s", data)
	}
	f.set("")
	if p := l.chooseProgram("latest"); p.Source != "cached" || f.asked["token"] != 0 { // asked a moment ago: not again
		t.Errorf("a second start soon after: %+v, asked %v", p, f.asked)
	}
	l.progAsked = time.Now().Add(-recheckAfter - time.Minute)
	if p := l.chooseProgram("latest"); p.Source != "cached" || f.asked["manifest"] != 1 || f.asked["blob"] != 0 {
		t.Errorf("a start hours later: %+v, asked %v", p, f.asked)
	}
	if p := l.chooseProgram("single"); p.Source != "downloaded" || len(keptPrograms(bin)) != 2 {
		t.Errorf("another tag: %+v, %d kept", p, len(keptPrograms(bin)))
	}

	// the registry is gone: the last good copy runs; the look-up is not repeated at once
	f.Close()
	l.progAsked = time.Time{}
	if p := l.chooseProgram("latest"); p.Source != "cached" || p.Digest != f.index {
		t.Errorf("no registry, a copy kept: %+v", p)
	}
	if l.progOK || time.Since(l.progAsked) > time.Minute {
		t.Errorf("a failed look-up was not noted")
	}
	tryProgram = func(file string) error { // the newest copy does not run on this machine
		if strings.Contains(file, strings.TrimPrefix(f.index, "sha256:")) {
			return os.ErrPermission
		}
		return nil
	}
	if p := l.chooseProgram("latest"); p.Source != "cached" || p.Digest != f.tags["single"] {
		t.Errorf("no registry, the newest copy does not run: %+v", p)
	}
	os.RemoveAll(bin)
	if p := l.chooseProgram("latest"); p.Source != "builtin" || p.Path != gluetun {
		t.Errorf("no registry, nothing kept: %+v", p)
	}
}

func TestTidy(t *testing.T) {
	bin := t.TempDir()
	var names []string
	for i, c := range []string{"a", "b", "c", "d"} {
		names = append(names, "sha256:"+strings.Repeat(c, 64))
		dir := filepath.Join(bin, strings.Repeat(c, 64))
		os.MkdirAll(dir, 0o755)
		at := time.Now().Add(time.Duration(i-10) * time.Hour)
		os.Chtimes(dir, at, at)
	}
	os.WriteFile(filepath.Join(bin, ".fetch-1.tmp"), []byte("x"), 0o644)
	tidy(bin, names[0]) // the oldest is the one in use: it stays, with the newest of the others
	if got := strings.Join(keptPrograms(bin), " "); got != names[3]+" "+names[0] || len(leftovers(bin)) != 0 {
		t.Errorf("kept %s", got)
	}
}

func TestGluetunVersion(t *testing.T) {
	s := goodSettings()
	if v, ok := s.gluetunVersion(); v != "latest" || !ok {
		t.Errorf("not set: %q", v)
	}
	_, key, _ := (&launcher{dir: "/config"}).gluetunEnv(s)
	s.VPN.Vars["gluetun_version"] = "v3.41.3"
	env, key2, names := (&launcher{dir: "/config"}).gluetunEnv(s)
	if strings.Contains(strings.Join(env, " ")+strings.Join(names, " "), "GLUETUN_VERSION") || key == key2 {
		t.Errorf("gluetun_version must not be handed to gluetun, and a change of it must start the VPN again")
	}
	if shown, _ := json.Marshal(s.masked()); !strings.Contains(string(shown), `"gluetun_version":"v3.41.3"`) || len(s.validate()) != 0 {
		t.Errorf("the page is not shown gluetun_version: %s", shown)
	}
	for _, bad := range []string{"../x", "a b", "sha256:abc", "-x"} {
		s.VPN.Vars["gluetun_version"] = bad
		if errs := s.validate(); len(errs) != 1 || errs[0].Field != "vpn.gluetun_version" {
			t.Errorf("%q: got %v", bad, errs)
		}
	}
}

// A build the VPN did not connect with after an update is left alone, here and after a restart.
func TestSkippedBuild(t *testing.T) {
	f := newFakeRegistry(t)
	oldRegs, oldTry := registries, tryProgram
	defer func() { registries, tryProgram = oldRegs, oldTry }()
	registries, tryProgram = []registry{f.reg()}, func(string) error { return nil }
	if runtime.GOARCH != "amd64" {
		t.Skip("the canned image is for amd64")
	}
	l := &launcher{dir: t.TempDir(), prog: builtinProgram}
	bin := binDir(l.dir)
	if p := l.chooseProgram("single"); p.Source != "downloaded" {
		t.Fatalf("the first build: %+v", p)
	}
	if p := l.chooseProgram("latest"); p.Source != "downloaded" || p.Digest != f.index {
		t.Fatalf("the newer build: %+v", p)
	}
	if skipped(bin) != "" {
		t.Fatalf("nothing is skipped yet: %q", skipped(bin))
	}
	writeWhole(filepath.Join(bin, "skip.json"), map[string]string{"digest": f.index})
	l.progAsked = time.Time{}
	if p := l.chooseProgram("latest"); p.Digest == f.index || p.Digest != f.tags["single"] {
		t.Errorf("the skipped build was chosen, or the one before was not: %+v", p)
	}
	os.WriteFile(filepath.Join(bin, "skip.json"), []byte(`{"digest":"nonsense"}`), 0o644)
	if skipped(bin) != "" {
		t.Errorf("a damaged note skips something: %q", skipped(bin))
	}
}
