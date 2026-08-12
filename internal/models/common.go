package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// APIWarning represents a non-fatal warning in an API response
type APIWarning struct {
	Code    string `json:"code"`              // Machine-readable code, e.g., "cache_invalidation_failed"
	Message string `json:"message"`           // Human-readable message
	Context string `json:"context,omitempty"` // Additional context, e.g., "user_id:123"
}

// PaginationMeta represents pagination metadata
type PaginationMeta struct {
	Page       int `json:"page"`
	Limit      int `json:"limit"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// SystemSetting represents a system configuration setting
type SystemSetting struct {
	ID          int       `json:"id"`
	Key         string    `json:"key"`
	Value       string    `json:"value"`
	ValueType   string    `json:"value_type"` // string, boolean, integer, json
	Description string    `json:"description"`
	Category    string    `json:"category"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SetupStatus represents the system setup status
type SetupStatus struct {
	SetupCompleted        bool `json:"setup_completed"`
	AdminUserCreated      bool `json:"admin_user_created"`
	TimeTrackingEnabled   bool `json:"time_tracking_enabled"`
	TestManagementEnabled bool `json:"test_management_enabled"`
}

// AIFeatureMode controls how a feature resolves its LLM connection.
type AIFeatureMode string

const (
	AIFeatureModeDefault  AIFeatureMode = "default"
	AIFeatureModeSpecific AIFeatureMode = "specific"
	AIFeatureModeDisabled AIFeatureMode = "disabled"
)

// AIFeatureConfig is the per-feature LLM configuration.
type AIFeatureConfig struct {
	Mode         AIFeatureMode `json:"mode"`
	ConnectionID int           `json:"connection_id"`
	Schedule     string        `json:"schedule,omitempty"` // "daily" (default) or "every_6h" — only meaningful for daily_briefing
}

// AIFeaturesConfig maps feature keys to their configuration.
type AIFeaturesConfig map[string]AIFeatureConfig

// SetupRequest represents the initial setup configuration
type SetupRequest struct {
	AdminUser      SetupUser      `json:"admin_user"`
	ModuleSettings ModuleSettings `json:"module_settings"`
}

// SetupUser represents a user for initial setup (includes password)
type SetupUser struct {
	Email     string `json:"email"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Password  string `json:"password"` // Plaintext; hashed server-side
}

// ModuleSettings represents module visibility settings
type ModuleSettings struct {
	TimeTrackingEnabled    bool `json:"time_tracking_enabled"`
	TestManagementEnabled  bool `json:"test_management_enabled"`
	WorkspaceManagedAgents bool `json:"workspace_managed_agents"`
}

// APIToken represents a bearer token for API authentication
type APIToken struct {
	ID          int        `json:"id"`
	UserID      int        `json:"user_id"`
	Name        string     `json:"name"`         // Human-readable name for the token
	Token       string     `json:"-"`            // The actual token (hashed in DB, not sent to client)
	TokenPrefix string     `json:"token_prefix"` // First few characters for identification
	Permissions string     `json:"permissions"`  // JSON array of permissions
	IsTemporary bool       `json:"is_temporary"` // True for SSH session tokens
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// OAuthClientID and OAuthResource are internal token-binding metadata.
	// They stay empty for user-created PATs and are never exposed through token
	// management APIs. MCP uses them to reject OAuth tokens minted for another
	// resource while preserving the existing PAT fallback.
	OAuthClientID string `json:"-"`
	OAuthResource string `json:"-"`
	// Joined fields
	UserEmail string `json:"user_email,omitempty"`
	UserName  string `json:"user_name,omitempty"`
}

// APITokenCreate represents the request for creating an API token
type APITokenCreate struct {
	Name          string     `json:"name"`
	UserID        *int       `json:"user_id,omitempty"` // Optional: admins can create tokens for other users
	Permissions   []string   `json:"permissions"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	IsTemporary   bool       `json:"-"` // Internal-only: hide ephemeral server-minted tokens from user/admin token lists
	OAuthClientID string     `json:"-"` // Internal-only: OAuth client that requested this token
	OAuthResource string     `json:"-"` // Internal-only: RFC 8707 resource audience
}

// UnmarshalJSON accepts expires_at as either an RFC3339 timestamp or a bare
// calendar date (YYYY-MM-DD). The token form's <input type="date"> submits a
// bare date, which the default time.Time decoder rejects — failing the whole
// request body with a 400 before any token logic runs. Parsing it here keeps
// every APITokenCreate decode path (currently POST /api-tokens) tolerant of
// both shapes without changing the field type the handler and store consume.
func (r *APITokenCreate) UnmarshalJSON(data []byte) error {
	// alias drops APITokenCreate's methods so the nested Unmarshal below does
	// not recurse into this one; ExpiresAt is shadowed as raw JSON so a bare
	// date does not trip the embedded time.Time decoder.
	type alias APITokenCreate
	aux := &struct {
		ExpiresAt json.RawMessage `json:"expires_at"`
		*alias
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	r.ExpiresAt = nil
	if len(aux.ExpiresAt) == 0 || string(aux.ExpiresAt) == "null" {
		return nil
	}
	var raw string
	if err := json.Unmarshal(aux.ExpiresAt, &raw); err != nil {
		return fmt.Errorf("expires_at must be a string date or RFC3339 timestamp: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	expiry, err := parseTokenExpiry(raw)
	if err != nil {
		return err
	}
	r.ExpiresAt = &expiry
	return nil
}

// parseTokenExpiry accepts an RFC3339 timestamp or a bare calendar date. A bare
// date is interpreted as the end of that day in UTC (23:59:59) so a token
// created to expire "today" stays valid through the day rather than expiring at
// midnight — otherwise the input's min=today would mint an already-expired token.
func parseTokenExpiry(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if d, err := time.Parse("2006-01-02", raw); err == nil {
		return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("invalid expires_at %q: want RFC3339 (2006-01-02T15:04:05Z) or a date (2006-01-02)", raw)
}

// APITokenResponse represents the response when creating an API token
type APITokenResponse struct {
	Token    string   `json:"token"`     // Only returned on creation
	APIToken APIToken `json:"api_token"` // Token metadata
}

// CacheStats represents permission cache performance metrics
type CacheStats struct {
	Hits                       int64   `json:"hits"`
	Misses                     int64   `json:"misses"`
	Errors                     int64   `json:"errors"`
	HitRatio                   float64 `json:"hit_ratio"`
	AvgLoadTime                int64   `json:"avg_load_time_ms"`
	TotalUsers                 int64   `json:"total_cached_users"`
	PermissionSnapshotDecodes  uint64  `json:"permission_snapshot_decodes"`
	ActiveWorkspaceCacheHits   uint64  `json:"active_workspace_cache_hits"`
	ActiveWorkspaceCacheMisses uint64  `json:"active_workspace_cache_misses"`
}
