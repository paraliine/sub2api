package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func rewriteCodexAccountGraphForTest(t *testing.T, metadata map[string]any, headers http.Header, account *Account, apiKeyID int64) map[string]any {
	t.Helper()
	embedded, err := json.Marshal(metadata)
	require.NoError(t, err)
	clientMetadata := make(map[string]any, len(metadata)+1)
	for key, value := range metadata {
		clientMetadata[key] = value
	}
	clientMetadata[openAIWSTurnMetadataHeader] = string(embedded)
	body, err := json.Marshal(map[string]any{
		"client_metadata":  clientMetadata,
		"prompt_cache_key": metadata["session_id"],
		"input":            []any{map[string]any{"role": "user", "content": "hello"}},
	})
	require.NoError(t, err)
	rewritten, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, apiKeyID, headers)
	require.NoError(t, err)
	require.True(t, changed)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.True(t, applyCodexAccountIdentityClientMetadataMap(decoded, account, apiKeyID, headers))
	encodedMap, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.JSONEq(t, string(encodedMap), string(rewritten))
	require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(rewritten, "input").Raw)
	result := decoded["client_metadata"].(map[string]any)
	var embeddedResult map[string]any
	require.NoError(t, json.Unmarshal([]byte(result[openAIWSTurnMetadataHeader].(string)), &embeddedResult))
	for field := range metadata {
		require.Equal(t, result[field], embeddedResult[field], field)
	}
	if _, ok := metadata["session_id"].(string); ok {
		require.Equal(t, result["session_id"], decoded["prompt_cache_key"])
	}
	return result
}

func TestCodexAccountIdentityGraphRootAcrossTransports(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "account-a"}}
	metadata := map[string]any{
		"installation_id": "same-raw", "session_id": "same-raw", "thread_id": "same-raw",
		"x-client-request-id": "same-raw", "turn_id": "same-raw", "window_id": "same-raw",
		"window_number": float64(3), "turn_started_at_unix_ms": float64(1234567890000),
		"request_kind": "turn", "custom_metadata": map[string]any{"enabled": true},
	}
	embedded, err := json.Marshal(metadata)
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 77})
	for _, name := range []string{"session-id", "thread-id", "x-client-request-id", "x-codex-installation-id", "x-codex-window-id"} {
		c.Request.Header.Set(name, "same-raw")
	}
	c.Request.Header.Set(openAIWSTurnMetadataHeader, string(embedded))
	originalHeaders := c.Request.Header.Clone()
	result := rewriteCodexAccountGraphForTest(t, metadata, c.Request.Header, account, 77)
	require.Equal(t, result["session_id"], result["thread_id"])
	require.Equal(t, result["thread_id"], result["x-client-request-id"])
	require.NotEqual(t, "same-raw", result["session_id"])
	// Reuse the existing session derivation so session/cache identities do not rotate.
	require.Equal(t, scopeCodexAccountIdentityValue(account, 77, "session", "same-raw"), result["session_id"])
	identities := map[any]bool{}
	for _, name := range []string{"installation_id", "session_id", "turn_id", "window_id"} {
		require.False(t, identities[result[name]], "independent identity domains must remain distinct: %s", name)
		identities[result[name]] = true
	}
	for _, name := range []string{"window_number", "turn_started_at_unix_ms", "request_kind", "custom_metadata"} {
		require.Equal(t, metadata[name], result[name], name)
	}
	require.Equal(t, originalHeaders, c.Request.Header)

	svc := &OpenAIGatewayService{}
	req, err := svc.buildUpstreamRequest(context.Background(), c, account,
		[]byte(`{"model":"gpt-5.6-codex","stream":true}`), "token", true, "same-raw", true)
	require.NoError(t, err)
	wsHeaders, _, err := svc.buildOpenAIWSHeaders(context.Background(), c, account, "token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "", string(embedded), "same-raw", "", "")
	require.NoError(t, err)
	for name, headers := range map[string]http.Header{"http": req.Header, "websocket": wsHeaders} {
		t.Run(name, func(t *testing.T) {
			var projected map[string]any
			require.NoError(t, json.Unmarshal([]byte(headers.Get(openAIWSTurnMetadataHeader)), &projected))
			for field := range metadata {
				require.Equal(t, result[field], projected[field], field)
			}
			require.Equal(t, result["installation_id"], headers.Get("x-codex-installation-id"))
			require.Equal(t, result["window_id"], headers.Get("x-codex-window-id"))
		})
	}
	for _, name := range []string{"session-id", "thread-id", "x-client-request-id"} {
		require.Equal(t, result["session_id"], wsHeaders.Get(name), name)
	}
}

