package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/oauth2"

	"github.com/stashapp/stash/pkg/drive"
	"github.com/stashapp/stash/pkg/logger"
)

// Optional built-in Google OAuth client. Leave empty to require a per-instance
// client entered in Settings (the flow supports both).
const (
	builtinGoogleClientID     = ""
	builtinGoogleClientSecret = ""
)

const (
	googleOAuthFile  = "gdrive_oauth.json"
	googleOAuthScope = "https://www.googleapis.com/auth/drive"
)

// googleOAuthConfig is the persisted connected-account state.
type googleOAuthConfig struct {
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret"`
	Token        *oauth2.Token `json:"token,omitempty"`
}

var (
	oauthStateMu sync.Mutex
	oauthStates  = map[string]struct{}{}
)

func (s *Manager) googleOAuthPath() string {
	return filepath.Join(s.Config.GetConfigPath(), googleOAuthFile)
}

func (s *Manager) loadGoogleOAuth() googleOAuthConfig {
	var c googleOAuthConfig
	if data, err := os.ReadFile(s.googleOAuthPath()); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

func (s *Manager) saveGoogleOAuth(c googleOAuthConfig) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.googleOAuthPath(), data, 0o600) // holds a refresh token
}

// googleClient returns the effective OAuth client: per-instance (settings)
// overrides the built-in default.
func (s *Manager) googleClient() (string, string) {
	c := s.loadGoogleOAuth()
	if c.ClientID != "" {
		return c.ClientID, c.ClientSecret
	}
	return builtinGoogleClientID, builtinGoogleClientSecret
}

// SetGoogleOAuthClient stores a per-instance OAuth client id/secret. Changing the
// client also clears any existing token (it belonged to the old client).
func (s *Manager) SetGoogleOAuthClient(id, secret string) error {
	c := s.loadGoogleOAuth()
	if c.ClientID != id || c.ClientSecret != secret {
		c.Token = nil
	}
	c.ClientID = id
	c.ClientSecret = secret
	return s.saveGoogleOAuth(c)
}

// DisconnectGoogle clears the connected-account token, and optionally the
// per-instance client credentials (so bad creds can be re-entered).
func (s *Manager) DisconnectGoogle(clearClient bool) error {
	c := s.loadGoogleOAuth()
	c.Token = nil
	if clearClient {
		c.ClientID = ""
		c.ClientSecret = ""
	}
	return s.saveGoogleOAuth(c)
}

// GoogleAuthStatus is the connect status surfaced to the UI.
type GoogleAuthStatus struct {
	Connected        bool
	ClientConfigured bool
	RedirectURI      string
}

func (s *Manager) GoogleAuthStatus(redirectURI string) GoogleAuthStatus {
	c := s.loadGoogleOAuth()
	id, _ := s.googleClient()
	return GoogleAuthStatus{
		Connected:        c.Token != nil && c.Token.RefreshToken != "",
		ClientConfigured: id != "",
		RedirectURI:      redirectURI,
	}
}

// googleConnectedAuth builds an Authenticator for the connected account.
func (s *Manager) googleConnectedAuth(ctx context.Context) (drive.Authenticator, error) {
	c := s.loadGoogleOAuth()
	if c.Token == nil {
		return nil, fmt.Errorf("no Google account connected")
	}
	id, secret := s.googleClient()
	if id == "" {
		return nil, fmt.Errorf("no Google OAuth client configured")
	}
	return drive.NewOAuthSource(ctx, id, secret, googleOAuthScope, c.Token)
}

// ListGoogleDrives lists My Drive + shared drives for the connected account.
func (s *Manager) ListGoogleDrives(ctx context.Context) ([]drive.DriveInfo, error) {
	auth, err := s.googleConnectedAuth(ctx)
	if err != nil {
		return nil, err
	}
	svc, err := auth.First(ctx)
	if err != nil {
		return nil, err
	}
	shared, err := drive.ListDrives(ctx, svc)
	if err != nil {
		return nil, err
	}
	return append([]drive.DriveInfo{{ID: "", Name: "My Drive", MyDrive: true}}, shared...), nil
}

func randState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func googleRedirectURI(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/oauth/google/callback"
}

// HandleGoogleOAuthLogin redirects the browser to Google's consent screen.
func (s *Manager) HandleGoogleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	id, secret := s.googleClient()
	if id == "" {
		http.Error(w, "no Google OAuth client configured", http.StatusBadRequest)
		return
	}
	state := randState()
	oauthStateMu.Lock()
	oauthStates[state] = struct{}{}
	oauthStateMu.Unlock()

	cfg := drive.OAuthConfig(id, secret, googleRedirectURI(r), googleOAuthScope)
	url := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	http.Redirect(w, r, url, http.StatusFound)
}

// HandleGoogleOAuthCallback exchanges the code and stores the token.
func (s *Manager) HandleGoogleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	oauthStateMu.Lock()
	_, ok := oauthStates[state]
	delete(oauthStates, state)
	oauthStateMu.Unlock()
	if !ok {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}

	id, secret := s.googleClient()
	cfg := drive.OAuthConfig(id, secret, googleRedirectURI(r), googleOAuthScope)
	tok, err := cfg.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	c := s.loadGoogleOAuth()
	c.Token = tok
	if c.ClientID == "" {
		c.ClientID, c.ClientSecret = id, secret
	}
	if err := s.saveGoogleOAuth(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logger.Info("Google Drive account connected via OAuth")
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(`<html><body style="font-family:sans-serif">Google Drive connected. You can close this window.<script>window.close()</script></body></html>`))
}
