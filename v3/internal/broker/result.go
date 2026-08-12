package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	toolResultSchema            = "agents-gateway.tool-result.v1"
	toolResultMediaType         = "application/json"
	maxDiagnosticInputBytes     = 64 << 10
	maxDiagnosticsPerResult     = 32
	maxDiagnosticMessageBytes   = 512
	maxDiagnosticPathBytes      = 512
	maxDiagnosticCoordinate     = 10_000_000
	maxStructuredResultBytes    = 64 << 10
	maxResultReferencePathBytes = 1024
)

// ToolResultReference identifies the bounded, immutable raw result retained by
// the broker. Path is a logical key below the authorized result store, never a
// host filesystem path. URI is a stable broker reference and contains no
// backend credentials or query material.
type ToolResultReference struct {
	URI       string `json:"uri"`
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	MediaType string `json:"mediaType"`
}

// ToolDiagnostic is the bounded, machine-readable projection of a compiler or
// test failure. Raw output is deliberately absent; callers use Reference on
// ToolResultSummary when deeper investigation is needed.
type ToolDiagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
}

// ToolResultSummary is the only result shape emitted for an error result or a
// successful result too large for the inline budget.
type ToolResultSummary struct {
	Schema        string              `json:"schema"`
	Status        string              `json:"status"`
	OriginalBytes int64               `json:"originalBytes"`
	Diagnostics   []ToolDiagnostic    `json:"diagnostics,omitempty"`
	Reference     ToolResultReference `json:"reference"`
}

type shapedToolResult struct {
	content   json.RawMessage
	reference *ToolResultReference
	digest    string
}

var (
	colonDiagnosticPattern   = regexp.MustCompile(`^\s*(.+?):([0-9]{1,9})(?::([0-9]{1,9}))?:\s*(.+?)\s*$`)
	parenDiagnosticPattern   = regexp.MustCompile(`^\s*(.+?)\(([0-9]{1,9}),([0-9]{1,9})\):\s*(.+?)\s*$`)
	arrowDiagnosticPattern   = regexp.MustCompile(`^\s*-->\s+(.+?):([0-9]{1,9})(?::([0-9]{1,9}))?\s*$`)
	bearerCredentialPattern  = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{4,}`)
	genericCredentialPattern = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?key|token|secret|password|authorization|cookie)\s*[:=]\s*)[A-Za-z0-9._~+/=-]{4,}`)
)

func (b *Broker) shapeToolResult(ctx context.Context, raw []byte, isError bool, credential []byte) (shapedToolResult, error) {
	var zero shapedToolResult
	if b == nil || ctx == nil || len(raw) == 0 || strictjson.ValidateObject(raw) != nil {
		return zero, ErrToolResultInvalid
	}
	sanitized, err := redactJSON(raw, credential)
	if err != nil || strictjson.ValidateObject(sanitized) != nil {
		return zero, ErrToolResultInvalid
	}
	if !isError && int64(len(sanitized)) <= b.limits.maxToolResult {
		return shapedToolResult{content: sanitized, digest: digestBytes(sanitized)}, nil
	}
	reference, err := b.persistToolResult(ctx, sanitized)
	if err != nil {
		return zero, err
	}
	status := "truncated"
	var diagnostics []ToolDiagnostic
	if isError {
		status = "error"
		diagnostics = parseToolDiagnostics(extractDiagnosticText(sanitized), b.workspaceRoot, credential, maxDiagnosticsPerResult)
	}
	payload, err := encodeToolResultSummary(ToolResultSummary{
		Schema:        toolResultSchema,
		Status:        status,
		OriginalBytes: int64(len(raw)),
		Diagnostics:   diagnostics,
		Reference:     reference,
	}, isError, b.limits.maxMCPResponse)
	if err != nil {
		return zero, err
	}
	return shapedToolResult{content: payload, reference: &reference, digest: reference.Digest}, nil
}

