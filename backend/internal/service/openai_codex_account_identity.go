package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexAccountIdentityNamespaceVersion = "v1"

const codexAccountIdentitySourceContextKey = "openai_codex_account_identity_source"
const codexIdentitySnapshotContextKey = "openai_codex_identity_snapshot"

type codexIdentitySnapshot struct {
	accountID          int64
	metadata           map[string]any
	subagentHeader     string
	promptCacheKey     string
	rawPromptCacheKey  string
	rawSessionID       string
	hasPromptCache     bool
	hasTurnMetadata    bool
	omitTurnMetadata   bool
	hasClientMetadata  bool
	omitClientMetadata bool
	mode               codexFingerprintMode
}

// prepareCodexAccountIdentitySource resolves credential shadows once per selected
// attempt. The handler reuses gin.Context across failover attempts, so every entry
// point overwrites the staged source before projecting outbound identity.
func (s *OpenAIGatewayService) prepareCodexAccountIdentitySource(ctx context.Context, c *gin.Context, account *Account) (*Account, error) {
	// gin.Context is reused across scheduler attempts. Clear both projections
	// before resolving the next credential so an early-returning attempt cannot
	// expose the previous account's fingerprint snapshot or cache-key source.
	stageCodexFingerprintIDs(c, nil)
	if c != nil {
		c.Set(codexFingerprintPromptCacheSourceKey, codexFingerprintPromptCacheSource{})
		c.Set(codexIdentitySnapshotContextKey, (*codexIdentitySnapshot)(nil))
	}
	source := account
	if account != nil && account.IsShadow() {
		resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return nil, err
		}
		source = resolved
	}
	if c != nil {
		c.Set(codexAccountIdentitySourceContextKey, source)
	}
	return source, nil
}

func codexAccountIdentitySource(c *gin.Context, fallback *Account) *Account {
	if c != nil {
		if staged, ok := c.Get(codexAccountIdentitySourceContextKey); ok {
			if source, ok := staged.(*Account); ok && source != nil {
				return source
			}
		}
	}
	return fallback
}

// codexAccountIdentityNamespace returns a stable, credential-scoped namespace.
// Multiple local rows that use the same ChatGPT account intentionally share the
// same namespace. Setup tokens use an irreversible bearer fingerprint because
// they have no refresh lifecycle or imported account metadata. Refreshable OAuth
// otherwise falls back only to a persistent fingerprint seed: local row IDs are
// deployment-relative and must never become upstream identity.
func codexAccountIdentityNamespace(account *Account) string {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return ""
	}
	if upstreamAccountID := strings.TrimSpace(account.GetChatGPTAccountID()); upstreamAccountID != "" {
		if upstreamUserID := strings.TrimSpace(account.GetCredential("chatgpt_user_id")); upstreamUserID != "" {
			return "chatgpt:" + upstreamAccountID + ":user:" + upstreamUserID
		}
		return "chatgpt:" + upstreamAccountID
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		return "seed:" + seed
	}
	if account.Type == AccountTypeSetupToken {
		if token := strings.TrimSpace(account.GetOpenAIAccessToken()); token != "" {
			sum := sha256.Sum256([]byte("openai-setup-token:" + token))
			return fmt.Sprintf("setup-token:%x", sum[:16])
		}
	}
	return ""
}

// isolateOpenAIUpstreamSessionID preserves the existing API-key isolation while
// adding the selected OAuth credential namespace. A scheduler failover therefore
// cannot send the same session/conversation identity through two upstream accounts.
func isolateOpenAIUpstreamSessionID(apiKeyID int64, account *Account, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	namespace := codexAccountIdentityNamespace(account)
	if namespace == "" {
		return isolateOpenAISessionID(apiKeyID, raw)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("u%d:a%s:%s", apiKeyID, namespace, raw)))
	return fmt.Sprintf("%x", sum[:8])
}

