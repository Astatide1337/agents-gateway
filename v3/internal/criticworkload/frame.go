package criticworkload

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
)

// MaxEncodedFrameBytes returns the exact upper bound accepted by the stdout
// reader for one canonical CorroborationInput frame.
func MaxEncodedFrameBytes(maxDecoded int64) (int64, error) {
	if maxDecoded <= 0 || maxDecoded > MaxOutputBytes {
		return 0, ErrOutputOversized
	}
	return int64(len(OutputProtocol) + base64.RawStdEncoding.EncodedLen(int(maxDecoded)) + 1), nil
}

// EncodeOutputFrame validates and wraps only canonical CorroborationInput
// bytes. A caller cannot use this helper to emit a result, score, or verdict.
func EncodeOutputFrame(input []byte) ([]byte, error) {
	if len(input) == 0 || len(input) > MaxOutputBytes {
		return nil, ErrOutputOversized
	}
	parsed, err := findingcorroboration.ParseCanonicalInput(input)
	if err != nil {
		return nil, fmt.Errorf("%w: corroboration input: %v", ErrOutputMalformed, err)
	}
	canonicalInput, err := findingcorroboration.CanonicalInputBytes(parsed)
	if err != nil || !bytes.Equal(canonicalInput, input) {
		return nil, ErrOutputMalformed
	}
	frame := OutputProtocol + base64.RawStdEncoding.EncodeToString(input) + "\n"
	if len(frame) > MaxFrameBytes {
		return nil, ErrOutputOversized
	}
	return []byte(frame), nil
}

// DecodeOutputFrame extracts the exact canonical CorroborationInput. The
// caller must authenticate the producing Job/Pod before calling this function.
func DecodeOutputFrame(frame []byte, maxDecoded int64) ([]byte, error) {
	maxFrame, err := MaxEncodedFrameBytes(maxDecoded)
	if err != nil || len(frame) == 0 || int64(len(frame)) > maxFrame {
		return nil, ErrOutputOversized
	}
	if !bytes.HasSuffix(frame, []byte("\n")) {
		return nil, ErrOutputMalformed
	}
	body := frame[:len(frame)-1]
	if bytes.ContainsAny(body, "\r\n") || !bytes.HasPrefix(body, []byte(OutputProtocol)) {
		return nil, ErrOutputMalformed
	}
	encoded := strings.TrimPrefix(string(body), OutputProtocol)
	if encoded == "" || strings.Contains(encoded, "=") {
		return nil, ErrOutputMalformed
	}
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || int64(len(decoded)) > maxDecoded {
		return nil, ErrOutputMalformed
	}
	parsed, err := findingcorroboration.ParseCanonicalInput(decoded)
	if err != nil {
		return nil, fmt.Errorf("%w: corroboration input: %v", ErrOutputMalformed, err)
	}
	canonicalInput, err := findingcorroboration.CanonicalInputBytes(parsed)
	if err != nil || !bytes.Equal(canonicalInput, decoded) {
		return nil, ErrOutputMalformed
	}
	return append([]byte(nil), decoded...), nil
}
