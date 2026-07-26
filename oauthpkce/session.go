package oauthpkce

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 30 * time.Second
	maxHeaderBytes    = 16 << 10
)

type sessionConfig struct {
	authorizationURL string
	redirectURI      string
	callbackPath     string
	redirectHost     string
	state            string
	verifier         string
	oauthConfig      oauth2.Config
	httpClient       *http.Client
	tokenExchanger   TokenExchanger
	clientID         string
	tokenURL         string
	listener         net.Listener
}

type callbackResult struct {
	code string
	err  error
}

// Session owns one loopback authorization attempt. A Session must not be
// copied.
type Session struct {
	authorizationURL string
	redirectURI      string
	callbackPath     string
	redirectHost     string
	state            string
	verifier         string
	oauthConfig      oauth2.Config
	httpClient       *http.Client
	tokenExchanger   TokenExchanger
	clientID         string
	tokenURL         string

	server     *http.Server
	results    chan callbackResult
	serveErrs  chan error
	done       chan struct{}
	serveDone  chan struct{}
	closeOnce  sync.Once
	terminalMu sync.Mutex
	terminal   bool
	waitMu     sync.Mutex
	waited     bool
}

func startSession(cfg sessionConfig) *Session {
	session := &Session{
		authorizationURL: cfg.authorizationURL,
		redirectURI:      cfg.redirectURI,
		callbackPath:     cfg.callbackPath,
		redirectHost:     cfg.redirectHost,
		state:            cfg.state,
		verifier:         cfg.verifier,
		oauthConfig:      cfg.oauthConfig,
		httpClient:       cfg.httpClient,
		tokenExchanger:   cfg.tokenExchanger,
		clientID:         cfg.clientID,
		tokenURL:         cfg.tokenURL,
		results:          make(chan callbackResult, 1),
		serveErrs:        make(chan error, 1),
		done:             make(chan struct{}),
		serveDone:        make(chan struct{}),
	}
	session.server = &http.Server{
		Handler:           http.HandlerFunc(session.handleCallback),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	go session.serve(cfg.listener)

	return session
}

// AuthorizationURL returns the URL the caller should open in a browser.
func (session *Session) AuthorizationURL() string {
	return session.authorizationURL
}

// RedirectURI returns the exact redirect URI registered for this session.
func (session *Session) RedirectURI() string {
	return session.redirectURI
}

// String returns a redacted representation of the session.
func (session *Session) String() string {
	return "oauthpkce.Session{credentials:<redacted>}"
}

// GoString returns a redacted Go-syntax representation of the session.
func (session *Session) GoString() string {
	return session.String()
}

// Wait waits for the first terminal callback with a valid state and exchanges
// its authorization code for tokens. Wait may be called only once.
func (session *Session) Wait(ctx context.Context) (TokenSet, error) {
	if ctx == nil {
		return TokenSet{}, errors.New("oauthpkce: nil context")
	}
	if err := session.markWaited(); err != nil {
		return TokenSet{}, err
	}

	select {
	case result := <-session.results:
		_ = session.Close()
		if result.err != nil {
			return TokenSet{}, result.err
		}
		return session.exchange(ctx, result.code)
	case err := <-session.serveErrs:
		_ = session.Close()
		return TokenSet{}, err
	case <-session.done:
		return TokenSet{}, ErrSessionClosed
	case <-ctx.Done():
		_ = session.Close()
		return TokenSet{}, ctx.Err()
	}
}

// Close stops the callback listener. It is safe to call Close more than once.
func (session *Session) Close() error {
	var closeErr error
	session.closeOnce.Do(func() {
		close(session.done)
		closeErr = session.server.Close()
		<-session.serveDone
	})
	if errors.Is(closeErr, http.ErrServerClosed) {
		return nil
	}
	return closeErr
}

func (session *Session) markWaited() error {
	session.waitMu.Lock()
	defer session.waitMu.Unlock()

	if session.waited {
		return ErrWaitCalled
	}
	session.waited = true
	return nil
}

func (session *Session) serve(listener net.Listener) {
	defer close(session.serveDone)

	err := session.server.Serve(listener)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return
	}

	select {
	case <-session.done:
	case session.serveErrs <- fmt.Errorf("oauthpkce: callback server: %w", err):
	default:
	}
}

