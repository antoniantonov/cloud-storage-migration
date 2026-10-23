package gdrive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/state"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
)

const installedJSON = `{"installed":{
	"client_id":"desktop-client",
	"client_secret":"CLIENT_SECRET_PRIVATE",
	"redirect_uris":["http://localhost"],
	"auth_uri":"https://accounts.google.com/o/oauth2/auth",
	"token_uri":"https://oauth2.googleapis.com/token"
}}`

func authTestConfig(t *testing.T) config.Config {
	t.Helper()
	dir := privateTestDir(t)
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(installedJSON), 0600); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		GoogleCredentialsFile: path, AuthDir: filepath.Join(dir, "auth"),
		GoogleOAuthListenHost: "127.0.0.1", GoogleOAuthPort: freePort(t), InteractiveAuth: true,
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func sampleToken() *oauth2.Token {
	return &oauth2.Token{AccessToken: "ACCESS_TOKEN_PRIVATE", TokenType: "Bearer", RefreshToken: "REFRESH_TOKEN_PRIVATE", Expiry: time.Now().Add(time.Hour)}
}

func assertNoSecrets(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{"CLIENT_SECRET_PRIVATE", "ACCESS_TOKEN_PRIVATE", "REFRESH_TOKEN_PRIVATE", "CODE_PRIVATE", "PRIVATE_RESPONSE", "https://private.invalid"} {
		if strings.Contains(text, secret) {
			t.Errorf("sensitive value leaked: %q", text)
		}
	}
}

func TestInstalledCredentialsOnlyAndPinnedEndpoints(t *testing.T) {
	cfg := authTestConfig(t)
	oauthConfig, err := installedConfig(cfg.GoogleCredentialsFile)
	if err != nil || oauthConfig.ClientID != "desktop-client" || len(oauthConfig.Scopes) != 1 || oauthConfig.Scopes[0] != drive.DriveScope {
		t.Fatalf("got %+v %v", oauthConfig, err)
	}
	for _, test := range []struct {
		name string
		data string
	}{
		{"web client", `{"web":{"client_id":"id","client_secret":"CLIENT_SECRET_PRIVATE"}}`},
		{"service account", `{"type":"service_account","private_key":"CLIENT_SECRET_PRIVATE"}`},
		{"malformed", `{"installed":"CLIENT_SECRET_PRIVATE`},
		{"missing installed object", `{"installed":null}`},
		{"missing secret", `{"installed":{"client_id":"id","redirect_uris":["http://localhost"]}}`},
		{"missing redirect", `{"installed":{"client_id":"id","client_secret":"CLIENT_SECRET_PRIVATE"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(cfg.GoogleCredentialsFile, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := installedConfig(cfg.GoogleCredentialsFile); err == nil {
				t.Fatal("unsupported credentials accepted")
			} else {
				assertNoSecrets(t, err.Error())
			}
		})
	}
	modified := strings.ReplaceAll(installedJSON, "https://oauth2.googleapis.com/token", "https://private.invalid/token")
	if err := os.WriteFile(cfg.GoogleCredentialsFile, []byte(modified), 0600); err != nil {
		t.Fatal(err)
	}
	oauthConfig, err = installedConfig(cfg.GoogleCredentialsFile)
	if err != nil || oauthConfig.Endpoint != google.Endpoint {
		t.Fatal("credential JSON can redirect credentials to another endpoint")
	}
}

func TestTokenCacheClientScopePermissionsAndCorruption(t *testing.T) {
	dir := privateTestDir(t)
	path := cachePath(dir, "client")
	if cachePath(dir, "other") == path {
		t.Fatal("token cache is not keyed by OAuth client")
	}
	if token, err := readToken(path, "client"); err != nil || token != nil {
		t.Fatalf("missing cache: %+v %v", token, err)
	}
	token := sampleToken()
	if err := tokenWriter(path, "client")(token); err != nil {
		t.Fatal(err)
	}
	got, err := readToken(path, "client")
	if err != nil || !sameToken(got, token) {
		t.Fatalf("round-trip: %+v %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("cache permissions: %v %v", info, err)
	}
	if _, err := readToken(path, "other"); err == nil {
		t.Fatal("cache from another client accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(path, "client"); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("public cache accepted: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{"token":"ACCESS_TOKEN_PRIVATE"`,
		`{"version":1,"client_id":"client","scope":"wrong","token":{"access_token":"ACCESS_TOKEN_PRIVATE"}}`,
		`{"version":1,"client_id":"client","scope":"https://www.googleapis.com/auth/drive","token":{}}`,
		`null`,
	} {
		if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readToken(path, "client"); err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("corrupt cache accepted: %v", err)
		} else {
			assertNoSecrets(t, err.Error())
		}
	}
}

