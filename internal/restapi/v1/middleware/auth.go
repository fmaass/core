// Package middleware provides HTTP middleware for the REST API v1.
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"windshift/internal/auth"
	"windshift/internal/models"
	"windshift/internal/restapi"
)

// tokenStoreRetryAfterSeconds is the Retry-After a client is given when the
// token store is unreachable. Short: a Postgres restart is usually seconds, and
// the client's own backoff takes it from there.
const tokenStoreRetryAfterSeconds = "5"

// BearerAuth middleware requires bearer token authentication for the public API
// It only accepts Authorization: Bearer crw_xxx tokens, not session cookies
type BearerAuth struct {
	tokenManager      *auth.TokenManager
	permissionService PermissionChecker
}

// PermissionChecker abstracts the permission check needed by the middleware.
type PermissionChecker interface {
	IsSystemAdmin(userID int) (bool, error)
}

// NewBearerAuthWithPermissions creates a BearerAuth with permission checking support.
func NewBearerAuthWithPermissions(tokenManager *auth.TokenManager, permSvc PermissionChecker) *BearerAuth {
	return &BearerAuth{
		tokenManager:      tokenManager,
		permissionService: permSvc,
	}
}

// RequireAuth returns middleware that requires valid bearer token authentication
func (ba *BearerAuth) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			restapi.RespondError(w, r, restapi.ErrUnauthorized)
			return
		}

		if !strings.HasPrefix(authHeader, "Bearer ") {
			restapi.RespondError(w, r, restapi.NewAPIError(
				http.StatusUnauthorized,
				restapi.ErrCodeInvalidToken,
				"Authorization header must use Bearer scheme",
			))
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" {
			restapi.RespondError(w, r, restapi.NewAPIError(
				http.StatusUnauthorized,
				restapi.ErrCodeInvalidToken,
				"Bearer token is empty",
			))
			return
		}

		user, apiToken, err := ba.tokenManager.ValidateToken(token)
		if err != nil {
			// An unreachable token store is not a bad token. Answering 401
			// INVALID_TOKEN while Postgres was down told every client its
			// credential had been rejected, which invites a rotation that
			// fixes nothing (INFRA-328). Classified by error type, before the
			// message is looked at at all.
			if auth.IsTokenStoreUnavailable(err) {
				slog.Error("token store unavailable", slog.String("component", "restapi.auth"), slog.Any("error", err))
				w.Header().Set("Retry-After", tokenStoreRetryAfterSeconds)
				restapi.RespondError(w, r, restapi.NewAPIError(
					http.StatusServiceUnavailable,
					restapi.ErrCodeServiceUnavailable,
					"Authentication store is temporarily unavailable",
				))
				return
			}
			// Check for specific error types
			errMsg := err.Error()
			if strings.Contains(errMsg, "expired") {
				restapi.RespondError(w, r, restapi.ErrTokenExpired)
				return
			}
			if strings.Contains(errMsg, "disabled") {
				restapi.RespondError(w, r, restapi.NewAPIError(
					http.StatusUnauthorized,
					restapi.ErrCodeUnauthorized,
					"User account is disabled",
				))
				return
			}
			restapi.RespondError(w, r, restapi.ErrInvalidToken)
			return
		}

		// Add user and token to context
		ctx := r.Context()
		ctx = context.WithValue(ctx, restapi.ContextKeyUser, user)
		ctx = context.WithValue(ctx, restapi.ContextKeyAPIToken, apiToken)
		ctx = context.WithValue(ctx, restapi.ContextKeyAuthMethod, "bearer")

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequirePermission returns middleware that checks if the token has required permissions
func (ba *BearerAuth) RequirePermission(permissions ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiToken, ok := r.Context().Value(restapi.ContextKeyAPIToken).(*models.APIToken)
			if !ok || apiToken == nil {
				restapi.RespondError(w, r, restapi.ErrUnauthorized)
				return
			}

			if !ba.tokenManager.CheckTokenPermissions(apiToken, permissions) {
				restapi.RespondError(w, r, restapi.NewAPIError(
					http.StatusForbidden,
					restapi.ErrCodeInsufficientPermission,
					"Token does not have required permissions",
				).WithDetails(map[string]any{
					"required": permissions,
				}))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireSystemAdmin returns middleware that checks if the authenticated user is a system admin.
func (ba *BearerAuth) RequireSystemAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := r.Context().Value(restapi.ContextKeyUser).(*models.User)
		if !ok || user == nil {
			restapi.RespondError(w, r, restapi.ErrUnauthorized)
			return
		}

		if ba.permissionService == nil {
			restapi.RespondError(w, r, restapi.ErrInternalError)
			return
		}

		isAdmin, err := ba.permissionService.IsSystemAdmin(user.ID)
		if err != nil || !isAdmin {
			restapi.RespondError(w, r, restapi.ErrAdminRequired)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// GetUser retrieves the authenticated user from context
func GetUser(ctx context.Context) *models.User {
	if user, ok := ctx.Value(restapi.ContextKeyUser).(*models.User); ok {
		return user
	}
	return nil
}

// GetAPIToken retrieves the API token from context
func GetAPIToken(ctx context.Context) *models.APIToken {
	if token, ok := ctx.Value(restapi.ContextKeyAPIToken).(*models.APIToken); ok {
		return token
	}
	return nil
}
