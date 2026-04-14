package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// OIDCConfig configures the OIDC browser flow.
type OIDCConfig struct {
	IssuerURL   string // e.g. http://localhost:8080/realms/hugr
	ClientID    string // Keycloak public client ID
	RedirectURL string // e.g. http://localhost:10000/auth/callback
	Logger      *slog.Logger
}

// OIDCStore implements TokenStore using Authorization Code + PKCE flow.
// On first Token() call it blocks until the user completes browser login.
// After that it refreshes transparently using the refresh token.
type OIDCStore struct {
	cfg      OIDCConfig
	logger   *slog.Logger
	authURL  string // authorization_endpoint
	tokenURL string // token_endpoint

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	ready        chan struct{} // closed when first login completes
	readyOnce    sync.Once
}

// oidcDiscovery is a subset of OpenID Connect Discovery response.
type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// oidcTokenResponse is the OIDC token endpoint response.
type oidcTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error,omitempty"`
	ErrorDesc    string `json:"error_description,omitempty"`
}

// NewOIDCStore creates a store and discovers OIDC endpoints.
// Call RegisterCallbackRoute to mount the /auth/* routes on your mux.
func NewOIDCStore(ctx context.Context, cfg OIDCConfig) (*OIDCStore, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	disc, err := discover(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}

	return &OIDCStore{
		cfg:      cfg,
		logger:   cfg.Logger,
		authURL:  disc.AuthorizationEndpoint,
		tokenURL: disc.TokenEndpoint,
		ready:    make(chan struct{}),
	}, nil
}

// Token returns a valid access token. On first call it blocks until
// the user completes browser login. After that it refreshes automatically.
func (s *OIDCStore) Token(ctx context.Context) (string, error) {
	// Wait for initial login.
	select {
	case <-s.ready:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if time.Now().Before(s.expiresAt) {
		return s.accessToken, nil
	}

	return s.refresh(ctx)
}

// RegisterCallbackRoute adds /auth/login and /auth/callback to the mux.
// Must be called before the HTTP server starts.
func (s *OIDCStore) RegisterCallbackRoute(mux *http.ServeMux) {
	// Per-login PKCE state.
	var (
		mu            sync.Mutex
		codeVerifier  string
		expectedState string
	)

	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		codeVerifier = generateCodeVerifier()
		expectedState = generateState()
		mu.Unlock()

		challenge := computeCodeChallenge(codeVerifier)

		params := url.Values{
			"response_type":         {"code"},
			"client_id":             {s.cfg.ClientID},
			"redirect_uri":          {s.cfg.RedirectURL},
			"scope":                 {"openid"},
			"state":                 {expectedState},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}

		http.Redirect(w, r, s.authURL+"?"+params.Encode(), http.StatusFound)
	})

	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cv := codeVerifier
		es := expectedState
		mu.Unlock()

		if errParam := r.URL.Query().Get("error"); errParam != "" {
			http.Error(w, fmt.Sprintf("OIDC error: %s — %s", errParam, r.URL.Query().Get("error_description")), http.StatusBadRequest)
			return
		}

		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		if code == "" || state != es {
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}

		tokens, err := s.exchangeCode(r.Context(), code, cv)
		if err != nil {
			http.Error(w, "token exchange failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		s.accessToken = tokens.AccessToken
		s.refreshToken = tokens.RefreshToken
		s.expiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
		s.mu.Unlock()

		// Signal that first login is complete.
		s.readyOnce.Do(func() { close(s.ready) })

		s.logger.Info("OIDC login successful")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><h2>Login successful</h2><p>You can close this tab.</p></body></html>`)
	})
}

// PromptLogin prints the login URL and optionally opens the browser.
func (s *OIDCStore) PromptLogin() {
	loginURL := strings.TrimSuffix(s.cfg.RedirectURL, "/auth/callback") + "/auth/login"
	s.logger.Info("OIDC login required — open in browser", "url", loginURL)
	fmt.Printf("\n  Login: %s\n\n", loginURL)
	_ = openBrowser(loginURL)
}

func (s *OIDCStore) exchangeCode(ctx context.Context, code, codeVerifier string) (*oidcTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.cfg.ClientID},
		"code":          {code},
		"redirect_uri":  {s.cfg.RedirectURL},
		"code_verifier": {codeVerifier},
	}
	return s.tokenRequest(ctx, data)
}

func (s *OIDCStore) refresh(ctx context.Context) (string, error) {
	if s.refreshToken == "" {
		return "", fmt.Errorf("oidc: no refresh token, re-login required")
	}

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {s.cfg.ClientID},
		"refresh_token": {s.refreshToken},
	}

	tokens, err := s.tokenRequest(ctx, data)
	if err != nil {
		return "", err
	}

	s.accessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		s.refreshToken = tokens.RefreshToken
	}
	s.expiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	return s.accessToken, nil
}

func (s *OIDCStore) tokenRequest(ctx context.Context, data url.Values) (*oidcTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc token request: %w", err)
	}
	defer resp.Body.Close()

	var result oidcTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("oidc token response: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("oidc: %s — %s", result.Error, result.ErrorDesc)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("oidc: empty access_token")
	}
	return &result, nil
}

func discover(ctx context.Context, issuerURL string) (*oidcDiscovery, error) {
	wellKnown := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery returned %d", resp.StatusCode)
	}

	var disc oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&disc); err != nil {
		return nil, err
	}
	if disc.AuthorizationEndpoint == "" || disc.TokenEndpoint == "" {
		return nil, fmt.Errorf("missing endpoints in discovery response")
	}
	return &disc, nil
}

func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Run()
	case "linux":
		return exec.Command("xdg-open", url).Run()
	default:
		return nil
	}
}