func TestTokenWriterReportsAtomicPersistenceFailure(t *testing.T) {
	dir := privateTestDir(t)
	path := cachePath(dir, "client")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	err := tokenWriter(path, "client")(sampleToken())
	if err == nil || !strings.Contains(err.Error(), "cannot persist") {
		t.Fatalf("write failure swallowed: %v", err)
	}
	assertNoSecrets(t, err.Error())
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("atomic-write staging leaked: %v %v", entries, err)
	}
}

func oauthContext(ctx context.Context, transport roundTripFunc) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: transport})
}

func TestDurableRefreshPreservesAndRotatesTokens(t *testing.T) {
	initial := sampleToken()
	initial.Expiry = time.Now().Add(-time.Hour)
	requests := 0
	ctx := oauthContext(context.Background(), func(r *http.Request) (*http.Response, error) {
		requests++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		expected := "REFRESH_TOKEN_PRIVATE"
		if requests > 1 {
			expected = "rotated"
		}
		if r.Form.Get("refresh_token") != expected || r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("wrong refresh parameters: grant=%q refresh matched=%v", r.Form.Get("grant_type"), r.Form.Get("refresh_token") == expected)
		}
		if requests == 1 {
			return response(200, `{"access_token":"first","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`), nil
		}
		return response(200, `{"access_token":"second","token_type":"Bearer","expires_in":3600}`), nil
	})
	path := cachePath(privateTestDir(t), "client")
	oauthConfig := &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: google.Endpoint}
	source := &durableTokenSource{ctx: ctx, config: oauthConfig, current: cloneToken(initial), persisted: cloneToken(initial), write: tokenWriter(path, "client")}
	for i := 0; i < 2; i++ {
		token, err := source.Token()
		if err != nil || token.RefreshToken != "rotated" {
			t.Fatalf("token %d: %+v %v", i, token, err)
		}
		cached, err := readToken(path, "client")
		if err != nil || !sameToken(cached, token) {
			t.Fatalf("refresh was not durable: %+v %v", cached, err)
		}
		source.current.Expiry = time.Now().Add(-time.Hour)
	}
	if requests != 2 {
		t.Errorf("refresh requests: %d", requests)
	}
}

func TestRefreshWriteFailureCannotExposeOrLoseNewToken(t *testing.T) {
	initial := sampleToken()
	initial.Expiry = time.Now().Add(-time.Hour)
	requests, writes := 0, 0
	ctx := oauthContext(context.Background(), func(*http.Request) (*http.Response, error) {
		requests++
		return response(200, `{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`), nil
	})
	source := &durableTokenSource{
		ctx: ctx, config: &oauth2.Config{Endpoint: google.Endpoint}, current: initial, persisted: cloneToken(initial),
		write: func(*oauth2.Token) error {
			writes++
			if writes == 1 {
				return diagnostic("cannot persist Google token cache")
			}
			return nil
		},
	}
	if token, err := source.Token(); err == nil || token != nil {
		t.Fatalf("unpersisted token exposed: %+v %v", token, err)
	}
	token, err := source.Token()
	if err != nil || token.RefreshToken != "new-refresh" || writes != 2 || requests != 1 {
		t.Fatalf("rotated credentials lost: %+v %v writes=%d requests=%d", token, err, writes, requests)
	}
}

