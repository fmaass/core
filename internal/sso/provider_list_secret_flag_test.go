package sso

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"windshift/internal/database"
)

// newProviderStore opens a real SQLite database (the driver the app itself
// uses), applies the shipped SSO schema and returns a store over it. Nothing
// is mocked: the queries under test are the queries that run in production.
func newProviderStore(t *testing.T) *ProviderStore {
	t.Helper()

	db, err := database.NewSQLiteDBWithPoolSizes(filepath.Join(t.TempDir(), "sso.db"), 2, 1)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema, err := os.ReadFile(filepath.Join("..", "database", "schema", "sso.sql"))
	if err != nil {
		t.Fatalf("read sso schema: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply sso schema: %v", err)
	}

	return NewProviderStore(db)
}

func seedProvider(t *testing.T, store *ProviderStore, slug, encryptedSecret string) {
	t.Helper()
	p := &SSOProvider{
		Slug:                  slug,
		Name:                  slug,
		ProviderType:          ProviderTypeOIDC,
		Enabled:               true,
		IssuerURL:             "https://idp.example/" + slug,
		ClientID:              slug + "-client",
		ClientSecretEncrypted: encryptedSecret,
		Scopes:                "openid email profile",
		RequireVerifiedEmail:  true,
	}
	if err := store.Create(p); err != nil {
		t.Fatalf("create provider %s: %v", slug, err)
	}
}

func listed(t *testing.T, store *ProviderStore, slug string) *SSOProvider {
	t.Helper()
	providers, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range providers {
		if p.Slug == slug {
			return p
		}
	}
	t.Fatalf("provider %s missing from List (%d rows)", slug, len(providers))
	return nil
}

// TestListAndDetailAgreeOnClientSecret is INFRA-103 itself: the collection
// query selects providerColumnsWithoutSecret, so before the fix the list
// serializer derived has_client_secret from an always-empty
// ClientSecretEncrypted and reported false for a provider whose detail
// endpoint reported true.
func TestListAndDetailAgreeOnClientSecret(t *testing.T) {
	store := newProviderStore(t)
	seedProvider(t, store, "with-secret", "enc:v1:a-configured-client-secret")
	seedProvider(t, store, "no-secret", "")

	for _, tc := range []struct {
		slug string
		want bool
	}{
		{"with-secret", true},
		{"no-secret", false},
	} {
		detail, err := store.GetBySlug(tc.slug)
		if err != nil {
			t.Fatalf("GetBySlug(%s): %v", tc.slug, err)
		}
		if got := detail.HasClientSecret(); got != tc.want {
			t.Errorf("detail %s: HasClientSecret() = %v, want %v", tc.slug, got, tc.want)
		}

		fromList := listed(t, store, tc.slug)
		if got := fromList.HasClientSecret(); got != tc.want {
			t.Errorf("list %s: HasClientSecret() = %v, want %v (list and detail must agree)", tc.slug, got, tc.want)
		}
		if fromList.HasClientSecret() != detail.HasClientSecret() {
			t.Errorf("list/detail disagree for %s: list=%v detail=%v", tc.slug, fromList.HasClientSecret(), detail.HasClientSecret())
		}
	}
}

// TestListEnabledAgreesOnClientSecret covers the second caller of the
// no-secret column list (ListEnabled), which feeds the public login surface.
func TestListEnabledAgreesOnClientSecret(t *testing.T) {
	store := newProviderStore(t)
	seedProvider(t, store, "with-secret", "enc:v1:a-configured-client-secret")

	providers, err := store.ListEnabled()
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("ListEnabled returned %d providers, want 1", len(providers))
	}
	if !providers[0].HasClientSecret() {
		t.Errorf("ListEnabled: HasClientSecret() = false for a provider with a configured secret")
	}
}

// TestListNeverCarriesTheSecretValue is the other half of the acceptance
// criterion: the flag must be derived in SQL, and the secret itself must
// never leave the database on the list path (nor be serialized on either).
func TestListNeverCarriesTheSecretValue(t *testing.T) {
	const secret = "enc:v1:a-configured-client-secret"
	store := newProviderStore(t)
	seedProvider(t, store, "with-secret", secret)

	fromList := listed(t, store, "with-secret")
	if fromList.ClientSecretEncrypted != "" {
		t.Errorf("List loaded the encrypted secret into the struct: %q", fromList.ClientSecretEncrypted)
	}
	if fromList.ClientSecret != "" {
		t.Errorf("List loaded a plaintext secret into the struct: %q", fromList.ClientSecret)
	}

	detail, err := store.GetBySlug("with-secret")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	for name, p := range map[string]*SSOProvider{"list": fromList, "detail": detail} {
		encoded, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal %s provider: %v", name, err)
		}
		if strings.Contains(string(encoded), secret) {
			t.Errorf("%s serialization leaks the client secret: %s", name, encoded)
		}
	}
}
