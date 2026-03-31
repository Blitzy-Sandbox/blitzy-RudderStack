package secrets

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
)

// testEncryptionKey is exactly 32 bytes for AES-256, used across all tests.
const testEncryptionKey = "test-encryption-key-32-bytes!!!!"

// captureArg implements the sqlmock.Argument interface to capture the actual
// driver.Value passed by the database call. This enables encryption round-trip
// testing: the encrypted ciphertext written by Set can be replayed in a Get mock
// without needing to know the encrypted value upfront (the nonce is random).
type captureArg struct {
	captured driver.Value
}

// Match satisfies the sqlmock.Argument interface. It stores the actual value and
// unconditionally returns true so the mock expectation always succeeds.
func (c *captureArg) Match(v driver.Value) bool {
	c.captured = v
	return true
}

// setupTestManager creates a SecretsManager backed by a go-sqlmock database
// for unit testing. It configures a known 32-byte AES-256 encryption key,
// attaches a NOP logger, and registers cleanup for the mock DB connection.
func setupTestManager(t *testing.T) (*SecretsManager, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	conf := config.New()
	conf.Set("Functions.Secrets.EncryptionKey", testEncryptionKey)

	mgr := New(conf, logger.NOP, db)
	require.NotNil(t, mgr)
	return mgr, mock
}

// =============================================================================
// Constructor Tests
// =============================================================================

// TestNewSecretsManager verifies the constructor creates a valid manager that
// stores references to the database, logger, and configuration.
func TestNewSecretsManager(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	conf := config.New()
	conf.Set("Functions.Secrets.EncryptionKey", testEncryptionKey)

	mgr := New(conf, logger.NOP, db)
	require.NotNil(t, mgr)
	require.NotNil(t, mgr.db)
	require.NotNil(t, mgr.conf)
	require.NotNil(t, mgr.log)
}

