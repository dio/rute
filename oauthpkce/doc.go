// Package oauthpkce implements OAuth 2.0 Authorization Code login sessions
// protected by PKCE using a short-lived loopback callback listener.
//
// Callers remain responsible for opening the authorization URL and storing the
// returned credentials securely.
package oauthpkce
