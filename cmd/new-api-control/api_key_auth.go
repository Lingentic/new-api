package main

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/model"
)

func handleAuthenticateAPIKey(writer http.ResponseWriter, request *http.Request) {
	// Identity and credential transport are explicit; do not inherit public relay
	// header aliases, query credentials or user impersonation headers.
	if request.URL.RawQuery != "" {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "query parameters are not accepted")
		return
	}
	for _, name := range []string{"Authorization", "Cookie", "New-Api-User", "X-API-Key", "apikey", "X-IAM-User-ID", "X-User-ID", "X-Organization-ID"} {
		if len(request.Header.Values(name)) != 0 {
			writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "credential headers are not accepted")
			return
		}
	}
	var input map[string]string
	if !decodeRequest(writer, request, &input) {
		return
	}
	_, hasSecret := input["secret"]
	_, hasIP := input["client_ip"]
	if len(input) != 2 || !hasSecret || !hasIP {
		writeAPIError(writer, http.StatusBadRequest, "INVALID_ARGUMENT", "only secret and client_ip are accepted")
		return
	}
	principal, err := model.AuthenticateIAMAPIKey(request.Context(), input["secret"], input["client_ip"])
	if err != nil {
		switch {
		case errors.Is(err, model.ErrIAMAPIKeyUnauthorized):
			writeAPIError(writer, http.StatusUnauthorized, "API_KEY_UNAUTHORIZED", "invalid or expired API key")
		case errors.Is(err, model.ErrIAMAPIKeyAccessDenied):
			writeAPIError(writer, http.StatusForbidden, "API_KEY_ACCESS_DENIED", "API key access denied")
		default:
			// Do not log an error that may contain a database parameter or secret.
			writeAPIError(writer, http.StatusServiceUnavailable, "UPSTREAM_INTERNAL", "API key authentication unavailable")
		}
		return
	}
	writeJSON(writer, http.StatusOK, envelope{Success: true, Data: principal})
}