func TestCodexAccountIdentityGraphLineageAcrossRequests(t *testing.T) {
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "account-a"}}
	childMetadata := map[string]any{
		"session_id": "root", "thread_id": "child", "thread-id": "child", "x-client-request-id": "child",
		"parent_thread_id": "root", "x-codex-parent-thread-id": "root", "forked_from_thread_id": "root",
		"turn_id": "child-turn", "parent_turn_id": "root-turn", "root_turn_id": "root-turn",
		"window_id": "child:1", "context_window_id": "context-next",
		"first_window_id": "context-first", "previous_window_id": "context-first", "window_number": float64(1),
	}
	// Resolve the child first: references must not depend on having seen the parent.
	child := rewriteCodexAccountGraphForTest(t, childMetadata, nil, account, 77)
	root := rewriteCodexAccountGraphForTest(t, map[string]any{
		"session_id": "root", "thread_id": "root", "turn_id": "root-turn",
		"window_id": "root:0", "context_window_id": "context-first",
	}, nil, account, 77)
	for _, field := range []string{"session_id", "parent_thread_id", "x-codex-parent-thread-id", "forked_from_thread_id"} {
		require.Equal(t, root["thread_id"], child[field], field)
	}
	require.NotEqual(t, root["thread_id"], child["thread_id"])
	require.Equal(t, child["thread_id"], child["thread-id"])
	require.Equal(t, child["thread_id"], child["x-client-request-id"])
	require.Equal(t, root["turn_id"], child["parent_turn_id"])
	require.Equal(t, root["turn_id"], child["root_turn_id"])
	require.NotEqual(t, root["turn_id"], child["turn_id"])
	require.Equal(t, root["context_window_id"], child["first_window_id"])
	require.Equal(t, root["context_window_id"], child["previous_window_id"])
	require.NotEqual(t, child["previous_window_id"], child["context_window_id"])
	headers := make(http.Header)
	for _, field := range []string{"x-codex-parent-thread-id", "parent_turn_id", "root_turn_id"} {
		headers.Set(field, childMetadata[field].(string))
	}
	applyCodexAccountIdentityHeaders(headers, account, 77)
	for _, field := range []string{"x-codex-parent-thread-id", "parent_turn_id", "root_turn_id"} {
		require.Equal(t, child[field], headers.Get(field), field)
	}

	sameCredential := *account
	sameCredential.ID = 99
	require.Equal(t, child, rewriteCodexAccountGraphForTest(t, childMetadata, nil, &sameCredential, 77))
	require.Equal(t, child, rewriteCodexAccountGraphForTest(t, childMetadata, nil, account, 77))
	otherAccount := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "account-b"}}
	for _, other := range []map[string]any{
		rewriteCodexAccountGraphForTest(t, childMetadata, nil, otherAccount, 77),
		rewriteCodexAccountGraphForTest(t, childMetadata, nil, account, 88),
	} {
		for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "turn_id", "root_turn_id", "previous_window_id"} {
			require.NotEqual(t, child[field], other[field], field)
		}
	}
}

