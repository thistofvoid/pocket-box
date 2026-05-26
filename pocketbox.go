package pocketbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

// ----------------------------------------------------------------------------
// Public configuration
// ----------------------------------------------------------------------------

// Options configures the s&box auth integration. The zero value is valid and
// uses the defaults documented on each field.
type Options struct {
	// CollectionName is the auth collection that holds Steam players.
	// Default: "players".
	CollectionName string

	// Route is the HTTP path of the auth endpoint.
	// Default: "/api/sbox-auth".
	Route string

	// ServiceName is informational only — the auth-method label attached to
	// the issued token and visible in PocketBase logs/meta.
	// Default: "sbox".
	ServiceName string

	// AutoMigrate controls whether the collection is created automatically on
	// bootstrap if it does not already exist.
	// Default: true.
	AutoMigrate *bool

	// Timeout is the HTTP timeout for the Facepunch verification call.
	// Default: 8s.
	Timeout time.Duration

	// BodyLimitBytes caps the request body size for the auth endpoint.
	// Default: 4096.
	BodyLimitBytes int64

	// FacepunchURL overrides the verification endpoint. Intended for tests.
	// Default: the official Facepunch services URL.
	FacepunchURL string

	// HTTPClient lets callers inject a custom client (proxies, tracing, tests).
	// Default: a client with Timeout applied.
	HTTPClient *http.Client

	// OnNewPlayer, if set, is called once when a player record is first
	// created, before it is saved. Use it to set default fields. The record
	// already has steam_id, email and a random password populated.
	OnNewPlayer func(app core.App, record *core.Record) error

	// OnAuth, if set, is called on every successful authentication after the
	// record is saved and before the response is written. Use it to refresh
	// profile data, log analytics, etc.
	OnAuth func(app core.App, record *core.Record) error
}

func (o *Options) applyDefaults() {
	if o.CollectionName == "" {
		o.CollectionName = "players"
	}
	if o.Route == "" {
		o.Route = "/api/sbox-auth"
	}
	if o.ServiceName == "" {
		o.ServiceName = "sbox"
	}
	if o.AutoMigrate == nil {
		t := true
		o.AutoMigrate = &t
	}
	if o.Timeout == 0 {
		o.Timeout = 8 * time.Second
	}
	if o.BodyLimitBytes == 0 {
		o.BodyLimitBytes = 4096
	}
	if o.FacepunchURL == "" {
		o.FacepunchURL = DefaultFacepunchURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.Timeout}
	}
}

// DefaultFacepunchURL is the official s&box auth token verification endpoint.
const DefaultFacepunchURL = "https://services.facepunch.com/sbox/auth/token"

// ----------------------------------------------------------------------------
// Entry points
// ----------------------------------------------------------------------------

// Register wires the s&box auth integration into the given PocketBase app.
// It is the simplest entry point and is all most callers need.
func Register(app core.App, opts Options) *Plugin {
	p := New(opts)
	p.Attach(app)
	return p
}

// Plugin is a configured, reusable s&box auth integration. Create one with
// New and attach it to an app with Attach. Most callers should just use
// Register instead.
type Plugin struct {
	opts     Options
	verifier *Verifier
}

// New builds a Plugin from the given options without attaching it to an app.
func New(opts Options) *Plugin {
	opts.applyDefaults()
	return &Plugin{
		opts: opts,
		verifier: &Verifier{
			url:    opts.FacepunchURL,
			client: opts.HTTPClient,
		},
	}
}

// Verifier exposes the underlying token verifier, e.g. if you want to verify
// tokens from your own custom routes.
func (p *Plugin) Verifier() *Verifier { return p.verifier }

// Attach binds the plugin's hooks and routes to a PocketBase app.
func (p *Plugin) Attach(app core.App) {
	if *p.opts.AutoMigrate {
		app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			return p.ensureCollection(app)
		})
	}

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		se.Router.POST(p.opts.Route, p.handleAuth(app)).
			Bind(apis.BodyLimit(p.opts.BodyLimitBytes))
		return se.Next()
	})
}

// ----------------------------------------------------------------------------
// Verifier — Facepunch token verification
// ----------------------------------------------------------------------------

// Verifier validates s&box auth tokens against the Facepunch API.
type Verifier struct {
	url    string
	client *http.Client
}

