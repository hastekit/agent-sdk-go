package llm

import "strings"

// ParseModelID splits the "Provider/model" notation the SDK names models in —
// "OpenAI/gpt-4o-mini", "Bedrock/us.anthropic.claude-sonnet-4-5-v1:0".
//
// Only the first separator divides the two, so a model id containing slashes
// survives intact. An id naming only a provider yields an empty model, which
// callers read as "whatever model was already asked for" rather than an error.
func ParseModelID(id string) (ProviderName, string) {
	provider, model, _ := strings.Cut(id, "/")
	return ProviderName(provider), model
}
