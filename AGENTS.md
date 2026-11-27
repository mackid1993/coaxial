# AGENTS.md: buffering history and rules for changing Coaxial

Every time playback through Coaxial (and the proxies before it) buffered, froze, spun, looped or
dropped: the real cause, how it was found, the fix, and the rules that came out of it. Read it before
touching the stream path, anything on a timer, or the tests. Coaxial's job is narrow: pass the
provider's bytes on as they arrive, share one provider connection per channel, rejoin a dropped channel
cleanly, and send it all through the VPN. It is a proxy, not a player.

## Rules for anyone changing Coaxial

1. **Nothing talks to the provider or the VPN on a timer while anything plays or records.** No account
   reads, list downloads, server pings, speed tests or health checks that leave the box. Background work
   runs only when nothing plays, as rarely as the job allows (hourly is the default, never minutes),
   and the page says "when nothing is playing". Every build that broke this caused freezes or
   spinners that stopped the moment the work was removed. Two things do run while something plays,
   by design and small: the status page's exit-address look-up and its VPN pings, only while the page
   is open; and gluetun's own server-list updater, off by default and only on when the user sets
   `update_hours`. Every stop, move or update of the VPN checks what plays right before it acts (no
   answer from the proxy part counts as playing, video still flowing counts as playing).
2. **Never hold or delay video in the proxy.** No reserve, no smoothing, no pacing of the opener, no
   steady delivery for recorders, no held tail packet. Anything held is lost when a recorder hangs up on
   the minute, and every reshaping of the provider's bursts tried so far made players worse.