func TestConcurrentRefreshOccursOnce(t *testing.T) {
	token := sampleToken()
	token.Expiry = time.Now().Add(-time.Hour)
	requests, writes := 0, 0
	ctx := oauthContext(context.Background(), func(*http.Request) (*http.Response, error) {
		requests++
		return response(200, `{"access_token":"new","expires_in":3600,"token_type":"Bearer"}`), nil
	})
	source := &durableTokenSource{
		ctx: ctx, config: &oauth2.Config{Endpoint: google.Endpoint}, current: token, persisted: cloneToken(token),
		write: func(*oauth2.Token) error { writes++; return nil },
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if token, err := source.Token(); err != nil || token.AccessToken != "new" {
				t.Errorf("concurrent token: %+v %v", token, err)
			}
		}()
	}
	group.Wait()
	if requests != 1 || writes != 1 {
		t.Errorf("duplicate refresh or writes: %d %d", requests, writes)
	}
}

func TestNewUsesCachedTokenWithoutConsent(t *testing.T) {
	cfg := authTestConfig(t)
	cfg.InteractiveAuth = false
	if err := tokenWriter(cachePath(cfg.AuthDir, "desktop-client"), "desktop-client")(sampleToken()); err != nil {
		t.Fatal(err)
	}
	requests := 0
	ctx := oauthContext(context.Background(), func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "www.googleapis.com" || r.Header.Get("Authorization") != "Bearer ACCESS_TOKEN_PRIVATE" {
			t.Error("cached token did not authenticate the official Drive request")
		}
		return response(200, `{"files":[{"id":"manual","name":"test-migration","mimeType":"application/vnd.google-apps.folder"}]}`), nil
	})
	var stderr bytes.Buffer
	client, err := New(ctx, cfg, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.EnsureFolder(ctx)
	if err != nil || id != "manual" || requests != 1 || stderr.Len() != 0 {
		t.Fatalf("got id=%q requests=%d output=%q err=%v", id, requests, stderr.String(), err)
	}
}

func TestDisabledInteractionAndRevokedTokenErrorsAreSafe(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached=%v", cached), func(t *testing.T) {
			cfg := authTestConfig(t)
			cfg.InteractiveAuth = false
			if cached {
				token := sampleToken()
				token.Expiry = time.Now().Add(-time.Hour)
				if err := tokenWriter(cachePath(cfg.AuthDir, "desktop-client"), "desktop-client")(token); err != nil {
					t.Fatal(err)
				}
			}
			ctx := oauthContext(context.Background(), func(*http.Request) (*http.Response, error) {
				return response(400, `{"error":"invalid_grant","error_description":"ACCESS_TOKEN_PRIVATE https://private.invalid/refresh"}`), nil
			})
			var stderr bytes.Buffer
			if _, err := New(ctx, cfg, &stderr); err == nil || !strings.Contains(err.Error(), "AUTH_INTERACTIVE=true") {
				t.Fatalf("missing actionable reauthorization: %v", err)
			} else {
				assertNoSecrets(t, err.Error())
			}
			if stderr.Len() != 0 {
				t.Error("noninteractive authentication printed consent instructions")
			}
		})
	}
}

func TestNewRejectsCorruptCacheWithoutInteraction(t *testing.T) {
	cfg := authTestConfig(t)
	path := cachePath(cfg.AuthDir, "desktop-client")
	if err := state.WriteFile(path, []byte(`{"ACCESS_TOKEN_PRIVATE":`)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if _, err := New(context.Background(), cfg, &stderr); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt cache was ignored: %v", err)
	} else {
		assertNoSecrets(t, err.Error())
	}
	if stderr.Len() != 0 {
		t.Fatal("corrupt cache was replaced via automatic consent")
	}
}

func TestRefreshHTTPTimeoutDoesNotPromptOrMutateSuppliedClient(t *testing.T) {
	cfg := authTestConfig(t)
	token := sampleToken()
	token.Expiry = time.Now().Add(-time.Hour)
	if err := tokenWriter(cachePath(cfg.AuthDir, "desktop-client"), "desktop-client")(token); err != nil {
		t.Fatal(err)
	}
	base := &http.Client{
		Timeout: 20 * time.Millisecond,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if _, bounded := r.Context().Deadline(); !bounded {
				t.Error("refresh request has no deadline")
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}),
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, base)
	var stderr bytes.Buffer
	if _, err := New(ctx, cfg, &stderr); err == nil {
		t.Fatal("refresh HTTP timeout was swallowed")
	} else {
		assertNoSecrets(t, err.Error())
	}
	if stderr.Len() != 0 || base.Timeout != 20*time.Millisecond {
		t.Fatal("network failure triggered consent or mutated the caller's HTTP client")
	}
}

