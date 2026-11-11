// Package auth keeps people's GitHub user tokens, so replay can post their work under their own
// names (decision D3, design 8.12). Tokens are encrypted with a key kept outside the database, a
// refreshed token is saved before the new access token is used (refresh tokens rotate, spike U),
// and any token that stops working is dropped, so the App posts instead.
package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/store"
)

type Users struct {
	GH    *ghclient.App
	Store *store.Store
	Log   *slog.Logger
	aead  cipher.AEAD
}

// New uses key (32 bytes, or 64 hex digits with optional surrounding whitespace) for AES-256-GCM.
func New(gh *ghclient.App, st *store.Store, log *slog.Logger, key []byte) (*Users, error) {
	if k, err := hex.DecodeString(string(bytes.TrimSpace(key))); err == nil {
		key = k
	}
	if len(key) != 32 {
		return nil, errors.New("user token key must be 32 bytes or 64 hex digits (e.g. `openssl rand -hex 32`)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Users{GH: gh, Store: st, Log: log, aead: aead}, nil
}

// The additional data ties a ciphertext to its row and field, so blobs can't be swapped between them.
func ad(tokenID int64, field string) []byte {
	return fmt.Appendf(nil, "gitcove user_token %d %s", tokenID, field)
}

func (u *Users) seal(tokenID int64, field, plain string) []byte {
	nonce := make([]byte, u.aead.NonceSize())
	rand.Read(nonce)
	return u.aead.Seal(nonce, nonce, []byte(plain), ad(tokenID, field))
}

func (u *Users) open(tokenID int64, field string, blob []byte) (string, error) {
	n := u.aead.NonceSize()
	if len(blob) < n {
		return "", errors.New("user token: short ciphertext")
	}
	plain, err := u.aead.Open(nil, blob[:n], blob[n:], ad(tokenID, field))
	return string(plain), err
}

func (u *Users) save(ctx context.Context, actor, action string, tokenID int64, login string, t ghclient.UserToken) error {
	return u.Store.SaveUserToken(ctx, actor, action, store.UserToken{TokenID: tokenID, Login: login,
		Access: u.seal(tokenID, "access", t.Access), AccessExpires: t.AccessExpires.UTC().Format(time.RFC3339),
		Refresh: u.seal(tokenID, "refresh", t.Refresh), RefreshExpires: t.RefreshExpires.UTC().Format(time.RFC3339)})
}

// Authorise runs GitHub's device flow for the holder of GitCove token tokenID: show is called
// with the code to enter at the URI, then it waits until they do, or the code expires.
func (u *Users) Authorise(ctx context.Context, tokenID int64, actor string, show func(code, uri string)) (string, error) {
	dc, err := u.GH.DeviceCode(ctx)
	if err != nil {
		return "", err
	}
	show(dc.UserCode, dc.VerificationURI)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(dc.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(max(dc.Interval, 1)) * time.Second
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the code expired or the request was cancelled: %w", ctx.Err())
		case <-time.After(interval):
		}
		t, err := u.GH.PollDevice(ctx, dc.DeviceCode)
		var oe *ghclient.OAuthError
		switch {
		case errors.As(err, &oe) && oe.Code == "authorization_pending":
			continue
		case errors.As(err, &oe) && oe.Code == "slow_down":
			interval += 5 * time.Second
			continue
		case err != nil:
			return "", err
		}
		login, err := u.GH.UserLogin(ctx, t.Access)
		if err != nil {
			return "", err
		}
		return login, u.save(ctx, actor, "user_token.authorise", tokenID, login, t)
	}
}

// Token returns the GitHub login and a live access token to act as the holder of GitCove token
// tokenID, or "" if they haven't authorised GitCove or their authorisation stopped working, in
// which case the App posts. A nil *Users means user tokens are off. An error means GitHub
// couldn't be asked; the caller retries later.
func (u *Users) Token(ctx context.Context, tokenID int64) (login, access string, err error) {
	if u == nil {
		return "", "", nil
	}
	row, err := u.Store.UserToken(ctx, tokenID)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", nil
	} else if err != nil {
		return "", "", err
	}
	if exp, _ := time.Parse(time.RFC3339, row.AccessExpires); time.Until(exp) > 5*time.Minute {
		if access, err := u.open(tokenID, "access", row.Access); err == nil {
			return row.Login, access, nil
		}
	}
	drop := func(reason string) (string, string, error) {
		u.Log.Warn("dropping a user token; the App posts their work instead", "token", tokenID, "login", row.Login, "reason", reason)
		return "", "", u.Store.DropUserToken(ctx, "replay", tokenID, reason)
	}
	if exp, _ := time.Parse(time.RFC3339, row.RefreshExpires); !time.Now().Before(exp) {
		return drop("refresh token expired")
	}
	refresh, err := u.open(tokenID, "refresh", row.Refresh)
	if err != nil {
		return drop("can't decrypt (was COVE_USER_KEY_FILE changed?)")
	}
	t, err := u.GH.RefreshUser(ctx, refresh)
	var oe *ghclient.OAuthError
	if errors.As(err, &oe) {
		return drop(oe.Error())
	} else if err != nil {
		return "", "", err
	}
	// GitHub has already rotated the refresh token: save the new one before anything else.
	if err := u.save(ctx, "replay", "user_token.refresh", tokenID, row.Login, t); err != nil {
		return "", "", err
	}
	return row.Login, t.Access, nil
}

// Drop forgets a token GitHub refused, so the App posts instead.
func (u *Users) Drop(ctx context.Context, tokenID int64, reason string) error {
	if u == nil {
		return nil
	}
	return u.Store.DropUserToken(ctx, "replay", tokenID, reason)
}
