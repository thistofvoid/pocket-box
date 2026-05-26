# pocketbox

A small library that adds **s&box (Facepunch) authentication** to a [PocketBase](https://pocketbase.io) application.

It registers a custom auth endpoint that verifies an s&box auth token against the Facepunch API and returns a standard PocketBase auth token. Steam players become normal PocketBase auth records — the JS/C# SDKs, API rules, realtime subscriptions, and file storage all work unchanged.

## Features

- **One-line setup** — `pocketbox.Register(app, pocketbox.Options{})` and you're done.
- **Real token verification** — forwards tokens to the Facepunch API and checks both the status and that the verified SteamID matches the claimed one.
- **Auto-migration** — creates the players auth collection on first boot (optional).
- **Standard auth tokens** — issues normal PocketBase JWTs, so the official SDKs treat Steam users like any other authenticated record.
- **Extension hooks** — `OnNewPlayer` and `OnAuth` callbacks for game-specific logic.
- **Testable** — the Facepunch endpoint is overridable, so you can point it at a mock server in tests.

## Installation

```sh
go get github.com/thistofvoid/pocketbox
```

This library is used alongside PocketBase as a Go framework:

```sh
go get github.com/pocketbase/pocketbase
```

## Quick start

```go
package main

import (
	"log"

	"github.com/pocketbase/pocketbase"
	"thistofvoid/pocketbox"
)

func main() {
	app := pocketbase.New()

	pocketbox.Register(app, pocketbox.Options{})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
```

Run it:

```sh
go run . serve
```

On first boot PocketBase prints a link to create the superuser account, and the library auto-creates the `players` auth collection. The admin dashboard is at `http://localhost:8090/_/`.

## How it works

The auth flow has two phases:

1. **In-game**, the s&box client requests a token:

   ```csharp
   var token = await Sandbox.Services.Auth.GetToken("YourServiceName");
   ```

2. The client sends `{steamid, token}` to the `/api/sbox-auth` endpoint. The library verifies the token with Facepunch, finds or creates the player's auth record, and returns a standard PocketBase auth token. The client then uses that token for all subsequent PocketBase requests.

The player's SteamID is always derived from the verified Facepunch response — never trusted from the request alone. A mismatch between the claimed and verified SteamID is rejected.

## API

### `POST /api/sbox-auth`

Request body:

```json
{
  "steamid": "76561197960287930",
  "token": "facepunch-auth-token",
  "display_name": "PlayerName"
}
```

`display_name` is optional. On success, returns a standard PocketBase auth payload:

```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "record": {
    "id": "abc123def456",
    "collectionName": "players",
    "steam_id": "76561197960287930",
    "display_name": "PlayerName",
    "verified": true
  }
}
```

Error responses follow PocketBase conventions:

| Status | Meaning                              |
| ------ | ------------------------------------ |
| `400`  | Invalid request body or steamid      |
| `401`  | Token rejected by Facepunch          |
| `500`  | Collection missing or database error |

## Configuration

`Options` controls the integration. The zero value is valid and uses the defaults below.

| Field            | Type            | Default               | Description                                       |
| ---------------- | --------------- | --------------------- | ------------------------------------------------- |
| `CollectionName` | `string`        | `"players"`           | Auth collection holding Steam players             |
| `Route`          | `string`        | `"/api/sbox-auth"`    | HTTP path of the auth endpoint                    |
| `ServiceName`    | `string`        | `"sbox"`              | Auth-method label on the issued token             |
| `AutoMigrate`    | `*bool`         | `true`                | Create the collection on bootstrap if missing     |
| `Timeout`        | `time.Duration` | `8s`                  | HTTP timeout for the Facepunch call               |
| `BodyLimitBytes` | `int64`         | `4096`                | Max request body size for the endpoint            |
| `FacepunchURL`   | `string`        | official URL          | Verification endpoint (override for tests)        |
| `HTTPClient`     | `*http.Client`  | client with `Timeout` | Custom HTTP client                                |
| `OnNewPlayer`    | `func`          | `nil`                 | Called once when a player record is first created |
| `OnAuth`         | `func`          | `nil`                 | Called on every successful authentication         |

### Example with hooks

```go
pocketbox.Register(app, pocketbox.Options{
	CollectionName: "steam_users",
	Route:          "/api/login",
	OnNewPlayer: func(app core.App, r *core.Record) error {
		r.Set("coins", 100) // starting balance for new players
		return nil
	},
	OnAuth: func(app core.App, r *core.Record) error {
		app.Logger().Info("player logged in", "steam_id", r.GetString("steam_id"))
		return nil
	},
})
```

`OnNewPlayer` fires once at account creation, before the record is saved. `OnAuth` fires on every login, after the record is saved and before the response is written.

## Storing player data

To store game data, create a regular collection (e.g. `player_state`) with a `relation` field pointing at the players collection, then set its API rules so players only access their own rows:

```
List / View / Update rule:   player.id = @request.auth.id
Create rule:                  @request.auth.id != ""
```

PocketBase enforces these automatically. Clients use the normal SDK CRUD calls with the token from `/api/sbox-auth`.

## Advanced usage

`Register` is the simple entry point. For more control, build and attach a `Plugin` directly:

```go
plugin := pocketbox.New(pocketbox.Options{})
plugin.Attach(app)

// Reuse the verifier from your own custom routes
verifier := plugin.Verifier()
err := verifier.Verify(ctx, steamID, token)
```

`Verify` returns the sentinel error `pocketbox.ErrInvalidToken` (checkable with `errors.Is`) when a token is rejected, distinct from network errors — so you can tell a bad token apart from Facepunch being unreachable.

## Testing

Because real Facepunch tokens are short-lived and awkward to obtain, point the library at a mock server in tests:

```go
mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"Status":  "ok",
		"SteamId": 76561197960287930,
	})
}))
defer mock.Close()

pocketbox.Register(app, pocketbox.Options{
	FacepunchURL: mock.URL,
})
```

## Curl example

```sh
curl -X POST http://localhost:8090/api/sbox-auth \
  -H "Content-Type: application/json" \
  -d '{"steamid":"76561197960287930","token":"YOUR_TOKEN","display_name":"TestPlayer"}'
```

Use the returned `token` for authenticated requests:

```sh
curl http://localhost:8090/api/collections/players/records \
  -H "Authorization: TOKEN_FROM_RESPONSE"
```

## License

MIT