func (session *Session) handleCallback(writer http.ResponseWriter, request *http.Request) {
	setCallbackHeaders(writer.Header())

	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path != session.callbackPath {
		http.NotFound(writer, request)
		return
	}
	if !session.validRequestHost(request.Host) {
		http.Error(writer, "invalid callback host", http.StatusBadRequest)
		return
	}

	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		http.Error(writer, "invalid callback parameters", http.StatusBadRequest)
		return
	}
	result, terminal, err := parseCallback(values, session.state)
	if err != nil {
		http.Error(writer, "invalid callback", http.StatusBadRequest)
		return
	}
	if !terminal {
		http.Error(writer, "invalid callback state", http.StatusBadRequest)
		return
	}
	if !session.reserveTerminal() {
		http.Error(writer, "authorization session already completed", http.StatusConflict)
		return
	}

	if result.err != nil {
		http.Error(writer, "authorization failed; you may close this window", http.StatusBadRequest)
		flushCallback(writer)
		session.deliver(result)
		return
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("Authorization complete. You may close this window."))
	flushCallback(writer)
	session.deliver(result)
}

func (session *Session) validRequestHost(requestHost string) bool {
	host, port, err := net.SplitHostPort(requestHost)
	if err != nil {
		return false
	}
	redirect, err := url.Parse(session.redirectURI)
	if err != nil {
		return false
	}
	redirectPort := redirect.Port()
	return strings.EqualFold(host, session.redirectHost) && port == redirectPort
}

func (session *Session) reserveTerminal() bool {
	session.terminalMu.Lock()
	defer session.terminalMu.Unlock()

	if session.terminal {
		return false
	}
	session.terminal = true
	return true
}

func (session *Session) deliver(result callbackResult) {
	session.results <- result
}

func (session *Session) exchange(ctx context.Context, code string) (TokenSet, error) {
	if session.tokenExchanger != nil {
		tokens, err := session.tokenExchanger.Exchange(ctx, session.httpClient, ExchangeRequest{
			ClientID:     session.clientID,
			TokenURL:     session.tokenURL,
			Code:         code,
			State:        session.state,
			RedirectURI:  session.redirectURI,
			CodeVerifier: session.verifier,
		})
		if err != nil {
			var exchangeErr *TokenExchangeError
			if errors.As(err, &exchangeErr) {
				return TokenSet{}, exchangeErr
			}
			return TokenSet{}, &TokenExchangeError{cause: err}
		}
		return tokens, nil
	}
	exchangeCtx := context.WithValue(ctx, oauth2.HTTPClient, session.httpClient)
	token, err := session.oauthConfig.Exchange(
		exchangeCtx,
		code,
		oauth2.VerifierOption(session.verifier),
	)
	if err != nil {
		return TokenSet{}, newTokenExchangeError(err)
	}

	return tokenSetFromOAuth2(token), nil
}

func newTokenExchangeError(err error) error {
	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		return &TokenExchangeError{
			cause: err,
		}
	}

	statusCode := 0
	if retrieveErr.Response != nil {
		statusCode = retrieveErr.Response.StatusCode
	}
	return &TokenExchangeError{
		StatusCode:  statusCode,
		Code:        retrieveErr.ErrorCode,
		Description: retrieveErr.ErrorDescription,
		URI:         retrieveErr.ErrorURI,
		cause:       err,
	}
}

func parseCallback(values url.Values, expectedState string) (callbackResult, bool, error) {
	states := values["state"]
	if len(states) != 1 {
		return callbackResult{}, false, errors.New("state must occur exactly once")
	}
	if !constantTimeEqual(states[0], expectedState) {
		return callbackResult{}, false, nil
	}

	errorCodes := values["error"]
	if len(errorCodes) > 1 {
		return callbackResult{}, true, errors.New("error must not occur more than once")
	}
	if len(errorCodes) == 1 && errorCodes[0] != "" {
		return callbackResult{
			err: &AuthorizationError{
				Code:        errorCodes[0],
				Description: singleValue(values, "error_description"),
				URI:         singleValue(values, "error_uri"),
			},
		}, true, nil
	}

	codes := values["code"]
	if len(codes) > 1 {
		return callbackResult{}, true, errors.New("code must not occur more than once")
	}
	if len(codes) != 1 || codes[0] == "" {
		return callbackResult{err: ErrMissingCode}, true, nil
	}

	return callbackResult{code: codes[0]}, true, nil
}

func singleValue(values url.Values, key string) string {
	entries := values[key]
	if len(entries) != 1 {
		return ""
	}
	return entries[0]
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func setCallbackHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}

func flushCallback(writer http.ResponseWriter) {
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}