func scopeCodexAccountIdentityValue(account *Account, apiKeyID int64, kind, raw string) string {
	raw = strings.TrimSpace(raw)
	namespace := codexAccountIdentityNamespace(account)
	if raw == "" || namespace == "" {
		return raw
	}
	return deriveStableUUIDv4(fmt.Sprintf(
		"sub2api:codex-account-identity:%s:user:%d:account:%s:kind:%s:value:%s",
		codexAccountIdentityNamespaceVersion,
		apiKeyID,
		namespace,
		kind,
		raw,
	))
}

// A session names its root thread. Thread identities and references therefore
// share the existing session domain, including across separate parent/child
// requests. Installation, turn, and window nodes retain their own domains.
var codexAccountIdentityFields = []struct {
	name string
	kind string
}{
	{name: "installation_id", kind: "installation"},
	{name: "x-codex-installation-id", kind: "installation"},
	{name: "session_id", kind: "session"},
	{name: "session-id", kind: "session"},
	{name: "thread_id", kind: "session"},
	{name: "thread-id", kind: "session"},
	{name: "x-client-request-id", kind: "session"},
	{name: "parent_thread_id", kind: "session"},
	{name: "x-codex-parent-thread-id", kind: "session"},
	{name: "forked_from_thread_id", kind: "session"},
	{name: "turn_id", kind: "turn"},
	{name: "turn-id", kind: "turn"},
	{name: "parent_turn_id", kind: "turn"},
	{name: "root_turn_id", kind: "turn"},
	{name: "window_id", kind: "window"},
	{name: "x-codex-window-id", kind: "window"},
	{name: "context_window_id", kind: "window"},
	{name: "first_window_id", kind: "window"},
	{name: "previous_window_id", kind: "window"},
}

var codexIdentityEmbeddedProjections = []struct {
	embedded string
	flat     []string
}{
	{embedded: "installation_id", flat: []string{"x-codex-installation-id", "installation_id"}},
	{embedded: "session_id", flat: []string{"session_id", "session-id"}},
	{embedded: "thread_id", flat: []string{"thread_id", "thread-id", "x-client-request-id"}},
	{embedded: "turn_id", flat: []string{"turn_id", "turn-id"}},
	{embedded: "window_id", flat: []string{"x-codex-window-id", "window_id"}},
	{embedded: "parent_thread_id", flat: []string{"x-codex-parent-thread-id", "parent_thread_id"}},
	{embedded: "parent_turn_id", flat: []string{"parent_turn_id"}},
	{embedded: "root_turn_id", flat: []string{"root_turn_id"}},
	{embedded: "forked_from_thread_id", flat: []string{"forked_from_thread_id"}},
	{embedded: "context_window_id", flat: []string{"context_window_id"}},
	{embedded: "first_window_id", flat: []string{"first_window_id"}},
	{embedded: "previous_window_id", flat: []string{"previous_window_id"}},
}

func codexAccountIdentityClientHeaders(c *gin.Context) http.Header {
	if c == nil || c.Request == nil {
		return nil
	}
	return c.Request.Header
}

func copyCodexIdentityHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for _, name := range [...]string{
		"x-codex-installation-id",
		"session-id",
		"thread-id",
		"x-client-request-id",
		"x-codex-window-id",
		"x-codex-parent-thread-id",
		"x-openai-subagent",
	} {
		if value := strings.TrimSpace(src.Get(name)); value != "" {
			dst.Set(name, value)
		}
	}
}

