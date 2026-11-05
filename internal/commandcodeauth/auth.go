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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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

// callbackOrigins are the Studio origins the callback answers, matching the
// CLI's own allowlist.
var callbackOrigins = map[string]bool{
	"http://localhost:3000":          true,
	"https://staging.commandcode.ai": true,
	"https://commandcode.ai":         true,
}

const maxCallbackBody = 64 << 10

// callbackPayload is the JSON body the Studio posts to the callback.
type callbackPayload struct {
	APIKey           string `json:"apiKey"`
	State            string `json:"state"`
	UserID           string `json:"userId"`
	UserName         string `json:"userName"`
	KeyName          string `json:"keyName"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// callbackHandler serves the Studio's callback. The Studio posts the result as
// JSON from its own page (a cross-origin request, so CORS is required); the
// query-string GET form is kept as a fallback for redirect-style handoffs.
func callbackHandler(state string, complete func(result)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(commandcode.CallbackPath, func(writer http.ResponseWriter, request *http.Request) {
		writeCallbackCORS(writer, request)
		switch request.Method {
		case http.MethodOptions:
			writer.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			handlePostedCallback(writer, request, state, complete)
		case http.MethodGet:
			handleBrowserCallback(writer, request, state, complete)
		default:
			writer.Header().Set("Allow", "GET, POST, OPTIONS")
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func writeCallbackCORS(writer http.ResponseWriter, request *http.Request) {
	origin := request.Header.Get("Origin")
	if !callbackOrigins[origin] {
		origin = "http://localhost:3000"
	}
	writer.Header().Set("Access-Control-Allow-Origin", origin)
	writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	// Chrome's Private Network Access preflight needs this to allow a public
	// page to reach a loopback server.
	writer.Header().Set("Access-Control-Allow-Private-Network", "true")
	writer.Header().Set("Vary", "Origin")
}

func handlePostedCallback(
	writer http.ResponseWriter,
	request *http.Request,
	state string,
	complete func(result),
) {
	mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaType != "application/json" && mediaType != "application/x-www-form-urlencoded" {
		writeCallbackJSON(writer, http.StatusUnsupportedMediaType, false, "unsupported content type")
		return
	}
	payload, err := readCallbackPayload(request, mediaType)
	if err != nil {
		writeCallbackJSON(writer, http.StatusBadRequest, false, err.Error())
		return
	}
	if !constantTimeEqual(payload.State, state) {
		writeCallbackJSON(writer, http.StatusForbidden, false, "invalid state token")
		complete(result{err: errors.New(
			"commandcodeauth: the callback did not match this login attempt; " +
				"open the URL rute printed, or start a new login",
		)})
		return
	}
	if payload.Error != "" {
		writeCallbackJSON(writer, http.StatusOK, true, "")
		message := strings.TrimSpace(payload.ErrorDescription)
		if message == "" {
			message = payload.Error
		}
		complete(result{err: fmt.Errorf("commandcodeauth: %s", message)})
		return
	}
	if payload.APIKey == "" {
		writeCallbackJSON(writer, http.StatusBadRequest, false, "missing required fields")
		complete(result{err: errors.New("commandcodeauth: callback omitted the API key")})
		return
	}
	writeCallbackJSON(writer, http.StatusOK, true, "")
	complete(result{credentials: Credentials{
		APIKey:          payload.APIKey,
		UserID:          payload.UserID,
		UserName:        payload.UserName,
		KeyName:         payload.KeyName,
		AuthenticatedAt: time.Now().UTC(),
	}})
}

func readCallbackPayload(
	request *http.Request,
	mediaType string,
) (callbackPayload, error) {
	if mediaType == "application/x-www-form-urlencoded" {
		if err := request.ParseForm(); err != nil {
			return callbackPayload{}, errors.New("invalid form body")
		}
		return callbackPayload{
			APIKey:   request.PostFormValue("apiKey"),
			State:    request.PostFormValue("state"),
			UserID:   request.PostFormValue("userId"),
			UserName: request.PostFormValue("userName"),
			KeyName:  request.PostFormValue("keyName"),
		}, nil
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxCallbackBody+1))
	if err != nil {
		return callbackPayload{}, errors.New("could not read the request body")
	}
	if len(body) > maxCallbackBody {
		return callbackPayload{}, errors.New("request body is too large")
	}
	var payload callbackPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return callbackPayload{}, errors.New("invalid JSON body")
	}
	return payload, nil
}

// handleBrowserCallback answers a GET callback, which some handoffs use with
// query parameters. A bare request (a probe, or a page opened directly) does
// not complete the login.
func handleBrowserCallback(
	writer http.ResponseWriter,
	request *http.Request,
	state string,
	complete func(result),
) {
	query := request.URL.Query()
	if query.Get("state") == "" && query.Get("apiKey") == "" && query.Get("error") == "" {
		writeCallbackPage(writer, http.StatusMethodNotAllowed, "Return to your terminal",
			"This page completes login automatically during sign-in. Start a new login from rute "+
				"if you reached it directly.")
		return
	}
	if !constantTimeEqual(query.Get("state"), state) {
		writeCallbackPage(writer, http.StatusForbidden, "Authorization failed",
			"The state token did not match this login attempt. Open the URL rute printed, "+
				"or start a new login.")
		if query.Get("apiKey") != "" || query.Get("error") != "" {
			complete(result{err: errors.New(
				"commandcodeauth: the callback did not match this login attempt; " +
					"open the URL rute printed, or start a new login",
			)})
		}
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
}

func writeCallbackJSON(writer http.ResponseWriter, status int, success bool, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	body := map[string]any{"success": success}
	if message != "" {
		body["error"] = message
	}
	_ = json.NewEncoder(writer).Encode(body)
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
