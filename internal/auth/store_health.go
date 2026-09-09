package auth

import "windshift/internal/database"

// IsTokenStoreUnavailable reports whether a token validation failed because the
// store backing it could not be reached, rather than because the credential was
// rejected. Callers must answer the two cases differently: a rejected token is
// the client's problem (401, rotate it), an unreachable store is the server's
// (503, come back later). Reporting the second as the first sent operators
// rotating perfectly good tokens during a Postgres restart (INFRA-328).
func IsTokenStoreUnavailable(err error) bool {
	return database.IsUnavailable(err)
}
