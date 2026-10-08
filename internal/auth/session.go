package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// refreshMargin is how early an access token is renewed.
const refreshMargin = 60 * time.Second

// Session hands out valid access tokens, refreshing and persisting them as
// needed. It is safe for concurrent use.
type Session struct {
	Config Config
	Store  Store

	mu  sync.Mutex
	tok *Token
}

// ConfigFromEnv returns the Connect web client's settings, overridable with
// PRUSA_ACCOUNT_URL, PRUSA_CLIENT_ID and PRUSA_REDIRECT_URI.
func ConfigFromEnv() Config {
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	return Config{
		AccountURL:  get("PRUSA_ACCOUNT_URL", DefaultAccountURL),
		ClientID:    get("PRUSA_CLIENT_ID", DefaultClientID),
		RedirectURI: get("PRUSA_REDIRECT_URI", DefaultRedirectURI),
		HTTP:        &http.Client{Timeout: 30 * time.Second},
	}
}

// NewSession uses the OS keyring for storage.
func NewSession() *Session {
	return &Session{Config: ConfigFromEnv(), Store: KeyringStore{}}
}

// AccessToken returns a token valid for at least a minute.
func (s *Session) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok.Valid(refreshMargin) {
		return s.tok.AccessToken, nil
	}

	unlock, err := lockRefresh()
	if err != nil {
		return "", fmt.Errorf("locking token refresh: %w", err)
	}
	defer unlock()

	// Another process may have refreshed while we waited for the lock.
	stored, err := s.Store.Load()
	if err != nil {
		return "", err
	}
	if stored.Valid(refreshMargin) {
		s.tok = stored
		return stored.AccessToken, nil
	}
	for attempt := 0; ; attempt++ {
		fresh, err := s.Config.Refresh(ctx, stored.RefreshToken)
		if err == nil {
			if err := s.Store.Save(fresh); err != nil {
				return "", fmt.Errorf("saving refreshed session: %w", err)
			}
			s.tok = fresh
			return fresh.AccessToken, nil
		}
		var oe *OAuthError
		if !errors.As(err, &oe) || !oe.Revoked() {
			return "", err
		}
		// The token was rejected. If the stored session changed meanwhile (a
		// new `prusactl login`, or a process that didn't take the lock), use
		// that one; only clear a session that is still the rejected one, or a
		// fresh login would be thrown away.
		again, lerr := s.Store.Load()
		if lerr == nil && again.RefreshToken != stored.RefreshToken && attempt == 0 {
			if again.Valid(refreshMargin) {
				s.tok = again
				return again.AccessToken, nil
			}
			stored = again
			continue
		}
		if lerr == nil && again.RefreshToken == stored.RefreshToken {
			_ = s.Store.Clear()
		}
		s.tok = nil
		return "", fmt.Errorf("%w (the saved session was rejected: %v)", ErrNotLoggedIn, err)
	}
}

// Invalidate forgets an access token the server rejected, so the next call to
// AccessToken refreshes instead of reusing it.
func (s *Session) Invalidate(access string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != nil && s.tok.AccessToken == access {
		s.tok.Expiry = time.Time{}
	}
	// Also drop it from the shared cache if it is still there. Hold the refresh
	// lock so this load-and-save can't write back a refresh token another
	// process has just rotated.
	unlock, err := lockRefresh()
	if err != nil {
		return
	}
	defer unlock()
	if stored, err := s.Store.Load(); err == nil && stored.AccessToken == access {
		stored.AccessToken, stored.Expiry = "", time.Time{}
		_ = s.Store.Save(stored)
	}
}

// Login signs in with the user's Prusa Account credentials (see
// PasswordLogin) and stores the resulting tokens.
func (s *Session) Login(ctx context.Context, prompt Prompter) (*Token, error) {
	return s.save(s.Config.PasswordLogin(ctx, prompt))
}

// LoginManual signs in with a pasted redirect URL (see Config.ManualLogin) and
// stores the resulting tokens.
func (s *Session) LoginManual(ctx context.Context, announce func(authURL string), read func() (string, error)) (*Token, error) {
	return s.save(s.Config.ManualLogin(ctx, announce, read))
}

// save stores the tokens a login produced.
func (s *Session) save(tok *Token, err error) (*Token, error) {
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("Prusa Account issued no refresh token; the session could not be kept")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockRefresh()
	if err != nil {
		return nil, fmt.Errorf("locking the saved session: %w", err)
	}
	defer unlock()
	if err := s.Store.Save(tok); err != nil {
		return nil, fmt.Errorf("saving the session: %w", err)
	}
	s.tok = tok
	return tok, nil
}

// Logout forgets the stored session.
func (s *Session) Logout() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tok = nil
	unlock, err := lockRefresh()
	if err != nil {
		return fmt.Errorf("locking the saved session: %w", err)
	}
	defer unlock()
	return s.Store.Clear()
}

// SignedIn reports whether a session is stored, without touching the network.
func (s *Session) SignedIn() bool {
	_, err := s.Store.Load()
	return err == nil
}
