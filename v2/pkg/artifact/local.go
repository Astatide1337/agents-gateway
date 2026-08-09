package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// LocalObjectClient stores objects below one private directory. It is an
// ObjectClient implementation for standalone installations; it deliberately
// exposes artifact:// references instead of filesystem paths.
type LocalObjectClient struct {
	root string
}

// NewLocalObjectClient opens an existing, private, non-symlink directory.
// The directory is not created or chmodded implicitly: storage permissions
// are an installation concern and a typo must fail closed.
func NewLocalObjectClient(root string) (*LocalObjectClient, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("local artifact root must be absolute")
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("stat local artifact root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("local artifact root must be a directory, not a symlink")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local artifact root must not be accessible by group or other users")
	}
	return &LocalObjectClient{root: root}, nil
}

func (c *LocalObjectClient) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, mediaType, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bucket == "" || body == nil || size < 0 || mediaType == "" {
		return errors.New("invalid local artifact metadata")
	}
	if err := validateObjectKey(key); err != nil {
		return err
	}
	if !validDigest(digest) {
		return errors.New("artifact digest must be a lowercase sha256 digest")
	}

	rootFD, err := openPrivateRoot(c.root)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)

	dirFD, leaf, err := openObjectDirectory(rootFD, key, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)

	temporary, temporaryName, err := createTemporary(dirFD)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = unix.Unlinkat(dirFD, temporaryName, 0)
		}
	}()

	if err := copyAndVerify(ctx, temporary, body, size, digest); err != nil {
		return err
	}
	if err := temporary.Chmod(0400); err != nil {
		return fmt.Errorf("set local artifact permissions: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync local artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close local artifact: %w", err)
	}
	if err := unix.Linkat(dirFD, temporaryName, dirFD, leaf, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errors.New("artifact already exists")
		}
		return fmt.Errorf("commit local artifact: %w", err)
	}
	if err := unix.Fsync(dirFD); err != nil {
		_ = unix.Unlinkat(dirFD, leaf, 0)
		return fmt.Errorf("sync local artifact directory: %w", err)
	}
	if err := verifyCommitted(dirFD, leaf, size); err != nil {
		_ = unix.Unlinkat(dirFD, leaf, 0)
		return err
	}
	committed = true
	if err := unix.Unlinkat(dirFD, temporaryName, 0); err != nil {
		return fmt.Errorf("remove local artifact staging file: %w", err)
	}
	return nil
}

func (c *LocalObjectClient) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bucket == "" || strings.ContainsAny(bucket, "/\\?#") {
		return errors.New("invalid local artifact bucket")
	}
	if err := validateObjectKey(key); err != nil {
		return err
	}
	rootFD, err := openPrivateRoot(c.root)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	dirFD, leaf, err := openObjectDirectory(rootFD, key, false)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	if err := unix.Unlinkat(dirFD, leaf, 0); err != nil {
		return fmt.Errorf("remove local artifact: %w", err)
	}
	if err := unix.Fsync(dirFD); err != nil {
		return fmt.Errorf("sync local artifact deletion: %w", err)
	}
	return nil
}

func (c *LocalObjectClient) PresignGet(_ context.Context, bucket, key string, _ time.Duration) (string, error) {
	if bucket == "" || strings.ContainsAny(bucket, "/\\?#") {
		return "", errors.New("invalid local artifact bucket")
	}
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	rootFD, err := openPrivateRoot(c.root)
	if err != nil {
		return "", err
	}
	defer unix.Close(rootFD)
	dirFD, leaf, err := openObjectDirectory(rootFD, key, false)
	if err != nil {
		return "", err
	}
	defer unix.Close(dirFD)
	if err := verifyCommitted(dirFD, leaf, -1); err != nil {
		return "", err
	}
	return "artifact://" + bucket + "/" + key, nil
}

func (c *LocalObjectClient) Open(ctx context.Context, bucket, key string) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if bucket == "" || strings.ContainsAny(bucket, "/\\?#") {
		return nil, 0, errors.New("invalid local artifact bucket")
	}
	if err := validateObjectKey(key); err != nil {
		return nil, 0, err
	}
	rootFD, err := openPrivateRoot(c.root)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(rootFD)
	dirFD, leaf, err := openObjectDirectory(rootFD, key, false)
	if err != nil {
		return nil, 0, err
	}
	defer unix.Close(dirFD)
	fd, err := unix.Openat(dirFD, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open local artifact: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, 0, fmt.Errorf("stat local artifact: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Mode&0400 == 0 || stat.Size < 0 {
		_ = unix.Close(fd)
		return nil, 0, errors.New("local artifact is not an immutable private regular file")
	}
	file := os.NewFile(uintptr(fd), leaf)
	if file == nil {
		_ = unix.Close(fd)
		return nil, 0, errors.New("open local artifact: invalid descriptor")
	}
	return file, stat.Size, nil
}

func validateObjectKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00") {
		return errors.New("invalid local artifact object key")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(key)))
	if clean != key || clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || strings.HasSuffix(clean, "/..") {
		return errors.New("invalid local artifact object key")
	}
	for _, segment := range strings.Split(clean, "/") {
		if !segmentPattern.MatchString(segment) {
			return errors.New("invalid local artifact object key segment")
		}
	}
	segments := strings.Split(clean, "/")
	// Store.Put always emits at least organization/project/run/<random-id>-name.
	// Requiring that shape prevents this low-level client from becoming an
	// arbitrary filesystem writer when it is used outside Store.
	if len(segments) < 4 {
		return errors.New("local artifact object key is not store-generated")
	}
	objectName := segments[len(segments)-1]
	if len(objectName) <= 33 || objectName[32] != '-' {
		return errors.New("local artifact object key is not store-generated")
	}
	for _, character := range objectName[:32] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("local artifact object key is not store-generated")
		}
	}
	if !segmentPattern.MatchString(objectName[33:]) {
		return errors.New("local artifact object key is not store-generated")
	}
	return nil
}

func validDigest(digest string) bool {
	if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	for _, character := range strings.TrimPrefix(digest, "sha256:") {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func openPrivateRoot(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open local artifact root: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("stat local artifact root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0077 != 0 {
		unix.Close(fd)
		return -1, errors.New("local artifact root is not a private directory")
	}
	return fd, nil
}

func openObjectDirectory(rootFD int, key string, create bool) (int, string, error) {
	segments := strings.Split(key, "/")
	leaf := segments[len(segments)-1]
	if len(segments) == 1 {
		fd, err := unix.Dup(rootFD)
		if err != nil {
			return -1, "", fmt.Errorf("duplicate local artifact root: %w", err)
		}
		return fd, leaf, nil
	}
	current := rootFD
	owned := false
	closeCurrent := func() {
		if owned {
			_ = unix.Close(current)
		}
	}
	for _, segment := range segments[:len(segments)-1] {
		fd, err := unix.Openat(current, segment, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(current, segment, 0700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				closeCurrent()
				return -1, "", fmt.Errorf("create local artifact directory: %w", mkdirErr)
			}
			fd, err = unix.Openat(current, segment, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			closeCurrent()
			return -1, "", fmt.Errorf("open local artifact directory: %w", err)
		}
		if owned {
			_ = unix.Close(current)
		}
		current, owned = fd, true
	}
	return current, leaf, nil
}

func createTemporary(dirFD int) (*os.File, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, "", fmt.Errorf("generate local artifact staging name: %w", err)
		}
		name := ".agw-" + hex.EncodeToString(random)
		fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0400)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create local artifact staging file: %w", err)
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(dirFD, name, 0)
			return nil, "", errors.New("create local artifact staging file: invalid descriptor")
		}
		return file, name, nil
	}
	return nil, "", errors.New("could not allocate local artifact staging file")
}

func copyAndVerify(ctx context.Context, destination *os.File, source io.Reader, expectedSize int64, expectedDigest string) error {
	hash := sha256.New()
	limited := io.LimitReader(contextReader{ctx: ctx, reader: source}, expectedSize+1)
	written, err := io.Copy(destination, io.TeeReader(limited, hash))
	if err != nil {
		return fmt.Errorf("write local artifact: %w", err)
	}
	if written != expectedSize {
		return fmt.Errorf("artifact size mismatch: got %d, want %d", written, expectedSize)
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != expectedDigest {
		return errors.New("artifact digest mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func verifyCommitted(dirFD int, leaf string, expectedSize int64) error {
	fd, err := unix.Openat(dirFD, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open local artifact: %w", err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat local artifact: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Mode&0400 == 0 {
		return errors.New("local artifact is not an immutable private regular file")
	}
	if expectedSize >= 0 && stat.Size != expectedSize {
		return errors.New("local artifact size verification failed")
	}
	return nil
}
