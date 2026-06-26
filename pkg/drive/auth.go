// Package drive provides native Google Drive integration for stash: a
// service-account credential pool, shared-drive listing and incremental
// change detection (Drive Changes API), and a models.FS implementation so
// scanning operates directly against Drive instead of a FUSE mount.
package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// DefaultScope grants full Drive access. Use drive.DriveReadonlyScope for
// read-only sources.
const DefaultScope = drive.DriveScope

// rclone's well-known default OAuth client, used when importing a remote that
// was authorized without its own client_id/secret.
const (
	rcloneDefaultClientID     = "202264815644.apps.googleusercontent.com"
	rcloneDefaultClientSecret = "X4Z3ca8xfWDb1Voo-F9a7ZxJ"
)

// Authenticator yields Drive services and access tokens. Both the
// service-account pool and the OAuth source implement it, so a Source can be
// backed by either.
type Authenticator interface {
	Next(ctx context.Context) (*drive.Service, error)
	First(ctx context.Context) (*drive.Service, error)
	Token(ctx context.Context) (string, error)
	Len() int
}

// OAuthSource authenticates as a Google user via an OAuth refresh token (e.g.
// imported from an rclone drive remote), rather than a service account.
type OAuthSource struct {
	ts  oauth2.TokenSource
	svc *drive.Service
}

// NewOAuthSource builds an OAuth-backed source from a client id/secret and a
// token (with a refresh token). An empty clientID falls back to rclone's
// default client.
func NewOAuthSource(ctx context.Context, clientID, clientSecret, scope string, token *oauth2.Token) (*OAuthSource, error) {
	if scope == "" {
		scope = DefaultScope
	}
	if clientID == "" {
		clientID = rcloneDefaultClientID
		clientSecret = rcloneDefaultClientSecret
	}
	// The token source is long-lived and refreshes on its own schedule, so it
	// must not capture a request/init context that may later be cancelled.
	ctx = context.WithoutCancel(ctx)
	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{scope},
	}
	ts := conf.TokenSource(ctx, token)
	svc, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	return &OAuthSource{ts: ts, svc: svc}, nil
}

// NewOAuthSourceFromRcloneToken builds an OAuth source from an rclone remote's
// token JSON ({"access_token","refresh_token","token_type","expiry"}).
func NewOAuthSourceFromRcloneToken(ctx context.Context, tokenJSON, clientID, clientSecret, scope string) (*OAuthSource, error) {
	var tok oauth2.Token
	if err := json.Unmarshal([]byte(tokenJSON), &tok); err != nil {
		return nil, fmt.Errorf("parsing rclone token: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("rclone token has no refresh_token")
	}
	return NewOAuthSource(ctx, clientID, clientSecret, scope, &tok)
}

func (o *OAuthSource) Next(ctx context.Context) (*drive.Service, error)  { return o.svc, nil }
func (o *OAuthSource) First(ctx context.Context) (*drive.Service, error) { return o.svc, nil }
func (o *OAuthSource) Len() int                                         { return 1 }
func (o *OAuthSource) Token(ctx context.Context) (string, error) {
	t, err := o.ts.Token()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

var _ Authenticator = (*SAPool)(nil)
var _ Authenticator = (*OAuthSource)(nil)

// SAPool holds one or more service-account credentials and hands out
// *drive.Service instances, rotating across accounts to spread API quota
// across the project's per-100s limits. A single shared drive listing is
// cheap (~1 call per 1000 files) so rotation mainly benefits parallel media
// downloads and rate-limit recovery.
type SAPool struct {
	scope string
	files []string
	next  uint64

	mu         sync.Mutex
	cache      map[string]*drive.Service
	tokenCreds *google.Credentials // lazily-built creds for the first SA, for raw tokens
}

// NewSAPool builds a pool from a path that is either a single service-account
// JSON file or a directory containing many of them (e.g. an rclone gdsa key
// directory).
func NewSAPool(path, scope string) (*SAPool, error) {
	if scope == "" {
		scope = DefaultScope
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("service account path %q: %w", path, err)
	}

	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			files = append(files, filepath.Join(path, e.Name()))
		}
	} else {
		files = []string{path}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no service account json files found at %q", path)
	}

	return &SAPool{
		scope: scope,
		files: files,
		cache: make(map[string]*drive.Service),
	}, nil
}

// Len returns the number of service accounts in the pool.
func (p *SAPool) Len() int { return len(p.files) }

// serviceFor builds (and caches) a *drive.Service for a specific SA file.
func (p *SAPool) serviceFor(ctx context.Context, file string) (*drive.Service, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if svc, ok := p.cache[file]; ok {
		return svc, nil
	}

	// The service (and its token source) is cached for the pool's lifetime, so
	// it must not capture a request/init context that may later be cancelled.
	ctx = context.WithoutCancel(ctx)

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	creds, err := google.CredentialsFromJSON(ctx, data, p.scope)
	if err != nil {
		return nil, fmt.Errorf("parse service account %s: %w", filepath.Base(file), err)
	}

	svc, err := drive.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, err
	}

	p.cache[file] = svc
	return svc, nil
}

// Next returns the next service in round-robin rotation. Safe for concurrent
// use.
func (p *SAPool) Next(ctx context.Context) (*drive.Service, error) {
	i := atomic.AddUint64(&p.next, 1)
	return p.serviceFor(ctx, p.files[int(i)%len(p.files)])
}

// First returns a stable service (the first SA) for operations that should
// not rotate mid-flight, such as paginated listings and change polls.
func (p *SAPool) First(ctx context.Context) (*drive.Service, error) {
	return p.serviceFor(ctx, p.files[0])
}

// Token returns a valid OAuth access token for the first service account. Used
// to build authenticated Drive download URLs that ffprobe/ffmpeg can read with
// HTTP range requests (so metadata probing doesn't download whole files). The
// underlying token source caches and refreshes tokens automatically.
func (p *SAPool) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	if p.tokenCreds == nil {
		data, err := os.ReadFile(p.files[0])
		if err != nil {
			p.mu.Unlock()
			return "", err
		}
		// cached for the pool lifetime — detach from the request context.
		creds, err := google.CredentialsFromJSON(context.WithoutCancel(ctx), data, p.scope)
		if err != nil {
			p.mu.Unlock()
			return "", err
		}
		p.tokenCreds = creds
	}
	creds := p.tokenCreds
	p.mu.Unlock()

	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}