// TestNewSecretsManager_DefaultEncryptionKey verifies that the encryption key
// is read from config and a Set/Get round-trip succeeds with the configured key.
func TestNewSecretsManager_DefaultEncryptionKey(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	// Verify encryption works with the configured key by performing a Set/Get round-trip.
	mock.ExpectExec("INSERT INTO function_secrets").
		WithArgs("func-cfg", "test_key", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := mgr.Set(ctx, "func-cfg", "test_key", "config_test_value")
	require.NoError(t, err)

	// Generate a valid ciphertext for the mock Get response using the same key.
	encrypted, err := mgr.encrypt("config_test_value")
	require.NoError(t, err)

	mock.ExpectQuery("SELECT encrypted_value FROM function_secrets").
		WithArgs("func-cfg", "test_key").
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value"}).AddRow(encrypted))

	val, err := mgr.Get(ctx, "func-cfg", "test_key")
	require.NoError(t, err)
	require.Equal(t, "config_test_value", val)
	require.NoError(t, mock.ExpectationsWereMet())
}

// =============================================================================
// Set Tests
// =============================================================================

// TestSecretsManager_Set verifies that a secret is encrypted and stored via an
// upsert query. The encrypted value is matched with AnyArg because the random
// nonce makes the ciphertext non-deterministic.
func TestSecretsManager_Set(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectExec("INSERT INTO function_secrets").
		WithArgs("func-123", "api_key", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := mgr.Set(ctx, "func-123", "api_key", "sk_test_12345")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Set_UpdateExisting verifies that calling Set twice with the
// same functionID+key triggers two upserts (INSERT … ON CONFLICT DO UPDATE).
func TestSecretsManager_Set_UpdateExisting(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	// First insert.
	mock.ExpectExec("INSERT INTO function_secrets").
		WithArgs("func-123", "api_key", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Second insert (upsert — ON CONFLICT DO UPDATE).
	mock.ExpectExec("INSERT INTO function_secrets").
		WithArgs("func-123", "api_key", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := mgr.Set(ctx, "func-123", "api_key", "original_value")
	require.NoError(t, err)

	err = mgr.Set(ctx, "func-123", "api_key", "updated_value")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Set_EmptyValue verifies that an empty string is a valid
// secret value (useful for optional settings that may be blank).
func TestSecretsManager_Set_EmptyValue(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectExec("INSERT INTO function_secrets").
		WithArgs("func-123", "optional_key", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := mgr.Set(ctx, "func-123", "optional_key", "")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Set_DBError verifies that a database error during the upsert
// is wrapped and returned to the caller.
func TestSecretsManager_Set_DBError(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectExec("INSERT INTO function_secrets").
		WithArgs("func-123", "api_key", sqlmock.AnyArg()).
		WillReturnError(fmt.Errorf("connection refused"))

	err := mgr.Set(ctx, "func-123", "api_key", "value")
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection refused")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Set_EmptyFunctionID verifies that an empty function ID is
// rejected with the ErrInvalidFunctionID sentinel error.
func TestSecretsManager_Set_EmptyFunctionID(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	err := mgr.Set(ctx, "", "key", "value")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFunctionID)
}

// TestSecretsManager_Set_EmptyKey verifies that an empty secret key is rejected
// with the ErrInvalidKey sentinel error.
func TestSecretsManager_Set_EmptyKey(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	err := mgr.Set(ctx, "func-123", "", "value")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidKey)
}

// =============================================================================
// Get Tests
// =============================================================================

// TestSecretsManager_Get verifies that a stored secret is correctly retrieved
// and decrypted back to its original plaintext.
func TestSecretsManager_Get(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	// Create valid ciphertext using the manager's internal encryption.
	encrypted, err := mgr.encrypt("sk_test_12345")
	require.NoError(t, err)

	mock.ExpectQuery("SELECT encrypted_value FROM function_secrets").
		WithArgs("func-123", "api_key").
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value"}).AddRow(encrypted))

	val, err := mgr.Get(ctx, "func-123", "api_key")
	require.NoError(t, err)
	require.Equal(t, "sk_test_12345", val)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Get_NotFound verifies that a request for a non-existent
// secret returns the ErrSecretNotFound sentinel error and an empty string.
func TestSecretsManager_Get_NotFound(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT encrypted_value FROM function_secrets").
		WithArgs("func-123", "nonexistent_key").
		WillReturnError(sql.ErrNoRows)

	val, err := mgr.Get(ctx, "func-123", "nonexistent_key")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSecretNotFound)
	require.Empty(t, val)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Get_DecryptionFailure verifies that corrupted ciphertext
// stored in the database is detected and returns a descriptive error. The hex
// string below is valid hex encoding of 30 random bytes: 12 for the GCM nonce
// and 18 for the body+tag, but the GCM authentication tag is invalid, causing
// decryption to fail.
func TestSecretsManager_Get_DecryptionFailure(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	// Valid hex (60 chars = 30 bytes), but the GCM authentication tag is all zeros
	// and will not pass integrity verification.
	corruptedHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d"

	mock.ExpectQuery("SELECT encrypted_value FROM function_secrets").
		WithArgs("func-123", "corrupted_key").
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value"}).AddRow(corruptedHex))

	val, err := mgr.Get(ctx, "func-123", "corrupted_key")
	require.Error(t, err)
	require.Empty(t, val)
	require.Contains(t, err.Error(), "decrypting secret")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Get_EmptyFunctionID verifies that an empty function ID on
// Get is rejected with ErrInvalidFunctionID.
func TestSecretsManager_Get_EmptyFunctionID(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	val, err := mgr.Get(ctx, "", "key")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFunctionID)
	require.Empty(t, val)
}

// TestSecretsManager_Get_EmptyKey verifies that an empty secret key on Get is
// rejected with ErrInvalidKey.
func TestSecretsManager_Get_EmptyKey(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	val, err := mgr.Get(ctx, "func-123", "")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidKey)
	require.Empty(t, val)
}

// =============================================================================
// GetAll Tests
// =============================================================================

// TestSecretsManager_GetAll verifies that all secrets for a function are retrieved,
// decrypted, and returned as a map[string]string.
func TestSecretsManager_GetAll(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	// Pre-encrypt values to populate mock rows.
	encAPIKey, err := mgr.encrypt("sk_test_12345")
	require.NoError(t, err)
	encDBPass, err := mgr.encrypt("db_password_secret")
	require.NoError(t, err)
	encWebhook, err := mgr.encrypt("https://hooks.example.com/secret")
	require.NoError(t, err)

	rows := sqlmock.NewRows([]string{"key", "encrypted_value"}).
		AddRow("api_key", encAPIKey).
		AddRow("db_password", encDBPass).
		AddRow("webhook_url", encWebhook)

	mock.ExpectQuery("SELECT key, encrypted_value FROM function_secrets").
		WithArgs("func-123").
		WillReturnRows(rows)

	result, err := mgr.GetAll(ctx, "func-123")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result, 3)
	require.Equal(t, "sk_test_12345", result["api_key"])
	require.Equal(t, "db_password_secret", result["db_password"])
	require.Equal(t, "https://hooks.example.com/secret", result["webhook_url"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_GetAll_Empty verifies that requesting secrets for a function
// with no stored secrets returns an empty (non-nil) map and no error.
func TestSecretsManager_GetAll_Empty(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT key, encrypted_value FROM function_secrets").
		WithArgs("func-no-secrets").
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value"}))

	result, err := mgr.GetAll(ctx, "func-no-secrets")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result)
	require.Len(t, result, 0)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_GetAll_DBError verifies that a database error during the
// query is wrapped and returned, with a nil map.
func TestSecretsManager_GetAll_DBError(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectQuery("SELECT key, encrypted_value FROM function_secrets").
		WithArgs("func-123").
		WillReturnError(fmt.Errorf("connection refused"))

	result, err := mgr.GetAll(ctx, "func-123")
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "connection refused")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_GetAll_EmptyFunctionID verifies that an empty function ID
// on GetAll is rejected with ErrInvalidFunctionID.
func TestSecretsManager_GetAll_EmptyFunctionID(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	result, err := mgr.GetAll(ctx, "")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFunctionID)
	require.Nil(t, result)
}

// =============================================================================
// Delete Tests
// =============================================================================

// TestSecretsManager_Delete verifies that a single secret is successfully deleted
// when one row is affected.
func TestSecretsManager_Delete(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectExec("DELETE FROM function_secrets").
		WithArgs("func-123", "api_key").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := mgr.Delete(ctx, "func-123", "api_key")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Delete_NotFound verifies that deleting a non-existent secret
// (0 rows affected) returns the ErrSecretNotFound sentinel error.
func TestSecretsManager_Delete_NotFound(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectExec("DELETE FROM function_secrets").
		WithArgs("func-123", "nonexistent").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := mgr.Delete(ctx, "func-123", "nonexistent")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSecretNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_Delete_EmptyFunctionID verifies that an empty function ID on
// Delete is rejected with ErrInvalidFunctionID.
func TestSecretsManager_Delete_EmptyFunctionID(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	err := mgr.Delete(ctx, "", "key")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFunctionID)
}

// TestSecretsManager_Delete_EmptyKey verifies that an empty secret key on Delete
// is rejected with ErrInvalidKey.
func TestSecretsManager_Delete_EmptyKey(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	err := mgr.Delete(ctx, "func-123", "")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidKey)
}

// =============================================================================
// DeleteAll Tests
// =============================================================================

// TestSecretsManager_DeleteAll verifies that all secrets for a function are
// successfully removed.
func TestSecretsManager_DeleteAll(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectExec("DELETE FROM function_secrets").
		WithArgs("func-123").
		WillReturnResult(sqlmock.NewResult(0, 3))

	err := mgr.DeleteAll(ctx, "func-123")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_DeleteAll_NoSecrets verifies that DeleteAll is idempotent —
// deleting nothing when the function has no stored secrets is not an error.
func TestSecretsManager_DeleteAll_NoSecrets(t *testing.T) {
	mgr, mock := setupTestManager(t)
	ctx := context.Background()

	mock.ExpectExec("DELETE FROM function_secrets").
		WithArgs("func-no-secrets").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := mgr.DeleteAll(ctx, "func-no-secrets")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestSecretsManager_DeleteAll_EmptyFunctionID verifies that an empty function ID
// on DeleteAll is rejected with ErrInvalidFunctionID.
func TestSecretsManager_DeleteAll_EmptyFunctionID(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	err := mgr.DeleteAll(ctx, "")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidFunctionID)
}

// =============================================================================
// Encryption Round-Trip Tests
// =============================================================================

// TestSecretsManager_EncryptionRoundTrip verifies the complete
// encrypt → store → retrieve → decrypt cycle via two sub-tests:
//  1. Direct encrypt/decrypt without database (baseline crypto correctness).
//  2. Full Set→Get flow with a mock capture arg that replays the exact ciphertext
//     written by Set into the Get mock (end-to-end round-trip through the API).
func TestSecretsManager_EncryptionRoundTrip(t *testing.T) {
	t.Run("direct encrypt decrypt", func(t *testing.T) {
		mgr, _ := setupTestManager(t)

		plaintext := "super_secret_value_123"
		encrypted, err := mgr.encrypt(plaintext)
		require.NoError(t, err)
		require.NotEmpty(t, encrypted)

		decrypted, err := mgr.decrypt(encrypted)
		require.NoError(t, err)
		require.Equal(t, plaintext, decrypted)
	})

	t.Run("set get round trip via mock capture", func(t *testing.T) {
		mgr, mock := setupTestManager(t)
		ctx := context.Background()

		// Use captureArg to grab the encrypted ciphertext written by Set.
		capture := &captureArg{}
		mock.ExpectExec("INSERT INTO function_secrets").
			WithArgs("func-rt", "secret_key", capture).
			WillReturnResult(sqlmock.NewResult(1, 1))

		err := mgr.Set(ctx, "func-rt", "secret_key", "super_secret_value_123")
		require.NoError(t, err)

		// The captured value must be a non-empty string different from the plaintext.
		capturedValue, ok := capture.captured.(string)
		require.True(t, ok, "captured argument must be a string")
		require.NotEmpty(t, capturedValue)
		require.NotEqual(t, "super_secret_value_123", capturedValue, "stored value must be encrypted, not plaintext")

		// Replay the exact ciphertext in the Get mock.
		mock.ExpectQuery("SELECT encrypted_value FROM function_secrets").
			WithArgs("func-rt", "secret_key").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value"}).AddRow(capturedValue))

		val, err := mgr.Get(ctx, "func-rt", "secret_key")
		require.NoError(t, err)
		require.Equal(t, "super_secret_value_123", val)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestSecretsManager_EncryptionDifferentValues verifies that encrypting two
// different plaintext values produces two different ciphertexts.
func TestSecretsManager_EncryptionDifferentValues(t *testing.T) {
	mgr, _ := setupTestManager(t)

	enc1, err := mgr.encrypt("value_one")
	require.NoError(t, err)
	enc2, err := mgr.encrypt("value_two")
	require.NoError(t, err)

	require.NotEqual(t, enc1, enc2)
}

// TestSecretsManager_EncryptionSameValueDifferentCiphertext verifies that
// encrypting the same plaintext twice produces different ciphertexts (because
// AES-GCM generates a fresh random 12-byte nonce each time). Both ciphertexts
// must decrypt to the original plaintext.
func TestSecretsManager_EncryptionSameValueDifferentCiphertext(t *testing.T) {
	mgr, _ := setupTestManager(t)

	plaintext := "same_secret_value"
	enc1, err := mgr.encrypt(plaintext)
	require.NoError(t, err)
	enc2, err := mgr.encrypt(plaintext)
	require.NoError(t, err)

	// Random nonce → different ciphertexts (critical security property).
	require.NotEqual(t, enc1, enc2)

	// Both must decrypt back to the original plaintext.
	dec1, err := mgr.decrypt(enc1)
	require.NoError(t, err)
	dec2, err := mgr.decrypt(enc2)
	require.NoError(t, err)

	require.Equal(t, plaintext, dec1)
	require.Equal(t, plaintext, dec2)
}

// =============================================================================
// Context Handling Tests
// =============================================================================

// TestSecretsManager_ContextCancellation verifies that a pre-cancelled context
// causes Set to return an error. The database/sql package checks context.Done()
// before acquiring a connection, so the error surfaces before reaching the mock.
func TestSecretsManager_ContextCancellation(t *testing.T) {
	mgr, _ := setupTestManager(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately before any database operation

	err := mgr.Set(ctx, "func-123", "key", "value")
	require.Error(t, err)
}
