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

For an hour after a game ends, however it ended, a reply to one of its
messages (in a DM, one of its photos) that asks the bot gets "That game is
over. Start another with /geo." instead of the model's answer. The bot
keeps this in memory only, so a restart forgets it.

Best of 3 wins: the nearest guess wins a round, and whoever wins the most
rounds wins the game, with points shown alongside. All 3 rounds are
played. If the top players are tied on wins, a tie-breaker round follows,
with the game's spare photo:
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
in `/geo offroad AU,NZ 90s`, and its intro says "off-road". Before any
photo is downloaded, GeoGuess sends the spots of up to 25 of an area's
candidate photos to OpenStreetMap's Overpass API in one query, which
returns the drivable roads within about 40 m of each (`highway` motorway
to residential, service, living street, busway and their links) and the
paths within about 20 m (path, footway, track, bridleway, steps,
cycleway, less sidewalks and crossings), with their shapes, and counts
for each spot the park, nature reserve, garden, common or recreation
ground (`leisure`), wood, scrub, heath, grassland, beach, sand, bare
rock, scree or wetland (`natural`), forest, meadow, grass, farmland,
vineyard or orchard (`landuse`) and national park or protected area
(`boundary`) areas it lies in (Overpass works that out, so a national
park's outline is never downloaded). GeoGuess then measures the roads
and paths itself: a drivable road within 40 m puts a spot on a road,
however green its surroundings (so a dashcam on a divided road beside a
park is turned down); otherwise it is off roads if a path is within 20 m
or it is inside one of those areas, and anything else is turned down.
Only the spots that pass are downloaded. The check is as good as
OpenStreetMap's map there: an unmapped road or trail can fool it.
Overpass is asked at most once every 2 seconds, one query for each area
searched, so off-road photos are slower to find. Overpass being busy,
rate-limiting or blocking the query stops that search, and off-road
searches there wait a minute while other searches go on. One search runs
at a time; a search only refilling the stock is stopped when a game
starts gathering photos of another kind, but one for another game isn't,
and may take up to 3 minutes. A plain `/geo` uses an off-road photo only
when the stock has more than it keeps for off-road games.

`/geo end` (or `stop`) reveals the round and the totals; in the lobby it
calls the game off. `/geo skip` goes on to the next round without
scoring this one. Only the player who started the game may do either.

The keys of the last 1,000 photos used are kept, so none comes round
again soon.

### A game's photos

A game never searches for a photo once it has begun. `/geo` reserves a
photo for each of its rounds, and a spare for a tie-breaker (4 in all for
3 rounds), from the stock of photos kept ready (below). With a photo for
each round in hand, or within about 3 seconds, the game starts as above.
Otherwise, in a DM or a group without a lobby, `/geo` answers "🔎 Fetching
spots for 3 rounds…" ("off-road spots" for `/geo offroad`); in a lobby,
`/geo` answers with the intro at once, the photos are gathered while
players join, and if some are still missing
when the countdown ends the bot posts "🔎 Still fetching: 2 of 3 spots
ready…". The search for a game's photos comes before any other, and round
1 follows as soon as each round has its photo; the spare may still be
found during play. Meanwhile `/geo` says round 1's photo is on its way, a
guess is told the game hasn't started, and the starter's `/geo end` calls
it off.

A game gathers photos for at most 5 minutes from `/geo`. By then, with
some, it plays that many rounds, saying so first ("Only found 2 spots, so
this game is 2 rounds."); with none, the bot posts "I couldn't find a
spot, so the game is off." ("an off-road spot" for `/geo offroad`). If
the top players are tied after the last round and the game has no spare,
points decide. However a game ends, the photos it reserved but didn't
show go back to the stock, where a game still gathering photos may take
them. A restart carries on gathering, to the same limit. A game being
started (its first few seconds) can't be ended yet: `/geo end` says to
try again in a moment.

### The stock

GeoGuess keeps photos ready, found ahead by one search at a time:
`GEO_BUFFER` (default 4, a full game with its spare) for `GEO_REGIONS`,
and a full game's 4 for each `GEO_KEEP` region and for each of the 6 kinds
of game (a region, `offroad`, or both, as in `offroad,AU`) most recently
played in the last 7 days, so after one `/geo offroad` the next usually
starts at once. The kinds are taken in turn, so a slow one (off-road
photos are) can't hold up the rest, and one whose search fails waits its
own backoff (5 seconds, doubling to 5 minutes, or a minute if Overpass is
busy) while the others go on. The stock is kept on disk, so it survives a
restart; at most 32 photos are kept in all, the stock and the photos games
have reserved together. If what the stock is kept filled with comes to
more than that (a large `GEO_BUFFER`, or many `GEO_KEEP` regions), some
kinds stay short; photos of a kind no longer kept are dropped first, then
the oldest. A game takes the photos found for its own kind first, then
those another kind can spare, then any other in its region (but an
off-road one only if spare).

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
minutes, 5 to gather its photos, then at most 45 for the game), so the
default `watch_max` of 1h is enough. A watch is set once, by `/geo`'s
answer, and can't be extended. `/geo` waits at most about 3 seconds for
photos before it answers, so any `timeout` above 5 seconds will do.
GeoGuess never waits past the bot's timeout anyway, less a few seconds to
send the answer, and starts no game the bot has given up on.

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
| `GEO_STATE_DIR` | signalbot's `addons/geo` | Where GeoGuess keeps `geo.json`, its log and its photos (`photos/`). Created, mode 700, if missing; GeoGuess exits at start if it can't create, list and write it |
| `GEO_MAPILLARY_TOKEN` | empty | Mapillary client token (`MLY\|…`), sent as `Authorization: OAuth <token>`. Empty means Panoramax only, and then rounds are in France unless `GEO_REGIONS` says otherwise |
| `GEO_MEMLIMIT` | `200MiB` | Memory limit, applied with `debug.SetMemoryLimit`, written as `GOMEMLIMIT` is (`B`, `KiB`, `MiB`, `GiB`, `TiB`), at least `16MiB` |
| `GEO_ROUND` | `3m` | Default round length, 30s to 30m |
| `GEO_REGIONS` | empty (world) | Continents (`europe`, `asia`, `africa`, `north-america`, `south-america`, `oceania`) or ISO country codes, comma-separated. Not `offroad`, which is a kind of game: use `GEO_KEEP=offroad` |
| `GEO_MIN_QUALITY` | `0.5` | Minimum Mapillary `quality_score` (0 to 1), when a photo has one |
| `GEO_MAX_AGE_YEARS` | `10` | Ignore older photos; more than 0, up to 100 |
| `GEO_BUFFER` | `4` | Checked photos kept ready for `GEO_REGIONS` (1 to 20); 4 is a full game with its spare |
| `GEO_KEEP` | empty | Regions to always keep a full game's photos ready for, comma-separated, in `GEO_REGIONS`'s names: `AU,NZ` keeps a game's worth for `/geo AU` and for `/geo NZ`. `offroad` keeps them for `/geo offroad`, and `offroad AU` for `/geo offroad AU` (commas separate kept regions, so `offroad,AU` keeps two: off roads anywhere, and in Australia). Kept as the regions games used lately are (see [the stock](#the-stock)), but for good |
| `GEO_NOMINATIM_URL` | `https://nominatim.openstreetmap.org` | Reverse geocoder for the place names; `https://` only, as it is sent each round's answer |
| `GEO_OVERPASS_URL` | `https://overpass-api.de/api/interpreter` | Overpass API interpreter for the off-road check, such as another instance's; `https://` only, as it is sent candidate photos' spots |
| `GEO_PANORAMAX_URL` | `https://api.panoramax.xyz` | Panoramax API (a STAC catalogue), such as one instance's; `https://` only |
| `GEO_MAPILLARY_URL` | `https://graph.mapillary.com` | Mapillary Graph API; `https://` only |

It reaches `graph.mapillary.com` and Mapillary's image CDN,
`api.panoramax.xyz` and the Panoramax instances' image hosts, the
Nominatim server, and for off-road games the Overpass server
(`overpass-api.de`), over TLS 1.3.

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
coordinates to Nominatim for the place's name, and in an off-road game
candidate photos' coordinates (nothing else) to Overpass, to check they
are off roads, each with the user agent
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

- the photos kept ready and those open games reserved: each one's
  source, ID, author, licence, link, where it was taken and the place's
  name, when it was found and the kind of game it was found for, and
  whether it passed the off-road check; its JPEG is in
  `$GEO_STATE_DIR/photos/`, one file each (mode 600), deleted once the
  photo is shown or leaves the stock. At most 32 are kept; at start,
  stocked photos over 7 days old are dropped, as are any whose file is
  missing;
- the kinds of game (regions, `offroad`) played in the last 7 days, the 6
  most recent, which are kept stocked.

`$GEO_STATE_DIR/geoguess.log` (moved to `geoguess.log.1` past 4 MiB at
start) records lobbies and their player counts, rounds started, ended
early and revealed with their guess counts, photos rejected by reason
(such as "on a road" and "not off-road", an off-road game's spot on a
road or neither on a path nor in a park, nature or countryside, or
"captioned", a photo with a caption bar burned into its top or bottom
taller than 12% of it; a shorter bar is cropped off), and API errors by status. It never holds a guess, a coordinate, a place or
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
  DM the answer is round 1's photo and its caption, or, with photos
  missing, the "Fetching" text, and round 1's photo follows as a `post`
  with the image. In a group, if the bot says it takes `watchReactions`,
  it is the lobby's intro and the watch has `reactions`; round 1's photo
  follows as a `post` when the lobby starts the game (after a "Still
  fetching" `post` if photos are missing). A recorded guess answers with
  the reaction 📍. `/geo` gives up in time for the bot's `deadline`.
- When a round ends GeoGuess queues a `post` with the results, its players
  as `{u_…}` placeholders, then the next round's photo, one the game
  reserved, as a `post` with the image; after the last round, the totals
  and an `unwatch`. `/geo end` (or `stop`) and
  `/geo skip` answer with the results themselves. Ops the bot hasn't read
  are saved in `geo.json`, with their IDs, so they survive a restart.
- On start, games still open get their timers back (a lobby's too, and
  the limit on gathering photos), and rounds whose time passed while
  GeoGuess was down are revealed at once.
