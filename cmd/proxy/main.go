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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dio/rute/internal/accountstore"
	"github.com/dio/rute/internal/admin"
	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/claudeauth"
	"github.com/dio/rute/internal/codexauth"
	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/latency"
	"github.com/dio/rute/internal/modelcatalog"
	"github.com/dio/rute/internal/proxy"
	"github.com/dio/rute/oauthpkce"
	"github.com/dio/rute/providers/claude"
	"github.com/dio/rute/providers/codex"
)

const (
	defaultListenAddress = "127.0.0.1:20128"
	defaultAdminAddress  = "127.0.0.1:20129"
	defaultRetention     = 30 * 24 * time.Hour
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
	metricsFile    string
	metricsDB      string
	retention      time.Duration
	adminListen    string
	codexMaxCalls  int
	claudeMaxCalls int
	codexBalance   balanceOptions
	claudeBalance  balanceOptions
}

type balanceOptions struct {
	mode  string
	order string
}

// apply configures pool's balance mode and limit.
func (balance balanceOptions) apply(pool *codexpool.Pool, provider string, limit int) error {
	mode, err := codexpool.ParseBalance(balance.mode)
	if err != nil {
		return fmt.Errorf("%s: %w", provider, err)
	}
	var names []string
	for name := range strings.SplitSeq(balance.order, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	err = pool.SetRouting(mode, names, limit)
	switch {
	case errors.Is(err, codexpool.ErrOrderNeedsOrderedBalance):
		return fmt.Errorf("-%s-account-order requires -%s-balance=priority or failover", provider, provider)
	case errors.Is(err, codexpool.ErrPriorityNeedsLimit):
		return fmt.Errorf("-%s-balance=priority requires -%s-max-in-flight; "+
			"use failover to move only on rate limits", provider, provider)
	}
	return err
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
	if len(args) > 0 && args[0] == "bench" {
		return runBench(ctx, args[1:], stdout, stderr, getenv)
	}
	if len(args) > 0 && args[0] == "stats" {
		return runStats(args[1:], stdout, stderr)
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

	if err := opts.codexBalance.apply(
		codexCredentials,
		accountstore.ProviderCodex,
		opts.codexMaxCalls,
	); err != nil {
		return err
	}
	if claudeCredentials != nil {
		if err := opts.claudeBalance.apply(
			claudeCredentials,
			accountstore.ProviderClaude,
			opts.claudeMaxCalls,
		); err != nil {
			return err
		}
	}
	var sinks latency.Sinks
	if opts.metricsFile != "" {
		writer, err := latency.OpenWriter(opts.metricsFile)
		if err != nil {
			return err
		}
		defer func() { _ = writer.Close() }()
		sinks = append(sinks, writer)
	}
	calls, err := openCallStore(opts.metricsDB)
	if err != nil {
		return err
	}
	if calls != nil {
		defer func() { _ = calls.Close() }()
		sinks = append(sinks, calls)
	}
	var metrics latency.Sink
	if len(sinks) > 0 {
		metrics = sinks
	}
	handler, err := proxy.NewMulti(proxy.Config{
		DefaultModel: opts.defaultModel,
		ModelCatalog: catalog,
		Metrics:      metrics,
		Log:          log.New(stderr, "rute proxy: ", 0),
	}, codexCredentials, claudeCredentials)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddress, err)
	}
	defer listener.Close()

	servers := []*http.Server{newHTTPServer(handler, stderr)}
	listeners := []net.Listener{listener}
	if opts.adminListen != "off" {
		adminAddress, err := normalizeListenAddress(opts.adminListen)
		if err != nil {
			return fmt.Errorf("admin-listen: %w", err)
		}
		pools := map[string]*codexpool.Pool{accountstore.ProviderCodex: codexCredentials}
		if claudeCredentials != nil {
			pools[accountstore.ProviderClaude] = claudeCredentials
		}
		adminHandler, err := admin.New(admin.Config{
			Version:      version,
			Started:      time.Now(),
			Pools:        pools,
			Calls:        calls,
			LoginTimeout: opts.loginTimeout,
		})
		if err != nil {
			return err
		}
		adminListener, err := net.Listen("tcp", adminAddress)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", adminAddress, err)
		}
		defer adminListener.Close()
		servers = append(servers, newHTTPServer(adminHandler, stderr))
		listeners = append(listeners, adminListener)
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
	if calls != nil {
		managerWait.Go(func() {
			calls.Run(managerCtx, opts.retention, func(err error) {
				_, _ = fmt.Fprintf(stderr, "rute proxy: call history: %v\n", err)
			})
		})
	}
	defer func() {
		cancelManager()
		managerWait.Wait()
	}()

	serveErrors := make(chan error, len(servers))
	for index, server := range servers {
		go func() {
			serveErrors <- server.Serve(listeners[index])
		}()
	}

	fmt.Fprintf(stderr, "rute proxy listening on http://%s\n", listener.Addr())
	if len(listeners) > 1 {
		_, _ = fmt.Fprintf(stderr, "rute management on http://%s\n", listeners[1].Addr())
	}
	fmt.Fprintln(stderr, "downstream authentication is disabled; loopback access only")
	if catalog.Path() != "" {
		_, _ = fmt.Fprintf(stderr, "loaded model mappings from %s\n", catalog.Path())
	}
	if opts.metricsFile != "" {
		_, _ = fmt.Fprintf(stderr, "writing call latency records to %s\n", opts.metricsFile)
	}
	if calls != nil {
		_, _ = fmt.Fprintf(stderr, "keeping call history in %s\n", opts.metricsDB)
	}

	var firstErr error
	select {
	case serveErr := <-serveErrors:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			firstErr = serveErr
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shut down HTTP server: %w", err)
		}
	}
	return firstErr
}

