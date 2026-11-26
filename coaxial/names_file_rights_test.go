package main

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// asProxyPart runs do with the rights over files that the proxy part has in the container: the launcher is
// root and starts it as a user with no privileges (65534). Only this goroutine's thread is changed, and it is
// put back.
func asProxyPart(do func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	syscall.Setfsgid(65534)
	syscall.Setfsuid(65534)
	defer func() { syscall.Setfsuid(0); syscall.Setfsgid(0) }()
	do()
}

// The channel names are kept in names.json in the settings folder so that a start does not lose them. The
// proxy part writes that file, and it runs without privileges; the settings folder is root's (the launcher or
// Docker made it, and only its owner may write in it). The proxy part could not make the file there, the
// failure was not said anywhere, and every start lost the names: the page showed bare channel numbers again
// until the next fetch, a day and an idle hour later.
func TestProxyPartKeepsChannelNamesInTheSettingsFolder(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root, as the launcher is in the container")
	}
	dir, err := os.MkdirTemp("", "coaxial-config-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o755) // the settings folder as Docker or the launcher makes it: root's, others may only read
	stray := filepath.Join(dir, "stray")
	asProxyPart(func() { err = os.WriteFile(stray, nil, 0o644) })
	if err == nil {
		t.Skip("rights over files cannot be given up here: the test would prove nothing")
	}

	const port = "18774"
	_, undo := namesProvider(t, port)
	defer undo()
	// (fetched as root, into the stand-in's own folder: only the names in memory matter here)
	<-fetchNames(port)
	// What the launcher does before it starts the proxy part, then the proxy part keeping its names.
	namesFile = namesHome(dir, 65534, 65534)
	asProxyPart(saveNames)
	mu.Lock()
	got := len(channelNames[port].names)
	delete(channelNames, port) // (the program starts again: nothing in memory)
	mu.Unlock()
	if got != 2 {
		t.Fatalf("fetched %d names, want 2", got)
	}
	asProxyPart(loadNames)
	mu.Lock()
	n := channelNames[port]
	mu.Unlock()
	if n == nil || n.names["1"] != "One" || n.names["2"] != "Two" {
		t.Fatalf("the proxy part runs without privileges and the settings folder is root's: after a start the channel names were gone (%+v)", n)
	}
	if list, _ := filepath.Glob(filepath.Join(dir, ".fetch-*")); len(list) != 0 {
		t.Errorf("files were left behind in the settings folder: %v", list)
	}
}

// Unraid makes an appdata folder root's with mode 0700: only root may enter it. The proxy part could not reach
// names.json there at all, though the file was its own, and every start (every image pulled) lost the names.
// The launcher now opens the file and hands it over open.
func TestProxyPartKeepsChannelNamesInAClosedSettingsFolder(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root, as the launcher is in the container")
	}
	dir, err := os.MkdirTemp("", "coaxial-config-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	os.Chmod(dir, 0o700) // the folder as Unraid makes it
	namesHome(dir, 65534, 65534)
	asProxyPart(func() { _, err = os.ReadFile(filepath.Join(dir, "names.json")) })
	if err == nil {
		t.Skip("rights over files cannot be given up here: the test would prove nothing")
	}

	const port = "18775"
	_, undo := namesProvider(t, port)
	defer undo()
	<-fetchNames(port)
	keptFile, keptFD := namesFile, namesFD
	defer func() { namesFile, namesFD = keptFile, keptFD }()
	namesFile, namesFD = filepath.Join(dir, "names.json"), namesOpen(dir) // what the launcher hands over
	if namesFD == nil {
		t.Fatal("the launcher could not open the names file")
	}
	defer namesFD.Close()
	asProxyPart(saveNames)
	mu.Lock()
	got := len(channelNames[port].names)
	delete(channelNames, port)
	mu.Unlock()
	if got != 2 {
		t.Fatalf("fetched %d names, want 2", got)
	}
	asProxyPart(loadNames)
	mu.Lock()
	n := channelNames[port]
	mu.Unlock()
	if n == nil || n.names["1"] != "One" || n.names["2"] != "Two" {
		t.Fatalf("in a settings folder only root may enter, the channel names were gone after a start (%+v)", n)
	}
}
