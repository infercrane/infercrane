package acceleratorworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileReceiptStore persists completed operation receipts on the worker host.
// It deliberately stores only request digests and immutable responses, never
// provider credentials or request bodies.
type FileReceiptStore struct {
	directory string
	mu        sync.Mutex
}

func NewFileReceiptStore(directory string) (*FileReceiptStore, error) {
	directory = filepath.Clean(directory)
	if !filepath.IsAbs(directory) {
		return nil, errors.New("receipt directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create receipt directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect receipt directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&0o077 != 0 {
		return nil, errors.New("receipt directory must be an owner-only directory")
	}
	return &FileReceiptStore{directory: directory}, nil
}

func (s *FileReceiptStore) Get(_ context.Context, key string) (Receipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(key)
}

func (s *FileReceiptStore) Put(_ context.Context, key string, receipt Receipt) error {
	if key == "" || receipt.RequestDigest == "" || len(receipt.Response) == 0 {
		return errors.New("receipt key, request digest, and response are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, found, err := s.read(key); err != nil {
		return err
	} else if found {
		if current.RequestDigest != receipt.RequestDigest {
			return errors.New("idempotency key was already used for a different request")
		}
		return nil
	}
	encoded, err := json.Marshal(Receipt{
		RequestDigest: receipt.RequestDigest,
		Response:      append(json.RawMessage(nil), receipt.Response...),
	})
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	temporary, err := os.CreateTemp(s.directory, ".receipt-*")
	if err != nil {
		return fmt.Errorf("create receipt: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(encoded)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write receipt: %w", err)
	}
	// A hard link is an atomic create-without-replace operation. If another
	// worker process committed the key first, verify that receipt rather than
	// overwriting it.
	finalName := s.path(key)
	if err = os.Link(temporaryName, finalName); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("commit receipt: %w", err)
		}
		current, found, readErr := s.read(key)
		if readErr != nil || !found {
			return fmt.Errorf("read concurrently committed receipt: %w", readErr)
		}
		if current.RequestDigest != receipt.RequestDigest {
			return errors.New("idempotency key was already used for a different request")
		}
		return nil
	}
	if directory, openErr := os.Open(s.directory); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (s *FileReceiptStore) read(key string) (Receipt, bool, error) {
	file, err := os.Open(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("open receipt: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxRequestBytes*2 {
		return Receipt{}, false, errors.New("receipt is not a bounded owner-only regular file")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxRequestBytes*2+1))
	decoder.DisallowUnknownFields()
	var receipt Receipt
	if err = decoder.Decode(&receipt); err != nil {
		return Receipt{}, false, fmt.Errorf("decode receipt: %w", err)
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Receipt{}, false, errors.New("receipt contains trailing JSON")
	}
	if receipt.RequestDigest == "" || len(receipt.Response) == 0 || !json.Valid(receipt.Response) {
		return Receipt{}, false, errors.New("receipt is incomplete")
	}
	receipt.Response = append(json.RawMessage(nil), receipt.Response...)
	return receipt, true, nil
}

func (s *FileReceiptStore) path(key string) string {
	digest := sha256.Sum256([]byte(key))
	return filepath.Join(s.directory, hex.EncodeToString(digest[:])+".json")
}
