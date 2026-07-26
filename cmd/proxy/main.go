package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/claudeauth"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/modelcatalog"
	"github.com/dio/rute/internal/proxy"
	"github.com/dio/rute/oauthpkce"
	"github.com/dio/rute/providers/claude"
	"github.com/dio/rute/providers/codex"
)

const (
	defaultListenAddress = "127.0.0.1:20128"
	defaultModel         = "gpt-5.6-sol"
	defaultLoginTimeout  = 5 * time.Minute
	shutdownTimeout      = 10 * time.Second
	defaultStoreKind     = "keyring"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

type options struct {
	listenAddress  string
	defaultModel   string
	loginTimeout   time.Duration
	credentialKind string
	credentialFile string
	modelsFile     string
}

type accountOptions struct {
	action         string
	provider       string
	name           string
	output         string
	loginTimeout   time.Duration
	credentialKind string
	credentialFile string
}

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	if err := run(
		ctx,
		os.Args[1:],
		os.Stdout,
		os.Stderr,
		os.Getenv,
	); err != nil {
		fmt.Fprintf(os.Stderr, "rute proxy: %v\n", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	getenv func(string) string,
) error {
	if len(args) > 0 && args[0] == "version" {
		if len(args) != 1 {
			return errors.New("version does not accept arguments")
		}
		_, err := fmt.Fprintf(
			stdout,
			"rute %s (commit %s, built %s, %s, %s/%s)\n",
			version,
			commit,
			buildDate,
			runtime.Version(),
			runtime.GOOS,
			runtime.GOARCH,
		)
		return err
	}
	if len(args) > 0 && args[0] == "account" {
		return runAccount(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	return runServe(ctx, args, stderr, getenv)
}

func runServe(
	ctx context.Context,
	args []string,
	stderr io.Writer,
	getenv func(string) string,
) error {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	listenAddress, err := normalizeListenAddress(opts.listenAddress)
	if err != nil {
		return err
	}
	catalog, err := modelcatalog.Load(opts.modelsFile)
	if err != nil {
		return err
	}
	oauthConfig := codex.Config()
	authConfig := codexauth.Config{
		ClientID: oauthConfig.ClientID,
		TokenURL: oauthConfig.TokenURL,
	}

	envTokens, err := tokensFromEnvironment(getenv)
	if err != nil {
		return err
	}
	var codexCredentials *codexpool.Pool
	var claudeCredentials *codexpool.Pool
	if envTokens.AccessToken != "" {
		codexCredentials, err = codexpool.NewEphemeral(authConfig, envTokens, stderr)
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, "using ephemeral credentials from RUTE_CODEX_*")
	} else {
		store, err := openStore(opts.credentialKind, opts.credentialFile)
		if err != nil {
			return err
		}
		codexCredentials, err = codexpool.NewProvider(
			ctx,
			store,
			accountstore.ProviderCodex,
			authConfig,
			stderr,
		)
		if err != nil {
			return storageHint(opts.credentialKind, err)
		}
		claudeCredentials, err = codexpool.NewProvider(
			ctx,
			store,
			accountstore.ProviderClaude,
			claudeauth.Config(),
			stderr,
		)
		if err != nil {
			return storageHint(opts.credentialKind, err)
		}
		if len(codexCredentials.Accounts()) == 0 &&
			len(claudeCredentials.Accounts()) == 0 {
			tokens, err := login(
				ctx,
				accountstore.ProviderCodex,
				opts.loginTimeout,
				stderr,
			)
			if err != nil {
				return err
			}
			if _, err := codexCredentials.Add(ctx, "default", tokens); err != nil {
				return storageHint(opts.credentialKind, err)
			}
			fmt.Fprintln(stderr, `saved Codex account "default"`)
		}
		if len(codexCredentials.Accounts()) > 0 {
			if err := codexCredentials.Bootstrap(ctx); err != nil {
				_, _ = fmt.Fprintf(
					stderr,
					"rute proxy: Codex credentials are not currently usable; "+
						"proxy will remain available: %v\n",
					err,
				)
			}
		}
		if len(claudeCredentials.Accounts()) > 0 {
			if err := claudeCredentials.Bootstrap(ctx); err != nil {
				_, _ = fmt.Fprintf(
					stderr,
					"rute proxy: Claude credentials are not currently usable; "+
						"proxy will remain available: %v\n",
					err,
				)
			}
		}
	}

	handler, err := proxy.NewMulti(proxy.Config{
		DefaultModel: opts.defaultModel,
		ModelCatalog: catalog,
	}, codexCredentials, claudeCredentials)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddress, err)
	}
	defer listener.Close()

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(stderr, "rute proxy: ", 0),
	}
	managerCtx, cancelManager := context.WithCancel(ctx)
	var managerWait sync.WaitGroup
	managerWait.Go(func() {
		codexCredentials.Run(managerCtx)
	})
	if claudeCredentials != nil {
		managerWait.Go(func() {
			claudeCredentials.Run(managerCtx)
		})
	}
	defer func() {
		cancelManager()
		managerWait.Wait()
	}()

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	fmt.Fprintf(stderr, "rute proxy listening on http://%s\n", listener.Addr())
	fmt.Fprintln(stderr, "downstream authentication is disabled; loopback access only")
	if catalog.Path() != "" {
		_, _ = fmt.Fprintf(stderr, "loaded model mappings from %s\n", catalog.Path())
	}

	select {
	case serveErr := <-serveErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		serveErr := <-serveErrors
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}

func runAccount(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
) error {
	opts, err := parseAccountOptions(args, stderr)
	if err != nil {
		return err
	}
	if opts.credentialKind == "memory" {
		return errors.New("account commands require persistent keyring or file storage")
	}
	store, err := openStore(opts.credentialKind, opts.credentialFile)
	if err != nil {
		return err
	}

	switch opts.action {
	case "history":
		events, err := accountstore.AuditHistory(ctx, store, accountstore.AuditFilter{
			Provider:    opts.provider,
			AccountName: opts.name,
		})
		if err != nil {
			return err
		}
		if opts.output == "json" {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(events)
		}
		if len(events) == 0 {
			_, err := fmt.Fprintln(stdout, "No account history recorded.")
			return err
		}
		if _, err := fmt.Fprintln(
			stdout,
			"TIME\tSEQUENCE\tPROVIDER\tNAME\tACTION\tSTATE\tHASH",
		); err != nil {
			return err
		}
		for _, event := range events {
			state := string(event.State)
			if state == "" {
				state = "-"
			}
			hash := event.Hash
			if len(hash) > 12 {
				hash = hash[:12]
			}
			if _, err := fmt.Fprintf(
				stdout,
				"%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				event.Timestamp.Format(time.RFC3339),
				event.Sequence,
				event.Provider,
				event.AccountName,
				event.Action,
				state,
				hash,
			); err != nil {
				return err
			}
		}
		return nil
	case "list":
		providers := []string{
			accountstore.ProviderCodex,
			accountstore.ProviderClaude,
		}
		if opts.provider != "" {
			providers = []string{opts.provider}
		}
		var accounts []accountstore.Account
		for _, provider := range providers {
			pool, err := newProviderPool(ctx, store, provider, stderr)
			if err != nil {
				return storageHint(opts.credentialKind, err)
			}
			accounts = append(accounts, pool.Accounts()...)
		}
		if len(accounts) == 0 {
			_, err := fmt.Fprintln(stdout, "No accounts configured.")
			return err
		}
		if _, err := fmt.Fprintln(
			stdout,
			"PROVIDER\tNAME\tSTATE\tUPSTREAM ACCOUNT",
		); err != nil {
			return err
		}
		for _, account := range accounts {
			upstreamID := account.AccountID
			if upstreamID == "" {
				upstreamID = "-"
			}
			if _, err := fmt.Fprintf(
				stdout,
				"%s\t%s\t%s\t%s\n",
				account.Provider,
				account.Name,
				account.State,
				upstreamID,
			); err != nil {
				return err
			}
		}
		return nil
	case "add":
		pool, err := newProviderPool(ctx, store, opts.provider, stderr)
		if err != nil {
			return storageHint(opts.credentialKind, err)
		}
		tokens, err := login(ctx, opts.provider, opts.loginTimeout, stderr)
		if err != nil {
			return err
		}
		account, err := pool.Add(ctx, opts.name, tokens)
		if err != nil {
			return storageHint(opts.credentialKind, err)
		}
		_, err = fmt.Fprintf(
			stdout,
			"Added %s account %q.\n",
			providerDisplayName(opts.provider),
			account.Name,
		)
		return err
	case "reauth":
		pool, err := newProviderPool(ctx, store, opts.provider, stderr)
		if err != nil {
			return storageHint(opts.credentialKind, err)
		}
		tokens, err := login(ctx, opts.provider, opts.loginTimeout, stderr)
		if err != nil {
			return err
		}
		if err := pool.Reauth(ctx, opts.name, tokens); err != nil {
			return storageHint(opts.credentialKind, err)
		}
		_, err = fmt.Fprintf(
			stdout,
			"Reauthenticated %s account %q.\n",
			providerDisplayName(opts.provider),
			opts.name,
		)
		return err
	case "remove":
		pool, err := newProviderPool(ctx, store, opts.provider, stderr)
		if err != nil {
			return storageHint(opts.credentialKind, err)
		}
		if err := pool.Delete(ctx, opts.name); err != nil {
			return storageHint(opts.credentialKind, err)
		}
		_, err = fmt.Fprintf(
			stdout,
			"Removed %s account %q.\n",
			providerDisplayName(opts.provider),
			opts.name,
		)
		return err
	default:
		return fmt.Errorf("unsupported account action %q", opts.action)
	}
}

func newProviderPool(
	ctx context.Context,
	store accountstore.Store,
	provider string,
	stderr io.Writer,
) (*codexpool.Pool, error) {
	var config codexauth.Config
	switch provider {
	case accountstore.ProviderCodex:
		oauthConfig := codex.Config()
		config = codexauth.Config{
			ClientID: oauthConfig.ClientID,
			TokenURL: oauthConfig.TokenURL,
		}
	case accountstore.ProviderClaude:
		config = claudeauth.Config()
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
	return codexpool.NewProvider(ctx, store, provider, config, stderr)
}

func providerDisplayName(provider string) string {
	if provider == accountstore.ProviderClaude {
		return "Claude"
	}
	return "Codex"
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	opts := options{}
	flags := flag.NewFlagSet("rute-proxy serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(
		&opts.listenAddress,
		"listen",
		defaultListenAddress,
		"loopback address to listen on",
	)
	flags.StringVar(&opts.defaultModel, "model", defaultModel, "default upstream model")
	flags.DurationVar(
		&opts.loginTimeout,
		"login-timeout",
		defaultLoginTimeout,
		"maximum time to complete browser login",
	)
	flags.StringVar(
		&opts.modelsFile,
		"models-file",
		"",
		"JSON model mapping file",
	)
	addStorageFlags(flags, &opts.credentialKind, &opts.credentialFile)
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(opts.defaultModel) == "" {
		return options{}, errors.New("model must not be empty")
	}
	if opts.loginTimeout <= 0 {
		return options{}, errors.New("login-timeout must be positive")
	}
	if err := validateStorageOptions(opts.credentialKind, opts.credentialFile); err != nil {
		return options{}, err
	}
	return opts, nil
}

func parseAccountOptions(args []string, stderr io.Writer) (accountOptions, error) {
	if len(args) == 0 {
		return accountOptions{}, errors.New(
			"account action is required: add, history, list, reauth, or remove",
		)
	}
	opts := accountOptions{action: args[0]}
	switch opts.action {
	case "add", "history", "reauth", "remove", "list":
	default:
		return accountOptions{}, fmt.Errorf("unsupported account action %q", opts.action)
	}
	flags := flag.NewFlagSet("rute-proxy account "+opts.action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	providerHelp := "account provider: codex or claude (default: codex)"
	if opts.action == "list" || opts.action == "history" {
		providerHelp = "account provider filter: codex or claude (default: all)"
	}
	flags.StringVar(
		&opts.provider,
		"provider",
		"",
		providerHelp,
	)
	if opts.action == "add" {
		flags.StringVar(&opts.name, "name", "", "account name")
	}
	if opts.action == "add" || opts.action == "reauth" {
		flags.DurationVar(
			&opts.loginTimeout,
			"login-timeout",
			defaultLoginTimeout,
			"maximum time to complete browser login",
		)
	}
	if opts.action == "history" {
		flags.StringVar(
			&opts.output,
			"output",
			"table",
			"history output: table or json",
		)
	}
	addStorageFlags(flags, &opts.credentialKind, &opts.credentialFile)
	if err := flags.Parse(args[1:]); err != nil {
		return accountOptions{}, err
	}
	switch opts.action {
	case "reauth", "remove":
		if flags.NArg() != 1 {
			return accountOptions{}, fmt.Errorf(
				"account %s requires exactly one account name",
				opts.action,
			)
		}
		opts.name = strings.TrimSpace(flags.Arg(0))
	case "history":
		if flags.NArg() > 1 {
			return accountOptions{}, errors.New(
				"account history accepts at most one account name",
			)
		}
		if flags.NArg() == 1 {
			opts.name = strings.TrimSpace(flags.Arg(0))
			if err := accountstore.ValidateName(opts.name); err != nil {
				return accountOptions{}, err
			}
		}
	default:
		if flags.NArg() != 0 {
			return accountOptions{}, errors.New("unexpected positional arguments")
		}
	}
	if opts.action != "list" && opts.action != "history" {
		if opts.provider == "" {
			opts.provider = accountstore.ProviderCodex
		}
		opts.name = strings.TrimSpace(opts.name)
		if err := accountstore.ValidateName(opts.name); err != nil {
			return accountOptions{}, err
		}
	}
	if opts.provider != "" &&
		opts.provider != accountstore.ProviderCodex &&
		opts.provider != accountstore.ProviderClaude {
		return accountOptions{}, errors.New("provider must be codex or claude")
	}
	if opts.action == "history" && opts.output != "table" && opts.output != "json" {
		return accountOptions{}, errors.New("output must be table or json")
	}
	if (opts.action == "add" || opts.action == "reauth") && opts.loginTimeout <= 0 {
		return accountOptions{}, errors.New("login-timeout must be positive")
	}
	if err := validateStorageOptions(opts.credentialKind, opts.credentialFile); err != nil {
		return accountOptions{}, err
	}
	return opts, nil
}

func addStorageFlags(
	flags *flag.FlagSet,
	kind *string,
	path *string,
) {
	flags.StringVar(
		kind,
		"credential-store",
		defaultStoreKind,
		"credential storage: keyring, file, or memory",
	)
	flags.StringVar(
		path,
		"credential-file",
		"",
		"plaintext credential path (file storage only)",
	)
}

func validateStorageOptions(kind, path string) error {
	switch kind {
	case "keyring", "file", "memory":
	default:
		return errors.New("credential-store must be keyring, file, or memory")
	}
	if path != "" && kind != "file" {
		return errors.New("credential-file requires credential-store=file")
	}
	return nil
}

func openStore(kind, path string) (accountstore.Store, error) {
	switch kind {
	case "memory":
		return accountstore.NewMemoryStore(), nil
	case "file":
		if path == "" {
			var err error
			path, err = accountstore.DefaultCredentialsPath()
			if err != nil {
				return nil, err
			}
		}
		return accountstore.NewFileStore(path)
	case "keyring":
		manifest, err := accountstore.DefaultManifestPath()
		if err != nil {
			return nil, err
		}
		return accountstore.NewKeyringStore(manifest)
	default:
		return nil, fmt.Errorf("unsupported credential store %q", kind)
	}
}

func storageHint(kind string, err error) error {
	if kind != "keyring" {
		return err
	}
	return fmt.Errorf(
		"%w (native keyring unavailable or locked; explicitly use "+
			"-credential-store=file or -credential-store=memory if appropriate)",
		err,
	)
}

func normalizeListenAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("invalid listen address: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	} else {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("listen address must use a loopback IP or localhost")
		}
		host = ip.String()
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return "", errors.New("listen address contains an invalid port")
	}
	return net.JoinHostPort(host, port), nil
}

func login(
	ctx context.Context,
	provider string,
	loginTimeout time.Duration,
	stderr io.Writer,
) (oauthpkce.TokenSet, error) {
	var oauthConfig oauthpkce.Config
	switch provider {
	case accountstore.ProviderCodex:
		oauthConfig = codex.Config()
	case accountstore.ProviderClaude:
		oauthConfig = claude.Config()
	default:
		return oauthpkce.TokenSet{}, fmt.Errorf("unsupported provider %q", provider)
	}
	client, err := oauthpkce.New(oauthConfig)
	if err != nil {
		return oauthpkce.TokenSet{}, err
	}
	session, err := client.Start()
	if err != nil {
		return oauthpkce.TokenSet{}, err
	}
	defer session.Close()

	displayName := providerDisplayName(provider)
	_, _ = fmt.Fprintf(
		stderr,
		"Open this URL in your browser to authorize %s:\n",
		displayName,
	)
	fmt.Fprintln(stderr, session.AuthorizationURL())

	loginCtx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	tokens, err := session.Wait(loginCtx)
	if err != nil {
		return oauthpkce.TokenSet{}, fmt.Errorf("%s login: %w", displayName, err)
	}
	return tokens, nil
}

func tokensFromEnvironment(getenv func(string) string) (oauthpkce.TokenSet, error) {
	tokens := oauthpkce.TokenSet{
		AccessToken:  strings.TrimSpace(getenv("RUTE_CODEX_ACCESS_TOKEN")),
		RefreshToken: strings.TrimSpace(getenv("RUTE_CODEX_REFRESH_TOKEN")),
		IDToken:      strings.TrimSpace(getenv("RUTE_CODEX_ID_TOKEN")),
	}
	rawExpiry := strings.TrimSpace(getenv("RUTE_CODEX_EXPIRES_AT"))
	if rawExpiry == "" {
		return tokens, nil
	}
	expiry, err := time.Parse(time.RFC3339, rawExpiry)
	if err != nil {
		return oauthpkce.TokenSet{}, errors.New("RUTE_CODEX_EXPIRES_AT must use RFC3339")
	}
	tokens.Expiry = expiry
	return tokens, nil
}