func TestCodexAccountIdentityGraphPromptCacheSessionSources(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "account-a"}}
	for _, tc := range []struct {
		name      string
		metadata  map[string]any
		headers   map[string]string
		cacheKey  string
		cacheKind string
	}{
		{name: "body", metadata: map[string]any{"session_id": "root"}, cacheKey: "root", cacheKind: "session"},
		{name: "body_alias", metadata: map[string]any{"session-id": "root"}, cacheKey: "root", cacheKind: "session"},
		{name: "header", headers: map[string]string{"session-id": "root"}, cacheKey: "root", cacheKind: "session"},
		{name: "legacy_header", headers: map[string]string{"session_id": "root"}, cacheKey: "root", cacheKind: "session"},
		{name: "embedded_body", metadata: map[string]any{openAIWSTurnMetadataHeader: `{"session_id":"root"}`}, cacheKey: "root", cacheKind: "session"},
		{name: "embedded_header", headers: map[string]string{openAIWSTurnMetadataHeader: `{"session_id":"root"}`}, cacheKey: "root", cacheKind: "session"},
		{name: "custom_cache", metadata: map[string]any{"session_id": "root"}, cacheKey: "custom", cacheKind: "prompt-cache"},
		{name: "thread_is_not_session", metadata: map[string]any{"thread_id": "child"}, cacheKey: "child", cacheKind: "prompt-cache"},
		{name: "invalid_metadata", metadata: map[string]any{openAIWSTurnMetadataHeader: `{"session_id":"root"`}, cacheKey: "root", cacheKind: "prompt-cache"},
		{name: "nonobject_metadata", headers: map[string]string{openAIWSTurnMetadataHeader: `["root"]`}, cacheKey: "root", cacheKind: "prompt-cache"},
		{name: "whitespace", metadata: map[string]any{"session_id": " root "}, cacheKey: "root", cacheKind: "session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			for name, value := range tc.headers {
				headers.Set(name, value)
			}
			body, err := json.Marshal(map[string]any{"client_metadata": tc.metadata, "prompt_cache_key": tc.cacheKey})
			require.NoError(t, err)
			rewritten, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 77, headers)
			require.NoError(t, err)
			require.True(t, changed)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(body, &decoded))
			require.True(t, applyCodexAccountIdentityClientMetadataMap(decoded, account, 77, headers))
			encodedMap, err := json.Marshal(decoded)
			require.NoError(t, err)
			require.JSONEq(t, string(encodedMap), string(rewritten))
			require.Equal(t, scopeCodexAccountIdentityValue(account, 77, tc.cacheKind, tc.cacheKey), decoded["prompt_cache_key"])
			if tc.metadata == nil {
				require.Nil(t, decoded["client_metadata"], "do not synthesize absent identity metadata")
			}
		})
	}
}

func TestCodexAccountIdentityGraphWithoutNamespaceIsUnchanged(t *testing.T) {
	body := []byte(`{"client_metadata":{"session_id":"root","thread_id":"root","parent_thread_id":"parent"},"prompt_cache_key":"root"}`)
	headers := make(http.Header)
	headers.Set("session-id", "root")
	headers.Set("thread-id", "root")
	for _, account := range []*Account{
		nil,
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
	} {
		rewritten, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 77, headers)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, rewritten)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))
		require.False(t, applyCodexAccountIdentityClientMetadataMap(decoded, account, 77, headers))
		projected := headers.Clone()
		applyCodexAccountIdentityHeaders(projected, account, 77)
		require.Equal(t, headers, projected)
	}
}

func newCodexIdentitySnapshotTestContext(t *testing.T, path string, headers http.Header) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Request.Header = headers.Clone()
	c.Set("api_key", &APIKey{ID: 77})
	return c
}

func decodeCodexIdentityBodyForTest(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	return decoded
}

func decodeCodexEmbeddedMetadataForTest(t *testing.T, clientMetadata map[string]any) map[string]any {
	t.Helper()
	raw, ok := clientMetadata[openAIWSTurnMetadataHeader].(string)
	require.True(t, ok)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &metadata))
	return metadata
}