func (b *Broker) persistToolResult(ctx context.Context, body []byte) (ToolResultReference, error) {
	var zero ToolResultReference
	if b == nil || ctx == nil || len(body) == 0 || int64(len(body)) > b.limits.maxArtifact || !utf8.Valid(body) || strictjson.ValidateObject(body) != nil {
		return zero, ErrToolResultUnavailable
	}
	store := b.resultStore
	if store == nil {
		return zero, ErrToolResultUnavailable
	}
	digest := digestBytes(body)
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	key := "runs/" + b.runUID + "/broker-results/" + hexDigest + ".json"
	if !validResultKey(key) || len(key) > maxResultReferencePathBytes {
		return zero, ErrToolResultUnavailable
	}
	created, backendURI, err := store.Put(ctx, key, append([]byte(nil), body...), toolResultMediaType)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		return zero, ErrToolResultUnavailable
	}
	if !created {
		existing, getErr := store.Get(ctx, key)
		if getErr != nil || !bytes.Equal(existing, body) {
			if getErr != nil && ctx.Err() != nil {
				return zero, ctx.Err()
			}
			return zero, ErrToolResultUnavailable
		}
	}
	// The backend URI is validated even though it is not returned. This keeps
	// an accidentally credential-bearing or query-bearing store implementation
	// from becoming an authority boundary. The agent receives only the logical
	// URI below.
	if !safeArtifactURI(backendURI) {
		return zero, ErrToolResultUnavailable
	}
	logicalURI := "artifact://agw/" + hexDigest
	reference := ToolResultReference{
		URI: logicalURI, Path: key, Digest: digest, SizeBytes: int64(len(body)), MediaType: toolResultMediaType,
	}
	if !safeArtifactURI(reference.URI) || !validResultKey(reference.Path) || !canonicalResultDigest(reference.Digest) {
		return zero, ErrToolResultUnavailable
	}
	return reference, nil
}

func canonicalResultDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func encodeToolResultSummary(summary ToolResultSummary, isError bool, maxBytes int) ([]byte, error) {
	if summary.Schema != toolResultSchema || (summary.Status != "error" && summary.Status != "truncated") || summary.OriginalBytes < 1 || !canonicalResultDigest(summary.Reference.Digest) || summary.Reference.SizeBytes < 1 || !safeArtifactURI(summary.Reference.URI) || !validResultKey(summary.Reference.Path) {
		return nil, ErrToolResultInvalid
	}
	if isError && summary.Status != "error" {
		return nil, ErrToolResultInvalid
	}
	if !isError && summary.Status != "truncated" {
		return nil, ErrToolResultInvalid
	}
	if len(summary.Diagnostics) > maxDiagnosticsPerResult {
		return nil, ErrToolResultInvalid
	}
	for index := range summary.Diagnostics {
		if !validToolDiagnostic(summary.Diagnostics[index]) {
			return nil, ErrToolResultInvalid
		}
	}
	for count := len(summary.Diagnostics); count >= 0; count-- {
		candidate := summary
		candidate.Diagnostics = append([]ToolDiagnostic(nil), summary.Diagnostics[:count]...)
		result := map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": map[bool]string{true: "tool execution failed; diagnostics are retained by immutable reference", false: "tool result exceeded the inline budget; the complete result is retained by immutable reference"}[isError],
			}},
			"structuredContent": candidate,
			"isError":           isError,
		}
		body, err := json.Marshal(result)
		if err == nil && len(body) <= maxBytes && len(body) <= maxStructuredResultBytes && strictjson.ValidateObject(body) == nil {
			return body, nil
		}
	}
	return nil, ErrToolResultInvalid
}

func validToolDiagnostic(diagnostic ToolDiagnostic) bool {
	_, pathOK := safeDiagnosticPath(diagnostic.File, "")
	return pathOK && diagnostic.Line >= 1 && diagnostic.Line <= maxDiagnosticCoordinate && diagnostic.Column >= 0 && diagnostic.Column <= maxDiagnosticCoordinate && validDiagnosticMessage(diagnostic.Message, nil)
}

func extractDiagnosticText(raw []byte) []string {
	var fields map[string]json.RawMessage
	if decodeStrictObject(raw, &fields) != nil {
		return nil
	}
	texts := make([]string, 0, 4)
	if content, ok := fields["content"]; ok {
		var entries []json.RawMessage
		if json.Unmarshal(content, &entries) == nil && len(entries) <= 256 {
			for _, entry := range entries {
				var item map[string]json.RawMessage
				if decodeStrictObject(entry, &item) != nil {
					continue
				}
				var kind, text string
				if decodeField(item, "type", &kind) == nil && kind == "text" && decodeField(item, "text", &text) == nil {
					texts = appendBoundedDiagnosticText(texts, text)
				}
			}
		}
	}
	if structured, ok := fields["structuredContent"]; ok {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(structured))
		decoder.UseNumber()
		if decoder.Decode(&value) == nil {
			collectDiagnosticStrings(value, &texts)
		}
	}
	return texts
}

