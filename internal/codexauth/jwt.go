package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const maxJWTPayloadBytes = 64 << 10

func accountID(tokens ...string) string {
	for _, token := range tokens {
		if account := accountIDFromJWT(token); account != "" {
			return account
		}
	}
	return ""
}

// accountIDFromJWT decodes metadata only. It is not authentication or signature
// verification; the bearer token itself remains the upstream credential.
func accountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > maxJWTPayloadBytes*2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > maxJWTPayloadBytes {
		return ""
	}
	var claims struct {
		OpenAIAuth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if claims.OpenAIAuth.AccountID != "" {
		return claims.OpenAIAuth.AccountID
	}
	return claims.AccountID
}
