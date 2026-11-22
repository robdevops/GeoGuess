# GeoGuess

A GeoGuessr-style game for [signalbot](https://github.com/robdevops/signalbot).
`/geo` posts a street-level photo, and players guess where it was taken by
sending a location or typed coordinates. When time is up, the bot names
the place and ranks players by how far off they were. A game is 3 rounds.

```
/geo                 start a game (rounds of GEO_ROUND, default 3 minutes)
/geo 90s | /geo 5m   rounds of that length (30s to 30m)
/geo europe          a game in a continent or countries (/geo AU,NZ)
/geo end, /geo stop  finish now, or call off the lobby (starter only)
/geo skip            skip this round without scoring (starter only)
/geo scores          this chat's totals over the last 7 days
```

## How to play

- **Join:** in a group, react to `/geo`'s answer with any emoji. The
  starter must react too. It starts 30s after the last join, once at
  least 2 have joined. It is called off after 5 minutes without 2.
- **Telegram:** the bot sees reactions only as the group's admin. With
  privacy mode on (the default), reply to the round's photo to guess.
- **DMs:** there is no lobby. The game starts at once, and a round ends
  as soon as you guess.
- **Answer:** send a location (Signal: **+** → Location; Telegram:
  📎 → Location), or type coordinates such as `-37.81, 144.96`.
- **Points:** the nearest guess wins a round. Most round wins takes the
  game, with points shown alongside. A tie gets a tie-breaker round.
- **Early end:** a round ends once all players have guessed.
- **No repeats:** none of the last 1,000 photos comes round again soon.
- **After:** for an hour, a reply to the game's messages that asks the
  bot gets "That game is over. Start another with /geo."

## Install

It needs signalbot main at d685491 or newer, and Go 1.27 or newer.

```
git clone https://github.com/robdevops/chat-plugin-geoguess.git
cd chat-plugin-geoguess
CGO_ENABLED=0 go build -ldflags='-s -w' -o /opt/signalbot-addons/geoguess .
```

Add it to the MCP servers file (`MCP_CONFIG`). It must be called `geo`:

```jsonc
"geo": {
  "command": "/opt/signalbot-addons/geoguess",
  "chat_commands": {"geo": {"tool": "geo", "direct": true, "example": "/geo 3m", "timeout": "60s"}},
  "addon": {"chats": ["<group id>"], "post": true, "react": true, "watch": true, "watch_max": "1h"}
}
```

- `post` sends the results.
- `react` puts 📍 on a counted guess.
- `watch` reads guesses and the lobby's reactions.
- `chats` takes group IDs, or `["all"]`. Add `"dms": true` to play in DMs too.
- Under Docker, mount `/opt/signalbot-addons` read-only at the same path.

The state dir needs no setup: signalbot sets it and GeoGuess creates it.

## Settings

| Variable | Default | Description |
|---|---|---|
| `GEO_STATE_DIR` | signalbot's `addons/geo` | Where it keeps `geo.json` and its log |
| `GEO_MAPILLARY_TOKEN` | empty | Mapillary client token (`MLY\|…`). Empty means Panoramax only, in France unless `GEO_REGIONS` says otherwise |
| `GEO_MEMLIMIT` | `200MiB` | Memory limit, written as `GOMEMLIMIT` is; at least `16MiB` |
| `GEO_ROUND` | `3m` | Default round length, 30s to 30m |
| `GEO_REGIONS` | empty (world) | Continents or ISO country codes, comma-separated |
| `GEO_MIN_QUALITY` | `0.5` | Minimum Mapillary `quality_score` (0 to 1) |
| `GEO_MAX_AGE_YEARS` | `10` | Ignore older photos (above 0, up to 100) |
| `GEO_BUFFER` | `3` | Checked photos kept ready (1 to 20) |
| `GEO_KEEP` | empty | Regions to always keep one photo ready for, such as `AU,NZ` |
| `GEO_WAIT` | `50s` | How long `/geo` waits for a photo, 5s to 110s. Keep the command's `timeout` above it |
| `GEO_NOMINATIM_URL` | `https://nominatim.openstreetmap.org` | Reverse geocoder for place names; `https://` only |
| `GEO_PANORAMAX_URL` | `https://api.panoramax.xyz` | Panoramax API; `https://` only |
| `GEO_MAPILLARY_URL` | `https://graph.mapillary.com` | Mapillary Graph API; `https://` only |

## Credits and privacy

Photos come from Mapillary and Panoramax, and place names from Nominatim.
Each round's results credit the photo's author, source and licence. Nothing
goes to the bot's model: commands and guesses never reach one. GeoGuess
never logs names or message text.

More: [docs/DETAILS.md](docs/DETAILS.md)
