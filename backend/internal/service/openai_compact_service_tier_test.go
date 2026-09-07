package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAICompactRequestBodyPreservesServiceTier(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"message","role":"user","content":"hello"}],
		"service_tier":"priority",
		"prompt_cache_key":"compact-cache-key",
		"store":false,
		"stream":true
	}`)

	normalized, changed, err := normalizeOpenAICompactRequestBody(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(normalized, "model").String())
	require.Equal(t, "priority", gjson.GetBytes(normalized, "service_tier").String())
	require.Equal(t, "compact-cache-key", gjson.GetBytes(normalized, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(normalized, "store").Exists())
	require.False(t, gjson.GetBytes(normalized, "stream").Exists())
}

func TestOpenAIOAuthCompactHTTPBuildersUsePreservedServiceTierInRoutingHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"message","role":"user","content":"hello"}],
		"service_tier":"priority",
		"stream":true
	}`)
	normalized, changed, err := normalizeOpenAICompactRequestBody(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "priority", gjson.GetBytes(normalized, "service_tier").String())

	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "test-account",
		},
	}
	svc := &OpenAIGatewayService{}

	tests := []struct {
		name  string
		build func(*gin.Context) (*http.Request, error)
	}{
		{
			name: "ordinary",
			build: func(c *gin.Context) (*http.Request, error) {
				return svc.buildUpstreamRequest(
					context.Background(), c, account, normalized, "test-token",
					false, "", true,
				)
			},
		},
		{
			name: "passthrough",
			build: func(c *gin.Context) (*http.Request, error) {
				return svc.buildUpstreamRequestOpenAIPassthrough(
					context.Background(), c, account, normalized, "test-token",
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(
				http.MethodPost,
				"/v1/responses/compact",
				bytes.NewReader(normalized),
			)

			req, buildErr := tt.build(c)
			require.NoError(t, buildErr)
			require.Equal(
				t,
				"model=gpt-5.6-sol;tier=priority",
				req.Header.Get(openAICodexRoutingHintHeader),
			)
			require.Equal(t, "priority", gjson.GetBytes(normalized, "service_tier").String())
		})
	}
}

func TestCodexCompactRetainsResponsesIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []codexFingerprintMode{codexFingerprintOff, codexFingerprintDevice} {
		for _, passthrough := range []bool{false, true} {
			for _, cacheKey := range []string{"root-session", "custom-cache"} {
				t.Run(fmt.Sprintf("%s/passthrough=%t/cache=%s", mode, passthrough, cacheKey), func(t *testing.T) {
					account := newTestOAuthAccount(4470, map[string]any{
						codexFingerprintModeExtraKey: string(mode), "openai_passthrough": passthrough,
					})
					account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
					var responsesHeaders http.Header
					var responsesCache string
					for _, compact := range []bool{false, true} {
						body := []byte(fmt.Sprintf(`{"model":"gpt-5.4","instructions":"compress","stream":true,"store":false,"prompt_cache_key":%q,"client_metadata":{"session_id":"root-session"},"input":[{"type":"message","role":"user","content":"hello"}]}`, cacheKey))
						path := "/v1/responses"
						if compact {
							path += "/compact"
							var err error
							body, _, err = normalizeOpenAICompactRequestBody(body)
							require.NoError(t, err)
						}
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest(http.MethodPost, path, nil)
						c.Set("api_key", &APIKey{ID: 77})
						c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
						c.Request.Header.Set("session-id", "root-session")
						c.Request.Header.Set("x-codex-installation-id", "installation")
						c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"installation_id":"installation","session_id":"root-session","thread_id":"root-session","turn_id":"turn","window_id":"root-session:0"}`)
						upstream := &httpUpstreamRecorder{resp: &http.Response{
							StatusCode: http.StatusOK,
							Header:     http.Header{"Content-Type": {"text/event-stream"}},
							Body:       io.NopCloser(strings.NewReader(compactProbeSSESuccessBody)),
						}}
						svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector()}
						_, err := svc.Forward(context.Background(), c, account, body)
						require.NoError(t, err)
						require.NotNil(t, upstream.lastReq)
						cache := gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
						require.NotEmpty(t, cache)
						if !compact {
							responsesHeaders = upstream.lastReq.Header.Clone()
							responsesCache = cache
							continue
						}
						require.Equal(t, responsesCache, cache)
						for _, header := range []string{"session_id", "session-id", "thread-id", "x-codex-installation-id", "x-codex-window-id"} {
							require.Equal(t, responsesHeaders.Get(header), upstream.lastReq.Header.Get(header), header)
						}
						require.Empty(t, upstream.lastReq.Header.Get("x-client-request-id"), "official compact does not project the request-id compatibility header")
						for _, field := range []string{"client_metadata", "store", "stream"} {
							require.False(t, gjson.GetBytes(upstream.lastBody, field).Exists(), field)
						}
						if cacheKey == "custom-cache" {
							require.Equal(t, scopeCodexAccountIdentityValue(account, 77, "prompt-cache", cacheKey), cache)
						}
						if ids := stagedCodexFingerprintIDs(c, account); ids != nil && ids.mode != codexFingerprintDevice {
							require.Equal(t, ids.turnID, gjson.Get(upstream.lastReq.Header.Get(openAIWSTurnMetadataHeader), "turn_id").String())
						}
					}
				})
			}
		}
	}
}
