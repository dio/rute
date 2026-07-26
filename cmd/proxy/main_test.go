package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if _, err := parseOptions(
		[]string{"-credential-store=keyring", "-credential-file=" + path},
		new(bytes.Buffer),
	); err == nil {
		t.Fatal("credential-file with keyring succeeded")
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
	if opts.action != "add" || opts.name != "personal" || opts.credentialKind != "file" {
		t.Errorf("options = %+v", opts)
	}
	if _, err := parseAccountOptions(
		[]string{"remove", "bad name"},
		new(bytes.Buffer),
	); err == nil {
		t.Fatal("invalid account name succeeded")
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
	if got := stdout.String(); got != "No Codex accounts configured.\n" {
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
