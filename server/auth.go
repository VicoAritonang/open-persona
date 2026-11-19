package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"slices"
)

// Scopes an API key can hold.
const (
	ScopeKnowledge = "knowledge" // retrieve knowledge and system prompts; no LLM call
	ScopeChat      = "chat"      // answers: sync, streaming, async jobs, MCP ask
	ScopeDownload  = "download"  // fetch the pack file to run it locally
)

// AllScopes is every scope.
var AllScopes = []string{ScopeKnowledge, ScopeChat, ScopeDownload}

// Errors that Authenticator, Catalog and JobStore implementations return.
var (
	ErrUnauthorized = errors.New("missing or invalid API key")
	ErrNotFound     = errors.New("not found")
)

// Principal is the caller an API key identifies.
type Principal struct {
	KeyID    string   `json:"key_id"`
	Owner    string   `json:"owner"`
	Personas []string `json:"personas"` // persona ids, or "*" for all
	Scopes   []string `json:"scopes"`
}

// Can reports whether p may use scope on personaID ("" checks the scope only).
func (p *Principal) Can(scope, personaID string) bool {
	if p == nil || !slices.Contains(p.Scopes, scope) {
		return false
	}
	return personaID == "" || slices.Contains(p.Personas, "*") || slices.Contains(p.Personas, personaID)
}

// canAny reports whether p may use personaID in any way.
func (p *Principal) canAny(personaID string) bool {
	for _, sc := range AllScopes {
		if p.Can(sc, personaID) {
			return true
		}
	}
	return false
}

// Authenticator turns a bearer token into a principal, or returns
// ErrUnauthorized.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// StaticKey is a single key with every scope on every persona, for a
// self-hosted server with one client.
type StaticKey string

// Authenticate implements Authenticator.
func (k StaticKey) Authenticate(_ context.Context, token string) (*Principal, error) {
	if k == "" || subtle.ConstantTimeCompare([]byte(token), []byte(k)) != 1 {
		return nil, ErrUnauthorized
	}
	return &Principal{KeyID: "static", Owner: "static", Personas: []string{"*"}, Scopes: AllScopes}, nil
}

// OpenAccess lets every request through with every scope. Use it only for
// a server bound to localhost or behind another authenticating layer.
type OpenAccess struct{}

// Authenticate implements Authenticator.
func (OpenAccess) Authenticate(context.Context, string) (*Principal, error) {
	return &Principal{KeyID: "open", Owner: "open", Personas: []string{"*"}, Scopes: AllScopes}, nil
}

// AnyOf accepts a token that any of its authenticators accepts, in order:
// for example an operator's StaticKey next to the customer keys in a
// database.
type AnyOf []Authenticator

// Authenticate implements Authenticator.
func (a AnyOf) Authenticate(ctx context.Context, token string) (*Principal, error) {
	for _, x := range a {
		p, err := x.Authenticate(ctx, token)
		if err == nil || !errors.Is(err, ErrUnauthorized) {
			return p, err
		}
	}
	return nil, ErrUnauthorized
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p, as the authentication middleware
// does. Transports other than the built-in HTTP API (package rpc) use it.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the caller stored by WithPrincipal, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}
