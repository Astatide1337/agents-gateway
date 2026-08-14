package verifier

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

const (
	maxTreeFiles      = 100_000
	maxFileBytes      = 256 << 20
	maxScanBytes      = 1 << 30
	maxPatchLineBytes = 1 << 20
)

// Analysis is derived from the immutable pristine base, the applied repo, and
// the exact patch artifact. The patch digest is calculated from bytes on disk,
// never from a caller-provided claim.
type Analysis struct {
	PatchDigest    string
	ChangedPaths   []string
	FilesChanged   int64
	LinesChanged   int64
	HasBinaryFiles bool
}

type treeEntry struct {
	Path string
	Mode fs.FileMode
	Size int64
}

type inspectedFile struct {
	Digest string
	Binary bool
	Bytes  int64
}

// Analyze independently reconstructs the bounded Gate observations. It
// rejects symlinks and special files anywhere under either tree, including
// unchanged files, because a verifier must not be tricked into following a
// path outside the repository while it later runs trusted Gate commands.
func Analyze(repoPath, basePath, patchPath string, maxPatchBytes int64) (Analysis, error) {
	var output Analysis
	if maxPatchBytes <= 0 || maxPatchBytes > HardMaxPatchBytes {
		return output, fmt.Errorf("%w: patch limit is invalid", ErrPatchLimit)
	}
	repo, err := scanTree(repoPath)
	if err != nil {
		return output, fmt.Errorf("%w: scan applied repository: %v", ErrAnalysis, err)
	}
	base, err := scanTree(basePath)
	if err != nil {
		return output, fmt.Errorf("%w: scan pristine base: %v", ErrAnalysis, err)
	}
	patch, err := readBoundedRegular(patchPath, maxPatchBytes)
	if err != nil {
		return output, fmt.Errorf("%w: read patch: %v", ErrPatchLimit, err)
	}
	output.PatchDigest = digestBytes(patch)
	var scanned int64
	repoContent, err := inspectTree(repoPath, repo, &scanned)
	if err != nil {
		return output, fmt.Errorf("%w: inspect applied repository: %v", ErrAnalysis, err)
	}
	baseContent, err := inspectTree(basePath, base, &scanned)
	if err != nil {
		return output, fmt.Errorf("%w: inspect pristine base: %v", ErrAnalysis, err)
	}
	changed := changedTreePaths(repo, base, repoContent, baseContent)
	if len(changed) > gate.MaxChangedPaths {
		return output, fmt.Errorf("%w: changed path limit exceeded", ErrAnalysis)
	}
	output.ChangedPaths = changed
	output.FilesChanged = int64(len(changed))

	for _, path := range changed {
		if _, leftOK := base[path]; leftOK {
			output.HasBinaryFiles = output.HasBinaryFiles || baseContent[path].Binary
		}
		if _, rightOK := repo[path]; rightOK {
			output.HasBinaryFiles = output.HasBinaryFiles || repoContent[path].Binary
		}
	}

	lineCount, patchPaths, patchBinary, err := analyzePatch(patch)
	if err != nil {
		return output, fmt.Errorf("%w: analyze patch: %v", ErrAnalysis, err)
	}
	if !equalStrings(output.ChangedPaths, patchPaths) {
		return output, fmt.Errorf("%w: patch paths do not match the applied tree", ErrAnalysis)
	}
	output.LinesChanged = lineCount
	output.HasBinaryFiles = output.HasBinaryFiles || patchBinary
	return output, nil
}

func scanTree(root string) (map[string]treeEntry, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("root is not a regular directory")
	}
	entries := make(map[string]treeEntry)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if name == ".git" || strings.HasPrefix(name, ".git/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, name)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("special file at %s", name)
		}
		if !gate.ValidateRepoPath(name) {
			return fmt.Errorf("%w: %s", ErrUnsafePath, name)
		}
		if len(entries) >= maxTreeFiles {
			return errors.New("tree file count exceeds bound")
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if fileInfo.Size() < 0 || fileInfo.Size() > maxFileBytes {
			return fmt.Errorf("file size exceeds bound: %s", name)
		}
		entries[name] = treeEntry{Path: name, Mode: fileInfo.Mode(), Size: fileInfo.Size()}
		return nil
	})
	return entries, err
}

func inspectTree(root string, entries map[string]treeEntry, scanned *int64) (map[string]inspectedFile, error) {
	output := make(map[string]inspectedFile, len(entries))
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		inspected, err := inspectRegular(filepath.Join(root, filepath.FromSlash(path)), scanned)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		output[path] = inspected
	}
	return output, nil
}

