// The log: one readable line per event, with the facts as key=value fields after the message, for example
//
//	2026-11-25T01:20:00Z WRN coaxial: the provider answered 403 kind=stream channel=1234 device=10.0.0.20 provider=Main status=403 ms=210
//
// Built on zerolog (see ACKNOWLEDGMENTS.md). Lines are handed to a small ring buffer and written by a goroutine
// of their own, so the stream code never waits for the container's log to take a line; if the log ever falls
// that far behind, lines are dropped and counted, never video. How much is written is the "log detail" setting on
// the web page (ui.log_level in settings.json): detailed (the default), normal or quiet. It is applied at once,
// in the launcher and in the proxy part, without a restart. Never a login, a password, a key or an address
// with a login in it: provider requests are named by what was asked (kind and channel or item number).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/diode"
)

// The log detail levels, as settings.json and the web page name them.
const (
	logDetailed = "detailed" // everything about streams and provider answers, background requests (the default)
	logNormal   = "normal"   // starts, ends and every problem
	logQuiet    = "quiet"    // problems only
)

// Seconds a live stream may get nothing from its provider before it is logged. Providers send live video in
// lumps with pauses of 6 to 12 s all day long: that is normal and never logged.
const gapWarn = 15

// Seconds between the "still playing" lines of one stream (at the detailed level).
const statEvery = 600

var (
	lg       zerolog.Logger // every line goes through this one logger; where it goes can be switched (logTo)
	logOut   switchWriter
	logMu    sync.Mutex // only around switching where the log goes (start-up, exit, tests)
	logSink  io.Closer  // the ring buffer in use, if any
	dropped  atomic.Int64
	logLevel atomic.Value // the level's name, as set
)

// noClose: a writer whose Close is not passed on (the ring buffer's Close would close standard output).
type noClose struct{ io.Writer }

// switchWriter: the writer lines go to, switchable while other goroutines log.
type switchWriter struct{ w atomic.Pointer[io.Writer] }

func (s *switchWriter) Write(p []byte) (int, error) { return (*s.w.Load()).Write(p) }

func (s *switchWriter) set(w io.Writer) { s.w.Store(&w) }

// zerologLevel: the zerolog level for a detail level ("" and anything unknown: detailed).
func zerologLevel(name string) zerolog.Level {
	switch name {
	case logNormal:
		return zerolog.InfoLevel
	case logQuiet:
		return zerolog.WarnLevel
	}
	return zerolog.DebugLevel
}

func validLogLevel(name string) bool {
	return name == "" || name == logDetailed || name == logNormal || name == logQuiet
}

// consoleTo: zerolog's console format, plain text, UTC times, "coaxial:" before the message.
func consoleTo(w io.Writer) zerolog.ConsoleWriter {
	return zerolog.ConsoleWriter{Out: w, NoColor: true, TimeFormat: time.RFC3339, TimeLocation: time.UTC,
		FormatMessage: func(i any) string { return "coaxial: " + fmt.Sprint(i) }}
}

// logTo sends the log to w (os.Stdout in the program). wait=false: through the ring buffer (never blocks the
// caller); wait=true: written at once (tests read it back right after; w must take writes from several goroutines).
func logTo(w io.Writer, wait bool) {
	logMu.Lock()
	defer logMu.Unlock()
	old := logSink
	logSink = nil
	if wait {
		logOut.set(consoleTo(w))
	} else {
		d := diode.NewWriter(noClose{consoleTo(w)}, 4096, 0, func(n int) { dropped.Add(int64(n)) })
		logOut.set(d)
		logSink = d
	}
	if old != nil {
		old.Close() // (what waited in it is written out)
	}
}

// logFlush writes out what waits in the ring buffer (before the program exits); later lines are written at once.
func logFlush() {
	logMu.Lock()
	defer logMu.Unlock()
	if logSink != nil {
		logOut.set(consoleTo(os.Stdout))
		logSink.Close()
		logSink = nil
	}
}

// setLogLevel applies a detail level at once. The line saying so is written at the more detailed of the two.
func setLogLevel(name string) {
	if !validLogLevel(name) {
		name = ""
	}
	if name == "" {
		name = logDetailed
	}
	old, _ := logLevel.Load().(string)
	if old == name {
		return
	}
	logLevel.Store(name)
	lvl := zerologLevel(name)
	if old != "" && lvl > zerolog.InfoLevel {
		lg.Info().Str("detail", name).Msg("log detail changed")
	}
	zerolog.SetGlobalLevel(lvl)
	if old != "" && lvl <= zerolog.InfoLevel {
		lg.Info().Str("detail", name).Msg("log detail changed")
	}
}

