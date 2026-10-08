package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ManualLogin signs in without a password passing through this process:
// announce gets the Prusa Account authorize URL, the user (or an agent driving
// a browser) approves it anywhere, and read returns what comes back, either the
// full URL the browser ended on or just its code. Prusa Account only accepts
// the Connect web app's own redirect for this client (it rejects a loopback
// one as "Mismatching redirect URI"), so the browser ends on
// https://connect.prusa3d.com/login/auth-callback with the code in the address
// bar, and nothing can listen for it.
func (c Config) ManualLogin(ctx context.Context, announce func(authURL string), read func() (string, error)) (*Token, error) {
	p, err := newPKCE()
	if err != nil {
		return nil, err
	}
	announce(c.authorizeURL(p))
	pasted, err := read()
	if err != nil {
		return nil, fmt.Errorf("reading the pasted sign-in URL: %w", err)
	}
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return nil, errors.New("nothing was pasted")
	}
	code := pasted
	if strings.ContainsAny(pasted, "?&=") {
		if !strings.Contains(pasted, "://") {
			pasted = "?" + strings.TrimPrefix(pasted, "?")
		}
		if code, err = c.codeFromCallback(pasted, p); err != nil {
			return nil, err
		}
	}
	return c.exchangeCode(ctx, code, p)
}
