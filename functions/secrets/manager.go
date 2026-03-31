// Package secrets provides per-function encrypted secrets and environment variable
// storage for the Functions runtime (Epic E-019). Secrets are injected into the
// Functions runtime as the "settings" parameter during handler execution.
//
// All secret values are encrypted at rest using AES-256-GCM with workspace-scoped
// encryption keys. Each encryption operation uses a fresh cryptographically random
// nonce to prevent nonce reuse attacks. Ciphertext is hex-encoded for safe storage
// in PostgreSQL text columns.
//
// The encryption key is read from the configuration key "Functions.Secrets.EncryptionKey"
// on every operation (not cached at construction time) to support key rotation
// without requiring a service restart.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
)

const (
	// secretsTableName is the database table name for storing function secrets.
	secretsTableName = "function_secrets"
)

// Sentinel errors for the SecretsManager operations.
var (
	// ErrSecretNotFound is returned when a requested secret does not exist.
	ErrSecretNotFound = errors.New("secret not found")

	// ErrInvalidFunctionID is returned when an empty function ID is provided.
	ErrInvalidFunctionID = errors.New("function ID must not be empty")

	// ErrInvalidKey is returned when an empty key is provided.
	ErrInvalidKey = errors.New("secret key must not be empty")
)

// SecretsManager provides encrypted storage and retrieval for per-function
// secrets and environment variables. Secrets are encrypted at rest using
// AES-256-GCM with a workspace-scoped encryption key.
//
// The encryption key is read from configuration at:
//
//	Functions.Secrets.EncryptionKey
//
// The key MUST be exactly 32 bytes (256 bits) for AES-256.
type SecretsManager struct {
	conf *config.Config
	log  logger.Logger
	db   *sql.DB
}

// New creates a new SecretsManager with the given configuration, logger, and
// database handle.
//
// Configuration keys:
//   - Functions.Secrets.EncryptionKey: AES-256 encryption key (must be exactly 32 bytes)
//
// The encryption key is read from config on each operation, not cached at
// construction time, to support key rotation without service restart.
func New(conf *config.Config, log logger.Logger, db *sql.DB) *SecretsManager {
	return &SecretsManager{
		conf: conf,
		log:  log.Child("functions_secrets"),
		db:   db,
	}
}

// getEncryptionKey reads the AES-256 encryption key from configuration.
// Returns an error if the key is missing or not exactly 32 bytes.
func (m *SecretsManager) getEncryptionKey() ([]byte, error) {
	key := m.conf.GetString("Functions.Secrets.EncryptionKey", "")
	if len(key) == 0 {
		return nil, fmt.Errorf("encryption key not configured: Functions.Secrets.EncryptionKey")
	}
	keyBytes := []byte(key)
	if len(keyBytes) != 32 {
		return nil, fmt.Errorf("encryption key must be exactly 32 bytes, got %d", len(keyBytes))
	}
	return keyBytes, nil
}

// encrypt encrypts plaintext using AES-256-GCM and returns hex-encoded ciphertext.
// The output format is: hex(nonce || ciphertext || tag)
// where nonce is 12 bytes (standard GCM nonce size).
//
// Each invocation generates a fresh cryptographically random nonce, ensuring that
// encrypting the same plaintext twice produces different ciphertexts.
func (m *SecretsManager) encrypt(plaintext string) (string, error) {
	key, err := m.getEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("getting encryption key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("creating AES cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("creating GCM: %w", err)
	}

	// Generate a random 12-byte nonce (standard GCM nonce size).
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}

	// Seal prepends the nonce to the ciphertext+tag output.
	ciphertext := aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)

	return hex.EncodeToString(ciphertext), nil
}

// decrypt decrypts hex-encoded AES-256-GCM ciphertext and returns the original
// plaintext. Expects the format: hex(nonce || ciphertext || tag).
//
// Returns an error for any tampered, corrupted, or truncated ciphertext —
// partial or corrupted plaintext is never returned.
func (m *SecretsManager) decrypt(hexCiphertext string) (string, error) {
	key, err := m.getEncryptionKey()
	if err != nil {
		return "", fmt.Errorf("getting encryption key: %w", err)
	}

	ciphertext, err := hex.DecodeString(hexCiphertext)
	if err != nil {
		return "", fmt.Errorf("decoding hex ciphertext: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("creating AES cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("creating GCM: %w", err)
	}

	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short: expected at least %d bytes, got %d", nonceSize, len(ciphertext))
	}

	nonce, ciphertextBody := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertextBody, nil)
	if err != nil {
		return "", fmt.Errorf("decrypting secret: %w", err)
	}

	return string(plaintext), nil
}

