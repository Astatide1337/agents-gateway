package capture

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

const (
	framePrefix       = OutputProtocol + "\n"
	frameMagic        = "AGWCAP02"
	frameHeaderBytes  = 8 + 8 + 8 + 8 + sha256.Size + sha256.Size + sha256.Size
	frameTrailingByte = 1
)

// EncodeOutputFrame creates the bounded, line-oriented stdout protocol used
// between the capture pod and the controller. The binary payload is base64
// encoded so CRI/Kubernetes log framing cannot alter patch bytes. The final
// newline is optional to the decoder and is included for normal CLI output.
func EncodeOutputFrame(resultJSON, patch, manifest []byte) ([]byte, error) {
	if len(resultJSON) == 0 || len(resultJSON) > int(HardMaxResultBytes) || len(patch) == 0 || int64(len(patch)) > HardMaxPatchBytes || len(manifest) == 0 || int64(len(manifest)) > HardMaxManifestBytes {
		return nil, ErrInvalidFrame
	}
	inner := make([]byte, frameHeaderBytes+len(resultJSON)+len(patch)+len(manifest))
	copy(inner[:8], frameMagic)
	binary.BigEndian.PutUint64(inner[8:16], uint64(len(resultJSON)))
	binary.BigEndian.PutUint64(inner[16:24], uint64(len(patch)))
	binary.BigEndian.PutUint64(inner[24:32], uint64(len(manifest)))
	resultDigest := sha256.Sum256(resultJSON)
	patchDigest := sha256.Sum256(patch)
	manifestDigest := sha256.Sum256(manifest)
	copy(inner[32:32+sha256.Size], resultDigest[:])
	copy(inner[32+sha256.Size:32+2*sha256.Size], patchDigest[:])
	copy(inner[32+2*sha256.Size:frameHeaderBytes], manifestDigest[:])
	copy(inner[frameHeaderBytes:frameHeaderBytes+len(resultJSON)], resultJSON)
	copy(inner[frameHeaderBytes+len(resultJSON):], patch)
	copy(inner[frameHeaderBytes+len(resultJSON)+len(patch):], manifest)

	encoded := make([]byte, len(framePrefix)+base64.RawStdEncoding.EncodedLen(len(inner))+frameTrailingByte)
	copy(encoded, framePrefix)
	base64.RawStdEncoding.Encode(encoded[len(framePrefix):], inner)
	encoded[len(encoded)-1] = '\n'
	return encoded, nil
}