func TestCodexIdentitySnapshotProjectsCanonicalMetadataAcrossTransports(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID:          41,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "canonical-account"},
	}
	canonical := map[string]any{
		"installation_id":      "canonical-install",
		"session_id":           "root",
		"thread_id":            "root",
		"turn_id":              "turn-2",
		"window_id":            "root:2",
		"window_number":        float64(2),
		"context_window_id":    "context-2",
		"previous_window_id":   "context-1",
		"first_window_id":      "context-0",
		"request_kind":         "turn",
		"tool_namespaces_info": map[string]any{"count": float64(1)},
		"custom_metadata":      "preserved",
	}
	canonicalJSON, err := json.Marshal(canonical)
	require.NoError(t, err)
	bodyMap := map[string]any{
		"model":            "gpt-5.6-codex",
		"prompt_cache_key": "root",
		"client_metadata": map[string]any{
			"x-codex-installation-id":  "conflicting-flat-install",
			"session_id":               "conflicting-flat-session",
			"thread_id":                "conflicting-flat-thread",
			"x-client-request-id":      "conflicting-flat-request",
			"x-codex-window-id":        "conflicting-flat-window:9",
			"x-openai-subagent":        "collab_spawn",
			openAIWSTurnMetadataHeader: string(canonicalJSON),
		},
	}
	body, err := json.Marshal(bodyMap)
	require.NoError(t, err)
	headerMetadata := `{"installation_id":"header-install","session_id":"header-session","thread_id":"header-thread","window_id":"header-thread:8","request_kind":"turn"}`
	clientHeaders := make(http.Header)
	clientHeaders.Set("x-codex-installation-id", "header-install")
	clientHeaders.Set("session-id", "header-session")
	clientHeaders.Set("thread-id", "header-thread")
	clientHeaders.Set("x-client-request-id", "header-request")
	clientHeaders.Set("x-codex-window-id", "header-thread:8")
	clientHeaders.Set("x-openai-subagent", "review")
	clientHeaders.Set(openAIWSTurnMetadataHeader, headerMetadata)
	c := newCodexIdentitySnapshotTestContext(t, "/v1/responses", clientHeaders)

	snapshot := prepareCodexIdentitySnapshot(c, account, body)
	require.NotNil(t, snapshot)
	wantSession := scopeCodexAccountIdentityValue(account, 77, "session", "root")
	wantInstallation := scopeCodexAccountIdentityValue(account, 77, "installation", "canonical-install")
	require.Equal(t, wantSession, snapshot.metadata["session_id"])
	require.Equal(t, wantSession, snapshot.metadata["thread_id"])
	require.Equal(t, wantSession+":2", snapshot.metadata["window_id"])
	require.Equal(t, wantSession, snapshot.promptCacheKey)
	require.Equal(t, "collab_spawn", snapshot.subagentHeader)

	mapProjection := decodeCodexIdentityBodyForTest(t, body)
	require.True(t, applyCodexIdentitySnapshotToBodyMap(mapProjection, snapshot, true))
	rawProjection, changed, err := applyCodexIdentitySnapshotToBodyRaw(body, snapshot, true)
	require.NoError(t, err)
	require.True(t, changed)
	encodedMap, err := json.Marshal(mapProjection)
	require.NoError(t, err)
	require.JSONEq(t, string(encodedMap), string(rawProjection), "transformed and passthrough projections must match")
	projectedCM := mapProjection["client_metadata"].(map[string]any)
	projectedMetadata := decodeCodexEmbeddedMetadataForTest(t, projectedCM)
	require.Equal(t, wantInstallation, projectedCM["x-codex-installation-id"])
	require.Equal(t, wantSession, projectedCM["session_id"])
	require.Equal(t, wantSession, projectedCM["thread_id"])
	require.Equal(t, wantSession, projectedCM["x-client-request-id"])
	require.Equal(t, wantSession+":2", projectedCM["x-codex-window-id"])
	require.Equal(t, wantSession, mapProjection["prompt_cache_key"])
	require.Equal(t, projectedCM["session_id"], projectedCM["thread_id"])
	require.Equal(t, projectedCM["thread_id"], projectedCM["x-client-request-id"])
	require.Equal(t, projectedCM["session_id"], mapProjection["prompt_cache_key"])
	require.Equal(t, map[string]any{"count": float64(1)}, projectedMetadata["tool_namespaces_info"])
	require.Equal(t, "preserved", projectedMetadata["custom_metadata"])

	svc := &OpenAIGatewayService{}
	httpRequest, err := svc.buildUpstreamRequest(context.Background(), c, account, rawProjection, "token", true, "root", true)
	require.NoError(t, err)
	httpHeaders := httpRequest.Header
	wsHeaders, _, err := svc.buildOpenAIWSHeaders(
		context.Background(), c, account, "token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		true, "", headerMetadata, "root", "gpt-5.6-codex", "",
	)
	require.NoError(t, err)
	for _, name := range []string{
		"x-codex-installation-id", "session-id", "thread-id", "x-client-request-id",
		"x-codex-window-id", "x-openai-subagent", openAIWSTurnMetadataHeader,
	} {
		require.Equal(t, httpHeaders.Get(name), wsHeaders.Get(name), name)
	}
	require.Equal(t, wantInstallation, httpHeaders.Get("x-codex-installation-id"))
	require.Equal(t, wantSession, httpHeaders.Get("session-id"))
	require.Equal(t, wantSession, httpHeaders.Get("thread-id"))
	require.Equal(t, wantSession, httpHeaders.Get("x-client-request-id"))
	require.Equal(t, wantSession+":2", httpHeaders.Get("x-codex-window-id"))
	var compatibilityMetadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(httpHeaders.Get(openAIWSTurnMetadataHeader)), &compatibilityMetadata))
	require.NotContains(t, compatibilityMetadata, "tool_namespaces_info")
	require.Equal(t, "preserved", compatibilityMetadata["custom_metadata"])

	compactBody, changed, err := applyCodexIdentitySnapshotToBodyRaw(body, snapshot, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(compactBody, "client_metadata").Exists())
	require.Equal(t, wantSession, gjson.GetBytes(compactBody, "prompt_cache_key").String())
	compactHeaders := clientHeaders.Clone()
	applyCodexIdentitySnapshotToHeaders(compactHeaders, snapshot, false)
	require.Equal(t, wantInstallation, compactHeaders.Get("x-codex-installation-id"))
	require.Equal(t, wantSession, compactHeaders.Get("session-id"))
	require.Equal(t, wantSession, compactHeaders.Get("thread-id"))
	require.Equal(t, wantSession+":2", compactHeaders.Get("x-codex-window-id"))
	require.Empty(t, compactHeaders.Get("x-client-request-id"))
}