func firstCodexIdentityString(values map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := values[name].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func synchronizeCodexEmbeddedIdentity(metadata, flat map[string]any) {
	if metadata == nil || flat == nil {
		return
	}
	for _, projection := range codexIdentityEmbeddedProjections {
		if value := firstCodexIdentityString(flat, projection.flat...); value != "" {
			metadata[projection.embedded] = value
		}
	}
}

func codexCompositeWindowID(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	separator := strings.LastIndexByte(raw, ':')
	if separator <= 0 || separator == len(raw)-1 {
		return "", "", false
	}
	number := raw[separator+1:]
	for i := 0; i < len(number); i++ {
		if number[i] < '0' || number[i] > '9' {
			return "", "", false
		}
	}
	return raw[:separator], number, true
}

func scopeCodexAccountIdentityField(account *Account, apiKeyID int64, name, kind, raw string) string {
	if name == "window_id" || name == "x-codex-window-id" {
		if threadID, windowNumber, ok := codexCompositeWindowID(raw); ok {
			return scopeCodexAccountIdentityValue(account, apiKeyID, "session", threadID) + ":" + windowNumber
		}
	}
	return scopeCodexAccountIdentityValue(account, apiKeyID, kind, raw)
}

// Resolve references before rewriting any metadata. Matching raw values retain
// the session relationship regardless of which original carrier declares it.
func codexAccountIdentityPromptCacheKind(raw string, clientMetadata map[string]any, clientHeaders http.Header) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "prompt-cache"
	}
	for _, field := range [...]string{"session_id", "session-id"} {
		if sessionID, ok := clientMetadata[field].(string); ok && strings.TrimSpace(sessionID) == raw {
			return "session"
		}
		if strings.TrimSpace(clientHeaders.Get(field)) == raw {
			return "session"
		}
	}
	originalBodyMetadata, _ := clientMetadata[openAIWSTurnMetadataHeader].(string)
	for _, encoded := range [...]string{originalBodyMetadata, clientHeaders.Get(openAIWSTurnMetadataHeader)} {
		if strings.TrimSpace(encoded) == "" {
			continue
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(encoded), &metadata); err != nil {
			continue
		}
		for _, field := range [...]string{"session_id", "session-id"} {
			if sessionID, ok := metadata[field].(string); ok && strings.TrimSpace(sessionID) == raw {
				return "session"
			}
		}
	}
	return "prompt-cache"
}

func applyCodexAccountIdentityFields(values map[string]any, account *Account, apiKeyID int64) bool {
	if values == nil || codexAccountIdentityNamespace(account) == "" {
		return false
	}
	changed := false
	for _, field := range codexAccountIdentityFields {
		raw, ok := values[field.name].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		next := scopeCodexAccountIdentityField(account, apiKeyID, field.name, field.kind, raw)
		if next != raw {
			values[field.name] = next
			changed = true
		}
	}
	return changed
}

func applyCodexAccountIdentityEmbeddedMetadata(values map[string]any, account *Account, apiKeyID int64) bool {
	rawValue, exists := values[openAIWSTurnMetadataHeader]
	if !exists {
		return false
	}
	raw, ok := rawValue.(string)
	metadata := map[string]any{}
	if !ok || strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &metadata) != nil || metadata == nil {
		delete(values, openAIWSTurnMetadataHeader)
		return true
	}
	synchronizeCodexEmbeddedIdentity(metadata, values)
	applyCodexAccountIdentityFields(metadata, account, apiKeyID)
	rebuilt, err := json.Marshal(metadata)
	if err != nil {
		return false
	}
	next := string(rebuilt)
	if next == raw {
		return false
	}
	values[openAIWSTurnMetadataHeader] = next
	return true
}

