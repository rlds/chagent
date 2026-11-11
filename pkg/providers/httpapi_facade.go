package providers

import httpapi "github.com/rlds/chagent/pkg/providers/httpapi"

type (
	HTTPProvider = httpapi.HTTPProvider
)

func NewHTTPProvider(apiKey, apiBase, proxy string) *HTTPProvider {
	return httpapi.NewHTTPProvider(apiKey, apiBase, proxy)
}

func NewHTTPProviderWithMaxTokensField(apiKey, apiBase, proxy, maxTokensField string) *HTTPProvider {
	return httpapi.NewHTTPProviderWithMaxTokensField(apiKey, apiBase, proxy, maxTokensField)
}

func NewHTTPProviderWithMaxTokensFieldAndRequestTimeout(
	apiKey, apiBase, proxy, maxTokensField, userAgent string,
	requestTimeoutSeconds int,
	extraBody map[string]any,
	customHeaders map[string]string,
) *HTTPProvider {
	return httpapi.NewHTTPProviderWithMaxTokensFieldAndRequestTimeout(
		apiKey,
		apiBase,
		proxy,
		maxTokensField,
		userAgent,
		requestTimeoutSeconds,
		extraBody,
		customHeaders,
	)
}