type instructionWriter struct{ messages chan string }

func (w instructionWriter) Write(data []byte) (int, error) {
	w.messages <- string(data)
	return len(data), nil
}

func consentURL(t *testing.T, writer instructionWriter) *url.URL {
	t.Helper()
	select {
	case message := <-writer.messages:
		assertNoSecrets(t, message)
		for _, line := range strings.Split(message, "\n") {
			if strings.HasPrefix(line, "https://") {
				u, err := url.Parse(line)
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
		}
		t.Fatal("authorization instructions contained no URL")
	case <-time.After(3 * time.Second):
		t.Fatal("authorization instructions were not printed")
	}
	return nil
}

type tokenResult struct {
	token *oauth2.Token
	err   error
}

func awaitToken(t *testing.T, result <-chan tokenResult) tokenResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("authorization did not finish")
		return tokenResult{}
	}
}

func callback(t *testing.T, authURL *url.URL, values url.Values) (int, string) {
	t.Helper()
	callbackURL := authURL.Query().Get("redirect_uri") + "?" + values.Encode()
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, string(body))
	return res.StatusCode, string(body)
}

func assertListenerClosed(t *testing.T, port int) {
	t.Helper()
	connection, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), 100*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Error("OAuth listener was not closed")
	}
}

func TestLoopbackConsentPKCEAndCommandLifetimeRefresh(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "0.0.0.0"} {
		t.Run(host, func(t *testing.T) {
			cfg := authTestConfig(t)
			cfg.GoogleOAuthListenHost = host
			oauthConfig, err := installedConfig(cfg.GoogleCredentialsFile)
			if err != nil {
				t.Fatal(err)
			}
			commands, cancel := context.WithCancel(context.Background())
			defer cancel()
			challenge := make(chan string, 1)
			calls := 0
			ctx := oauthContext(commands, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Context().Err() != nil {
					t.Error("token exchange/refresh inherited a cancelled callback context")
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if calls == 1 {
					hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
					if base64.RawURLEncoding.EncodeToString(hash[:]) != <-challenge ||
						r.Form.Get("code") != "CODE_PRIVATE" || r.Form.Get("redirect_uri") != "http://127.0.0.1:"+strconv.Itoa(cfg.GoogleOAuthPort)+"/" {
						t.Error("PKCE exchange or browser redirect is incorrect")
					}
				} else if r.Form.Get("grant_type") != "refresh_token" {
					t.Error("expected post-callback token refresh")
				}
				return response(200, `{"access_token":"ACCESS_TOKEN_PRIVATE","refresh_token":"REFRESH_TOKEN_PRIVATE","token_type":"Bearer","expires_in":3600}`), nil
			})
			writer := instructionWriter{messages: make(chan string, 1)}
			finished := make(chan tokenResult, 1)
			go func() {
				token, err := authorize(ctx, cfg, oauthConfig, writer, time.Second)
				finished <- tokenResult{token, err}
			}()
			authURL := consentURL(t, writer)
			query := authURL.Query()
			if authURL.Host != "accounts.google.com" || query.Get("scope") != drive.DriveScope ||
				query.Get("access_type") != "offline" || query.Get("prompt") != "consent" ||
				query.Get("code_challenge_method") != "S256" || len(query.Get("state")) < 43 {
				t.Error("missing broad scope, offline access, random state, or PKCE")
			}
			challenge <- query.Get("code_challenge")
			if query.Get("code_challenge") == query.Get("state") {
				t.Error("state and PKCE verifier are not independent")
			}
			status, _ := callback(t, authURL, url.Values{"state": {query.Get("state")}, "code": {"CODE_PRIVATE"}})
			if status != 200 {
				t.Errorf("callback status: %d", status)
			}
			result := awaitToken(t, finished)
			if result.err != nil || result.token == nil {
				t.Fatalf("consent: %+v", result)
			}
			assertListenerClosed(t, cfg.GoogleOAuthPort)
			result.token.Expiry = time.Now().Add(-time.Hour)
			source := &durableTokenSource{
				ctx: ctx, config: oauthConfig, current: result.token,
				write: tokenWriter(cachePath(cfg.AuthDir, "desktop-client"), "desktop-client"),
			}
			if _, err := source.Token(); err != nil || calls != 2 {
				t.Fatalf("refresh after callback shutdown failed: %v calls=%d", err, calls)
			}
			cancel()
			if _, err := source.Token(); !errors.Is(err, context.Canceled) {
				t.Fatalf("command cancellation ignored: %v", err)
			}
		})
	}
}