func applyCodexAccountIdentityClientMetadataMap(requestBody map[string]any, account *Account, apiKeyID int64, clientHeaders http.Header) bool {
	if requestBody == nil || codexAccountIdentityNamespace(account) == "" {
		return false
	}
	changed := false
	clientMetadata, _ := requestBody["client_metadata"].(map[string]any)
	if _, exists := requestBody["client_metadata"]; exists && clientMetadata == nil {
		delete(requestBody, "client_metadata")
		changed = true
	}
	promptCacheKey, _ := requestBody["prompt_cache_key"].(string)
	promptCacheKind := codexAccountIdentityPromptCacheKind(promptCacheKey, clientMetadata, clientHeaders)
	if clientMetadata != nil {
		if applyCodexAccountIdentityEmbeddedMetadata(clientMetadata, account, apiKeyID) {
			changed = true
		}
		if applyCodexAccountIdentityFields(clientMetadata, account, apiKeyID) {
			changed = true
		}
	}
	if strings.TrimSpace(promptCacheKey) != "" {
		next := scopeCodexAccountIdentityValue(account, apiKeyID, promptCacheKind, promptCacheKey)
		if next != promptCacheKey {
			requestBody["prompt_cache_key"] = next
			changed = true
		}
	}
	return changed
}

// applyCodexAccountIdentityClientMetadataRaw scopes only the small identity
// subobjects with gjson/sjson. The passthrough hot path never unmarshals the
// potentially multi-megabyte request body.
func applyCodexAccountIdentityClientMetadataRaw(body []byte, account *Account, apiKeyID int64, clientHeaders http.Header) ([]byte, bool, error) {
	if len(body) == 0 || codexAccountIdentityNamespace(account) == "" {
		return body, false, nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, false, nil
	}

	next := body
	changed := false
	var clientMetadata map[string]any
	cm := gjson.GetBytes(body, "client_metadata")
	if cm.IsObject() {
		if err := json.Unmarshal([]byte(cm.Raw), &clientMetadata); err != nil {
			return body, false, fmt.Errorf("decode client_metadata for account identity: %w", err)
		}
	} else if cm.Exists() {
		var err error
		next, err = sjson.DeleteBytes(next, "client_metadata")
		if err != nil {
			return body, false, fmt.Errorf("remove malformed client_metadata: %w", err)
		}
		changed = true
	}
	promptCacheKey := gjson.GetBytes(body, "prompt_cache_key")
	promptCacheKind := codexAccountIdentityPromptCacheKind(promptCacheKey.String(), clientMetadata, clientHeaders)
	if clientMetadata != nil {
		metadataChanged := applyCodexAccountIdentityEmbeddedMetadata(clientMetadata, account, apiKeyID)
		metadataChanged = applyCodexAccountIdentityFields(clientMetadata, account, apiKeyID) || metadataChanged
		if metadataChanged {
			raw, err := json.Marshal(clientMetadata)
			if err != nil {
				return body, false, fmt.Errorf("encode account-scoped client_metadata: %w", err)
			}
			var setErr error
			next, setErr = sjson.SetRawBytes(next, "client_metadata", raw)
			if setErr != nil {
				return body, false, fmt.Errorf("splice account-scoped client_metadata: %w", setErr)
			}
			changed = true
		}
	}
	if promptCacheKey.Type == gjson.String && strings.TrimSpace(promptCacheKey.String()) != "" {
		raw := promptCacheKey.String()
		scoped := scopeCodexAccountIdentityValue(account, apiKeyID, promptCacheKind, raw)
		if scoped != raw {
			rewritten, err := sjson.SetBytes(next, "prompt_cache_key", scoped)
			if err != nil {
				return body, false, fmt.Errorf("splice account-scoped prompt_cache_key: %w", err)
			}
			next = rewritten
			changed = true
		}
	}
	return next, changed, nil
}

