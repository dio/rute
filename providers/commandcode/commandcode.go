// Package commandcode provides the Command Code Provider API endpoints.
//
// Unlike providers/claude and providers/codex, this provider is API-key only:
// it has no OAuth authorization-code profile, so it does not expose an
// oauthpkce.Config. Accounts authenticate with a static API key created in
// Command Code Studio, which is why docs/adding-a-provider.md's OAuth sections
// do not apply to it.
package commandcode

const (
	// APIBaseURL is the Provider API root. The wire endpoints below are
	// appended to it.
	APIBaseURL = "https://api.commandcode.ai/provider/v1"

	// MessagesPath serves Claude models over the Anthropic Messages schema.
	MessagesPath = "/messages"
	// ResponsesPath serves OpenAI and open models over the OpenAI Responses
	// schema.
	ResponsesPath = "/responses"
	// ChatCompletionsPath serves OpenAI and open models over the OpenAI Chat
	// Completions schema.
	ChatCompletionsPath = "/chat/completions"
	// ModelsPath lists the models the account can call.
	ModelsPath = "/models"

	// APIKeyEnv is the primary environment variable consulted for an API key.
	APIKeyEnv = "COMMANDCODE_API_KEY"
	// APIKeyEnvAlt is accepted as a fallback environment variable name.
	APIKeyEnvAlt = "CMD_API_KEY"
)

// Studio login endpoints. Command Code authorizes a CLI login in the browser
// and redirects back to a loopback callback with a freshly created API key,
// the same flow `cmd login` uses.
const (
	// StudioBaseURL is the production Studio origin.
	StudioBaseURL = "https://commandcode.ai"
	// StudioStagingBaseURL is the staging Studio origin.
	StudioStagingBaseURL = "https://staging.commandcode.ai"
	// StudioAuthPath is the Studio CLI authorization endpoint.
	StudioAuthPath = "/studio/auth/cli"
	// CallbackPath is the loopback callback path the Studio redirects to.
	CallbackPath = "/callback"
)

// Endpoint URLs derived from APIBaseURL.
const (
	MessagesURL        = APIBaseURL + MessagesPath
	ResponsesURL       = APIBaseURL + ResponsesPath
	ChatCompletionsURL = APIBaseURL + ChatCompletionsPath
	ModelsURL          = APIBaseURL + ModelsPath
)
