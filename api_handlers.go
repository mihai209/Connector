package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// APIResponse represents a standard response from the API.
type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// tokenAuthMiddleware validates the Bearer token in the Authorization header.
func (s *Service) tokenAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		expectedToken := "Bearer " + strings.TrimSpace(s.cfg.Connector.Token)

		if expectedToken == "Bearer" || subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedToken)) != 1 {
			s.sendAPIFail(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		next(w, r)
	}
}

// sendAPIJSON sends a JSON response with the provided status code.
func (s *Service) sendAPIJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

// sendAPISuccess sends a success response with the provided data.
func (s *Service) sendAPISuccess(w http.ResponseWriter, data interface{}) {
	s.sendAPIJSON(w, http.StatusOK, APIResponse{Success: true, Data: data})
}

// sendAPIFail sends an error response with the provided status code and message.
func (s *Service) sendAPIFail(w http.ResponseWriter, code int, message string) {
	s.sendAPIJSON(w, code, APIResponse{Success: false, Error: message})
}

// handleServerPowerAPI handles POST /api/servers/:id/power
func (s *Service) handleServerPowerAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.sendAPIFail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	serverID, err := extractServerIDFromURL(r.URL.Path)
	if err != nil {
		s.sendAPIFail(w, http.StatusBadRequest, "invalid server id")
		return
	}

	var payload struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.sendAPIFail(w, http.StatusBadRequest, "invalid payload")
		return
	}

	action := strings.ToLower(strings.TrimSpace(payload.Action))
	if err := s.executePowerAction(serverID, action, ""); err != nil {
		s.sendAPIFail(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.sendAPISuccess(w, map[string]string{"status": "accepted", "action": action})
}

// handleServerStatsAPI handles GET /api/servers/:id/stats
func (s *Service) handleServerStatsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.sendAPIFail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	serverID, err := extractServerIDFromURL(r.URL.Path)
	if err != nil {
		s.sendAPIFail(w, http.StatusBadRequest, "invalid server id")
		return
	}

	stats, err := s.getServerStats(serverID)
	if err != nil {
		s.sendAPIFail(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.sendAPISuccess(w, stats)
}

// extractServerIDFromURL pulls the server ID from paths like /api/servers/123/...
func extractServerIDFromURL(path string) (int, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 {
		return 0, fmt.Errorf("invalid path")
	}
	return strconv.Atoi(parts[2])
}