func applyCodexAccountIdentityHeaders(headers http.Header, account *Account, apiKeyID int64) {
	if headers == nil || codexAccountIdentityNamespace(account) == "" {
		return
	}
	metadataRaw := strings.TrimSpace(headers.Get(openAIWSTurnMetadataHeader))
	if metadataRaw != "" {
		metadata := map[string]any{}
		if err := json.Unmarshal([]byte(metadataRaw), &metadata); err != nil || metadata == nil {
			headers.Del(openAIWSTurnMetadataHeader)
		} else {
			flat := make(map[string]any, len(codexAccountIdentityFields))
			for _, field := range codexAccountIdentityFields {
				if raw := strings.TrimSpace(headers.Get(field.name)); raw != "" {
					flat[field.name] = raw
				}
			}
			synchronizeCodexEmbeddedIdentity(metadata, flat)
			applyCodexAccountIdentityFields(metadata, account, apiKeyID)
			if rebuilt, err := json.Marshal(metadata); err == nil {
				headers.Set(openAIWSTurnMetadataHeader, string(rebuilt))
			}
		}
	}
	for _, field := range codexAccountIdentityFields {
		// Underscore session/conversation headers are rebuilt separately from the
		// prompt cache key by each request builder.
		if field.name == "session_id" {
			continue
		}
		raw := strings.TrimSpace(headers.Get(field.name))
		if raw != "" {
			headers.Set(field.name, scopeCodexAccountIdentityField(account, apiKeyID, field.name, field.kind, raw))
		}
	}
	if threadID := strings.TrimSpace(headers.Get("thread-id")); threadID != "" {
		headers.Set("x-client-request-id", threadID)
	}
}

func decodeCodexTurnMetadata(raw any) map[string]any {
	encoded, ok := raw.(string)
	if !ok || strings.TrimSpace(encoded) == "" {
		return nil
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(encoded), &metadata) != nil || metadata == nil {
		return nil
	}
	return metadata
}