func appendBoundedDiagnosticText(texts []string, value string) []string {
	if len(texts) >= 32 || value == "" {
		return texts
	}
	if len(value) > maxDiagnosticInputBytes {
		value = truncateUTF8(value, maxDiagnosticInputBytes)
	}
	used := 0
	for _, text := range texts {
		used += len(text)
	}
	if used >= maxDiagnosticInputBytes {
		return texts
	}
	remaining := maxDiagnosticInputBytes - used
	if len(value) > remaining {
		value = truncateUTF8(value, remaining)
	}
	return append(texts, value)
}

func collectDiagnosticStrings(value any, texts *[]string) {
	if texts == nil || len(*texts) >= 32 {
		return
	}
	switch current := value.(type) {
	case string:
		*texts = appendBoundedDiagnosticText(*texts, current)
	case []any:
		for _, item := range current {
			collectDiagnosticStrings(item, texts)
		}
	case map[string]any:
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectDiagnosticStrings(current[key], texts)
		}
	}
}

func parseToolDiagnostics(texts []string, workspaceRoot string, credential []byte, maxCount int) []ToolDiagnostic {
	if maxCount < 1 {
		return nil
	}
	result := make([]ToolDiagnostic, 0, minInt(maxCount, maxDiagnosticsPerResult))
	seen := make(map[string]struct{})
	for _, text := range texts {
		if len(text) > maxDiagnosticInputBytes {
			text = truncateUTF8(text, maxDiagnosticInputBytes)
		}
		for _, line := range strings.Split(text, "\n") {
			if len(result) >= maxCount || len(result) >= maxDiagnosticsPerResult {
				sortToolDiagnostics(result)
				return result
			}
			diagnostic, ok := parseDiagnosticLine(line, workspaceRoot, credential)
			if !ok {
				continue
			}
			key := diagnostic.File + "\x00" + strconv.Itoa(diagnostic.Line) + "\x00" + strconv.Itoa(diagnostic.Column) + "\x00" + diagnostic.Message
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, diagnostic)
		}
	}
	sortToolDiagnostics(result)
	return result
}

func sortToolDiagnostics(result []ToolDiagnostic) {
	sort.Slice(result, func(i, j int) bool {
		if result[i].File != result[j].File {
			return result[i].File < result[j].File
		}
		if result[i].Line != result[j].Line {
			return result[i].Line < result[j].Line
		}
		if result[i].Column != result[j].Column {
			return result[i].Column < result[j].Column
		}
		return result[i].Message < result[j].Message
	})
}

func parseDiagnosticLine(line, workspaceRoot string, credential []byte) (ToolDiagnostic, bool) {
	line = stripDiagnosticANSI(strings.TrimSpace(line))
	if line == "" || len(line) > maxDiagnosticInputBytes {
		return ToolDiagnostic{}, false
	}
	if match := arrowDiagnosticPattern.FindStringSubmatch(line); len(match) != 0 {
		return makeDiagnostic(match[1], match[2], match[3], "compiler location", workspaceRoot, credential)
	}
	if match := parenDiagnosticPattern.FindStringSubmatch(line); len(match) != 0 {
		return makeDiagnostic(match[1], match[2], match[3], match[4], workspaceRoot, credential)
	}
	if match := colonDiagnosticPattern.FindStringSubmatch(line); len(match) != 0 {
		return makeDiagnostic(match[1], match[2], match[3], match[4], workspaceRoot, credential)
	}
	return ToolDiagnostic{}, false
}

func makeDiagnostic(file, line, column, message, workspaceRoot string, credential []byte) (ToolDiagnostic, bool) {
	lineNumber, err := strconv.Atoi(line)
	if err != nil || lineNumber < 1 || lineNumber > maxDiagnosticCoordinate {
		return ToolDiagnostic{}, false
	}
	columnNumber := 0
	if column != "" {
		columnNumber, err = strconv.Atoi(column)
		if err != nil || columnNumber < 1 || columnNumber > maxDiagnosticCoordinate {
			return ToolDiagnostic{}, false
		}
	}
	file, ok := safeDiagnosticPath(file, workspaceRoot)
	if !ok {
		return ToolDiagnostic{}, false
	}
	message = redactDiagnosticText(message, credential)
	if !validDiagnosticMessage(message, credential) {
		return ToolDiagnostic{}, false
	}
	return ToolDiagnostic{File: file, Line: lineNumber, Column: columnNumber, Message: message}, true
}