func changedTreePaths(repo, base map[string]treeEntry, repoContent, baseContent map[string]inspectedFile) []string {
	set := make(map[string]struct{}, len(repo)+len(base))
	for path, entry := range repo {
		other, ok := base[path]
		if !ok || entry.Size != other.Size || gitExecutableBits(entry.Mode) != gitExecutableBits(other.Mode) || repoContent[path].Digest != baseContent[path].Digest {
			set[path] = struct{}{}
		}
	}
	for path, entry := range base {
		other, ok := repo[path]
		if !ok || entry.Size != other.Size || gitExecutableBits(entry.Mode) != gitExecutableBits(other.Mode) || repoContent[path].Digest != baseContent[path].Digest {
			set[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Git records the executable bit for regular files, not the writable bits.
// The fetch hand-off intentionally makes the applied tree writable and the
// pristine tree read-only, so comparing full filesystem permissions would
// misclassify every unchanged file as part of the patch.
func gitExecutableBits(mode fs.FileMode) fs.FileMode {
	return mode.Perm() & 0111
}

func inspectRegular(path string, scanned *int64) (inspectedFile, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return inspectedFile{}, ErrSymlink
	}
	if info.Size() < 0 || info.Size() > maxFileBytes {
		return inspectedFile{}, errors.New("file size exceeds bound")
	}
	if scanned == nil || info.Size() > maxScanBytes-*scanned {
		return inspectedFile{}, errors.New("total scan size exceeds bound")
	}
	*scanned += info.Size()
	file, err := os.Open(path)
	if err != nil {
		return inspectedFile{}, err
	}
	defer file.Close()
	hash := sha256.New()
	var total int64
	var binary bool
	var pending []byte
	buffer := make([]byte, 32<<10)
	for {
		read, readErr := file.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			total += int64(read)
			_, _ = hash.Write(chunk)
			if bytes.IndexByte(chunk, 0) >= 0 {
				binary = true
			}
			combined := make([]byte, 0, len(pending)+len(chunk))
			combined = append(combined, pending...)
			combined = append(combined, chunk...)
			pending = pending[:0]
			for len(combined) > 0 {
				runeValue, size := utf8.DecodeRune(combined)
				if runeValue == utf8.RuneError && size == 1 {
					if !utf8.FullRune(combined) {
						pending = append(pending[:0], combined...)
						combined = nil
						break
					}
					binary = true
					combined = combined[1:]
					continue
				}
				combined = combined[size:]
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return inspectedFile{}, readErr
		}
	}
	if !utf8.Valid(pending) {
		binary = true
	}
	return inspectedFile{Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), Binary: binary, Bytes: total}, nil
}

func readBoundedRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrSymlink
	}
	if info.Size() < 0 || info.Size() > limit {
		return nil, ErrPatchLimit
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrPatchLimit
	}
	return body, nil
}

func analyzePatch(patch []byte) (int64, []string, bool, error) {
	if len(patch) == 0 {
		return 0, []string{}, false, nil
	}
	if !utf8.Valid(patch) {
		return 0, nil, false, errors.New("patch is not valid UTF-8")
	}
	reader := bufio.NewReader(bytes.NewReader(patch))
	paths := make(map[string]struct{})
	var linesChanged int64
	hasBinary := false
	headerCount := 0
	for {
		line, err := reader.ReadString('\n')
		if len(line) > maxPatchLineBytes {
			return 0, nil, false, errors.New("patch line exceeds bound")
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "diff --git ") {
			headerCount++
			headerPaths, parseErr := parseDiffHeader(line)
			if parseErr != nil {
				return 0, nil, false, parseErr
			}
			for _, path := range headerPaths {
				if _, exists := paths[path]; exists {
					return 0, nil, false, fmt.Errorf("duplicate patch path %q", path)
				}
				paths[path] = struct{}{}
			}
		}
		if strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ") {
			hasBinary = true
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			linesChanged++
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- ") {
			linesChanged++
		}
		if linesChanged > gate.MaxObservedLines {
			return 0, nil, false, errors.New("patch line count exceeds bound")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, nil, false, err
		}
	}
	if headerCount == 0 {
		return 0, nil, false, errors.New("non-empty patch has no diff headers")
	}
	output := make([]string, 0, len(paths))
	for path := range paths {
		output = append(output, path)
	}
	sort.Strings(output)
	return linesChanged, output, hasBinary, nil
}

func parseDiffHeader(line string) ([]string, error) {
	rest := strings.TrimPrefix(line, "diff --git ")
	if rest == line || rest == "" {
		return nil, errors.New("missing diff header paths")
	}
	var left, right string
	if strings.HasPrefix(rest, "\"") {
		var consumed int
		var err error
		left, consumed, err = parseGitToken(rest)
		if err != nil {
			return nil, err
		}
		rest = rest[consumed:]
		if len(rest) == 0 || rest[0] != ' ' {
			return nil, errors.New("diff header has no second path")
		}
		right, consumed, err = parseGitToken(rest[1:])
		if err != nil || consumed != len(rest)-1 {
			return nil, errors.New("diff header has trailing path data")
		}
	} else {
		for index := 0; index < len(rest); index++ {
			if rest[index] != ' ' || index+1 >= len(rest) || !strings.HasPrefix(rest[index+1:], "b/") {
				continue
			}
			candidateLeft := rest[:index]
			candidateRight := rest[index+1:]
			if strings.HasPrefix(candidateLeft, "a/") && strings.HasPrefix(candidateRight, "b/") {
				left, right = candidateLeft, candidateRight
				break
			}
		}
		if left == "" || right == "" {
			return nil, errors.New("diff header paths are not parseable")
		}
	}
	left = strings.TrimPrefix(left, "a/")
	right = strings.TrimPrefix(right, "b/")
	if !gate.ValidateRepoPath(left) || !gate.ValidateRepoPath(right) {
		return nil, ErrUnsafePath
	}
	if left == right {
		return []string{left}, nil
	}
	return []string{left, right}, nil
}

func parseGitToken(value string) (string, int, error) {
	if value == "" || value[0] != '"' {
		return "", 0, errors.New("quoted git path is missing")
	}
	for index := 1; index < len(value); index++ {
		if value[index] != '"' || value[index-1] == '\\' {
			continue
		}
		decoded, err := strconv.Unquote(value[:index+1])
		if err != nil {
			return "", 0, err
		}
		return decoded, index + 1, nil
	}
	return "", 0, errors.New("unterminated quoted git path")
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
