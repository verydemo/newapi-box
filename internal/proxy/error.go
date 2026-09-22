package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/verydemo/newapi-box/relaykit/types"
)

// writeError renders an error in the shape the calling protocol expects.
// Clients routinely branch on these envelopes, so a converter that always
// answered with OpenAI's shape would break Claude and Gemini SDKs.
func writeError(w http.ResponseWriter, format types.RelayFormat, status int, code string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	var payload any
	switch format {
	case types.RelayFormatClaude:
		payload = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    claudeErrorType(status),
				"message": message,
			},
		}
	case types.RelayFormatGemini:
		payload = map[string]any{
			"error": map[string]any{
				"code":    status,
				"message": message,
				"status":  strings.ToUpper(code),
			},
		}
	default:
		payload = map[string]any{
			"error": map[string]any{
				"message": message,
				"type":    code,
				"code":    code,
			},
		}
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// claudeErrorType maps an HTTP status onto the Anthropic error taxonomy.
func claudeErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}
