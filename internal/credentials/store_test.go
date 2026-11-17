package credentials

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

type fakeSecretsClient struct {
	getValue   *string
	getErr     error
	getCalls   int
	setCalls   int
	lastSetVal string
}

func (f *fakeSecretsClient) GetSecret(ctx context.Context, name string, version string, options *azsecrets.GetSecretOptions) (azsecrets.GetSecretResponse, error) {
	f.getCalls++
	if f.getErr != nil {
		return azsecrets.GetSecretResponse{}, f.getErr
	}
	return azsecrets.GetSecretResponse{Secret: azsecrets.Secret{Value: f.getValue}}, nil
}

func (f *fakeSecretsClient) SetSecret(ctx context.Context, name string, parameters azsecrets.SetSecretParameters, options *azsecrets.SetSecretOptions) (azsecrets.SetSecretResponse, error) {
	f.setCalls++
	f.lastSetVal = *parameters.Value
	return azsecrets.SetSecretResponse{}, nil
}

func TestLoadReturnsNotFoundWhenSecretMissing(t *testing.T) {
	fake := &fakeSecretsClient{getErr: &azcore.ResponseError{StatusCode: 404}}
	store := &Store{client: fake}

	_, err := store.Load(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load() error = %v, want ErrNotFound", err)
	}
}

func TestLoadParsesStoredJSON(t *testing.T) {
	val := `{"apple_id":"me@example.com","app_specific_password":"abcd-efgh-ijkl-mnop"}`
	fake := &fakeSecretsClient{getValue: &val}
	store := &Store{client: fake}

	creds, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if creds.AppleID != "me@example.com" || creds.AppSpecificPassword != "abcd-efgh-ijkl-mnop" {
		t.Fatalf("Load() = %+v, unexpected", creds)
	}
}

func TestLoadCachesAcrossCalls(t *testing.T) {
	val := `{"apple_id":"me@example.com","app_specific_password":"secret"}`
	fake := &fakeSecretsClient{getValue: &val}
	store := &Store{client: fake}

	ctx := context.Background()
	if _, err := store.Load(ctx); err != nil {
		t.Fatalf("first Load() error = %v", err)
	}
	if _, err := store.Load(ctx); err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if fake.getCalls != 1 {
		t.Fatalf("GetSecret called %d times, want 1 (second Load should hit the cache)", fake.getCalls)
	}
}

func TestInvalidateForcesRefetch(t *testing.T) {
	val := `{"apple_id":"me@example.com","app_specific_password":"secret"}`
	fake := &fakeSecretsClient{getValue: &val}
	store := &Store{client: fake}

	ctx := context.Background()
	if _, err := store.Load(ctx); err != nil {
		t.Fatalf("first Load() error = %v", err)
	}
	store.Invalidate()
	if _, err := store.Load(ctx); err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if fake.getCalls != 2 {
		t.Fatalf("GetSecret called %d times, want 2 (Invalidate should force a refetch)", fake.getCalls)
	}
}

func TestSaveWritesJSONAndUpdatesCache(t *testing.T) {
	fake := &fakeSecretsClient{}
	store := &Store{client: fake}

	creds := &Credentials{AppleID: "me@example.com", AppSpecificPassword: "xyz"}
	if err := store.Save(context.Background(), creds); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if fake.setCalls != 1 {
		t.Fatalf("SetSecret called %d times, want 1", fake.setCalls)
	}
	if fake.lastSetVal != `{"apple_id":"me@example.com","app_specific_password":"xyz"}` {
		t.Fatalf("SetSecret value = %q, unexpected", fake.lastSetVal)
	}

	// Save should prime the cache so a subsequent Load doesn't hit Key Vault.
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() after Save() error = %v", err)
	}
	if got.AppleID != creds.AppleID {
		t.Fatalf("Load() after Save() = %+v, want %+v", got, creds)
	}
	if fake.getCalls != 0 {
		t.Fatalf("GetSecret called %d times, want 0 (Save should have primed the cache)", fake.getCalls)
	}
}

func TestLoadRejectsIncompleteSecret(t *testing.T) {
	val := `{"apple_id":"me@example.com"}`
	store := &Store{client: &fakeSecretsClient{getValue: &val}}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatalf("Load() of a secret with no password succeeded, want an error")
	}
	if store.cached != nil {
		t.Errorf("an incomplete secret was cached")
	}
}
