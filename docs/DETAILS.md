# GeoGuess: details

The deep detail behind the [README](../README.md): game rules, install
notes, settings, privacy and the protocol.

## How a game goes

Players answer by sending a location (on Signal, the **+** button →
Location; on Telegram, 📎 → Location; then pinch to zoom out, drag the
pin, confirm on Signal, and send), or typed coordinates such as
`-37.81, 144.96`; when time is up the bot names the place and ranks
players by how far off they were.

A game is 3 rounds, 3 photos. A round ends at its time, or as soon as all
the game's players have guessed (3s later, for guesses still on their
way), so a game goes as fast as its players. Anyone else may still guess
and score, but isn't waited for. Each round's results come out when it
ends, then, 5 seconds later so they can be read, the next photo, whose
time starts then.

In a group, a game starts with a lobby: react to `/geo`'s answer with any
emoji to join (take the reaction back to leave). Whoever starts it must
react too. It starts 30 seconds after the last player joins, once there
are at least 2 of you, and is called off if fewer than 2 have joined after
5 minutes. On Telegram the bot sees reactions only as the group's admin,
so without that nobody can join. In a DM there's no lobby: the game
starts at once, and a round ends as soon as you guess.

If no photo is ready (often so for `/geo offroad`, a slow region, or just
after GeoGuess starts), `/geo` waits about 3 seconds for one, then starts
the game anyway. In a DM, or a group without a lobby, it answers "🔎
Fetching a spot… round 1 follows as soon as it's found." ("an off-road
spot" for `/geo offroad`), and posts round 1's photo once it is found.
In a lobby the photo is looked for while players join; if it still isn't
there when the lobby ends, the same "Fetching" message is posted and round
1 follows. Meanwhile `/geo` says round 1's photo is on its way, a guess is
told the game hasn't started, and the starter's `/geo end` calls it off.
With no photo 5 minutes after the fetch began (when `/geo` answered, or
when the lobby ended), the bot posts "I couldn't find a spot, so the game
is off." ("an off-road spot" for `/geo offroad`) and the game ends. A restart carries on waiting, to the same
limit.

For an hour after a game ends, however it ended, a reply to one of its
messages (in a DM, one of its photos) that asks the bot gets "That game is
over. Start another with /geo." instead of the model's answer. The bot
keeps this in memory only, so a restart forgets it.

Best of 3 wins: the nearest guess wins a round, and whoever wins the most
rounds wins the game, with points shown alongside. All 3 rounds are
played. If the top players are tied on wins, a tie-breaker round follows:
anyone may guess, but only the tied players can win it, and it ends once
they have all guessed. If none of them wins it, points decide.

A guess scores 5000·e^(−km/2000) points, rounded, and the full 5000
within 25 m.

A round's length is 30s to 30m. The game, with a possible tie-breaker,
lasts at most 45 minutes; `/geo` with rounds too long for that says so.
A length and regions combine, as in `/geo europe 90s`.

`/geo offroad` plays a game whose every round's photo was taken off
roads: on a footpath, track or trail, or in a park, nature, a beach or
the countryside. It combines with a length and regions in any order, as
in `/geo offroad AU,NZ 90s`, and its intro says "off-road". Before a
photo is downloaded, GeoGuess asks Nominatim what is at its spot (a
reverse lookup at zoom 17) and keeps it only if that is a path, footway,
cycleway, track, bridleway or steps (`highway`); anything `natural` or a
`mountain_pass`; a river, stream, waterfall or rapids (`waterway`); a park, nature reserve, garden, common, recreation
ground, golf course or beach resort (`leisure`); a national park or
protected area (`boundary`); forest, meadow, grass, farmland, vineyard,
orchard, allotments or a recreation ground (`landuse`); a viewpoint,
picnic site, camp site or hut (`tourism`); or a locality, island or islet
(`place`). A road, building, shop, address or a spot Nominatim can't
name is turned down. The check is approximate: it judges the nearest
feature Nominatim knows, so an unnamed trail may be passed over and a
road through a park may get through. That is one more request per photo
looked at, at Nominatim's one a second, so off-road photos are slower to
find (up to 6 are looked up in each area searched, and Nominatim
rate-limiting or blocking them stops the search for a minute);
`GEO_KEEP=offroad` keeps one ready. One search runs at a time, so a slow
off-road search can hold up a `/geo` for another region for up to its 3
minutes. While a round of a game with a region is played, the next
round's photo is already being looked for.
A plain `/geo` may use an off-road photo too, but leaves the one
`GEO_KEEP` keeps when another will do.

`/geo end` (or `stop`) reveals the round and the totals; in the lobby it
calls the game off. `/geo skip` goes on to the next round without
scoring this one. Only the player who started the game may do either.

The keys of the last 1,000 photos used are kept, so none comes round
again soon.

### Reaching the bot

On Signal nobody needs to mention the bot: a message that starts with
`/geo`, and any guess during a round, reaches it as it is. On Telegram
`/geo` works the same way, but plain guesses reach the bot only with
privacy mode off (or the bot an admin); with privacy mode on, which is the
default, players reply to the round's photo instead.