func TestCodexIdentitySnapshotPreservesExplicitPromptCacheOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 40, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "cache-account"}}
	embedded := `{"session_id":"root","thread_id":"root","window_id":"root:0","request_kind":"turn"}`
	body := []byte(`{"prompt_cache_key":"explicit-cache","client_metadata":{"x-codex-turn-metadata":` + strconv.Quote(embedded) + `}}`)
	c := newCodexIdentitySnapshotTestContext(t, "/v1/responses", http.Header{})
	snapshot := prepareCodexIdentitySnapshot(c, account, body)
	require.NotNil(t, snapshot)
	require.Equal(t, scopeCodexAccountIdentityValue(account, 77, "prompt-cache", "explicit-cache"), snapshot.promptCacheKey)
	require.NotEqual(t, snapshot.metadata["session_id"], snapshot.promptCacheKey)
}

func TestCodexIdentitySnapshotDeviceOverridesOnlyInstallation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "device-account"},
		Extra: map[string]any{
			codexFingerprintModeExtraKey: "device",
			codexFingerprintSeedExtraKey: testCodexFingerprintSeed,
			"openai_device_id":           "device-d1",
		},
	}
	embedded := `{"installation_id":"client-install","session_id":"root","thread_id":"root","window_id":"root:0","request_kind":"turn"}`
	body := []byte(`{"prompt_cache_key":"root","client_metadata":{"x-codex-installation-id":"client-install","installation_id":"client-install","session_id":"root","thread_id":"root","x-client-request-id":"root","x-codex-window-id":"root:0","x-codex-turn-metadata":` + strconv.Quote(embedded) + `}}`)
	c := newCodexIdentitySnapshotTestContext(t, "/v1/responses", http.Header{})
	snapshot := prepareCodexIdentitySnapshot(c, account, body)
	require.NotNil(t, snapshot)
	projected, changed, err := applyCodexIdentitySnapshotToBodyRaw(body, snapshot, true)
	require.NoError(t, err)
	require.True(t, changed)
	decoded := decodeCodexIdentityBodyForTest(t, projected)
	clientMetadata := decoded["client_metadata"].(map[string]any)
	turnMetadata := decodeCodexEmbeddedMetadataForTest(t, clientMetadata)
	wantSession := scopeCodexAccountIdentityValue(account, 77, "session", "root")
	require.Equal(t, "device-d1", clientMetadata["x-codex-installation-id"])
	require.Equal(t, "device-d1", clientMetadata["installation_id"])
	require.Equal(t, "device-d1", turnMetadata["installation_id"])
	require.Equal(t, wantSession, clientMetadata["session_id"])
	require.Equal(t, wantSession, clientMetadata["thread_id"])
	require.Equal(t, wantSession, clientMetadata["x-client-request-id"])
	require.Equal(t, wantSession+":0", clientMetadata["x-codex-window-id"])
	require.Equal(t, wantSession, decoded["prompt_cache_key"])
}