// Set encrypts and stores a secret for a function. If the secret already exists
// for the given function ID and key, it is updated (upsert). The value parameter
// may be an empty string — this is a valid use case for optional settings.
//
// Secret values are NEVER logged. Only function IDs and key names appear in logs.
func (m *SecretsManager) Set(ctx context.Context, functionID string, key string, value string) error {
	if functionID == "" {
		return ErrInvalidFunctionID
	}
	if key == "" {
		return ErrInvalidKey
	}

	encryptedValue, err := m.encrypt(value)
	if err != nil {
		return fmt.Errorf("encrypting secret for function %s: %w", functionID, err)
	}

	_, err = m.db.ExecContext(ctx,
		`INSERT INTO `+secretsTableName+` (function_id, key, encrypted_value)
		VALUES ($1, $2, $3)
		ON CONFLICT (function_id, key) DO UPDATE SET encrypted_value = $3`,
		functionID, key, encryptedValue,
	)
	if err != nil {
		return fmt.Errorf("setting secret for function %s: %w", functionID, err)
	}

	m.log.Debugn("secret stored",
		logger.NewStringField("function_id", functionID),
		logger.NewStringField("key", key),
	)
	return nil
}

// Get retrieves and decrypts a single secret for a function.
// Returns ErrSecretNotFound if no secret exists with the given function ID and key.
func (m *SecretsManager) Get(ctx context.Context, functionID string, key string) (string, error) {
	if functionID == "" {
		return "", ErrInvalidFunctionID
	}
	if key == "" {
		return "", ErrInvalidKey
	}

	var encryptedValue string
	err := m.db.QueryRowContext(ctx,
		`SELECT encrypted_value FROM `+secretsTableName+` WHERE function_id = $1 AND key = $2`,
		functionID, key,
	).Scan(&encryptedValue)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrSecretNotFound
		}
		return "", fmt.Errorf("getting secret for function %s key %s: %w", functionID, key, err)
	}

	plaintext, err := m.decrypt(encryptedValue)
	if err != nil {
		return "", fmt.Errorf("getting secret for function %s key %s: %w", functionID, key, err)
	}

	return plaintext, nil
}

// GetAll retrieves and decrypts all secrets for a function.
// Returns an empty (non-nil) map if no secrets exist for the function.
func (m *SecretsManager) GetAll(ctx context.Context, functionID string) (map[string]string, error) {
	if functionID == "" {
		return nil, ErrInvalidFunctionID
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT key, encrypted_value FROM `+secretsTableName+` WHERE function_id = $1`,
		functionID,
	)
	if err != nil {
		return nil, fmt.Errorf("getting all secrets for function %s: %w", functionID, err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]string)
	for rows.Next() {
		var secretKey, encryptedValue string
		if err := rows.Scan(&secretKey, &encryptedValue); err != nil {
			return nil, fmt.Errorf("scanning secret row for function %s: %w", functionID, err)
		}
		plaintext, err := m.decrypt(encryptedValue)
		if err != nil {
			return nil, fmt.Errorf("getting all secrets for function %s: decrypting key %s: %w", functionID, secretKey, err)
		}
		result[secretKey] = plaintext
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating secret rows for function %s: %w", functionID, err)
	}

	return result, nil
}

// Delete removes a single secret for a function.
// Returns ErrSecretNotFound if no secret exists with the given function ID and key.
func (m *SecretsManager) Delete(ctx context.Context, functionID string, key string) error {
	if functionID == "" {
		return ErrInvalidFunctionID
	}
	if key == "" {
		return ErrInvalidKey
	}

	result, err := m.db.ExecContext(ctx,
		`DELETE FROM `+secretsTableName+` WHERE function_id = $1 AND key = $2`,
		functionID, key,
	)
	if err != nil {
		return fmt.Errorf("deleting secret for function %s key %s: %w", functionID, key, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("getting rows affected for function %s key %s: %w", functionID, key, err)
	}
	if rowsAffected == 0 {
		return ErrSecretNotFound
	}

	m.log.Debugn("secret deleted",
		logger.NewStringField("function_id", functionID),
		logger.NewStringField("key", key),
	)
	return nil
}

// DeleteAll removes all secrets for a function.
// Does not return an error if the function has no secrets (idempotent delete).
func (m *SecretsManager) DeleteAll(ctx context.Context, functionID string) error {
	if functionID == "" {
		return ErrInvalidFunctionID
	}

	_, err := m.db.ExecContext(ctx,
		`DELETE FROM `+secretsTableName+` WHERE function_id = $1`,
		functionID,
	)
	if err != nil {
		return fmt.Errorf("deleting all secrets for function %s: %w", functionID, err)
	}

	m.log.Debugn("all secrets deleted",
		logger.NewStringField("function_id", functionID),
	)
	return nil
}
