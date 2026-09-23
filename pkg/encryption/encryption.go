package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

var (
	ErrInvalidCipherText = errors.New("invalid cipher text")
	ErrInvalidKeyLength  = errors.New("invalid key length")
)

// EncryptionService handles encryption and decryption of sensitive data
type EncryptionService struct {
	key []byte
}

// NewEncryptionService creates a new encryption service
func NewEncryptionService() *EncryptionService {
	// Get encryption key from environment or generate one
	secretKey := os.Getenv("ENCRYPTION_KEY")
	if secretKey == "" {
		// Use JWT secret as fallback
		secretKey = os.Getenv("JWT_SECRET")
		if secretKey == "" {
			secretKey = "default-booking-platform-secret-key-2024" // Default for development
		}
	}

	// Create a 256-bit key from the secret
	hash := sha256.Sum256([]byte(secretKey))

	return &EncryptionService{
		key: hash[:],
	}
}

// Encrypt encrypts plaintext using AES-GCM
func (e *EncryptionService) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	// Create a new AES cipher block
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", err
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// Create a nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	// Encrypt the plaintext
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)

	// Encode to base64
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts ciphertext using AES-GCM
func (e *EncryptionService) Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}

	// Decode from base64
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}

	// Create a new AES cipher block
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", err
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// Check minimum length
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", ErrInvalidCipherText
	}

	// Extract nonce and ciphertext
	nonce, cipherData := data[:nonceSize], data[nonceSize:]

	// Decrypt
	plaintext, err := gcm.Open(nil, nonce, cipherData, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

// EncryptFields encrypts multiple fields in a map
func (e *EncryptionService) EncryptFields(fields map[string]string) (map[string]string, error) {
	encrypted := make(map[string]string)

	for key, value := range fields {
		if value != "" {
			encryptedValue, err := e.Encrypt(value)
			if err != nil {
				return nil, err
			}
			encrypted[key] = encryptedValue
		} else {
			encrypted[key] = ""
		}
	}

	return encrypted, nil
}

// DecryptFields decrypts multiple fields in a map
func (e *EncryptionService) DecryptFields(fields map[string]string) (map[string]string, error) {
	decrypted := make(map[string]string)

	for key, value := range fields {
		if value != "" {
			decryptedValue, err := e.Decrypt(value)
			if err != nil {
				return nil, err
			}
			decrypted[key] = decryptedValue
		} else {
			decrypted[key] = ""
		}
	}

	return decrypted, nil
}

// Global encryption service instance
var GlobalEncryption = NewEncryptionService()