func currentLogLevel() string {
	s, _ := logLevel.Load().(string)
	return s
}

func init() {
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC().Truncate(time.Second) }
	zerolog.TimeFieldFormat = time.RFC3339
	lg = zerolog.New(&logOut).With().Timestamp().Logger()
	logTo(os.Stdout, false)
	setLogLevel(logDetailed)
	go func() { // lines the log could not take, said once a minute at most
		for range time.Tick(time.Minute) {
			if n := dropped.Swap(0); n > 0 {
				lg.Warn().Int64("lines", n).Msg("the log fell behind and some lines were left out")
			}
		}
	}()
}

// say: a plain line at the normal level, as the program always wrote. Never an address or a login.
func say(format string, a ...any) { lg.Info().Msgf(format, a...) }

// warn: a problem.
func warn(format string, a ...any) { lg.Warn().Msgf(format, a...) }

// ---------------------------------------------------------------------------
// What a request to a provider (or anything else outside) was for, so its answer can be logged without its
// address: the kind of request, the channel or item number, the device it was for and the provider's name.
// ---------------------------------------------------------------------------

type askLog struct {
	kind, id, device, provider string
	idName                     string // what id is: "channel" (when empty), "item" (a movie or an episode), "action"
	background                 bool   // a request Coaxial makes on its own (no player asked for it)
	routine                    bool   // asked over and over while something plays (playlist reloads, video pieces):
	//                                 a good answer is not logged at all, only a bad one
}

func (a *askLog) fields(e *zerolog.Event) *zerolog.Event {
	e = e.Str("kind", a.kind)
	if a.id != "" {
		e = e.Str(or(a.idName, "channel"), a.id)
	}
	if a.device != "" {
		e = e.Str("device", a.device)
	}
	if a.provider != "" {
		e = e.Str("provider", a.provider)
	}
	return e
}

// who: who answered, as the message names it.
func (a *askLog) who() string {
	if a.background {
		return "background: " + a.kind
	}
	return "the provider"
}

// answered logs a provider's answer: anything but 2xx and 3xx as a warning, the rest at the detailed level
// (routine asks: not at all).
func (a *askLog) answered(status int, took time.Duration) {
	if a == nil {
		return
	}
	if status >= 400 || status < 200 {
		a.fields(lg.Warn()).Int("status", status).Int64("ms", took.Milliseconds()).
			Msg(a.who() + " answered " + statusWords(status))
		return
	}
	if a.routine {
		return
	}
	a.fields(lg.Debug()).Int("status", status).Int64("ms", took.Milliseconds()).Msg(a.who() + " answered " + statusWords(status))
}

// failed logs a request that got no answer: a warning, unless the one it was for had already gone (called
// off), which is noted at the detailed level.
func (a *askLog) failed(err error, calledOff bool, took time.Duration) {
	if a == nil {
		return
	}
	if calledOff {
		if !a.routine {
			a.fields(lg.Debug()).Int64("ms", took.Milliseconds()).Msg(a.who() + ": the request was called off (nobody waits for it any more)")
		}
		return
	}
	var r *refused
	e := a.fields(lg.Warn()).Int64("ms", took.Milliseconds())
	switch {
	case errors.As(err, &r):
		e.Int("status", r.status).Msg(a.who() + ": the request was refused here (an address that leads home, or not a web address)")
	case isTimeout(err):
		e.Str("reason", "timed out").Msg(a.who() + " did not answer in time")
	default:
		e.Str("reason", pyName(err)).Msg(a.who() + " could not be reached")
	}
}

func statusWords(status int) string {
	if t := http.StatusText(status); t != "" {
		return fmt.Sprintf("%d (%s)", status, strings.ToLower(t))
	}
	return fmt.Sprint(status)
}

// ---------------------------------------------------------------------------
// One live stream from a provider, as the log tells it: its re-asks, any silence of gapWarn seconds or more,
// a line every statEvery seconds (detailed level) and one when it ends. Only the stream's own goroutine
// touches it, so it needs no lock; per read it adds a few numbers and compares two times.
// ---------------------------------------------------------------------------

type streamStats struct {
	a                       *askLog
	opened, lastByte, since float64 // when asked for; the last bytes from the provider; when this stretch began
	total, part             int64   // bytes in all, and since the last "still playing" line
	longest, partLongest    float64 // longest silence in all, and in this stretch
	reasks                  int
}

func newStreamStats(a *askLog) *streamStats {
	if a == nil {
		a = &askLog{kind: "stream"}
	}
	now := mono()
	return &streamStats{a: a, opened: now, since: now}
}

