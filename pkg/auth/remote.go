package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// RemoteStore exchanges an expired access token for a new one via an external
// token provider URL.
//
// The provider itself handles refresh logic — it always holds a valid token.
// If the provider hasn't refreshed yet, it may return the same (old) token,
// so RemoteStore retries with backoff: 5s, 30s, 150s.
type RemoteStore struct {
	tokenURL string
	client   *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewRemoteStore creates a RemoteStore with the initial access token
// and the URL of the token exchange service.
func NewRemoteStore(accessToken, tokenURL string) *RemoteStore {
	return &RemoteStore{
		tokenURL:  tokenURL,
		token:     accessToken,
		expiresAt: time.Now().Add(30 * time.Second), // use initial token briefly before first refresh
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *RemoteStore) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && time.Now().Before(s.expiresAt) {
		return s.token, nil
	}

	return s.refresh(ctx)
}

// refresh exchanges the expired token for a new one.
// Retries up to 3 times (5s, 30s, 150s) if the provider returns the same token.
func (s *RemoteStore) refresh(ctx context.Context) (string, error) {
	oldToken := s.token
	backoff := []time.Duration{5 * time.Second, 30 * time.Second, 150 * time.Second}

	for attempt, wait := range backoff {
		newToken, expiresIn, err := s.exchange(ctx, oldToken)
		if err != nil {
			return "", err // 401/403 — fatal, no retry
		}

		if newToken != oldToken {
			s.token = newToken
			s.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
			return s.token, nil
		}

		// Provider returned the same token — hasn't refreshed yet, wait and retry.
		if attempt < len(backoff)-1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
		}
	}

	return "", fmt.Errorf("token exchange: provider did not return a new token after %d retries", len(backoff))
}

type exchangeRequest struct {
	Token string `json:"token"`
}

type exchangeResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error,omitempty"`
}

func (s *RemoteStore) exchange(ctx context.Context, expiredToken string) (string, int, error) {
	body, _ := json.Marshal(exchangeRequest{Token: expiredToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("token exchange: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	// Check status before decoding — non-200 may return HTML, not JSON.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", 0, fmt.Errorf("token exchange: %d — credentials rejected", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token exchange: unexpected status %d", resp.StatusCode)
	}

	var result exchangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", 0, fmt.Errorf("token exchange: decode response: %w", err)
	}
	if result.AccessToken == "" {
		return "", 0, fmt.Errorf("token exchange: empty access_token in response")
	}
	return result.AccessToken, result.ExpiresIn, nil
}
