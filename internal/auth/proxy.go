package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TokenCache struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ProxyAuth struct {
	loginURL        string
	tokenCookieName string
	proxyPort       int
	tokenCacheFile  string
	token           string
}

func NewProxyAuth(loginURL, tokenCookieName string, proxyPort int, tokenCacheFile string) *ProxyAuth {
	return &ProxyAuth{
		loginURL:        loginURL,
		tokenCookieName: tokenCookieName,
		proxyPort:       proxyPort,
		tokenCacheFile:  tokenCacheFile,
	}
}

func (p *ProxyAuth) GetToken(ctx context.Context) (string, error) {
	if p.token != "" {
		return p.token, nil
	}

	cachedToken, err := p.loadTokenFromCache()
	if err == nil && cachedToken != "" {
		p.token = cachedToken
		return p.token, nil
	}

	if err := p.performAuth(ctx); err != nil {
		return "", fmt.Errorf("authentication failed: %w", err)
	}

	return p.token, nil
}

func (p *ProxyAuth) loadTokenFromCache() (string, error) {
	expanded := expandPath(p.tokenCacheFile)
	data, err := os.ReadFile(expanded)
	if err != nil {
		return "", err
	}

	var cache TokenCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return "", err
	}

	if time.Now().After(cache.ExpiresAt) {
		return "", fmt.Errorf("token expired")
	}

	return cache.Token, nil
}

func (p *ProxyAuth) saveTokenToCache(token string) error {
	expanded := expandPath(p.tokenCacheFile)

	dir := filepath.Dir(expanded)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	cache := TokenCache{
		Token:     token,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}

	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}

	return os.WriteFile(expanded, data, 0600)
}

func (p *ProxyAuth) performAuth(ctx context.Context) error {
	log.Printf("Starting authentication flow...")
	log.Printf("Please log in at: %s", p.loginURL)

	server := &http.Server{
		Addr: fmt.Sprintf(":%d", p.proxyPort),
	}

	var authToken string

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		dump, _ := httputil.DumpRequest(r, true)

		for _, cookie := range r.Cookies() {
			if cookie.Name == p.tokenCookieName {
				authToken = cookie.Value
				log.Printf("Captured token from cookie: %s", p.tokenCookieName)

				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, "<html><body><h1>Login successful!</h1><p>You can close this window.</p></body></html>")

				go func() {
					server.Shutdown(context.Background())
				}()
				return
			}
		}

		log.Printf("Request: %s", string(dump))

		if authToken == "" {
			w.Header().Set("Location", p.loginURL)
			w.WriteHeader(http.StatusFound)
		}
	})

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Proxy server error: %v", err)
		}
	}()

	timeout := 5 * time.Minute
	select {
	case <-ctx.Done():
		server.Shutdown(context.Background())
		return ctx.Err()
	case <-time.After(timeout):
		server.Shutdown(context.Background())
		return fmt.Errorf("authentication timeout after %v", timeout)
	case <-func() chan struct{} {
		ch := make(chan struct{})
		go func() {
			for authToken == "" {
				time.Sleep(500 * time.Millisecond)
			}
			close(ch)
		}()
		return ch
	}():
	}

	if authToken == "" {
		return fmt.Errorf("failed to capture token")
	}

	p.token = authToken

	if err := p.saveTokenToCache(authToken); err != nil {
		log.Printf("Warning: failed to save token to cache: %v", err)
	}

	log.Printf("Authentication successful")
	return nil
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

var _ = io.Discard
