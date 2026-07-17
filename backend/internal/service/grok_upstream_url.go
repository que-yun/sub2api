package service

import (
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

func grokBaseURLValidator(account *Account, cfg *config.Config) (xai.BaseURLValidator, error) {
	if account == nil || !account.IsGrok() {
		return nil, fmt.Errorf("grok account is required")
	}
	switch account.Type {
	case AccountTypeOAuth:
		// Subscription credentials are never governed by the operator's API-key
		// URL policy. They stay pinned to the supported CLI gateway.
		return redactedGrokBaseURLValidator(xai.ValidateTrustedBaseURL), nil
	case AccountTypeAPIKey:
		if cfg == nil {
			return redactedGrokBaseURLValidator(xai.ValidateBaseURL), nil
		}
		if !cfg.Security.URLAllowlist.Enabled {
			return redactedGrokBaseURLValidator(func(raw string) (string, error) {
				return urlvalidator.ValidateURLFormat(raw, cfg.Security.URLAllowlist.AllowInsecureHTTP)
			}), nil
		}
		return redactedGrokBaseURLValidator(func(raw string) (string, error) {
			return urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{
				AllowedHosts:     cfg.Security.URLAllowlist.UpstreamHosts,
				RequireAllowlist: true,
				AllowPrivate:     cfg.Security.URLAllowlist.AllowPrivateHosts,
			})
		}), nil
	default:
		return nil, fmt.Errorf("unsupported grok account type: %s", account.Type)
	}
}

func redactedGrokBaseURLValidator(validator xai.BaseURLValidator) xai.BaseURLValidator {
	return func(raw string) (string, error) {
		validated, err := validator(raw)
		if err != nil {
			return "", errors.New("base URL rejected by URL security policy")
		}
		return validated, nil
	}
}

func buildGrokURLWithBase(account *Account, cfg *config.Config, baseURL string, build func(string, xai.BaseURLValidator) (string, error)) (string, error) {
	validator, err := grokBaseURLValidator(account, cfg)
	if err != nil {
		return "", err
	}
	return build(baseURL, validator)
}

func buildGrokResponsesURLWithBase(account *Account, cfg *config.Config, baseURL string) (string, error) {
	return buildGrokURLWithBase(account, cfg, baseURL, xai.BuildResponsesURLWithValidator)
}

func buildGrokChatCompletionsURLWithBase(account *Account, cfg *config.Config, baseURL string) (string, error) {
	return buildGrokURLWithBase(account, cfg, baseURL, xai.BuildChatCompletionsURLWithValidator)
}

func buildGrokMediaURLWithBase(account *Account, cfg *config.Config, endpoint GrokMediaEndpoint, requestID, baseURL string) (string, error) {
	validator, err := grokBaseURLValidator(account, cfg)
	if err != nil {
		return "", err
	}
	switch endpoint {
	case GrokMediaEndpointImagesGenerations:
		return xai.BuildImagesGenerationsURLWithValidator(baseURL, validator)
	case GrokMediaEndpointImagesEdits:
		return xai.BuildImagesEditsURLWithValidator(baseURL, validator)
	case GrokMediaEndpointVideosGenerations:
		return xai.BuildVideosGenerationsURLWithValidator(baseURL, validator)
	case GrokMediaEndpointVideosEdits:
		return xai.BuildVideosEditsURLWithValidator(baseURL, validator)
	case GrokMediaEndpointVideosExtensions:
		return xai.BuildVideosExtensionsURLWithValidator(baseURL, validator)
	case GrokMediaEndpointVideoStatus:
		return xai.BuildVideoURLWithValidator(baseURL, requestID, validator)
	default:
		return "", fmt.Errorf("unsupported grok media endpoint: %s", endpoint)
	}
}

func buildGrokResponsesURL(account *Account, cfg *config.Config) (string, error) {
	if account == nil {
		return "", fmt.Errorf("grok account is required")
	}
	return buildGrokResponsesURLWithBase(account, cfg, account.GetGrokBaseURL())
}

func buildGrokChatCompletionsURL(account *Account, cfg *config.Config) (string, error) {
	if account == nil {
		return "", fmt.Errorf("grok account is required")
	}
	return buildGrokChatCompletionsURLWithBase(account, cfg, account.GetGrokBaseURL())
}

func buildGrokMediaURL(account *Account, cfg *config.Config, endpoint GrokMediaEndpoint, requestID string) (string, error) {
	if account == nil {
		return "", fmt.Errorf("grok account is required")
	}
	return buildGrokMediaURLWithBase(account, cfg, endpoint, requestID, account.GetGrokMediaBaseURL())
}
