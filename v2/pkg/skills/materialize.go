// Package skills resolves immutable Agent Skills packages into a sandbox
// staging directory without trusting archive paths or file types.
package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxPackageBytes  = 64 << 20
	MaxExpandedBytes = 64 << 20
	MaxFiles         = 2048
)

type Fetcher interface {
	Fetch(context.Context, string) (io.ReadCloser, error)
}

type Reference struct{ Source, Digest string }

type Limits struct {
	PackageBytes  int64
	ExpandedBytes int64
	Files         int
}

type Materializer struct {
	Fetcher Fetcher
	Limits  Limits
}

func (m Materializer) limits() Limits {
	limits := m.Limits
	if limits.PackageBytes <= 0 {
		limits.PackageBytes = MaxPackageBytes
	}
	if limits.ExpandedBytes <= 0 {
		limits.ExpandedBytes = MaxExpandedBytes
	}
	if limits.Files <= 0 {
		limits.Files = MaxFiles
	}
	return limits
}

func (m Materializer) Materialize(ctx context.Context, reference Reference, destination string) error {
	if m.Fetcher == nil {
		return errors.New("skill fetcher is required")
	}
	if reference.Source == "" || !validDigest(reference.Digest) {
		return errors.New("skill source and sha256 digest are required")
	}
	if destination == "" {
		return errors.New("destination is required")
	}
	limits := m.limits()
	reader, err := m.Fetcher.Fetch(ctx, reference.Source)
	if err != nil {
		return fmt.Errorf("fetch skill package: %w", err)
	}
	defer reader.Close()
	packageBytes, err := io.ReadAll(io.LimitReader(reader, limits.PackageBytes+1))
	if err != nil {
		return fmt.Errorf("read skill package: %w", err)
	}
	if int64(len(packageBytes)) > limits.PackageBytes {
		return errors.New("skill package exceeds size limit")
	}
	sum := sha256.Sum256(packageBytes)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != reference.Digest {
		return fmt.Errorf("skill digest mismatch: expected %s, got %s", reference.Digest, actual)
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return fmt.Errorf("create skill destination: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(destination)
		}
	}()
	archiveReader, closeArchive, err := openArchive(packageBytes)
	if err != nil {
		return err
	}
	defer closeArchive()
	files := 0
	var expandedBytes int64
	foundManifest := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := archiveReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read skill archive: %w", err)
		}
		files++
		if files > limits.Files {
			return errors.New("skill package exceeds file-count limit")
		}
		clean, err := safePath(header.Name)
		if err != nil {
			return err
		}
		if clean == "." {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if !within(destination, target) {
			return errors.New("skill archive path escaped destination")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > limits.ExpandedBytes-expandedBytes {
				return errors.New("skill package exceeds expanded-size limit")
			}
			expandedBytes += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return fmt.Errorf("create skill file: %w", err)
			}
			_, copyErr := io.CopyN(file, archiveReader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("write skill file: %w", copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
			mode := os.FileMode(0444)
			if header.FileInfo().Mode()&0111 != 0 {
				mode = 0555
			}
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
			if strings.EqualFold(filepath.Base(clean), "SKILL.md") {
				foundManifest = true
			}
		default:
			return fmt.Errorf("skill archive contains forbidden entry type %d at %q", header.Typeflag, header.Name)
		}
	}
	if !foundManifest {
		return errors.New("skill package does not contain SKILL.md")
	}
	if err := makeDirectoriesReadOnly(destination); err != nil {
		return err
	}
	ok = true
	return nil
}

func openArchive(payload []byte) (*tar.Reader, func(), error) {
	if len(payload) >= 2 && payload[0] == 0x1f && payload[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, func() {}, errors.New("invalid gzip skill package")
		}
		return tar.NewReader(gz), func() { _ = gz.Close() }, nil
	}
	return tar.NewReader(bytes.NewReader(payload)), func() {}, nil
}

func safePath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("unsafe skill archive path %q", name)
	}
	if strings.ContainsRune(clean, '\x00') {
		return "", errors.New("skill path contains NUL")
	}
	return clean, nil
}
func within(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
func makeDirectoriesReadOnly(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0555)
		}
		return nil
	})
}