## How it runs

It is a separate program that signalbot starts as an
[MCP addon](https://github.com/robdevops/signalbot/blob/main/docs/FILES_LINKS_TOOLS_MCP.md#addons)
server. It speaks MCP 2025-06-18 over stdio (newline-delimited JSON-RPC),
with signalbot's addon extension, through signalbot's own Go package for
addons,
[`github.com/robdevops/signalbot/addon`](https://github.com/robdevops/signalbot/tree/main/addon),
and no other MCP library. It needs no model: its commands and guesses
never reach one.

## Install notes

Build it with Go 1.27 or newer, as a static binary, which the Docker image
needs (it has no shell or runtimes) and systemd is happy with.

The build downloads signalbot's module, at the commit `go.mod` names, for
its `addon` package; only that package, which needs nothing beyond Go's
standard library, is built into GeoGuess.

`-ldflags='-s -w'` leaves out debug information, taking the binary from
about 11 MB to 7.5 MB. Install it wherever you like, such as
`/usr/local/bin/geoguess` under systemd, and give that path as `command`.

Give its full path as `command`. Under systemd there's nothing to mount:
the unit's `ProtectSystem=strict` leaves `/opt` readable. Under Docker,
mount the directory read-only at the same path, by adding
`- /opt/signalbot-addons:/opt/signalbot-addons:ro` to the volumes in
`compose.yaml`, so the same file works for both. Then add it to the MCP
servers file (`MCP_CONFIG`). The server must be called `geo`, which is
what gives it the `GEO_` settings.

`chats` takes the groups' IDs as Signal or Telegram shows them, or
`["all"]`; add `"dms": true` to play in DMs too. GeoGuess needs `post`
(for the results), `react` (the 📍 on a counted guess) and `watch` (to
read guesses and the lobby's reactions).

### Timeouts

A game's watch lasts at most 55 minutes and 5 seconds (a lobby's 5
minutes, up to 5 fetching round 1's photo, then at most 45 for the game),
so the default `watch_max` of 1h is enough. `/geo` waits at most about 3
seconds for a photo before it answers, so any `timeout` above 5 seconds
will do. GeoGuess never waits past the bot's timeout anyway, less a few
seconds to send the answer, and starts no game the bot has given up on.

### State directory and memory

GeoGuess keeps its own files in `GEO_STATE_DIR`. There's nothing to set
up: signalbot sets it to `addons/geo` in its own state directory
(`/var/lib/signalbot/addons/geo` under systemd, `/data/addons/geo` in
Docker), and GeoGuess creates it, for the bot's user alone. To keep them
elsewhere, set `GEO_STATE_DIR` in `/etc/signalbot.env` to a directory the
bot's user may create or write; one you set already keeps working.

It shares the bot's memory limit, so allow for `GEO_MEMLIMIT` in
`mem_limit` or `MemoryMax`.

## Settings in full

signalbot passes an addon server called `geo` every `GEO_` variable from
its environment file, plus `PATH`, `HOME` and, if set, `TZ` (the round's
deadline is written in it), `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`,
`SSL_CERT_FILE` and `SSL_CERT_DIR` (so it reaches the photo sources
through the bot's proxy, and trusts its CA), and nothing else.

| Variable | Default | Description |
|---|---|---|
| `GEO_STATE_DIR` | signalbot's `addons/geo` | Where GeoGuess keeps `geo.json` and its log. Created, mode 700, if missing; GeoGuess exits at start if it can't create, list and write it |
| `GEO_MAPILLARY_TOKEN` | empty | Mapillary client token (`MLY\|…`), sent as `Authorization: OAuth <token>`. Empty means Panoramax only, and then rounds are in France unless `GEO_REGIONS` says otherwise |
| `GEO_MEMLIMIT` | `200MiB` | Memory limit, applied with `debug.SetMemoryLimit`, written as `GOMEMLIMIT` is (`B`, `KiB`, `MiB`, `GiB`, `TiB`), at least `16MiB` |
| `GEO_ROUND` | `3m` | Default round length, 30s to 30m |
| `GEO_REGIONS` | empty (world) | Continents (`europe`, `asia`, `africa`, `north-america`, `south-america`, `oceania`) or ISO country codes, comma-separated. Not `offroad`, which is a kind of game: use `GEO_KEEP=offroad` |
| `GEO_MIN_QUALITY` | `0.5` | Minimum Mapillary `quality_score` (0 to 1), when a photo has one |
| `GEO_MAX_AGE_YEARS` | `10` | Ignore older photos; more than 0, up to 100 |
| `GEO_BUFFER` | `3` | Checked photos kept ready (1 to 20) |
| `GEO_KEEP` | empty | Regions to always keep a photo ready for, comma-separated, in `GEO_REGIONS`'s names, one photo each: `AU,NZ` keeps one Australian and one New Zealand photo, so `/geo AU` answers at once. `offroad` keeps one off-road photo ready for `/geo offroad`, and `offroad AU` one off roads in Australia, for `/geo offroad AU` (commas separate kept regions, so `offroad,AU` keeps two photos: one off roads anywhere and one in Australia). Searched before topping up `GEO_BUFFER` (a region whose search fails is tried again after a backoff, and the buffer fills meanwhile), never dropped to make room or used by a `/geo` another photo can serve, and refilled once used |
| `GEO_WAIT` | `50s` | How long a game waits for a later round's photo when none is ready, 5s to 110s, before ending the game there. It doesn't delay `/geo`'s answer: with no photo ready, round 1 is fetched for up to 5 minutes |
| `GEO_NOMINATIM_URL` | `https://nominatim.openstreetmap.org` | Reverse geocoder for the place names; `https://` only, as it is sent each round's answer |
| `GEO_PANORAMAX_URL` | `https://api.panoramax.xyz` | Panoramax API (a STAC catalogue), such as one instance's; `https://` only |
| `GEO_MAPILLARY_URL` | `https://graph.mapillary.com` | Mapillary Graph API; `https://` only |

It reaches `graph.mapillary.com` and Mapillary's image CDN,
`api.panoramax.xyz` and the Panoramax instances' image hosts, and the
Nominatim server, over TLS 1.3.

## Privacy

GeoGuess gets only what signalbot's addon block allows: the text after
`/geo`, and during a game the messages that look like a location (a map
link or coordinates) and the emoji put on or taken off its messages, each
with the chat and sender as pseudonyms and the time the bot got it. It
never sees a phone number, account ID, group ID or, unless `names` is set,
a display name; it writes players as `{u_…}` and the bot puts their names
in. Guesses are claimed by the bot, so they are never answered, sent to
the model or kept in the chat's history.

What it sends out is about photos, never about players: map areas to
search and photo IDs to Mapillary and Panoramax, and the answer's
coordinates to Nominatim for the place's name (and, in an off-road game,
each candidate photo's coordinates, to check it is off roads), each with
the user agent
`geoguess (+https://github.com/robdevops/GeoGuess)`.

A guess deleted for everyone afterwards is not passed on to GeoGuess,
which keeps only its distance anyway.

### What it keeps

`$GEO_STATE_DIR/geo.json`, rewritten whole on each change:

- open games: the chat's pseudonym, who started it, who joined its lobby
  and who it waits for, its points and round wins so far, the open round's
  deadline, answer and credit, and for each player (a per-chat pseudonym)
  only how far their last guess was and when. Where anyone guessed is
  never kept, in memory or on disk;
- the keys of the last 1,000 photos used, so none comes round again soon;
- 7 days of points per chat, by player pseudonym;
- outbox ops the bot hasn't read yet, for at most an hour.

The photos kept ready are held in memory only, each marked if it passed
the off-road check; an open game's region keeps `offroad` for an
off-road game, so its later rounds are off roads too.

`$GEO_STATE_DIR/geoguess.log` (moved to `geoguess.log.1` past 4 MiB at
start) records lobbies and their player counts, rounds started, ended
early and revealed with their guess counts, photos rejected by reason
(such as "on a road", an off-road game's spot on a road), and API errors
by status. It never holds a guess, a coordinate, a place or
person's name, a pseudonym or message text.

## The protocol

The `addon` package speaks the protocol: the handshake, tool calls and
their cancellation, and the outbox the bot reads. GeoGuess adds:

- `geo` (`{args}`) and `geo_guess` (`{text}`, or `{reaction, removed}` for
  a reaction), both for the bot alone, so neither is offered to the model.
- The caption gives the steps for the network the bot said it is on
  (Signal's **+** button, or Telegram's 📎); with none, it gives both.
- Starting a game answers with one watch for `geo_guess` for the whole
  game, until 5s past its latest end, with `guard` "No hints during a
  round 😉" and `over` "That game is over. Start another with /geo.". In a
  DM the answer is round 1's photo and its caption, or, with no photo
  ready, the "Fetching" text, and round 1's photo follows as a `post`
  with the image. In a group, if the bot says it takes `watchReactions`,
  it is the lobby's intro and the watch has `reactions`; round 1's photo
  follows as a `post` when the lobby starts the game (after a "Fetching"
  `post` if it isn't ready). A recorded guess answers with the reaction
  📍. `/geo` gives up in time for the bot's `deadline`.
- When a round ends GeoGuess queues a `post` with the results, its players
  as `{u_…}` placeholders, then the next round's photo as a `post` with
  the image (from the buffer, waiting up to `GEO_WAIT`); after the last
  round, the totals and an `unwatch`. `/geo end` (or `stop`) and
  `/geo skip` answer with the results themselves. Ops the bot hasn't read
  are saved in `geo.json`, with their IDs, so they survive a restart.
- On start, games still open get their timers back (a lobby's too, and a
  fetch's limit), and rounds whose time passed while GeoGuess was down
  are revealed at once.