func TestCodexIdentitySnapshotOmitsMalformedMetadataAndRefreshesOnFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"prompt_cache_key":"root","client_metadata":{"session_id":"root","thread_id":"root","x-client-request-id":"root","x-codex-window-id":"root:0","x-codex-turn-metadata":"{broken"}}`)
	clientHeaders := make(http.Header)
	clientHeaders.Set(openAIWSTurnMetadataHeader, "[broken")
	clientHeaders.Set("session-id", "header-session")
	firstAccount := &Account{ID: 51, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "account-a"}}
	secondAccount := &Account{ID: 52, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "account-b"}}
	c := newCodexIdentitySnapshotTestContext(t, "/v1/responses", clientHeaders)
	svc := &OpenAIGatewayService{}

	_, err := svc.prepareCodexAccountIdentitySource(context.Background(), c, firstAccount)
	require.NoError(t, err)
	first := prepareCodexIdentitySnapshot(c, firstAccount, body)
	firstBody, _, err := applyCodexIdentitySnapshotToBodyRaw(body, first, true)
	require.NoError(t, err)
	firstDecoded := decodeCodexIdentityBodyForTest(t, firstBody)
	firstCM := firstDecoded["client_metadata"].(map[string]any)
	require.NotContains(t, firstCM, openAIWSTurnMetadataHeader)
	firstHeaders := clientHeaders.Clone()
	applyCodexIdentitySnapshotToHeaders(firstHeaders, first, true)
	require.Empty(t, firstHeaders.Get(openAIWSTurnMetadataHeader))
	require.Equal(t, firstCM["session_id"], firstHeaders.Get("session-id"))
	require.Equal(t, firstCM["thread_id"], firstHeaders.Get("x-client-request-id"))

	_, err = svc.prepareCodexAccountIdentitySource(context.Background(), c, secondAccount)
	require.NoError(t, err)
	require.Nil(t, stagedCodexIdentitySnapshot(c, firstAccount))
	second := prepareCodexIdentitySnapshot(c, secondAccount, body)
	require.Same(t, second, stagedCodexIdentitySnapshot(c, secondAccount))
	require.NotEqual(t, first.metadata["session_id"], second.metadata["session_id"])
	require.NotEqual(t, first.promptCacheKey, second.promptCacheKey)
}

func TestCodexIdentitySnapshotPreservesResumeAndWindowProgression(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 61, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "resume-account"}}
	build := func(windowNumber int, contextWindowID, previousWindowID string) *codexIdentitySnapshot {
		metadata := map[string]any{
			"session_id": "root", "thread_id": "root", "turn_id": fmt.Sprintf("turn-%d", windowNumber),
			"window_id": fmt.Sprintf("root:%d", windowNumber), "window_number": float64(windowNumber),
			"context_window_id": contextWindowID, "first_window_id": "context-0", "request_kind": "turn",
		}
		if previousWindowID != "" {
			metadata["previous_window_id"] = previousWindowID
		}
		embedded, err := json.Marshal(metadata)
		require.NoError(t, err)
		body, err := json.Marshal(map[string]any{
			"prompt_cache_key": "root",
			"client_metadata":  map[string]any{openAIWSTurnMetadataHeader: string(embedded)},
		})
		require.NoError(t, err)
		c := newCodexIdentitySnapshotTestContext(t, "/v1/responses", http.Header{})
		return prepareCodexIdentitySnapshot(c, account, body)
	}
	before := build(0, "context-0", "")
	after := build(1, "context-1", "context-0")
	require.Equal(t, before.metadata["session_id"], after.metadata["session_id"])
	require.Equal(t, before.metadata["thread_id"], after.metadata["thread_id"])
	require.Equal(t, before.promptCacheKey, after.promptCacheKey)
	require.Equal(t, before.metadata["thread_id"].(string)+":0", before.metadata["window_id"])
	require.Equal(t, after.metadata["thread_id"].(string)+":1", after.metadata["window_id"])
	require.Equal(t, before.metadata["context_window_id"], after.metadata["previous_window_id"])
	require.Equal(t, before.metadata["first_window_id"], after.metadata["first_window_id"])
	require.NotEqual(t, before.metadata["context_window_id"], after.metadata["context_window_id"])
}
