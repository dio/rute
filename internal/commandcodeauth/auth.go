// Package commandcodeauth runs the Command Code browser login flow.
//
// Command Code authorizes a CLI login in the browser: the CLI opens
// <studio>/studio/auth/cli with a loopback callback and a random state, the
// Studio redirects the browser back to the callback with a freshly created
// API key, and the CLI stores that key. This is the same flow `cmd login`
// uses; it provisions a static API key rather than an OAuth refresh token.
package commandcodeauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dio/rute/providers/commandcode"
)

const (
	// DefaultListenHost is the loopback host the callback server binds to.
	DefaultListenHost = "127.0.0.1"
	// DefaultTimeout is how long to wait for the browser to authorize.
	DefaultTimeout = 5 * time.Minute

	stateBytes = 32
)

// Credentials are the API key and account metadata the Studio returns.
type Credentials struct {
	APIKey          string
	UserID          string
	UserName        string
	KeyName         string
	AuthenticatedAt time.Time
}

// Options configures a login.
type Options struct {
	// StudioBaseURL defaults to the production Studio origin.
	StudioBaseURL string
	// ListenHost defaults to 127.0.0.1.
	ListenHost string
	// Port is the callback port; zero picks a free port.
	Port int
	// Timeout defaults to DefaultTimeout.
	Timeout time.Duration
	// OnAuthorizationURL, when set, receives the URL to open before the
	// browser is launched.
	OnAuthorizationURL func(authorizationURL string)
	// OpenBrowser opens the authorization URL. Best-effort: a failure is
	// ignored so the caller can open the URL manually.
	OpenBrowser func(authorizationURL string) error
}

// Session is an in-progress browser login. The caller supplies the
// authorization URL to the user (and usually opens it), then calls Wait.
type Session struct {
	authorizationURL string
	timeout          time.Duration
	server           *http.Server
	results          chan result
	closeOnce        sync.Once
}

// Start begins a login without opening a browser.
func Start(opts Options) (*Session, error) {
	studio := strings.TrimRight(strings.TrimSpace(opts.StudioBaseURL), "/")
	if studio == "" {
		studio = commandcode.StudioBaseURL
	}
	host := strings.TrimSpace(opts.ListenHost)
	if host == "" {
		host = DefaultListenHost
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(opts.Port)))
	if err != nil {
		return nil, fmt.Errorf("commandcodeauth: listen for callback: %w", err)
	}
	callbackURL := "http://" + listener.Addr().String() + commandcode.CallbackPath

	var (
		once    sync.Once
		results = make(chan result, 1)
	)
	complete := func(value result) {
		once.Do(func() { results <- value })
	}
	server := &http.Server{
		Handler:           callbackHandler(state, complete),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()
	return &Session{
		authorizationURL: buildAuthorizationURL(studio, callbackURL, state),
		timeout:          timeout,
		server:           server,
		results:          results,
	}, nil
}

// AuthorizationURL is the URL the user must open to authorize the login.
func (session *Session) AuthorizationURL() string {
	return session.authorizationURL
}

// Wait blocks until the browser completes the login or ctx is done, or the
// login times out.
func (session *Session) Wait(ctx context.Context) (Credentials, error) {
	waitCtx, cancel := context.WithTimeout(ctx, session.timeout)
	defer cancel()
	select {
	case value := <-session.results:
		return value.credentials, value.err
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return Credentials{}, errors.New("commandcodeauth: timed out waiting for browser authorization")
		}
		return Credentials{}, waitCtx.Err()
	}
}

// Close stops the callback server.
func (session *Session) Close() error {
	session.closeOnce.Do(func() { _ = session.server.Close() })
	return nil
}

// Authenticate runs the browser login and returns the provisioned credentials.
func Authenticate(ctx context.Context, opts Options) (Credentials, error) {
	if opts.OpenBrowser == nil {
		return Credentials{}, errors.New("commandcodeauth: OpenBrowser is required")
	}
	session, err := Start(opts)
	if err != nil {
		return Credentials{}, err
	}
	defer func() { _ = session.Close() }()
	if opts.OnAuthorizationURL != nil {
		opts.OnAuthorizationURL(session.AuthorizationURL())
	}
	_ = opts.OpenBrowser(session.AuthorizationURL())
	return session.Wait(ctx)
}

type result struct {
	credentials Credentials
	err         error
}

func callbackHandler(
	state string,
	complete func(result),
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(commandcode.CallbackPath, func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if !constantTimeEqual(query.Get("state"), state) {
			writeCallbackPage(writer, http.StatusForbidden, "Authorization failed",
				"The state token did not match this login attempt. Return to your terminal and retry.")
			return
		}
		if code := query.Get("error"); code != "" {
			writeCallbackPage(writer, http.StatusOK, "Authorization denied",
				"Command Code was not authorized. No API key was created.")
			transport := strings.TrimSpace(query.Get("error_description"))
			if transport == "" {
				transport = code
			}
			complete(result{err: fmt.Errorf("commandcodeauth: %s", transport)})
			return
		}
		apiKey := query.Get("apiKey")
		if apiKey == "" {
			writeCallbackPage(writer, http.StatusBadRequest, "Authorization failed",
				"The callback did not include an API key.")
			complete(result{err: errors.New("commandcodeauth: callback omitted the API key")})
			return
		}
		writeCallbackPage(writer, http.StatusOK, "Authenticated",
			"You can close this window and return to rute.")
		complete(result{credentials: Credentials{
			APIKey:          apiKey,
			UserID:          query.Get("userId"),
			UserName:        query.Get("userName"),
			KeyName:         query.Get("keyName"),
			AuthenticatedAt: time.Now().UTC(),
		}})
	})
	return mux
}

func writeCallbackPage(writer http.ResponseWriter, status int, heading, detail string) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(
		writer,
		"<!DOCTYPE html><html><head><title>%s</title></head>"+
			"<body style=\"font-family:sans-serif;text-align:center;padding:40px\">"+
			"<h1>%s</h1><p>%s</p></body></html>",
		heading,
		heading,
		detail,
	)
}

func buildAuthorizationURL(studioBase, callbackURL, state string) string {
	values := url.Values{}
	values.Set("callback", callbackURL)
	values.Set("state", state)
	values.Set("mode", "redirect")
	return studioBase + commandcode.StudioAuthPath + "?" + values.Encode()
}

func randomState() (string, error) {
	var value [stateBytes]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("commandcodeauth: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