func TestLoopbackRejectsInvalidCallbacksAndCloses(t *testing.T) {
	for _, kind := range []string{"state mismatch", "duplicate state", "denied", "no code", "duplicate code"} {
		t.Run(kind, func(t *testing.T) {
			cfg := authTestConfig(t)
			oauthConfig, _ := installedConfig(cfg.GoogleCredentialsFile)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = oauthContext(ctx, func(*http.Request) (*http.Response, error) {
				t.Error("invalid callback triggered token exchange")
				return response(400, `{}`), nil
			})
			writer := instructionWriter{messages: make(chan string, 1)}
			finished := make(chan tokenResult, 1)
			go func() {
				token, err := authorize(ctx, cfg, oauthConfig, writer, time.Second)
				finished <- tokenResult{token, err}
			}()
			authURL := consentURL(t, writer)
			query := url.Values{"state": {authURL.Query().Get("state")}, "code": {"CODE_PRIVATE"}}
			switch kind {
			case "state mismatch":
				query.Set("state", "wrong")
			case "duplicate state":
				query.Add("state", query.Get("state"))
			case "denied":
				query.Set("error", "access_denied")
				query.Set("error_description", "PRIVATE_RESPONSE https://private.invalid")
			case "no code":
				query.Del("code")
			case "duplicate code":
				query.Add("code", "other")
			}
			status, _ := callback(t, authURL, query)
			result := awaitToken(t, finished)
			if status != 400 || result.err == nil || result.token != nil {
				t.Fatalf("invalid callback accepted: status=%d %+v", status, result)
			}
			assertNoSecrets(t, result.err.Error())
			assertListenerClosed(t, cfg.GoogleOAuthPort)
		})
	}
}

