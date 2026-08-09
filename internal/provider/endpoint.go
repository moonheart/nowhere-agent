package provider

import "strings"

// EndpointSuffixes are the request paths appended to a provider's API base URL.
// A base URL may be configured as either a bare base ("https://api.openai.com/v1")
// or the full legacy endpoint (".../v1/chat/completions"); NormalizeBase reduces
// both to the base so the same root serves chat and model-list calls.
const (
	EndpointChat   = "chat/completions"
	EndpointMsg    = "messages"
	EndpointModels = "models"
)

// NormalizeBase reduces a configured base URL to its API root by stripping any
// recognized request-path suffix. A bare base (".../v1") or a test host with no
// path passes through unchanged.
func NormalizeBase(url string) string {
	url = strings.TrimRight(url, "/")
	for _, suffix := range []string{EndpointChat, EndpointMsg, EndpointModels} {
		if strings.HasSuffix(url, "/"+suffix) {
			return strings.TrimSuffix(url, "/"+suffix)
		}
	}
	return url
}

// ResolveEndpoint joins a configured base URL (root or full endpoint) with a
// request-path suffix, producing the concrete URL a call is sent to. A base
// that is already the full endpoint is used verbatim; otherwise the suffix is
// appended (after stripping a trailing slash).
func ResolveEndpoint(base, suffix string) string {
	base = NormalizeBase(base)
	if strings.HasSuffix(base, "/"+suffix) {
		return base
	}
	return base + "/" + suffix
}