type facepunchResponse struct {
	SteamId int64  `json:"SteamId"`
	Status  string `json:"Status"`
}

// ErrInvalidToken is returned when Facepunch rejects a token or the verified
// SteamID does not match the claimed one.
var ErrInvalidToken = errors.New("sboxauth: invalid token")

// Verify checks a token for the given SteamID. It returns nil if the token is
// valid, ErrInvalidToken if it is rejected, or another error if the request
// to Facepunch fails.
func (v *Verifier) Verify(ctx context.Context, steamID int64, token string) error {
	if token == "" {
		return fmt.Errorf("%w: empty token", ErrInvalidToken)
	}

	payload, _ := json.Marshal(map[string]any{
		"steamid": steamID,
		"token":   token,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.url,
		strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("sboxauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("sboxauth: facepunch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sboxauth: facepunch status %d", resp.StatusCode)
	}

	var fr facepunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return fmt.Errorf("sboxauth: decode response: %w", err)
	}

	if fr.Status != "ok" {
		return fmt.Errorf("%w: status=%q", ErrInvalidToken, fr.Status)
	}
	if fr.SteamId != steamID {
		return fmt.Errorf("%w: steamid mismatch (claimed=%d verified=%d)",
			ErrInvalidToken, steamID, fr.SteamId)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Collection bootstrap
// ----------------------------------------------------------------------------

func (p *Plugin) ensureCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(p.opts.CollectionName); err == nil {
		return nil // already exists
	}

	col := core.NewAuthCollection(p.opts.CollectionName)
	col.Fields.Add(&core.TextField{
		Name:     "steam_id",
		Required: true,
		Max:      20,
	})
	col.Fields.Add(&core.TextField{Name: "display_name", Max: 64})
	col.AddIndex("idx_"+p.opts.CollectionName+"_steam_id", true, "steam_id", "")

	// Players may only read/update their own record; creation is server-side.
	ownerRule := "id = @request.auth.id"
	col.ListRule = &ownerRule
	col.ViewRule = &ownerRule
	col.UpdateRule = &ownerRule
	col.CreateRule = nil
	col.DeleteRule = nil

	if err := app.Save(col); err != nil {
		return fmt.Errorf("sboxauth: create collection: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// HTTP handler
// ----------------------------------------------------------------------------

type authRequest struct {
	SteamID     string `json:"steamid"`
	Token       string `json:"token"`
	DisplayName string `json:"display_name"`
}

func (p *Plugin) handleAuth(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		var body authRequest
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("invalid request body", err)
		}

		steamID, err := strconv.ParseInt(body.SteamID, 10, 64)
		if err != nil || steamID <= 0 {
			return e.BadRequestError("invalid steamid", nil)
		}

		// 1. Verify with Facepunch.
		if err := p.verifier.Verify(e.Request.Context(), steamID, body.Token); err != nil {
			app.Logger().Warn("sboxauth: verification failed",
				"steamid", steamID, "err", err.Error())
			return e.UnauthorizedError("token verification failed", nil)
		}

		// 2. Find or create the player's auth record.
		collection, err := e.App.FindCollectionByNameOrId(p.opts.CollectionName)
		if err != nil {
			return e.InternalServerError("players collection missing", err)
		}

		record, err := e.App.FindFirstRecordByData(
			p.opts.CollectionName, "steam_id", body.SteamID)
		isNew := err != nil
		if isNew {
			record = core.NewRecord(collection)
			record.Set("steam_id", body.SteamID)
			record.SetEmail(body.SteamID + "@steam.local")
			record.SetPassword(security.RandomString(40))
			record.SetVerified(true)

			if p.opts.OnNewPlayer != nil {
				if err := p.opts.OnNewPlayer(app, record); err != nil {
					return e.InternalServerError("OnNewPlayer hook failed", err)
				}
			}
		}

		if body.DisplayName != "" {
			record.Set("display_name", body.DisplayName)
		}

		if err := e.App.Save(record); err != nil {
			return e.InternalServerError("could not persist player", err)
		}

		if p.opts.OnAuth != nil {
			if err := p.opts.OnAuth(app, record); err != nil {
				return e.InternalServerError("OnAuth hook failed", err)
			}
		}

		// 3. Issue a standard PocketBase auth token + record payload.
		return apis.RecordAuthResponse(e, record, p.opts.ServiceName, nil)
	}
}
