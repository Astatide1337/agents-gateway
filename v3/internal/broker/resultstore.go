package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// NewFilesystemResultStore creates an immutable result store rooted at an
// operator-authorized directory. The root is opened once and all subsequent
// paths are resolved relative to that descriptor with O_NOFOLLOW. It is
// intentionally not a general workspace writer: callers can only address
// bounded, broker-generated relative keys.
//
// The returned store exposes logical artifact:// references. It never returns
// the host filesystem path to an agent or to an HTTP response.
func NewFilesystemResultStore(root string, maxBytes int64) (ArtifactStore, error) {
	if !validResultRoot(root) || maxBytes < 1 {
		return nil, ErrInvalidConfig
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o002 != 0 {
		return nil, ErrInvalidConfig
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return nil, ErrInvalidConfig
	}
	return &filesystemResultStore{root: root, rootFD: fd, maxBytes: maxBytes}, nil
}

type filesystemResultStore struct {
	root     string
	rootFD   int
	maxBytes int64
	mu       sync.Mutex
}

func (s *filesystemResultStore) Put(ctx context.Context, key string, body []byte, contentType string) (bool, string, error) {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if s == nil || ctx == nil || ctx.Err() != nil || !validResultKey(key) || len(body) == 0 || int64(len(body)) > s.maxBytes || !validResultContentType(contentType) {
		return false, "", ErrToolResultUnavailable
	}
	if err := ctx.Err(); err != nil {
		return false, "", err
	}
	directory, filename, err := s.openParent(key, true)
	if err != nil {
		return false, "", ErrToolResultUnavailable
	}
	defer unix.Close(directory)

	fd, err := unix.Openat(directory, filename, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if errors.Is(err, unix.EEXIST) {
		existing, readErr := s.readAt(directory, filename)
		if readErr != nil {
			return false, "", ErrToolResultUnavailable
		}
		if !bytes.Equal(existing, body) {
			return false, "", ErrArtifactConflict
		}
		return false, logicalResultURI(key), nil
	}
	if err != nil {
		return false, "", ErrToolResultUnavailable
	}
	file := os.NewFile(uintptr(fd), filename)
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(directory, filename, 0)
		return false, "", ErrToolResultUnavailable
	}
	writeErr := writeBoundedFile(file, body, s.maxBytes)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = unix.Unlinkat(directory, filename, 0)
		return false, "", ErrToolResultUnavailable
	}
	return true, logicalResultURI(key), nil
}

func (s *filesystemResultStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if s == nil || ctx == nil || ctx.Err() != nil || !validResultKey(key) {
		return nil, ErrToolResultUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, filename, err := s.openParent(key, false)
	if err != nil {
		return nil, ErrToolResultUnavailable
	}
	defer unix.Close(directory)
	return s.readAt(directory, filename)
}

func (s *filesystemResultStore) openParent(key string, create bool) (int, string, error) {
	parts := strings.Split(key, "/")
	if len(parts) < 2 || !validResultKey(key) {
		return -1, "", ErrInvalidRequest
	}
	current := s.rootFD
	owned := false
	for _, part := range parts[:len(parts)-1] {
		fd, err := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(current, part, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				if owned {
					_ = unix.Close(current)
				}
				return -1, "", mkdirErr
			}
			fd, err = unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if owned {
			_ = unix.Close(current)
		}
		if err != nil {
			return -1, "", err
		}
		current = fd
		owned = true
	}
	return current, parts[len(parts)-1], nil
}

func (s *filesystemResultStore) readAt(directory int, filename string) ([]byte, error) {
	fd, err := unix.Openat(directory, filename, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filename)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("invalid result file descriptor")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > s.maxBytes {
		return nil, ErrToolResultUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(file, s.maxBytes+1))
	if err != nil || int64(len(body)) > s.maxBytes {
		return nil, ErrToolResultUnavailable
	}
	return body, nil
}

func writeBoundedFile(file *os.File, body []byte, maxBytes int64) error {
	if file == nil || int64(len(body)) > maxBytes {
		return ErrToolResultUnavailable
	}
	for len(body) > 0 {
		written, err := file.Write(body)
		if err != nil || written <= 0 {
			return ErrToolResultUnavailable
		}
		body = body[written:]
	}
	if err := file.Sync(); err != nil {
		return ErrToolResultUnavailable
	}
	return nil
}

func validResultRoot(root string) bool {
	return root != "" && len(root) <= 4096 && utf8.ValidString(root) && filepath.IsAbs(root) && filepath.Clean(root) == root && root != "/" && !strings.HasSuffix(root, string(filepath.Separator)) && !strings.ContainsAny(root, "\x00\r\n")
}

func validResultKey(key string) bool {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || strings.ContainsAny(key, "\\\x00\r\n") {
		return false
	}
	parts := strings.Split(key, "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, char := range part {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char)) {
				return false
			}
		}
	}
	return true
}

func validResultContentType(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func logicalResultURI(key string) string {
	base := filepath.Base(key)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return "artifact://agw/" + base
}
