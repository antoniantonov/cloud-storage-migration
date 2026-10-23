package gdrive

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/state"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
)

const consentTimeout = 5 * time.Minute

type providerError struct {
	message string
	reauth  bool
}

func (e *providerError) Error() string { return e.message }

func diagnostic(message string) error { return &providerError{message: message} }

func reauthorization() error {
	return &providerError{
		message: "Google authorization is missing, expired, or revoked; rerun with AUTH_INTERACTIVE=true to sign in",
		reauth:  true,
	}
}

func installedConfig(path string) (*oauth2.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, diagnostic("cannot read GOOGLE_CLIENT_CREDENTIALS_FILE; provide the downloaded Desktop OAuth client JSON")
	}
	var envelope struct {
		Installed json.RawMessage `json:"installed"`
		Web       json.RawMessage `json:"web"`
		Type      string          `json:"type"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Installed) == 0 ||
		string(envelope.Installed) == "null" || len(envelope.Web) != 0 || envelope.Type != "" {
		return nil, diagnostic("GOOGLE_CLIENT_CREDENTIALS_FILE must contain a registered Desktop (installed) OAuth client, not a service account or web client")
	}
	cfg, err := google.ConfigFromJSON(data, drive.DriveScope)
	if err != nil || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, diagnostic("invalid Google Desktop OAuth client JSON; download the installed client credentials again")
	}
	// Credential-file endpoint fields must not redirect tokens to a different service.
	cfg.Endpoint = google.Endpoint
	return cfg, nil
}

type tokenCache struct {
	Version  int           `json:"version"`
	ClientID string        `json:"client_id"`
	Scope    string        `json:"scope"`
	Token    *oauth2.Token `json:"token"`
}

func cachePath(dir, clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return filepath.Join(dir, "google-"+hex.EncodeToString(sum[:])+".json")
}

func readToken(path, clientID string) (*oauth2.Token, error) {
	data, err := state.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, diagnostic("cannot read Google token cache; check AUTH_DIR and private file permissions (0600)")
	}
	var cached tokenCache
	if json.Unmarshal(data, &cached) != nil || cached.Version != 1 || cached.ClientID != clientID ||
		cached.Scope != drive.DriveScope || cached.Token == nil || cached.Token.AccessToken == "" ||
		cached.Token.RefreshToken == "" || cached.Token.Expiry.IsZero() {
		return nil, diagnostic("Google token cache is corrupt or incompatible; remove " +
			filepath.Base(path) + " from AUTH_DIR and rerun with AUTH_INTERACTIVE=true")
	}
	return cached.Token, nil
}

func tokenWriter(path, clientID string) func(*oauth2.Token) error {
	return func(token *oauth2.Token) error {
		data, err := json.Marshal(tokenCache{Version: 1, ClientID: clientID, Scope: drive.DriveScope, Token: token})
		if err != nil {
			return diagnostic("cannot encode Google token cache")
		}
		if state.WriteFile(path, data) != nil {
			return diagnostic("cannot persist Google token cache; check AUTH_DIR permissions and available disk space")
		}
		return nil
	}
}

type durableTokenSource struct {
	mu        sync.Mutex
	ctx       context.Context
	config    *oauth2.Config
	current   *oauth2.Token
	persisted *oauth2.Token
	write     func(*oauth2.Token) error
}

func cloneToken(token *oauth2.Token) *oauth2.Token {
	if token == nil {
		return nil
	}
	copied := *token
	return &copied
}

func sameToken(a, b *oauth2.Token) bool {
	return a != nil && b != nil && a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken &&
		a.TokenType == b.TokenType && a.Expiry.Equal(b.Expiry)
}

func (s *durableTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if s.current == nil || s.current.RefreshToken == "" {
		return nil, reauthorization()
	}
	if !s.current.Valid() {
		token, err := s.config.TokenSource(s.ctx, cloneToken(s.current)).Token()
		if err != nil {
			return nil, oauthError(s.ctx, err)
		}
		next := cloneToken(token)
		if next.RefreshToken == "" {
			next.RefreshToken = s.current.RefreshToken
		}
		if !next.Valid() || next.Expiry.IsZero() {
			return nil, diagnostic("Google refresh did not return a usable expiring access token; retry or reauthorize")
		}
		// Keep a rotated refresh token in memory even if persisting it fails.
		// No caller receives the new access token until the write succeeds.
		s.current = next
	}
	if !sameToken(s.current, s.persisted) {
		if err := s.write(s.current); err != nil {
			return nil, err
		}
		s.persisted = cloneToken(s.current)
	}
	return cloneToken(s.current), nil
}

func oauthError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var response *oauth2.RetrieveError
	if errors.As(err, &response) {
		switch response.ErrorCode {
		case "invalid_grant", "invalid_token":
			return reauthorization()
		case "invalid_client", "unauthorized_client":
			return diagnostic("Google rejected the OAuth client; check the registered Desktop app and GOOGLE_CLIENT_CREDENTIALS_FILE")
		case "access_denied":
			return diagnostic("Google authorization was denied; rerun with AUTH_INTERACTIVE=true when ready to consent")
		}
		if response.Response != nil {
			return diagnostic(fmt.Sprintf("Google OAuth request failed (HTTP %d); retry later or check the OAuth app configuration", response.Response.StatusCode))
		}
	}
	return diagnostic("Google OAuth request failed; check connectivity and retry (provider details suppressed to protect credentials)")
}

func authenticatedClient(ctx context.Context, cfg config.Config, stderr io.Writer) (*http.Client, error) {
	baseHTTPClient := http.DefaultClient
	if supplied, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && supplied != nil {
		baseHTTPClient = supplied
	}
	oauthHTTPClient := *baseHTTPClient
	if oauthHTTPClient.Timeout <= 0 || oauthHTTPClient.Timeout > 30*time.Second {
		oauthHTTPClient.Timeout = 30 * time.Second
	}
	// Token refresh deliberately uses the command context, so it also needs its
	// own HTTP timeout rather than relying on a Drive request's deadline.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &oauthHTTPClient)
	oauthConfig, err := installedConfig(cfg.GoogleCredentialsFile)
	if err != nil {
		return nil, err
	}
	path := cachePath(cfg.AuthDir, oauthConfig.ClientID)
	token, err := readToken(path, oauthConfig.ClientID)
	if err != nil {
		return nil, err
	}
	source := &durableTokenSource{
		ctx: ctx, config: oauthConfig, current: cloneToken(token), persisted: cloneToken(token),
		write: tokenWriter(path, oauthConfig.ClientID),
	}
	_, err = source.Token()
	if err != nil {
		var authErr *providerError
		if !errors.As(err, &authErr) || !authErr.reauth || !cfg.InteractiveAuth {
			return nil, err
		}
		token, err = authorize(ctx, cfg, oauthConfig, stderr, consentTimeout)
		if err != nil {
			return nil, err
		}
		source.current = cloneToken(token)
		if _, err := source.Token(); err != nil {
			return nil, err
		}
	}
	// Refreshing uses the command's context, never the closed callback's context.
	client := oauth2.NewClient(ctx, source)
	client.Timeout = 2 * time.Minute
	return client, nil
}

type authorizationResult struct {
	code string
	err  error
}

func authorize(ctx context.Context, cfg config.Config, oauthConfig *oauth2.Config, stderr io.Writer, timeout time.Duration) (*oauth2.Token, error) {
	if cfg.GoogleOAuthPort < 1 || cfg.GoogleOAuthPort > 65535 ||
		(cfg.GoogleOAuthListenHost != "127.0.0.1" && cfg.GoogleOAuthListenHost != "0.0.0.0") {
		return nil, diagnostic("Google OAuth requires a valid GOOGLE_OAUTH_PORT and GOOGLE_OAUTH_LISTEN_HOST of 127.0.0.1 (native) or 0.0.0.0 (container)")
	}
	authCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := authCtx.Err(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(cfg.GoogleOAuthListenHost, strconv.Itoa(cfg.GoogleOAuthPort)))
	if err != nil {
		return nil, diagnostic("cannot listen for Google OAuth callback; check GOOGLE_OAUTH_PORT for a port collision and retry")
	}
	defer listener.Close()
	stateValue, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	flowConfig := *oauthConfig
	flowConfig.RedirectURL = "http://127.0.0.1:" + strconv.Itoa(cfg.GoogleOAuthPort) + "/"
	result := make(chan authorizationResult, 1)
	var once sync.Once
	finish := func(value authorizationResult) { once.Do(func() { result <- value }) }
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Referrer-Policy", "no-referrer")
			if r.Method != http.MethodGet || r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			query, parseErr := url.ParseQuery(r.URL.RawQuery)
			response := authorizationResult{}
			switch {
			case parseErr != nil || len(query["state"]) != 1 ||
				subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(stateValue)) != 1:
				response.err = diagnostic("Google OAuth callback state did not match; authorization rejected, rerun to sign in")
			case len(query["error"]) != 0:
				response.err = diagnostic("Google authorization was denied or cancelled; rerun with AUTH_INTERACTIVE=true when ready to consent")
			case len(query["code"]) != 1 || strings.TrimSpace(query.Get("code")) == "":
				response.err = diagnostic("Google OAuth callback did not contain an authorization code; rerun to sign in")
			default:
				response.code = query.Get("code")
			}
			if response.err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "Authorization failed. Return to the command for instructions.\n")
			} else {
				_, _ = io.WriteString(w, "Authorization received. You may close this browser tab and return to the command.\n")
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			finish(response)
		}),
	}
	defer server.Close()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	if stderr == nil {
		stderr = io.Discard
	}
	authURL := flowConfig.AuthCodeURL(stateValue, oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("prompt", "consent"))
	if _, err := fmt.Fprintf(stderr, "Open this URL in your host browser to authorize Google Drive:\n%s\n", authURL); err != nil {
		return nil, diagnostic("cannot write Google authorization instructions to stderr")
	}
	select {
	case <-authCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, diagnostic("Google authorization timed out; rerun and finish browser consent within five minutes")
	case <-served:
		return nil, diagnostic("Google OAuth callback listener stopped unexpectedly")
	case callback := <-result:
		_ = server.Close()
		if callback.err != nil {
			return nil, callback.err
		}
		token, err := flowConfig.Exchange(authCtx, callback.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, oauthError(authCtx, err)
		}
		if !token.Valid() || token.RefreshToken == "" || token.Expiry.IsZero() {
			return nil, diagnostic("Google did not issue usable offline credentials; rerun with AUTH_INTERACTIVE=true and grant consent")
		}
		return token, nil
	}
}