func TestLoopbackCancellationTimeoutCollisionAndBadWriter(t *testing.T) {
	for _, kind := range []string{"cancelled", "timeout", "port collision", "bad writer"} {
		t.Run(kind, func(t *testing.T) {
			cfg := authTestConfig(t)
			oauthConfig, _ := installedConfig(cfg.GoogleCredentialsFile)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "port collision" {
				listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(cfg.GoogleOAuthPort))
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				_, err = authorize(ctx, cfg, oauthConfig, io.Discard, time.Second)
				if err == nil || !strings.Contains(err.Error(), "port collision") {
					t.Fatalf("port collision not reported: %v", err)
				}
				return
			}
			if kind == "bad writer" {
				_, err := authorize(ctx, cfg, oauthConfig, failingWriter{}, time.Second)
				if err == nil || !strings.Contains(err.Error(), "instructions") {
					t.Fatalf("writer failure not reported: %v", err)
				}
				assertListenerClosed(t, cfg.GoogleOAuthPort)
				return
			}
			writer := instructionWriter{messages: make(chan string, 1)}
			finished := make(chan tokenResult, 1)
			go func() {
				token, err := authorize(ctx, cfg, oauthConfig, writer, 100*time.Millisecond)
				finished <- tokenResult{token, err}
			}()
			_ = consentURL(t, writer)
			if kind == "cancelled" {
				cancel()
			}
			result := awaitToken(t, finished)
			if result.err == nil || result.token != nil {
				t.Fatalf("unfinished consent succeeded: %+v", result)
			}
			if kind == "cancelled" && !errors.Is(result.err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", result.err)
			}
			assertListenerClosed(t, cfg.GoogleOAuthPort)
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("PRIVATE_RESPONSE") }

func TestNewInitialConsentIsDurableAndRefreshDenialCanReauthorize(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoked=%v", revoked), func(t *testing.T) {
			cfg := authTestConfig(t)
			if revoked {
				token := sampleToken()
				token.Expiry = time.Now().Add(-time.Hour)
				if err := tokenWriter(cachePath(cfg.AuthDir, "desktop-client"), "desktop-client")(token); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = oauthContext(ctx, func(r *http.Request) (*http.Response, error) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("grant_type") == "refresh_token" {
					return response(400, `{"error":"invalid_grant","error_description":"PRIVATE_RESPONSE"}`), nil
				}
				return response(200, `{"access_token":"ACCESS_TOKEN_PRIVATE","refresh_token":"REFRESH_TOKEN_PRIVATE","expires_in":3600,"token_type":"Bearer"}`), nil
			})
			writer := instructionWriter{messages: make(chan string, 1)}
			finished := make(chan error, 1)
			go func() {
				_, err := New(ctx, cfg, writer)
				finished <- err
			}()
			authURL := consentURL(t, writer)
			callback(t, authURL, url.Values{"state": {authURL.Query().Get("state")}, "code": {"CODE_PRIVATE"}})
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("New did not finish after consent")
			}
			token, err := readToken(cachePath(cfg.AuthDir, "desktop-client"), "desktop-client")
			if err != nil || token == nil || token.RefreshToken == "" {
				t.Fatalf("consent token not persisted: %+v %v", token, err)
			}
			info, err := os.Stat(cfg.AuthDir)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("auth directory is not private: %v %v", info, err)
			}
			assertListenerClosed(t, cfg.GoogleOAuthPort)
		})
	}
}

func TestOAuthErrorsSuppressUntrustedProviderDetails(t *testing.T) {
	for _, code := range []string{"invalid_grant", "invalid_client", "unauthorized_client", "access_denied", "other PRIVATE_RESPONSE"} {
		raw := &oauth2.RetrieveError{
			Response: &http.Response{StatusCode: 400}, ErrorCode: code,
			ErrorDescription: "ACCESS_TOKEN_PRIVATE https://private.invalid", Body: []byte("PRIVATE_RESPONSE"),
		}
		err := oauthError(context.Background(), raw)
		if err == nil {
			t.Fatal("OAuth failure swallowed")
		}
		assertNoSecrets(t, err.Error())
	}
	err := oauthError(context.Background(), &url.Error{Op: "Post", URL: "https://private.invalid", Err: errors.New("ACCESS_TOKEN_PRIVATE")})
	assertNoSecrets(t, err.Error())
}

func TestConsentExchangeFailuresDoNotPersistOrLeak(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"exchange rejected", 400, `{"error":"invalid_grant","error_description":"PRIVATE_RESPONSE https://private.invalid"}`},
		{"missing refresh token", 200, `{"access_token":"ACCESS_TOKEN_PRIVATE","expires_in":3600}`},
		{"missing token expiry", 200, `{"access_token":"ACCESS_TOKEN_PRIVATE","refresh_token":"REFRESH_TOKEN_PRIVATE"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := authTestConfig(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = oauthContext(ctx, func(*http.Request) (*http.Response, error) {
				return response(test.status, test.body), nil
			})
			writer := instructionWriter{messages: make(chan string, 1)}
			finished := make(chan error, 1)
			go func() {
				_, err := New(ctx, cfg, writer)
				finished <- err
			}()
			authURL := consentURL(t, writer)
			callback(t, authURL, url.Values{"state": {authURL.Query().Get("state")}, "code": {"CODE_PRIVATE"}})
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("unusable consent response was accepted")
				}
				assertNoSecrets(t, err.Error())
			case <-time.After(3 * time.Second):
				t.Fatal("failed exchange did not finish")
			}
			assertListenerClosed(t, cfg.GoogleOAuthPort)
			if _, err := os.Stat(cachePath(cfg.AuthDir, "desktop-client")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unusable consent response was persisted")
			}
		})
	}
}
