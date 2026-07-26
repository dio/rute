package oauthpkce

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

const randomValueBytes = 32

func generateRandomValue() (string, error) {
	return generateRandomValueFrom(rand.Reader)
}

func generateRandomValueFrom(source io.Reader) (string, error) {
	value := make([]byte, randomValueBytes)
	if _, err := io.ReadFull(source, value); err != nil {
		return "", fmt.Errorf("oauthpkce: generate secure random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