3. **Measure a stream fix as seconds of video in hand at the player, never bytes.** Bytes reaching the
   player in the same second they arrive from the provider do not clear the proxy. Compute, per reader,
   video duration (PCR/PTS) delivered ahead of wall clock at the moment the next lump arrives after a
   provider pause. On the player-lead bench's recorded feed (`live_lead_test.go`, `COAXIAL_REPLAY_TS`) a
   healthy build's fewest seconds in hand are 5.8 s for the first player and 7.8 s for the joiner, with 0
   stalls through a drop; the broken build fell to 0.3 s. (Earlier write-ups said 9 to 11 s, measured on
   another feed at another moment: compare a change with the bench's own numbers before and after.)
4. **Never rewrite proven live-path code to fix a rare glitch found only by synthetic tests.** The live
   path in `main.go` is the game-day build's (`bbbd5bf`). Any change to it must first match or beat that
   build on the player-lead bench (`live_lead_test.go`, first opener and joiner, through a provider drop).
   A change that loses lead is a regression no matter how many frame-exact bugs it fixes.
5. **Test on real recorded feeds.** Replay a recorded feed at its recorded read timing
   (`COAXIAL_REPLAY_TS`, `COAXIAL_REPLAY_CLIPS`). Fakes must act like real providers: an opening burst,
   6 to 12 s lumps, reads ending mid-packet, drops, one session per channel, kick-oldest at the cap,
   302 redirects. A fake written to behave the way the fix assumes proves nothing.
6. **6 to 12 s provider lumps are normal.** Every provider seen sends live MPEG-TS as an opening burst,
   then lumps 6 to 12 s apart. Never report the lumps as a finding or as the cause of a freeze.
7. **When the owner says a regression is in our code, it is.** Start from that premise and list
   everything new that runs on the playback path, stream code and background features alike. Do not
   answer with "the bytes are on time", "the provider paused" or a player theory. Only the owner's own
   A/B (credentials straight in the player, no Coaxial) moves a symptom outside Coaxial. If it works
   direct and fails through Coaxial, Coaxial fixes it, even when it passed the bytes unchanged.
8. **"It was fine before build X" means any build since the last smooth one.** Diff from the build the
   owner last watched smoothly, not only from the newest commit.
9. **Always forward.** Never redeploy or rebuild an old commit to bisect. To undo a bad change, restore
   the whole affected file from the trusted build inside a new commit and add back only the glue that
   newer code needs. Piecemeal hunk restores missed the cause twice.
10. **No restart while anything plays or records** unless the owner says so for that moment (it can
    leave a corrupt spot that freezes a recording for good). Pull logs first: a recreate wipes them.
11. **A test that fails correct design behavior is a bad test.** Delete it; do not change the code to
    satisfy it. Every new live-path test must pass on the game-day build first.
12. **Coaxial must never be worse than no proxy.** If common open-source players cope with something a
    provider sends (unflagged keyframes, clock restarts), Coaxial must cope too, with a small fail-safe
    that leaves normal streams byte-identical.
13. **Clean up only what you made** (never `docker image prune` on a shared host) and stop your own
    pollers before playback. Public text names no provider, host, device or person; the pre-push scan
    must block the push, not just print.
14. **Every wait on a path a player uses is a freeze waiting to happen.** A provider connection can go
    quiet without closing; Coaxial exists to run through a VPN tunnel, where this happens. Assume the
    player is simple and never recovers on its own: it waits for whatever Coaxial waits, and a player
    that does reconnect lands back on the same shared channel. So no wait on a live path may be
    longer than a player's own (ExoPlayer reads 8 s, AetherEngine 20 s): give up, ask again at once on a
    new connection, and carry a half-sent piece on with a Range. HLS uses 8 s (`pieceWait`). Live
    MPEG-TS uses longer waits on purpose: `liveQuiet` 20 s between reads, because healthy providers pause
    15 to 17 s between lumps, and `firstWait` 12 s for a channel's first answer and video, counted from
    the ask, so a provider that needs a few seconds still plays as it would straight at a player. When one path gets a timeout fix, check
    the same rule on every other path (live MPEG-TS, HLS playlists, HLS pieces, the HLS-to-TS bridge,
    HLS built from TS) in the same change. Watch for these log lines; each one is a connection that went
    quiet: `asked again on a new connection`, `stopped part way`, `a channel asked for again`.
15. **Do not stop until the owner's freeze is found in our code.** When he reports freezes, the answer
    is never "the provider paused" or "it can't be reproduced". List every wait, retry and timeout on
    the path his device uses, read each one, and test each with a fake that goes quiet (not one that
    closes or keeps sending). A log filtered for noise is not evidence that a device was absent: count
    its lines unfiltered first.
16. **Every rule is tested both ways, on a provider that acts like a real one.** A test that a rule acts
    when it should is half a test: the other half is that it stays quiet through healthy real use. Real
    providers state a target duration their playlists do not keep (stated 4 s, moving every 6.2 to
    6.7 s; stated 11 s, moving every 11 to 13.3 s), send pieces slowly at times, stand still 10 to
    16 s on busy channels, refuse reopens, and go quiet without closing. The `*_realistic_test.go`
    stand-ins do this; use them, replay real paces logged on a live system where you can, and prove a
    new test bites by breaking the rule it guards in a scratch copy. The six real-clip replays skip in
    CI: run them by hand (`COAXIAL_REPLAY_CLIPS`, `COAXIAL_REPLAY_TS`, `COAXIAL_REPLAY_EPISODE`) after
    any change to the live path or the clock code. Fix every known bug before shipping, not after.

## Incident log

Dates are November 2026. "Commit" is in this repository unless the entry says otherwise.

### 1. Third-party restreaming proxies (Nov 8 to 11)
- **Seen:** live HLS froze and restarted about every 30 s; m3u8 fetches took 10 to 20 s; guides came
  back short or days old; a provider drop showed as a visible hitch.
- **Cause:** the proxies went back to the provider's front address on every HLS index refresh, and that
  address opens a new session (with new piece addresses) every time; newest session wins, so the old one
  died. One of them also queued m3u8 requests single file, sent 200 and then ignored a failed guide
  copy (a truncated guide looked complete), served a cached guide of any age, and spliced reconnects
  with no marker.
- **Found:** reading the proxies' source and fetching one m3u8 three times to compare piece names.
- **Fix:** proxies removed; nginx in front of the VPN that remembers each channel's provider redirect,
  so every reader stays on one session (later the relay's `/hl`, `c8f3326`).
- **Lesson:** one provider session per channel; an error after a 200 must abort the response.

### 2. A recorder and a viewer on one channel ended each other (Nov 11 to 14)
- **Seen:** a freeze about every 25 s, 10 s fragments in recordings, channels restarting in a loop.
- **Cause:** the provider allows one session per channel per account and counts an MPEG-TS request and an
  HLS request as two. The recorder pulled TS while players pulled HLS; a format mismatch between the
  channel-list manager and the players did the same.
- **Found:** request extensions per device in the logs; direct tests at the provider.
- **Fix:** the relay (`d61f19c`): one provider connection per channel shared by every reader, HLS-to-TS
  bridge. A first attempt inside nginx's script module leaked memory or damaged the stream and was
  dropped; the small Python relay was the first design that worked. Later HLS is cut from the shared TS
  (`34e4f89`).
- **Lesson:** mixed formats on one channel must never break playback; never answer "use one format".

### 3. Two days of stutter behind nginx (Nov 12 to 13)
- **Seen:** brief stutters on live HLS that came and went, on a player with a thin buffer.
- **Cause:** a new encrypted provider connection for every m3u8 and piece, spread over 20 nginx
  workers (the image default), each with its own pool. **Found:** the owner spotted the worker count.
- **Fix:** `dccf4ca` (one worker, kept-open connections to learned video servers; 99.5% reuse).
- **Lesson:** read the defaults nobody chose. Averages looked fine; the fault was the occasional 1 s.

### 4. A config reload dropped the player (Nov 13)
- **Seen:** a channel cut whenever a new video server was learned. **Cause:** learning one reloaded
  nginx, which retires the worker holding the player. **Fix:** batched reloads and a saved list
  (`dccf4ca`), gone with Caddy (`c8f3326`). **Lesson:** a reload mid-play is a stream drop.

### 5. Hours of buffering blamed on the feed were the VPN route (Nov 13 to 18)
- **Seen:** every device buffered for hours; agents blamed the provider's lumpy feed.
- **Cause:** a VPN host-name pin also matched a multi-hop route entering in another country (123 to
  238 ms, 10% loss) while the exit still showed the right city. One other server froze for unproven
  reasons.
- **Found:** pinging through the tunnel and listing every entry the pin matched.
- **Fix:** pin by server name, exclude multi-hop routes; the picker now filters them (`pick.go`).
- **Lesson:** measure the tunnel before blaming the provider. MPEG-TS has almost no player buffer, so a
  long route shows on TS and hides on HLS.

### 6. Agents' own tests disturbed live playback (Nov 12 to 18, and later)
- **Seen:** HLS stalls, channel restarts and spinners that lined up with agent activity.
- **Cause:** speed tests, downloads and side tunnels through the live tunnel; reads made straight at the
  provider, which ended the owner's session on that channel; heavy test runs on the same host and line.
- **Lesson:** while anything plays, read logs only. Read through Coaxial (it shares the session),
  never straight at the provider. Name your own runs first when a freeze is reported.

### 7. Recordings from an HLS provider grew in jumps (Nov 15)
- **Seen:** a spinner when tuning a channel being recorded. **Cause:** the HLS-to-TS bridge re-read the
  m3u8 a whole target duration late and handed pieces on in bunches. **Fix:** `231148e`.

### 8. Freeze-and-catch-up after a second device joined (Nov 15, Nov 21, Nov 22)
- **Seen:** a joiner showed a picture, raced ahead, then stopped at the next provider pause; later a
  player that hung up and re-asked during its tune got a thin start and spun at a 16 s pause; a reader in
  a channel's first seconds got one lump and then nothing.
- **Fix:** `28e3eaf` (joiner cushion), `5ea6d6d` (low-rate joins got nothing), `bf6abd8` (a re-ask with
  nobody else on the channel gets the full start), `a03cfef` (early joiner).
- **Lesson:** joiner and opener are different paths; read the `started`/`joined` log lines first.

### 9. Holding video in the proxy, tried four times, rejected every time (Nov 14 to 22)
- **Seen:** a per-provider buffer setting (Nov 14) was removed the same night. "Steady delivery" to
  recorders (`1151d4d`) cut the last seconds off recordings and was reverted (`bcf9295`). An adaptive
  reserve emptied at each whole minute (Nov 22) left the feed bare once a minute and froze. Pacing the
  opener (Nov 21) gave spinners and then fast-forward playback.
- **Real cause of the recording spinners they chased:** a bug in the DVR server's playback of a
  recording still in progress. The owner updated it and the spinners stopped, Coaxial's feed unchanged.
- **Lesson:** rule 2. Update the client before redesigning the proxy around its buffering.

### 10. Provider drop rewound the clock (Nov 21)
- **Seen:** a 20 s freeze; a recording jumped back 11.7 s. **Cause:** a re-asked provider starts with
  its opening burst from before the drop. **Fix:** `968c7ba` (`spliceAt`, next keyframe past the clock).

### 11. A restart during a recording froze it for good (Nov 21 to 22)
- **Seen:** the TV froze on one frame playing back a recording. **Cause:** the relay compiled for 15 s
  at start; the recorder got 502s and wrote a half packet and a 26 s clock jump into the file.
- **Fix:** prebuilt image that starts in under a second (`fba2184`); gluetun updates and VPN server
  moves wait until nothing plays (`gluetunfetch.go`, `pick.go`); rule 10.

### 12. Tune freeze on one player after the move to Caddy (Nov 20 to 21)
- **Seen:** a channel opened, froze about 1 s, lost the live marker, then played.
- **Cause:** not the proxy. The owner's own A/B reproduced it playing straight from the provider and on
  a line with none of this stack; multi-audio channels triggered a track switch in that player.
- **What went wrong:** a night of server-side reshaping deployed under a watching viewer. **Lesson:**
  the owner's direct A/B settles "proxy or player"; ask for it early and build nothing until it is in.

### 13. Catch-up of a live channel cut both every minute or two (Nov 22)
- **Seen:** a recording lost video at each cut; catch-up froze and jumped back.
- **Cause:** one provider counts catch-up of a channel as that channel; each side's re-ask ended the
  other. Another provider allowed both.
- **Fix:** `5e82b89`, `cce585a`, then the per-provider "one connection per channel" option (`67c78f5`).
- **Lesson:** provider rules differ; test on more than one before calling a rule standard.

### 14. The hardening rewrite froze every channel (Nov 22, 5 PM, to Nov 23, 4 AM)
- **Seen:** random buffering and freezes, worst in the first minute after a tune, on every channel.
  The game-day build before it had played smoothly all day.
- **Cause:** eight rounds of adversarial review rewrote the live path (`45a5043`, `cb2ab54`): a new
  provider reader, exact resume (`resumeAt`), PID renumbering, the joiner cushion in 64 KB pieces, and
  `whole()`, which held the last packet of every lump until the next lump. After a 12 s provider pause
  players kept 0.3 to 6.5 s in hand instead of 9 to 11 s. The exact guilty line was never named.
- **How it went:** agents argued for hours from per-second byte counts that delivery was real time, and
  called the held packet an accepted limit. Frame-exact tests all passed on the broken path. Two
  partial restores (`e31bef2`, `4bc41a1`) did not stop the freezes.
- **Found:** the owner insisted it was the build; another agent measured seconds in hand per reader.
- **Fix:** `dbe0f33` restored the whole game-day `main.go` with glue only; `78fa67b` put back the one
  glue line it lost (a stale admission hold caused false "limit reached" refusals). The review-era
  helpers and their tests were deleted (`ce34980`) and the player-lead test added.
- **Lesson:** rules 3, 4, 9 and 11. This is the incident the rules were written for.

### 15. A provider drop right after a channel change ended a recording (found Nov 23)
- **Cause:** "do not re-ask a dropped channel within 15 s of another tune" was meant for one-connection
  accounts but applied to all. **Found:** an overnight agent modeled the real sequence (a DVR records,
  the TV changes channel). **Fix:** `726c9ba`. **Lesson:** model real use, not abstract edge cases.

### 16. 24/7 channels looped at every episode change (Nov 23 to 24)
- **Seen:** at each new episode the player replayed video it already had, in a loop. Agents first said
  Coaxial passed the provider's bytes unchanged; it failed through Coaxial, so Coaxial owned the fix.
- **Cause:** the provider plays one file per episode and restarts PCR/PTS/DTS near zero mid-connection.
  It sets the discontinuity flag, but FFmpeg, VLC and Media3 ignore that flag, and FFmpeg reads the
  drop as a 33-bit wrap.
- **Fix:** `c0bec5c` (set the flag) was a dead end. `918fad9` (`clockFix`) adds one offset to every
  timestamp after a jump so the clock carries on; streams that never jump pass byte-identical.
- **Lesson:** check that real players use a mechanism before relying on it. Prove a stream fix on the
  captured failing bytes, in uneven read sizes, and prove a normal capture comes out unchanged.

### 17. After a clock restart, a reconnect rewound or stalled (Nov 24)
- **Cause:** `spliceAt` compared the last clock sent (with the offset) to the re-asked stream's raw
  clock, so the join landed in the wrong place: video repeated about 8 to 11 s, or readers stalled.
- **Found:** an overnight review, on the real functions. **Fix:** `4ecccfe` (compare raw clocks).
- **Lesson:** any state that rewrites timestamps must be undone wherever old and new clocks meet. Grep
  every reader of a rewritten value before shipping.

### 18. VPN checks during playback gave spinners every few minutes (Nov 23 to 24)
- **Seen:** spinners about once per stream on regular channels, right after a build.
- **Cause:** an in-tunnel speed check while playing (`4617174`), then pings to every VPN server every
  5 minutes and to the one in use every minute, during playback (`3357919`). **Found:** the owner said
  the new code did it; agents first answered that the pings were tiny and left outside the tunnel.
- **Fix:** no pinging while anything plays (`33e7a0d`), then hourly and idle only (`09b7165`). Spinners
  stopped at once. A 3.5 hour A/B later could not reproduce the mechanism; the owner's result stands.

### 19. A packet cut short broke the clock fix; a provider restart ended the channel (Nov 24)
- **Cause:** `clockFix` walked a fixed 188-byte grid from the start, so one short packet from the
  provider made it miss every later restart (and risked rewriting payload bytes). Separately, a drop
  followed by 404s while the provider restarted the channel ended it for every reader.
- **Found:** an idle-time survey that recorded real channels through Coaxial and scanned them.
- **Fix:** `8426df7` (track the real packet start and resync; re-ask with backoff for about 60 s while
  readers remain, through the same gates as the first re-ask). Proven by sweeping every read size on the
  real clips, new build against old.

### 20. Background account reads froze a news channel (Nov 24 evening)
- **Seen:** a news channel froze a few minutes after tuning, on the first evening of a new build.
- **Cause:** `e952bdd` logged in to each provider at start, every 30 s while any read failed, then
  every 10 minutes, to learn the connection limit: the only new code making requests during playback.
  The mechanism was never captured (a recreate wiped the evidence).
- **Fix:** removed (`2eed69a`); the limit is a number the user types (`d944ee6`). No freezes since. An
  earlier watch loop logged in to every provider every 20 s (removed in `45a5043`).
- **Lesson:** rule 1. Anything the user knows, the user types in; Coaxial does not poll for it.

### 21. Random freezes for weeks: quiet connections held players up to a minute (Nov 25)
- **Seen:** random freezes on live TV through Coaxial since its first build, on HLS and on MPEG-TS,
  that came back on their own. One was logged as "sent nothing for 18 s" on a news channel.
- **Cause:** every live path waited far longer than any player on a connection that had gone quiet
  without closing: live MPEG-TS 60 s (`quietWait`) before asking for the channel again; an HLS piece
  through the pass-through 60 s; an HLS playlist reload 25 s plus 10 s to connect, after which the
  channel was opened again at the provider, which one provider refuses (403) for minutes. A player
  asking direct would have reconnected after its own 8 to 20 s.
- **How it went:** `c316ea1` and `2a20913` gave the HLS-to-TS bridge an 8 s limit, but nobody checked the
  same rule on the paths players use, so the freezes went on. The agent then retracted the fix. Two
  agents blamed the provider. One read a log filtered for noise, concluded the TV was not on Coaxial at
  all, and built a theory on that. Every fake provider in the tests either sent data or closed; none
  went quiet, so nothing caught it. The owner said throughout that it was our code. He was right.
- **Fix:** `liveQuiet` (20 s) for live MPEG-TS, with a quiet connection asked again however long it ran;
  `livePlaylist` and `resumeBody`: 8 s (`pieceWait`), then a new connection, a Range for a half-sent
  piece, and no re-open of the channel for a quiet reload. Tests: `quiet_connection_test.go` (four
  fail on the old code; a normal 17 s provider pause is still left alone).
- **Lesson:** rules 14 and 15.

### 22. A live HLS player froze for good on a stuck provider session (Nov 26)
- **Seen:** twice in one day, a player alone on a live HLS channel froze on one picture and stayed
  frozen on every channel until its app was closed. Nothing failed in the log.
- **Cause:** the provider's session for the channel stopped moving (12.5 s on 5 s pieces, about 11 s
  on 9 s pieces) while a new session moved at once. Coaxial pins every viewer to one session, so it
  kept handing the player the stopped playlist; the player's own recovery hung. Straight at the
  provider, a fresh tune gets a fresh session.
- **How it went:** the first fix only renewed when the player asked for the channel's own address
  again, and at 15 s; the second only when it re-tuned. The player that froze only reloaded. The next
  fix renewed on reloads after 1.5 target durations, and on a live evening it opened new sessions
  20 times in half an hour on a channel whose playlist moves slower than its stated target (each
  found the provider no further on). Independent reviews then found the re-tune path never learned
  the pace, a refused re-tune was retried at every refresh, and the long-piece ceiling was too high.
  The tests had passed every time because every stand-in moved exactly on its stated target.
- **Fix:** `6ed5382`, `dcb640b`, `b352eb3`, `da64dc8`, `22546f0`, `a05f853`: a lone device's session is
  renewed on a reload or a re-tune once its playlist has stood still past `loneLimit` (1.5 target
  durations, at most `stallCap` 10 s, never under one target); a renewal that finds the provider at
  most one piece on teaches the channel its pace (`learned`, kept per channel in `paces`) and holds
  until the playlist moves; refused renewals wait `renewGap`. Stall lengths are logged exactly
  (`logMove`). Tests: `hls_real_cadence_test.go` (real paces replayed), `hls_*realistic_test.go`.
- **Also found by the same reviews and fixed:** a channel's first MPEG-TS connection that went quiet
  was never asked again (504 after 20 to 30 s; `06e4a5f`, `firstWait`); a proxy part that did not
  answer read as "nothing plays" before a VPN server move or update (`52437ae`); a film with no video
  content type did not count as playing (`52437ae`); a failing VPN health check restarted a tunnel
  video still used (`0b5a82a`: judged by video through the tunnel, as VPN clients judge theirs by
  traffic).
- **Lesson:** rule 16. A fix tested only against fakes that behave the way the fix assumes passed
  six times and misfired on the first real evening.

### Not in Coaxial (so no one re-chases them)
- A network switch near one TV slowed down every few days (300 ms pings); a scheduled power cycle fixed
  it. Ping the player first when everything is slow. Also incidents 9 and 12 (client side), late-night
  slow provider servers and catch-up files with clock jumps, both identical with bare credentials.

## Common threads of buffering

1. **Background traffic during playback.** Account logins every 20 s, then every 10 minutes; a speed
   check; ping sweeps every minute; agents' own tests. Each was small, each was defended as harmless,
   and each lined up with freezes that stopped when it stopped. While something plays, add none.
2. **Reshaping or holding the provider's bytes.** Buffer settings, steady delivery, reserves, opener
   pacing, joiner cushions handed out wrong, a held tail packet, the rewritten reader. The provider's
   burst-and-lump shape is what players are built for; every attempt to improve it in the proxy cost
   players their lead or cost recorders their last seconds.
3. **Measuring the wrong thing.** Bytes acked, frame counts, CC errors and "delivery in the same second"
   all looked perfect on builds that froze. Seconds of video in hand per reader was the number that
   separated good builds from bad, and only on a real recorded feed.
4. **Synthetic review loops rewriting proven code.** Reviewers find what their fakes act out. Fakes that
   behaved the way the fix assumed passed the fix, and the fixed code froze in real use.
5. **Reconnect and clock edges.** Opening bursts that rewind the clock, providers restarting clocks per
   file, short packets, 404s during a channel restart, and an offset compared with a raw clock. Each
   was found on real feeds, fixed on captured bytes, and left normal streams byte-identical.
6. **Arguing instead of hunting.** In most incidents the first agent answer was "the provider pauses",
   "the player jumps to live" or "the bytes are on time". The owner was right every time it was our
   code; the two times it was not, his own A/B or a client update showed it. Start from the report,
   list what changed on the playback path, and remove the biggest suspect.
7. **Restarts under a viewer.** Deploy-observe-redeploy nights, reloads and surprise recreates made
   many of the freezes being chased. One tested deploy, only when nothing plays.
8. **Waits longer than a player's.** A quiet connection costs the player whatever Coaxial waits. Fix a
   timeout on every path at once, and test with a fake that goes quiet.
9. **Fakes that keep the rules the code assumes.** Playlists that move exactly on their stated target,
   pieces that arrive at once, a provider that never refuses. Every rescue and timer passed against
   them and misfired on real ones. Test both ways on realistic stand-ins (rule 16).
