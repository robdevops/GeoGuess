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

## Quick start

You need signalbot main at `d685491` or newer, and Go 1.27 or newer.

1. **Build it:**

   ```
   git clone https://github.com/robdevops/GeoGuess.git
   cd GeoGuess
   CGO_ENABLED=0 go build -ldflags='-s -w' -o /tmp/geoguess . && sudo install -D -m 755 /tmp/geoguess /opt/signalbot-addons/geoguess
   ```

2. **Get a Mapillary token.** Register an app at
   [mapillary.com/dashboard/developers](https://www.mapillary.com/dashboard/developers)
   and copy its client token (`MLY|…`). Put it in signalbot's environment
   file (`/etc/signalbot.env`):

   ```
   GEO_MAPILLARY_TOKEN=MLY|…
   ```

   Without it, photos come from Panoramax only, in France unless
   `GEO_REGIONS` says otherwise.

3. **Add it to the MCP servers file** (`MCP_CONFIG`). It must be called
   `geo`. `chats: ["all"]` plays in every group; list group IDs to limit
   it, and add `"dms": true` to play in DMs too.

   ```jsonc
   "geo": {
     "command": "/opt/signalbot-addons/geoguess",
     "chat_commands": {"geo": {"tool": "geo", "direct": true, "example": "/geo 3m", "timeout": "60s"}},
     "addon": {"chats": ["all"], "post": true, "react": true, "watch": true, "watch_max": "1h"}
   }
   ```

4. **Restart signalbot** and type `/geo` in the group.

Everything else has a sensible default. All the settings, the
permissions, and the Docker and systemd notes are in
[docs/DETAILS.md](docs/DETAILS.md#settings-in-full).

## Credits and privacy

Photos come from Mapillary and Panoramax, and place names from Nominatim.
Each round's results credit the photo's author, source and licence. Nothing
goes to the bot's model: commands and guesses never reach one. GeoGuess
never logs names or message text.

More: [docs/DETAILS.md](docs/DETAILS.md)
