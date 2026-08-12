package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/broker"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const maxCredentialFileBytes int64 = broker.MaxCredentialBytes

type secretFileSpec struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

type fileCredentialResolver struct {
	byReference map[string]string
}

func parseSecretFileMap(body []byte) ([]secretFileSpec, error) {
	if len(body) == 0 || len(body) > maxSecretFileMapBytes || strictjson.Validate(body) != nil {
		return nil, configError{code: "broker_secret_file_map_invalid"}
	}
	var entries []secretFileSpec
	if err := decodeStrictValue(body, &entries); err != nil || len(entries) > maxProjectedFiles {
		return nil, configError{code: "broker_secret_file_map_invalid"}
	}
	seenKeys := make(map[string]struct{}, len(entries))
	seenPaths := make(map[string]struct{}, len(entries))
	for index, entry := range entries {
		if !validProjectedKey(entry.Key) || entry.Path != "item-"+strconv.Itoa(index) {
			return nil, configError{code: "broker_secret_file_map_invalid"}
		}
		if _, exists := seenKeys[entry.Key]; exists {
			return nil, configError{code: "broker_secret_file_map_invalid"}
		}
		if _, exists := seenPaths[entry.Path]; exists {
			return nil, configError{code: "broker_secret_file_map_invalid"}
		}
		seenKeys[entry.Key] = struct{}{}
		seenPaths[entry.Path] = struct{}{}
	}
	return entries, nil
}

func validateCredentialContract(toolSet v1alpha1.ToolSetSpec, route v1alpha1.ModelRouteSpec, entries []secretFileSpec, directory string) error {
	if len(entries) > 0 {
		if err := validateDirectDirectory(directory); err != nil {
			return configError{code: "credentials_directory_unreadable"}
		}
	}
	byKey := make(map[string]string, len(entries))
	for _, entry := range entries {
		byKey[entry.Key] = filepath.Join(directory, entry.Path)
	}
	refs := sortedCredentialRefs(toolSet, route)
	if len(refs) != len(entries) {
		return configError{code: "broker_secret_projection_mismatch"}
	}
	byReference := make(map[string]string, len(refs))
	for _, ref := range refs {
		key := credentialProjectedKey(ref)
		path, ok := byKey[key]
		if !ok || !validFilePath(path) {
			return configError{code: "broker_secret_projection_mismatch"}
		}
		if err := validateRegularFile(path, maxCredentialFileBytes); err != nil {
			return configError{code: "broker_secret_file_unreadable"}
		}
		byReference[ref] = path
	}
	return nil
}

func newFileCredentialResolver(toolSet v1alpha1.ToolSetSpec, route v1alpha1.ModelRouteSpec, entries []secretFileSpec, directory string) (*fileCredentialResolver, error) {
	byKey := make(map[string]string, len(entries))
	for _, entry := range entries {
		byKey[entry.Key] = filepath.Join(directory, entry.Path)
	}
	refs := sortedCredentialRefs(toolSet, route)
	if len(refs) != len(entries) {
		return nil, configError{code: "broker_secret_projection_mismatch"}
	}
	byReference := make(map[string]string, len(refs))
	for _, ref := range refs {
		path, ok := byKey[credentialProjectedKey(ref)]
		if !ok {
			return nil, configError{code: "broker_secret_projection_mismatch"}
		}
		byReference[ref] = path
	}
	return &fileCredentialResolver{byReference: byReference}, nil
}

func (r *fileCredentialResolver) ResolveToken(ctx context.Context, logicalRef string) ([]byte, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("credential unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, ok := r.byReference[logicalRef]
	if !ok {
		return nil, errors.New("credential unavailable")
	}
	value, err := readBoundedFile(path, maxCredentialFileBytes, true)
	if err != nil {
		return nil, errors.New("credential unavailable")
	}
	return value, nil
}

func validProjectedKey(value string) bool {
	if len(value) != len("broker-")+64 || !strings.HasPrefix(value, "broker-") {
		return false
	}
	for _, char := range value[len("broker-"):] {
		if !((char >= 'a' && char <= 'f') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

func validateRegularFile(path string, maxBytes int64) error {
	if !validFilePath(path) || maxBytes < 1 {
		return errors.New("invalid credential file")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxBytes {
		return errors.New("invalid credential file")
	}
	return nil
}

func validateDirectDirectory(path string) error {
	if !validAbsoluteDirectory(path) {
		return errors.New("invalid credential directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid credential directory")
	}
	return nil
}

// readBoundedFile uses O_NOFOLLOW and verifies the opened descriptor after
// opening. Kubernetes Secret volumes are intentionally not accepted if their
// item path is an atomic-writer symlink; the deployment contract must provide
// direct regular projected files or an explicit broker-only copy step.
func readBoundedFile(path string, maxBytes int64, printable bool) ([]byte, error) {
	if validateRegularFile(path, maxBytes) != nil {
		return nil, errors.New("invalid file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("file open failed")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxBytes {
		_ = file.Close()
		return nil, errors.New("file metadata invalid")
	}
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if closeErr != nil || err != nil || len(body) == 0 || int64(len(body)) > maxBytes || int64(len(body)) != info.Size() {
		wipeBytes(body)
		return nil, errors.New("file read invalid")
	}
	if printable {
		for _, value := range body {
			if value < 0x21 || value > 0x7e {
				wipeBytes(body)
				return nil, errors.New("credential bytes invalid")
			}
		}
	}
	return body, nil
}

func wipeBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