func newHTTPServer(handler http.Handler, stderr io.Writer) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(stderr, "rute proxy: ", 0),
	}
}

// openCallStore opens the call history database, or returns nil when path is
// "off".
func openCallStore(path string) (*callstore.Store, error) {
	if path == "off" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create call history directory: %w", err)
	}
	return callstore.Open(path)
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
	flags.StringVar(
		&opts.adminListen,
		"admin-listen",
		defaultAdminAddress,
		`loopback address for the management dashboard and API, or "off"`,
	)
	flags.StringVar(
		&opts.metricsDB,
		"metrics-db",
		"",
		`SQLite per-minute call counters for the dashboard (default: metrics.db in the state directory), or "off"`,
	)
	flags.DurationVar(
		&opts.retention,
		"metrics-retention",
		defaultRetention,
		"how long to keep per-minute call counters in -metrics-db",
	)
	flags.StringVar(
		&opts.metricsFile,
		"metrics-file",
		"",
		"append one JSONL latency record per inference call to this file",
	)
	flags.IntVar(
		&opts.codexMaxCalls,
		"codex-max-in-flight",
		0,
		"calls per Codex account above which sessions move to a less loaded account (0: no limit)",
	)
	flags.IntVar(
		&opts.claudeMaxCalls,
		"claude-max-in-flight",
		0,
		"calls per Claude account above which sessions move to a less loaded account (0: no limit)",
	)
	for _, provider := range []struct {
		name    string
		balance *balanceOptions
	}{
		{accountstore.ProviderCodex, &opts.codexBalance},
		{accountstore.ProviderClaude, &opts.claudeBalance},
	} {
		flags.StringVar(
			&provider.balance.mode,
			provider.name+"-balance",
			string(codexpool.BalanceSpread),
			"how calls use "+provider.name+" accounts: spread (least loaded), "+
				"priority (in order, next one when busy or rate-limited), or "+
				"failover (in order, next one only when rate-limited)",
		)
		flags.StringVar(
			&provider.balance.order,
			provider.name+"-account-order",
			"",
			"comma-separated "+provider.name+" account names, most preferred first "+
				"(priority and failover; default: order added)",
		)
	}
	addStorageFlags(flags, &opts.credentialKind, &opts.credentialFile)
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if opts.retention <= 0 {
		return options{}, errors.New("metrics-retention must be positive")
	}
	if opts.metricsDB == "" {
		dir, err := accountstore.DefaultStateDir()
		if err != nil {
			return options{}, err
		}
		opts.metricsDB = filepath.Join(dir, "metrics.db")
	}
	if opts.codexMaxCalls < 0 || opts.claudeMaxCalls < 0 {
		return options{}, errors.New("max-in-flight limits must not be negative")
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
