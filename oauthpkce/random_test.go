package oauthpkce

import (
	"errors"
	"io"
	"regexp"
	"testing"
)

func TestGenerateRandomValue(t *testing.T) {
	t.Parallel()

	first, err := generateRandomValue()
	if err != nil {
		t.Fatalf("generateRandomValue() error = %v", err)
	}
	second, err := generateRandomValue()
	if err != nil {
		t.Fatalf("generateRandomValue() second error = %v", err)
	}

	if len(first) != 43 {
		t.Fatalf("random value length = %d, want 43", len(first))
	}
	if matched := regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(first); !matched {
		t.Fatalf("random value %q is not unpadded base64url", first)
	}
	if first == second {
		t.Fatal("two random values are equal")
	}
}

func TestGenerateRandomValue_EntropyFailure(t *testing.T) {
	t.Parallel()

	want := errors.New("entropy unavailable")
	_, err := generateRandomValueFrom(errorReader{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("generateRandomValueFrom() error = %v, want wrapped %v", err, want)
	}
}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

var _ io.Reader = errorReader{}