func cloneCodexMetadata(values map[string]any) map[string]any {
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func mergeCodexIdentitySource(dst, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	for _, projection := range codexIdentityEmbeddedProjections {
		if firstCodexIdentityString(dst, projection.embedded) != "" {
			continue
		}
		if value := firstCodexIdentityString(src, append([]string{projection.embedded}, projection.flat...)...); value != "" {
			dst[projection.embedded] = value
		}
	}
}

func codexIdentityHeaderSource(headers http.Header) map[string]any {
	if headers == nil {
		return nil
	}
	values := make(map[string]any, len(codexAccountIdentityFields))
	for _, field := range codexAccountIdentityFields {
		if value := strings.TrimSpace(headers.Get(field.name)); value != "" {
			values[field.name] = value
		}
	}
	return values
}

// prepareCodexIdentitySnapshot resolves one request's identity graph before any
// carrier is rewritten. The canonical embedded body metadata wins, followed by
// flat body compatibility fields, embedded headers, and flat headers.
func prepareCodexIdentitySnapshot(c *gin.Context, account *Account, body []byte) *codexIdentitySnapshot {
	if c != nil {
		c.Set(codexIdentitySnapshotContextKey, (*codexIdentitySnapshot)(nil))
	}
	if account == nil || !account.UsesOpenAICodexProtocol() {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode != codexFingerprintOff && mode != codexFingerprintDevice {
		return nil
	}

	headers := codexAccountIdentityClientHeaders(c)
	var flatBody map[string]any
	clientMetadata := gjson.GetBytes(body, "client_metadata")
	hasClientMetadata := clientMetadata.IsObject()
	omitClientMetadata := clientMetadata.Exists() && !hasClientMetadata
	if hasClientMetadata {
		_ = json.Unmarshal([]byte(clientMetadata.Raw), &flatBody)
	}
	subagentHeader := firstCodexIdentityString(flatBody, "x-openai-subagent")
	if subagentHeader == "" && headers != nil {
		subagentHeader = strings.TrimSpace(headers.Get("x-openai-subagent"))
	}
	bodyTurnValue, bodyHasTurnMetadata := flatBody[openAIWSTurnMetadataHeader]
	bodyTurnMetadata := decodeCodexTurnMetadata(bodyTurnValue)
	headerTurnRaw := ""
	if headers != nil {
		headerTurnRaw = strings.TrimSpace(headers.Get(openAIWSTurnMetadataHeader))
	}
	headerTurnMetadata := decodeCodexTurnMetadata(headerTurnRaw)

	metadata := map[string]any{}
	if bodyTurnMetadata != nil {
		metadata = cloneCodexMetadata(bodyTurnMetadata)
	} else if headerTurnMetadata != nil {
		metadata = cloneCodexMetadata(headerTurnMetadata)
	}
	for _, field := range codexAccountIdentityFields {
		delete(metadata, field.name)
	}
	mergeCodexIdentitySource(metadata, bodyTurnMetadata)
	mergeCodexIdentitySource(metadata, flatBody)
	mergeCodexIdentitySource(metadata, headerTurnMetadata)
	mergeCodexIdentitySource(metadata, codexIdentityHeaderSource(headers))

	rawSessionID := firstCodexIdentityString(metadata, "session_id")
	source := codexAccountIdentitySource(c, account)
	apiKeyID := getAPIKeyIDFromContext(c)
	applyCodexAccountIdentityFields(metadata, source, apiKeyID)

	promptCache := gjson.GetBytes(body, "prompt_cache_key")
	snapshot := &codexIdentitySnapshot{
		accountID:          account.ID,
		metadata:           metadata,
		subagentHeader:     subagentHeader,
		hasTurnMetadata:    bodyTurnMetadata != nil || headerTurnMetadata != nil,
		omitTurnMetadata:   (bodyHasTurnMetadata && bodyTurnMetadata == nil) || (headerTurnRaw != "" && headerTurnMetadata == nil),
		hasClientMetadata:  hasClientMetadata,
		omitClientMetadata: omitClientMetadata,
		mode:               mode,
		rawSessionID:       rawSessionID,
	}
	if promptCache.Type == gjson.String && strings.TrimSpace(promptCache.String()) != "" {
		snapshot.hasPromptCache = true
		snapshot.rawPromptCacheKey = strings.TrimSpace(promptCache.String())
		kind := "prompt-cache"
		if strings.TrimSpace(promptCache.String()) == rawSessionID {
			kind = "session"
		}
		snapshot.promptCacheKey = scopeCodexAccountIdentityValue(source, apiKeyID, kind, promptCache.String())
	}
	if mode == codexFingerprintDevice {
		if seed, ok := codexFingerprintSeed(account.Extra); ok {
			if installationID := resolveConvergedInstallationID(account, seed); installationID != "" {
				snapshot.metadata["installation_id"] = installationID
			}
		}
	}
	if c != nil {
		c.Set(codexIdentitySnapshotContextKey, snapshot)
	}
	return snapshot
}

func completeCodexIdentitySnapshotPromptCache(c *gin.Context, account *Account, snapshot *codexIdentitySnapshot, raw string) {
	if snapshot == nil || snapshot.hasPromptCache || strings.TrimSpace(raw) == "" {
		return
	}
	snapshot.hasPromptCache = true
	snapshot.rawPromptCacheKey = strings.TrimSpace(raw)
	kind := "prompt-cache"
	if snapshot.rawPromptCacheKey == snapshot.rawSessionID {
		kind = "session"
	}
	snapshot.promptCacheKey = scopeCodexAccountIdentityValue(
		codexAccountIdentitySource(c, account),
		getAPIKeyIDFromContext(c),
		kind,
		snapshot.rawPromptCacheKey,
	)
}

func stagedCodexIdentitySnapshot(c *gin.Context, account *Account) *codexIdentitySnapshot {
	if c == nil || account == nil {
		return nil
	}
	value, ok := c.Get(codexIdentitySnapshotContextKey)
	if !ok {
		return nil
	}
	snapshot, ok := value.(*codexIdentitySnapshot)
	if !ok || snapshot == nil || snapshot.accountID != account.ID {
		return nil
	}
	return snapshot
}

func setCodexProjectionString(values map[string]any, key, value string) bool {
	if value == "" || values[key] == value {
		return false
	}
	values[key] = value
	return true
}

func applyCodexIdentitySnapshotToClientMetadata(values map[string]any, snapshot *codexIdentitySnapshot) bool {
	if values == nil || snapshot == nil {
		return false
	}
	changed := false
	project := func(identity string, canonical string, aliases ...string) {
		value := firstCodexIdentityString(snapshot.metadata, identity)
		if value == "" {
			return
		}
		changed = setCodexProjectionString(values, canonical, value) || changed
		for _, alias := range aliases {
			if _, exists := values[alias]; exists {
				changed = setCodexProjectionString(values, alias, value) || changed
			}
		}
	}
	project("installation_id", "x-codex-installation-id", "installation_id")
	project("session_id", "session_id", "session-id")
	project("thread_id", "thread_id", "thread-id", "x-client-request-id")
	project("turn_id", "turn_id", "turn-id")
	project("window_id", "x-codex-window-id", "window_id")
	project("parent_thread_id", "x-codex-parent-thread-id", "parent_thread_id")
	project("parent_turn_id", "parent_turn_id")
	project("root_turn_id", "root_turn_id")
	if snapshot.subagentHeader != "" {
		changed = setCodexProjectionString(values, "x-openai-subagent", snapshot.subagentHeader) || changed
	}
	if snapshot.omitTurnMetadata {
		if _, exists := values[openAIWSTurnMetadataHeader]; exists {
			delete(values, openAIWSTurnMetadataHeader)
			changed = true
		}
	} else if snapshot.hasTurnMetadata {
		if len(snapshot.metadata) == 0 {
			if _, exists := values[openAIWSTurnMetadataHeader]; exists {
				delete(values, openAIWSTurnMetadataHeader)
				changed = true
			}
		} else if rebuilt, err := json.Marshal(snapshot.metadata); err == nil {
			changed = setCodexProjectionString(values, openAIWSTurnMetadataHeader, string(rebuilt)) || changed
		}
	}
	return changed
}

func applyCodexIdentitySnapshotToBodyMap(requestBody map[string]any, snapshot *codexIdentitySnapshot, includeClientMetadata bool) bool {
	if requestBody == nil || snapshot == nil {
		return false
	}
	changed := false
	if snapshot.hasPromptCache && requestBody["prompt_cache_key"] != snapshot.promptCacheKey {
		requestBody["prompt_cache_key"] = snapshot.promptCacheKey
		changed = true
	}
	if !includeClientMetadata {
		if _, exists := requestBody["client_metadata"]; exists {
			delete(requestBody, "client_metadata")
			changed = true
		}
		return changed
	}
	if snapshot.omitClientMetadata {
		if _, exists := requestBody["client_metadata"]; exists {
			delete(requestBody, "client_metadata")
			changed = true
		}
		return changed
	}
	if !snapshot.hasClientMetadata && snapshot.mode != codexFingerprintDevice {
		return changed
	}
	clientMetadata, ok := requestBody["client_metadata"].(map[string]any)
	if !ok {
		clientMetadata = map[string]any{}
	}
	if !snapshot.hasClientMetadata && snapshot.mode == codexFingerprintDevice {
		installationID := firstCodexIdentityString(snapshot.metadata, "installation_id")
		if installationID != "" && setCodexProjectionString(clientMetadata, "x-codex-installation-id", installationID) {
			requestBody["client_metadata"] = clientMetadata
			changed = true
		}
		return changed
	}
	if applyCodexIdentitySnapshotToClientMetadata(clientMetadata, snapshot) {
		requestBody["client_metadata"] = clientMetadata
		changed = true
	}
	return changed
}

func applyCodexIdentitySnapshotToBodyRaw(body []byte, snapshot *codexIdentitySnapshot, includeClientMetadata bool) ([]byte, bool, error) {
	if len(body) == 0 || snapshot == nil || !gjson.ParseBytes(body).IsObject() {
		return body, false, nil
	}
	next := body
	changed := false
	if snapshot.hasPromptCache {
		var err error
		next, err = sjson.SetBytes(next, "prompt_cache_key", snapshot.promptCacheKey)
		if err != nil {
			return body, false, fmt.Errorf("project canonical prompt_cache_key: %w", err)
		}
		changed = true
	}
	if !includeClientMetadata {
		if gjson.GetBytes(next, "client_metadata").Exists() {
			var err error
			next, err = sjson.DeleteBytes(next, "client_metadata")
			if err != nil {
				return body, false, fmt.Errorf("omit compact client_metadata: %w", err)
			}
			changed = true
		}
		return next, changed, nil
	}
	if snapshot.omitClientMetadata {
		if gjson.GetBytes(next, "client_metadata").Exists() {
			var err error
			next, err = sjson.DeleteBytes(next, "client_metadata")
			if err != nil {
				return body, false, fmt.Errorf("omit malformed client_metadata: %w", err)
			}
			changed = true
		}
		return next, changed, nil
	}
	if !snapshot.hasClientMetadata && snapshot.mode != codexFingerprintDevice {
		return next, changed, nil
	}
	clientMetadata := map[string]any{}
	if current := gjson.GetBytes(next, "client_metadata"); current.IsObject() {
		if err := json.Unmarshal([]byte(current.Raw), &clientMetadata); err != nil {
			return body, false, fmt.Errorf("decode client_metadata projection: %w", err)
		}
	}
	if !snapshot.hasClientMetadata && snapshot.mode == codexFingerprintDevice {
		installationID := firstCodexIdentityString(snapshot.metadata, "installation_id")
		if installationID == "" || !setCodexProjectionString(clientMetadata, "x-codex-installation-id", installationID) {
			return next, changed, nil
		}
	} else if !applyCodexIdentitySnapshotToClientMetadata(clientMetadata, snapshot) {
		return next, changed, nil
	}
	rebuilt, err := json.Marshal(clientMetadata)
	if err != nil {
		return body, false, fmt.Errorf("encode client_metadata projection: %w", err)
	}
	next, err = sjson.SetRawBytes(next, "client_metadata", rebuilt)
	if err != nil {
		return body, false, fmt.Errorf("project canonical client_metadata: %w", err)
	}
	return next, true, nil
}

func applyCodexIdentitySnapshotToHeaders(headers http.Header, snapshot *codexIdentitySnapshot, includeClientRequestID bool) {
	if headers == nil || snapshot == nil {
		return
	}
	for _, name := range [...]string{
		"x-codex-installation-id", "session-id", "thread-id", "x-client-request-id",
		"x-codex-window-id", "x-codex-parent-thread-id", "x-openai-subagent",
	} {
		headers.Del(name)
	}
	set := func(header, identity string) {
		if value := firstCodexIdentityString(snapshot.metadata, identity); value != "" {
			headers.Set(header, value)
		}
	}
	set("x-codex-installation-id", "installation_id")
	set("session-id", "session_id")
	if threadID := firstCodexIdentityString(snapshot.metadata, "thread_id"); threadID != "" {
		headers.Set("thread-id", threadID)
		if includeClientRequestID {
			headers.Set("x-client-request-id", threadID)
		}
	}
	set("x-codex-window-id", "window_id")
	set("x-codex-parent-thread-id", "parent_thread_id")
	if snapshot.subagentHeader != "" {
		headers.Set("x-openai-subagent", snapshot.subagentHeader)
	}
	if snapshot.omitTurnMetadata {
		headers.Del(openAIWSTurnMetadataHeader)
	} else if snapshot.hasTurnMetadata {
		metadata := cloneCodexMetadata(snapshot.metadata)
		delete(metadata, "tool_namespaces_info")
		if len(metadata) == 0 {
			headers.Del(openAIWSTurnMetadataHeader)
		} else if rebuilt, err := json.Marshal(metadata); err == nil {
			headers.Set(openAIWSTurnMetadataHeader, string(rebuilt))
		}
	}
}