func (s *streamStats) fields(e *zerolog.Event) *zerolog.Event {
	e = e.Str("channel", s.a.id)
	if s.a.device != "" {
		e = e.Str("device", s.a.device)
	}
	return e.Str("provider", s.a.provider)
}

// asking: what one ask of the stream is, for the log (the first, or a re-ask after the provider ended it).
func (s *streamStats) asking(again bool) *askLog {
	a := *s.a
	if again {
		a.kind = "re-ask"
	}
	return &a
}

// got: n bytes came from the provider.
func (s *streamStats) got(n int) {
	now := mono()
	if s.lastByte > 0 {
		if gap := now - s.lastByte; gap >= gapWarn {
			s.fields(lg.Warn()).Float64("seconds", round1(gap)).
				Msgf("the provider sent nothing for %.0f s on channel %s, then carried on", gap, s.a.id)
			s.longest, s.partLongest = max(s.longest, gap), max(s.partLongest, gap)
		} else if gap > s.partLongest {
			s.partLongest = gap
			s.longest = max(s.longest, gap)
		}
	}
	s.lastByte = now
	s.total += int64(n)
	s.part += int64(n)
	if now-s.since >= statEvery {
		s.fields(lg.Debug()).Float64("mb", mb(s.part)).Float64("mbit_s", round1(float64(s.part)*8/1e6/(now-s.since))).
			Float64("longest_gap", round1(s.partLongest)).Float64("minutes", round1((now-s.opened)/60)).
			Msg("a channel is still playing")
		s.since, s.part, s.partLongest = now, 0, 0
	}
}

// end: the stream is over (err as its source returned it).
func (s *streamStats) end(ctx context.Context, err error) {
	took := mono() - s.opened
	reason, problem := "", true
	var r *refused
	switch {
	case ctx.Err() != nil || errors.Is(err, errStop):
		reason, problem = "nobody is watching it any more", false
	case err == nil:
		reason = "the provider closed it"
	case errors.As(err, &r):
		reason = "the provider answered " + strconv.Itoa(r.status)
	default:
		reason = endReason(err)
	}
	e := lg.Info()
	if problem {
		e = lg.Warn()
	}
	if s.total == 0 { // never played: its answer was logged already
		e = lg.Debug()
	}
	rate := 0.0
	if took > 0 {
		rate = float64(s.total) * 8 / 1e6 / took
	}
	s.fields(e).Str("reason", reason).Float64("seconds", round1(took)).Float64("mb", mb(s.total)).
		Float64("mbit_s", round1(rate)).Float64("longest_gap", round1(s.longest)).Int("reasks", s.reasks).
		Msg("a channel's stream from the provider ended")
}

// endReason: why a provider's stream ended, in words (never the error's text: it can hold the address).
func endReason(err error) string {
	switch {
	case err == nil:
		return "the provider closed it"
	case isTimeout(err):
		return "the provider sent nothing for too long"
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE):
		return "the provider broke the connection off"
	}
	return pyName(err)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

func mb(n int64) float64 { return round1(float64(n) / 1048576) }

// askFor: what a player's request at a provider is, for the log, from the address as the player wrote it
// (with the player login): its kind and number, never the address itself.
func (f *front) askFor(u *url.URL, device string) *askLog {
	a, p := &askLog{kind: "other", device: device, provider: f.name}, u.Path
	switch {
	case f.liveTS.MatchString(p):
		a.kind, a.id = "stream", channelID(p)
	case f.liveHLS.MatchString(p):
		a.kind, a.id = "live hls", channelID(p)
	case f.catchUpOf(u) != "":
		a.kind, a.id = "catch-up", f.catchUpOf(u)
	default:
		if m := f.vodPath.FindStringSubmatch(p); m != nil {
			a.kind, a.id, a.idName = map[string]string{"movie": "movie", "series": "episode"}[m[1]], channelID(m[2]), "item"
		} else if low := strings.ToLower(cleanPath(p)); low == "/player_api.php" || low == "/panel_api.php" {
			a.kind, a.idName = strings.TrimPrefix(strings.TrimSuffix(low, ".php"), "/"), "action"
			a.id = actionRe.ReplaceAllString(strings.ToLower(u.Query().Get("action")), "")
			a.id = a.id[:min(len(a.id), 40)]
			if a.id == "" {
				a.id = "login"
			}
		} else if low == "/xmltv.php" {
			a.kind = "guide"
		}
	}
	return a
}

var actionRe = regexp.MustCompile(`[^a-z_]`)