func safeDiagnosticPath(value, workspaceRoot string) (string, bool) {
	value = stripDiagnosticANSI(strings.TrimSpace(value))
	if value == "" || len(value) > maxDiagnosticPathBytes || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", false
	}
	if filepath.IsAbs(value) {
		if workspaceRoot == "" || !filepath.IsAbs(workspaceRoot) || filepath.Clean(workspaceRoot) != workspaceRoot {
			return "", false
		}
		for _, part := range strings.Split(filepath.ToSlash(value), "/") {
			if part == ".." {
				return "", false
			}
		}
		relative, err := filepath.Rel(workspaceRoot, filepath.Clean(value))
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", false
		}
		value = filepath.ToSlash(relative)
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	if filepath.IsAbs(value) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))) != value {
		return "", false
	}
	return value, true
}

func validDiagnosticMessage(value string, credential []byte) bool {
	value = redactDiagnosticText(value, credential)
	return value != "" && len(value) <= maxDiagnosticMessageBytes && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func redactDiagnosticText(value string, credential []byte) string {
	value = stripDiagnosticANSI(strings.TrimSpace(value))
	if len(credential) > 0 {
		secret := string(credential)
		value = strings.ReplaceAll(value, "Bearer "+secret, "Bearer [REDACTED]")
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	value = redactSensitiveString(value)
	var builder strings.Builder
	for _, char := range value {
		if unicode.IsControl(char) && char != '\t' {
			builder.WriteByte(' ')
			continue
		}
		if builder.Len()+utf8.RuneLen(char) > maxDiagnosticMessageBytes {
			break
		}
		builder.WriteRune(char)
	}
	return strings.TrimSpace(truncateUTF8(builder.String(), maxDiagnosticMessageBytes))
}

func stripDiagnosticANSI(value string) string {
	for {
		start := strings.IndexByte(value, 0x1b)
		if start < 0 || start+1 >= len(value) {
			return value
		}
		end := start + 2
		if value[start+1] == '[' {
			for end < len(value) && ((value[end] >= '0' && value[end] <= '9') || value[end] == ';' || value[end] == '?') {
				end++
			}
			if end < len(value) {
				end++
			}
		}
		value = value[:start] + value[end:]
	}
}

func redactJSON(raw, credential []byte) ([]byte, error) {
	if len(raw) == 0 || strictjson.Validate(raw) != nil {
		return nil, ErrToolResultInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrToolResultInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, ErrToolResultInvalid
	}
	redactJSONValue(&value, string(credential))
	encoded, err := json.Marshal(value)
	if err != nil || strictjson.Validate(encoded) != nil {
		return nil, ErrToolResultInvalid
	}
	return encoded, nil
}

func redactJSONValue(value *any, secret string) {
	if value == nil {
		return
	}
	switch current := (*value).(type) {
	case string:
		if secret != "" {
			current = strings.ReplaceAll(current, "Bearer "+secret, "Bearer [REDACTED]")
			current = strings.ReplaceAll(current, secret, "[REDACTED]")
		}
		*value = redactSensitiveString(current)
	case []any:
		for index := range current {
			redactJSONValue(&current[index], secret)
		}
	case map[string]any:
		for key, item := range current {
			newKey := key
			if secret != "" {
				newKey = strings.ReplaceAll(strings.ReplaceAll(newKey, "Bearer "+secret, "Bearer [REDACTED]"), secret, "[REDACTED]")
			}
			newKey = redactSensitiveString(newKey)
			if sensitiveResultField(key) {
				item = "[REDACTED]"
			} else {
				redactJSONValue(&item, secret)
			}
			if newKey != key {
				delete(current, key)
			}
			current[newKey] = item
		}
	}
}

func redactSensitiveString(value string) string {
	value = bearerCredentialPattern.ReplaceAllString(value, "Bearer [REDACTED]")
	return genericCredentialPattern.ReplaceAllString(value, "$1[REDACTED]")
}

func sensitiveResultField(key string) bool {
	normalized := strings.ToLower(strings.Map(func(char rune) rune {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			return char
		}
		return -1
	}, key))
	return strings.Contains(normalized, "password") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "token") || strings.Contains(normalized, "apikey") || strings.Contains(normalized, "accesskey") || normalized == "authorization" || normalized == "cookie"
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func validWorkspaceRoot(root string) bool {
	return root != "" && len(root) <= 4096 && utf8.ValidString(root) && filepath.IsAbs(root) && filepath.Clean(root) == root && root != "/" && !strings.HasSuffix(root, string(filepath.Separator)) && !strings.ContainsAny(root, "\x00\r\n")
}