// DecodeOutputFrame bounds and authenticates a frame before exposing its three
// byte slices. It rejects extra data, invalid base64, length overflow, and
// digest mismatches. The limits must already be the controller's effective
// limits; hard ceilings are still enforced here.
func DecodeOutputFrame(frame []byte, maxResultBytes, maxPatchBytes, maxManifestBytes int64) (CapturedOutput, error) {
	var zero CapturedOutput
	if maxResultBytes <= 0 || maxResultBytes > HardMaxResultBytes || maxPatchBytes <= 0 || maxPatchBytes > HardMaxPatchBytes || maxManifestBytes <= 0 || maxManifestBytes > HardMaxManifestBytes {
		return zero, fmt.Errorf("%w: frame limits are invalid", ErrInvalidFrame)
	}
	if len(frame) == 0 {
		return zero, ErrInvalidFrame
	}
	if frame[len(frame)-1] == '\n' {
		frame = frame[:len(frame)-1]
	}
	if len(frame) < len(framePrefix) || !bytes.HasPrefix(frame, []byte(framePrefix)) {
		return zero, ErrInvalidFrame
	}
	encoded := frame[len(framePrefix):]
	if len(encoded) == 0 || bytes.IndexByte(encoded, '\n') >= 0 || bytes.IndexByte(encoded, '\r') >= 0 || bytes.IndexByte(encoded, ' ') >= 0 || bytes.IndexByte(encoded, '\t') >= 0 {
		return zero, ErrInvalidFrame
	}
	maxInner := frameHeaderBytes + maxResultBytes + maxPatchBytes + maxManifestBytes
	maxEncoded := int64(base64.RawStdEncoding.EncodedLen(int(maxInner)))
	if int64(len(encoded)) > maxEncoded {
		return zero, fmt.Errorf("%w: encoded frame exceeds bound", ErrInvalidFrame)
	}
	inner, err := base64.RawStdEncoding.DecodeString(string(encoded))
	if err != nil || len(inner) < frameHeaderBytes || string(inner[:8]) != frameMagic {
		return zero, ErrInvalidFrame
	}
	resultLen := binary.BigEndian.Uint64(inner[8:16])
	patchLen := binary.BigEndian.Uint64(inner[16:24])
	manifestLen := binary.BigEndian.Uint64(inner[24:32])
	if resultLen == 0 || patchLen == 0 || manifestLen == 0 || resultLen > uint64(maxResultBytes) || patchLen > uint64(maxPatchBytes) || manifestLen > uint64(maxManifestBytes) || resultLen > uint64(^uint(0)>>1) || patchLen > uint64(^uint(0)>>1) || manifestLen > uint64(^uint(0)>>1) {
		return zero, fmt.Errorf("%w: frame payload exceeds bound", ErrInvalidFrame)
	}
	if resultLen+patchLen+manifestLen > uint64(len(inner)-frameHeaderBytes) || frameHeaderBytes+int(resultLen)+int(patchLen)+int(manifestLen) != len(inner) {
		return zero, ErrInvalidFrame
	}
	resultStart := frameHeaderBytes
	resultEnd := resultStart + int(resultLen)
	patchEnd := resultEnd + int(patchLen)
	manifestEnd := patchEnd + int(manifestLen)
	resultJSON := append([]byte(nil), inner[resultStart:resultEnd]...)
	patch := append([]byte(nil), inner[resultEnd:patchEnd]...)
	manifest := append([]byte(nil), inner[patchEnd:manifestEnd]...)
	resultDigest := sha256.Sum256(resultJSON)
	patchDigest := sha256.Sum256(patch)
	manifestDigest := sha256.Sum256(manifest)
	if !bytes.Equal(inner[32:32+sha256.Size], resultDigest[:]) || !bytes.Equal(inner[32+sha256.Size:32+2*sha256.Size], patchDigest[:]) || !bytes.Equal(inner[32+2*sha256.Size:frameHeaderBytes], manifestDigest[:]) {
		return zero, ErrDigestMismatch
	}
	return CapturedOutput{ResultJSON: resultJSON, Patch: patch, Manifest: manifest}, nil
}

// MaxEncodedFrameBytes returns a safe upper bound for OutputReader. It is
// intentionally integer-only and rejects overflow by returning zero.
func MaxEncodedFrameBytes(maxResultBytes, maxPatchBytes int64, manifestLimit ...int64) int64 {
	maxManifestBytes := DefaultMaxManifestBytes
	if len(manifestLimit) > 1 {
		return 0
	}
	if len(manifestLimit) == 1 {
		maxManifestBytes = manifestLimit[0]
	}
	if maxResultBytes <= 0 || maxPatchBytes <= 0 || maxManifestBytes <= 0 || maxResultBytes > HardMaxResultBytes || maxPatchBytes > HardMaxPatchBytes || maxManifestBytes > HardMaxManifestBytes {
		return 0
	}
	inner := int64(frameHeaderBytes) + maxResultBytes + maxPatchBytes + maxManifestBytes
	if inner < 0 || inner > int64(^uint(0)>>1) {
		return 0
	}
	encoded := int64(base64.RawStdEncoding.EncodedLen(int(inner)))
	if encoded > (1<<62)-int64(len(framePrefix))-frameTrailingByte {
		return 0
	}
	return int64(len(framePrefix)) + encoded + frameTrailingByte
}
