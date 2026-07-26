package oauthpkce

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

const defaultHTTPTimeout = 30 * time.Second

// Client creates independent OAuth login sessions from a validated public
// client configuration.
type Client struct {
	config     Config
	httpClient *http.Client
}

// New validates cfg and creates a PKCE client.
func New(cfg Config) (*Client, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}

	httpClient := normalized.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: defaultHTTPTimeout,
		}
	}

	return &Client{
		config:     normalized,
		httpClient: httpClient,
	}, nil
}

// Start binds a new loopback callback listener and creates a fresh PKCE
// session. The caller must close the returned session.
func (client *Client) Start() (*Session, error) {
	verifier, err := generateRandomValue()
	if err != nil {
		return nil, err
	}
	state, err := generateRandomValue()
	if err != nil {
		return nil, err
	}

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(client.config.Loopback.Port))
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("oauthpkce: listen on loopback callback: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := (&url.URL{
		Scheme: "http",
		Host: net.JoinHostPort(
			client.config.Loopback.RedirectHost,
			strconv.Itoa(port),
		),
		Path: client.config.Loopback.CallbackPath,
	}).String()

	oauthConfig := oauth2.Config{
		ClientID:    client.config.ClientID,
		RedirectURL: redirectURI,
		Scopes:      append([]string{}, client.config.Scopes...),
		Endpoint: oauth2.Endpoint{
			AuthURL:   client.config.AuthorizationURL,
			TokenURL:  client.config.TokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}

	authorizationURL, err := buildAuthorizationURL(
		client.config,
		redirectURI,
		state,
		verifier,
	)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	return startSession(sessionConfig{
		authorizationURL: authorizationURL,
		redirectURI:      redirectURI,
		callbackPath:     client.config.Loopback.CallbackPath,
		redirectHost:     client.config.Loopback.RedirectHost,
		state:            state,
		verifier:         verifier,
		oauthConfig:      oauthConfig,
		httpClient:       client.httpClient,
		tokenExchanger:   client.config.TokenExchanger,
		clientID:         client.config.ClientID,
		tokenURL:         client.config.TokenURL,
		listener:         listener,
	}), nil
}

func buildAuthorizationURL(
	cfg Config,
	redirectURI string,
	state string,
	verifier string,
) (string, error) {
	endpoint, err := validateEndpoint("authorization", cfg.AuthorizationURL)
	if err != nil {
		return "", err
	}

	params := endpoint.Query()
	for key := range params {
		if _, reserved := reservedAuthorizationParams[key]; reserved {
			return "", fmt.Errorf("oauthpkce: authorization endpoint query parameter %q is reserved", key)
		}
	}
	for key, values := range cfg.AuthorizationParams {
		for _, value := range values {
			params.Add(key, value)
		}
	}

	params.Set("client_id", cfg.ClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", strings.Join(cfg.Scopes, " "))
	params.Set("state", state)
	params.Set("code_challenge", oauth2.S256ChallengeFromVerifier(verifier))
	params.Set("code_challenge_method", "S256")

	// QueryEscape represents spaces as '+'. OAuth accepts either form, but some
	// public clients require the percent-encoded representation for parity.
	endpoint.RawQuery = strings.ReplaceAll(params.Encode(), "+", "%20")

	return endpoint.String(), nil
}
