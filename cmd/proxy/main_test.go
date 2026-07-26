package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dio/rute/internal/accountstore"
)

func TestNormalizeListenAddress(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "IPv4", input: "127.0.0.1:1234", want: "127.0.0.1:1234"},
		{name: "localhost", input: "localhost:1234", want: "127.0.0.1:1234"},
		{name: "IPv6", input: "[::1]:1234", want: "[::1]:1234"},
		{name: "wildcard", input: "0.0.0.0:1234", wantErr: true},
		{name: "public", input: "192.0.2.1:1234", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeListenAddress(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("address = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseOptionsCredentialStore(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "credentials.json")
	opts, err := parseOptions([]string{
		"-credential-store=file",
		"-credential-file=" + path,
	}, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("parse options: %v", err)
	}
	if opts.credentialKind != "file" || opts.credentialFile != path {
		t.Errorf("storage options = %q, %q", opts.credentialKind, opts.credentialFile)
	}
	if opts.modelsFile != "" {
		t.Errorf("models file = %q, want empty", opts.modelsFile)
	}
	if _, err := parseOptions(
		[]string{"-credential-store=keyring", "-credential-file=" + path},
		new(bytes.Buffer),
	); err == nil {
		t.Fatal("credential-file with keyring succeeded")
	}

	opts, err = parseOptions(
		[]string{"-models-file=/tmp/models.json"},
		new(bytes.Buffer),
	)
	if err != nil {
		t.Fatalf("parse models file: %v", err)
	}
	if opts.modelsFile != "/tmp/models.json" {
		t.Errorf("models file = %q", opts.modelsFile)
	}
}

func TestParseAccountOptions(t *testing.T) {
	t.Parallel()

	opts, err := parseAccountOptions(
		[]string{"add", "-name=personal", "-credential-store=file"},
		new(bytes.Buffer),
	)
	if err != nil {
		t.Fatalf("parse account options: %v", err)
	}
	if opts.action != "add" ||
		opts.provider != "codex" ||
		opts.name != "personal" ||
		opts.credentialKind != "file" {
		t.Errorf("options = %+v", opts)
	}
	opts, err = parseAccountOptions(
		[]string{"add", "-provider=claude", "-name=personal"},
		new(bytes.Buffer),
	)
	if err != nil {
		t.Fatalf("parse Claude account options: %v", err)
	}
	if opts.provider != "claude" {
		t.Errorf("provider = %q, want claude", opts.provider)
	}
	opts, err = parseAccountOptions(
		[]string{"history", "-provider=claude", "-output=json", "personal"},
		new(bytes.Buffer),
	)
	if err != nil {
		t.Fatalf("parse history options: %v", err)
	}
	if opts.provider != "claude" || opts.name != "personal" || opts.output != "json" {
		t.Errorf("history options = %+v", opts)
	}
	if _, err := parseAccountOptions(
		[]string{"remove", "bad name"},
		new(bytes.Buffer),
	); err == nil {
		t.Fatal("invalid account name succeeded")
	}
}

func TestRunAccountHistoryFileStore(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "credentials.json")
	store, err := accountstore.NewFileStore(path)
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	for _, event := range []accountstore.AuditEvent{
		{
			Action:      accountstore.AuditAccountAdded,
			Provider:    accountstore.ProviderCodex,
			RecordID:    "codex-record",
			AccountName: "personal",
			State:       accountstore.StateActive,
		},
		{
			Action:      accountstore.AuditAccountAdded,
			Provider:    accountstore.ProviderClaude,
			RecordID:    "claude-record",
			AccountName: "work",
			State:       accountstore.StateActive,
		},
	} {
		if err := store.RecordAudit(context.Background(), event); err != nil {
			t.Fatalf("record audit event: %v", err)
		}
	}

	var stdout bytes.Buffer
	err = run(
		context.Background(),
		[]string{
			"account",
			"history",
			"-provider=codex",
			"-output=json",
			"-credential-store=file",
			"-credential-file=" + path,
			"personal",
		},
		&stdout,
		new(bytes.Buffer),
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatalf("run account history: %v", err)
	}
	var events []accountstore.AuditEvent
	if err := json.Unmarshal(stdout.Bytes(), &events); err != nil {
		t.Fatalf("decode history output: %v", err)
	}
	if len(events) != 1 ||
		events[0].Provider != accountstore.ProviderCodex ||
		events[0].AccountName != "personal" {
		t.Errorf("history events = %+v", events)
	}
}

func TestRunAccountListFileStore(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := run(
		context.Background(),
		[]string{
			"account",
			"list",
			"-credential-store=file",
			"-credential-file=" + filepath.Join(t.TempDir(), "state", "credentials.json"),
		},
		&stdout,
		new(bytes.Buffer),
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatalf("run account list: %v", err)
	}
	if got := stdout.String(); got != "No accounts configured.\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunVersion(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := run(
		context.Background(),
		[]string{"version"},
		&stdout,
		new(bytes.Buffer),
		func(string) string { return "" },
	)
	if err != nil {
		t.Fatalf("run version: %v", err)
	}
	for _, want := range []string{"rute dev", "commit none", "built unknown"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}

	if err := run(
		context.Background(),
		[]string{"version", "extra"},
		new(bytes.Buffer),
		new(bytes.Buffer),
		func(string) string { return "" },
	); err == nil {
		t.Fatal("version with an argument succeeded")
	}
}

func TestTokensFromEnvironment(t *testing.T) {
	t.Parallel()

	expiry := time.Now().UTC().Truncate(time.Second)
	values := map[string]string{
		"RUTE_CODEX_ACCESS_TOKEN":  " access ",
		"RUTE_CODEX_REFRESH_TOKEN": " refresh ",
		"RUTE_CODEX_ID_TOKEN":      " id ",
		"RUTE_CODEX_EXPIRES_AT":    expiry.Format(time.RFC3339),
	}
	tokens, err := tokensFromEnvironment(func(key string) string {
		return values[key]
	})
	if err != nil {
		t.Fatalf("tokens from environment: %v", err)
	}
	if tokens.AccessToken != "access" ||
		tokens.RefreshToken != "refresh" ||
		tokens.IDToken != "id" ||
		!tokens.Expiry.Equal(expiry) {
		t.Errorf("tokens = %v", tokens)
	}
}
