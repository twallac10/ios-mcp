// Package credentials stores and retrieves the iCloud app-specific password
// in Azure Key Vault.
//
// Unlike the Concept2 MCP server's OAuth token, an iCloud app-specific
// password doesn't expire on a schedule and has no refresh flow — it's a
// static credential the user generates at appleid.apple.com and that stays
// valid until they revoke it. So there's no silent-refresh logic here, just
// get/set, backed by DefaultAzureCredential (the local `az login` session
// when run locally, the Function App's Managed Identity in Azure — no
// environment-specific branching needed).
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// SecretName is the Key Vault secret holding the iCloud credentials, stored
// as a JSON object: {"apple_id": "...", "app_specific_password": "..."}.
const SecretName = "icloud-credentials"

// ErrNotFound is returned by Load when no credentials have been seeded yet.
var ErrNotFound = errors.New("no iCloud credentials found in Key Vault")

// Credentials holds the Apple ID and app-specific password used to
// authenticate to iCloud's CalDAV server over HTTP Basic Auth.
//
// Never log these; the whole point of Key Vault is that this value exists
// only in memory and in the vault, never on disk or in a log line.
type Credentials struct {
	AppleID             string `json:"apple_id"`
	AppSpecificPassword string `json:"app_specific_password"`
}

// secretsClient is the subset of *azsecrets.Client that Store needs, kept as
// an interface so tests can substitute a fake.
type secretsClient interface {
	GetSecret(ctx context.Context, name string, version string, options *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error)
	SetSecret(ctx context.Context, name string, parameters azsecrets.SetSecretParameters, options *azsecrets.SetSecretOptions) (azsecrets.SetSecretResponse, error)
}

// Store reads and writes Credentials in Azure Key Vault, with a simple
// in-memory cache so a warm Function instance doesn't call Key Vault on
// every single MCP tool call.
type Store struct {
	client secretsClient

	mu     sync.Mutex
	cached *Credentials
}

// NewStore creates a Store backed by the named Key Vault, authenticating via
// DefaultAzureCredential.
func NewStore(vaultName string) (*Store, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: building DefaultAzureCredential: %w", err)
	}
	client, err := azsecrets.NewClient(fmt.Sprintf("https://%s.vault.azure.net", vaultName), cred, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials: building Key Vault client: %w", err)
	}
	return &Store{client: client}, nil
}

// Load returns the cached credentials if present, otherwise fetches them
// from Key Vault and caches the result. Returns ErrNotFound if no secret has
// been seeded yet (see cmd/seed-credentials).
func (s *Store) Load(ctx context.Context) (*Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil {
		return s.cached, nil
	}

	creds, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	s.cached = creds
	return creds, nil
}

// Invalidate clears the in-memory cache, forcing the next Load to re-fetch
// from Key Vault. Call this after iCloud rejects the cached credentials
// (HTTP 401), in case they were rotated via cmd/seed-credentials since this
// instance started.
func (s *Store) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = nil
}

func (s *Store) fetch(ctx context.Context) (*Credentials, error) {
	resp, err := s.client.GetSecret(ctx, SecretName, "", nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("credentials: fetching %q from Key Vault: %w", SecretName, err)
	}
	if resp.Value == nil {
		return nil, ErrNotFound
	}

	var creds Credentials
	if err := json.Unmarshal([]byte(*resp.Value), &creds); err != nil {
		return nil, fmt.Errorf("credentials: stored secret is not valid JSON: %w", err)
	}
	if creds.AppleID == "" || creds.AppSpecificPassword == "" {
		return nil, fmt.Errorf("credentials: stored secret %q is missing apple_id or app_specific_password; re-run cmd/seed-credentials", SecretName)
	}
	return &creds, nil
}

// Save writes creds to Key Vault, overwriting any previous value, and
// updates the in-memory cache. Used by cmd/seed-credentials; the deployed
// server never calls this.
func (s *Store) Save(ctx context.Context, creds *Credentials) error {
	data, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("credentials: marshaling credentials: %w", err)
	}
	value := string(data)
	if _, err := s.client.SetSecret(ctx, SecretName, azsecrets.SetSecretParameters{Value: &value}, nil); err != nil {
		return fmt.Errorf("credentials: saving %q to Key Vault: %w", SecretName, err)
	}

	s.mu.Lock()
	s.cached = creds
	s.mu.Unlock()
	return nil
}
