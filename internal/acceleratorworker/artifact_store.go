package acceleratorworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
)

type ArtifactStore interface {
	Put(context.Context, string, io.Reader, int64) (acceleratorlab.Artifact, error)
	Open(context.Context, string) (io.ReadCloser, int64, error)
}

type LocalArtifactStore struct {
	directory string
	baseURL   string
	maxBytes  int64
}

func NewLocalArtifactStore(directory, publicBaseURL string, maxBytes int64) (*LocalArtifactStore, error) {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if !filepath.IsAbs(directory) || maxBytes < 1 || maxBytes > 4<<30 {
		return nil, errors.New("artifact store requires an absolute directory and a 1 byte to 4 GiB size limit")
	}
	base, err := url.Parse(strings.TrimSpace(publicBaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("artifact public URL must be an absolute origin")
	}
	loopback := strings.EqualFold(base.Hostname(), "localhost") || net.ParseIP(base.Hostname()) != nil && net.ParseIP(base.Hostname()).IsLoopback()
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback) {
		return nil, errors.New("artifact public URL must use HTTPS except on loopback")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	if err = os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("artifact directory must be an owner-only directory")
	}
	return &LocalArtifactStore{directory: directory, baseURL: base.String(), maxBytes: maxBytes}, nil
}

func (s *LocalArtifactStore) Put(ctx context.Context, kind string, reader io.Reader, declaredSize int64) (acceleratorlab.Artifact, error) {
	if strings.TrimSpace(kind) == "" || declaredSize < 1 || declaredSize > s.maxBytes {
		return acceleratorlab.Artifact{}, errors.New("artifact kind or declared size is invalid")
	}
	temporary, err := os.CreateTemp(s.directory, ".artifact-*")
	if err != nil {
		return acceleratorlab.Artifact{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err = temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return acceleratorlab.Artifact{}, err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), &contextReader{ctx: ctx, reader: io.LimitReader(reader, s.maxBytes+1)})
	if err == nil && written != declaredSize {
		err = fmt.Errorf("artifact size mismatch: declared %d observed %d", declaredSize, written)
	}
	if err == nil && written > s.maxBytes {
		err = errors.New("artifact exceeds size limit")
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return acceleratorlab.Artifact{}, err
	}
	hexDigest := hex.EncodeToString(hash.Sum(nil))
	finalName := filepath.Join(s.directory, hexDigest)
	if err = os.Link(temporaryName, finalName); err != nil && !errors.Is(err, os.ErrExist) {
		return acceleratorlab.Artifact{}, fmt.Errorf("commit artifact: %w", err)
	}
	if directory, openErr := os.Open(s.directory); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return acceleratorlab.Artifact{
		Kind: kind, URI: s.baseURL + "/v1/artifacts/sha256/" + hexDigest,
		SHA256: "sha256:" + hexDigest, Size: written,
	}, nil
}

func (s *LocalArtifactStore) Open(_ context.Context, digest string) (io.ReadCloser, int64, error) {
	digest = strings.ToLower(strings.TrimSpace(digest))
	if len(digest) != 64 {
		return nil, 0, os.ErrNotExist
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, 0, os.ErrNotExist
	}
	file, err := os.Open(filepath.Join(s.directory, digest))
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() < 1 || info.Size() > s.maxBytes {
		file.Close()
		return nil, 0, errors.New("artifact is not a bounded owner-only regular file")
	}
	return file, info.Size(), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(value)
}
