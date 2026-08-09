// Package strictjson implements the one JSON contract used at capability,
// persistence, and replay boundaries.
//
// The parser is deliberately small and does not use encoding/json's permissive
// string or number behavior. It rejects duplicate object members recursively,
// invalid UTF-8, unpaired UTF-16 surrogates, PostgreSQL-incompatible NULs, and
// numbers outside the bounded decimal contract. Numeric comparison operates on
// a trimmed significand and a bounded base-10 exponent; it never converts to
// float64, big.Rat, or an expanded decimal string.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"unicode/utf8"
)

const (
	// MaxDocumentBytes is the maximum input accepted by the shared contract.
	MaxDocumentBytes = 1 << 20
	// MaxNumberLexemeBytes bounds one JSON number before any number storage is
	// allocated. It also prevents exponent-digit floods.
	MaxNumberLexemeBytes = 16 << 10
	// MaxMantissaDigits bounds all digits in one number's mantissa.
	MaxMantissaDigits = 8192
	// MaxAbsExponent bounds the written exponent before decimal placement.
	MaxAbsExponent = 4096
	// MaxIntegerDigits and MaxFractionDigits are conservative subsets of the
	// PostgreSQL numeric/jsonb limits.
	MaxIntegerDigits  = 12288
	MaxFractionDigits = 4096

	maxDepth         = 128
	maxObjectMembers = 1024
	maxArrayItems    = 1024
	maxStringBytes   = 64 << 10
	// Four digits represent 4096. Six permits a small amount of leading zero
	// padding while rejecting exponent digit floods before allocation.
	maxExponentDigits = 6
)

var (
	ErrInvalid      = errors.New("invalid JSON")
	ErrNotObject    = errors.New("JSON value must be an object")
	ErrDuplicateKey = errors.New("JSON object contains duplicate keys")
	ErrNumberBudget = errors.New("JSON number exceeds the strict budget")
	ErrLimit        = errors.New("JSON value exceeds the strict limit")
)

// Value is an internal parsed JSON value. It is intentionally opaque; callers
// should use Validate, Equal, and Normalize rather than depend on a decoded
// representation that could reintroduce permissive JSON behavior.
type Value struct {
	kind    valueKind
	boolean bool
	string  string
	number  decimal
	object  map[string]Value
	array   []Value
}

type valueKind uint8

const (
	nullValue valueKind = iota
	booleanValue
	stringValue
	numberValue
	objectValue
	arrayValue
)

type decimal struct {
	negative bool
	digits   string
	exponent int64
}

// Validate accepts exactly one JSON value under the shared contract.
func Validate(document []byte) error {
	_, err := parse(document)
	return err
}

// ValidateObject accepts exactly one JSON object under the shared contract.
func ValidateObject(document []byte) error {
	value, err := parse(document)
	if err != nil {
		return err
	}
	if value.kind != objectValue {
		return ErrNotObject
	}
	return nil
}

// Equal compares two JSON values structurally. Object order is ignored, array
// order is preserved, and numbers compare mathematically.
func Equal(left, right []byte) bool {
	leftValue, leftErr := parse(left)
	if leftErr != nil {
		return false
	}
	rightValue, rightErr := parse(right)
	if rightErr != nil {
		return false
	}
	return equalValue(leftValue, rightValue)
}

// EqualObjects is Equal with the additional object-only constraint used by
// ToolGrant.arguments and MCP tool calls.
func EqualObjects(expected, actual []byte) bool {
	leftValue, leftErr := parse(expected)
	if leftErr != nil || leftValue.kind != objectValue {
		return false
	}
	rightValue, rightErr := parse(actual)
	if rightErr != nil || rightValue.kind != objectValue {
		return false
	}
	return equalValue(leftValue, rightValue)
}

// Normalize returns deterministic JSON with sorted object keys, preserved
// array order, and bounded canonical number notation. Null members are kept.
func Normalize(document []byte) ([]byte, error) {
	value, err := parse(document)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeValue(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func parse(document []byte) (Value, error) {
	if len(document) == 0 || len(document) > MaxDocumentBytes || !utf8.Valid(document) {
		return Value{}, ErrInvalid
	}
	p := parser{data: document}
	p.skipSpace()
	if p.pos == len(p.data) {
		return Value{}, ErrInvalid
	}
	value, err := p.parseValue(0)
	if err != nil {
		return Value{}, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return Value{}, ErrInvalid
	}
	return value, nil
}

type parser struct {
	data []byte
	pos  int
}

func (p *parser) parseValue(depth int) (Value, error) {
	if depth > maxDepth {
		return Value{}, ErrLimit
	}
	if p.pos >= len(p.data) {
		return Value{}, ErrInvalid
	}
	switch p.data[p.pos] {
	case 'n':
		if p.consumeLiteral("null") {
			return Value{kind: nullValue}, nil
		}
	case 't':
		if p.consumeLiteral("true") {
			return Value{kind: booleanValue, boolean: true}, nil
		}
	case 'f':
		if p.consumeLiteral("false") {
			return Value{kind: booleanValue}, nil
		}
	case '"':
		value, err := p.parseString()
		if err != nil {
			return Value{}, err
		}
		return Value{kind: stringValue, string: value}, nil
	case '{':
		return p.parseObject(depth)
	case '[':
		return p.parseArray(depth)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		number, err := p.parseNumber()
		if err != nil {
			return Value{}, err
		}
		return Value{kind: numberValue, number: number}, nil
	}
	return Value{}, ErrInvalid
}

func (p *parser) parseObject(depth int) (Value, error) {
	p.pos++
	object := make(map[string]Value)
	p.skipSpace()
	if p.consume('}') {
		return Value{kind: objectValue, object: object}, nil
	}
	for {
		if len(object) >= maxObjectMembers {
			return Value{}, ErrLimit
		}
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return Value{}, ErrInvalid
		}
		key, err := p.parseString()
		if err != nil {
			return Value{}, err
		}
		if _, exists := object[key]; exists {
			return Value{}, ErrDuplicateKey
		}
		p.skipSpace()
		if !p.consume(':') {
			return Value{}, ErrInvalid
		}
		p.skipSpace()
		child, err := p.parseValue(depth + 1)
		if err != nil {
			return Value{}, err
		}
		object[key] = child
		p.skipSpace()
		if p.consume('}') {
			return Value{kind: objectValue, object: object}, nil
		}
		if !p.consume(',') {
			return Value{}, ErrInvalid
		}
		p.skipSpace()
	}
}

func (p *parser) parseArray(depth int) (Value, error) {
	p.pos++
	array := make([]Value, 0)
	p.skipSpace()
	if p.consume(']') {
		return Value{kind: arrayValue, array: array}, nil
	}
	for {
		if len(array) >= maxArrayItems {
			return Value{}, ErrLimit
		}
		child, err := p.parseValue(depth + 1)
		if err != nil {
			return Value{}, err
		}
		array = append(array, child)
		p.skipSpace()
		if p.consume(']') {
			return Value{kind: arrayValue, array: array}, nil
		}
		if !p.consume(',') {
			return Value{}, ErrInvalid
		}
		p.skipSpace()
	}
}

func (p *parser) parseString() (string, error) {
	if !p.consume('"') {
		return "", ErrInvalid
	}
	decoded := make([]byte, 0, 32)
	for p.pos < len(p.data) {
		current := p.data[p.pos]
		switch {
		case current == '"':
			p.pos++
			if len(decoded) > maxStringBytes {
				return "", ErrLimit
			}
			return string(decoded), nil
		case current == '\\':
			p.pos++
			if p.pos >= len(p.data) {
				return "", ErrInvalid
			}
			escape := p.data[p.pos]
			p.pos++
			switch escape {
			case '"', '\\', '/':
				decoded = append(decoded, escape)
			case 'b':
				decoded = append(decoded, '\b')
			case 'f':
				decoded = append(decoded, '\f')
			case 'n':
				decoded = append(decoded, '\n')
			case 'r':
				decoded = append(decoded, '\r')
			case 't':
				decoded = append(decoded, '\t')
			case 'u':
				code, err := p.parseHexEscape()
				if err != nil {
					return "", err
				}
				switch {
				case code == 0:
					return "", ErrInvalid
				case code >= 0xd800 && code <= 0xdbff:
					if p.pos+2 > len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
						return "", ErrInvalid
					}
					p.pos += 2
					low, err := p.parseHexEscape()
					if err != nil || low < 0xdc00 || low > 0xdfff {
						return "", ErrInvalid
					}
					runeValue := 0x10000 + ((code - 0xd800) << 10) + (low - 0xdc00)
					var encoded [utf8.UTFMax]byte
					size := utf8.EncodeRune(encoded[:], rune(runeValue))
					decoded = append(decoded, encoded[:size]...)
				case code >= 0xdc00 && code <= 0xdfff:
					return "", ErrInvalid
				default:
					var encoded [utf8.UTFMax]byte
					size := utf8.EncodeRune(encoded[:], rune(code))
					decoded = append(decoded, encoded[:size]...)
				}
			default:
				return "", ErrInvalid
			}
		case current < 0x20:
			return "", ErrInvalid
		case current < utf8.RuneSelf:
			decoded = append(decoded, current)
			p.pos++
		default:
			start := p.pos
			for p.pos < len(p.data) && p.data[p.pos] >= utf8.RuneSelf && p.data[p.pos] != '"' && p.data[p.pos] != '\\' {
				p.pos++
			}
			chunk := p.data[start:p.pos]
			if !utf8.Valid(chunk) {
				return "", ErrInvalid
			}
			decoded = append(decoded, chunk...)
		}
		if len(decoded) > maxStringBytes {
			return "", ErrLimit
		}
	}
	return "", ErrInvalid
}

func (p *parser) parseHexEscape() (int, error) {
	if p.pos+4 > len(p.data) {
		return 0, ErrInvalid
	}
	value := 0
	for _, digit := range p.data[p.pos : p.pos+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += int(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += int(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value += int(digit-'A') + 10
		default:
			return 0, ErrInvalid
		}
	}
	p.pos += 4
	return value, nil
}

func (p *parser) parseNumber() (decimal, error) {
	start := p.pos
	negative := p.consume('-')
	if p.pos >= len(p.data) {
		return decimal{}, ErrInvalid
	}
	if p.data[p.pos] == '0' {
		p.pos++
		if p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			return decimal{}, ErrInvalid
		}
	} else if p.data[p.pos] >= '1' && p.data[p.pos] <= '9' {
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	} else {
		return decimal{}, ErrInvalid
	}
	fractionDigits := int64(0)
	if p.consume('.') {
		fractionStart := p.pos
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
		if p.pos == fractionStart {
			return decimal{}, ErrInvalid
		}
		fractionDigits = int64(p.pos - fractionStart)
	}
	mantissaEnd := p.pos
	exponent := int64(0)
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		exponentNegative := p.consume('-')
		if !exponentNegative {
			p.consume('+')
		}
		exponentStart := p.pos
		exponentDigits := 0
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			exponentDigits++
			if exponentDigits > maxExponentDigits {
				return decimal{}, ErrNumberBudget
			}
			digit := int64(p.data[p.pos] - '0')
			if exponent > (MaxAbsExponent-digit)/10 {
				return decimal{}, ErrNumberBudget
			}
			exponent = exponent*10 + digit
			if exponent > MaxAbsExponent {
				return decimal{}, ErrNumberBudget
			}
			p.pos++
		}
		if p.pos == exponentStart {
			return decimal{}, ErrInvalid
		}
		if exponentNegative {
			exponent = -exponent
		}
	}
	if p.pos-start > MaxNumberLexemeBytes {
		return decimal{}, ErrNumberBudget
	}
	if p.pos < len(p.data) && !isValueDelimiter(p.data[p.pos]) {
		return decimal{}, ErrInvalid
	}

	mantissa := make([]byte, 0, p.pos-start)
	for index := start; index < mantissaEnd; index++ {
		if p.data[index] >= '0' && p.data[index] <= '9' {
			mantissa = append(mantissa, p.data[index])
		}
	}
	if len(mantissa) > MaxMantissaDigits {
		return decimal{}, ErrNumberBudget
	}
	first := 0
	for first < len(mantissa) && mantissa[first] == '0' {
		first++
	}
	if first == len(mantissa) {
		fractionalDigits := fractionDigits
		if exponent < 0 {
			fractionalDigits += -exponent
		}
		if fractionalDigits > MaxFractionDigits {
			return decimal{}, ErrNumberBudget
		}
		return decimal{}, nil
	}
	mantissa = mantissa[first:]
	trailingZeros := 0
	for len(mantissa)-trailingZeros > 1 && mantissa[len(mantissa)-1-trailingZeros] == '0' {
		trailingZeros++
	}
	if trailingZeros > 0 {
		mantissa = mantissa[:len(mantissa)-trailingZeros]
	}
	exponent += int64(trailingZeros) - fractionDigits
	integerDigits := int64(len(mantissa)) + exponent
	if exponent > MaxAbsExponent || exponent < -MaxAbsExponent || integerDigits > MaxIntegerDigits || exponent < -MaxFractionDigits {
		return decimal{}, ErrNumberBudget
	}
	return decimal{negative: negative, digits: string(mantissa), exponent: exponent}, nil
}

func (p *parser) consume(want byte) bool {
	if p.pos < len(p.data) && p.data[p.pos] == want {
		p.pos++
		return true
	}
	return false
}

func (p *parser) consumeLiteral(literal string) bool {
	if len(p.data)-p.pos < len(literal) || string(p.data[p.pos:p.pos+len(literal)]) != literal {
		return false
	}
	p.pos += len(literal)
	return true
}

func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func isValueDelimiter(value byte) bool {
	return value == ',' || value == ']' || value == '}' || value == ' ' || value == '\t' || value == '\n' || value == '\r'
}

func equalValue(left, right Value) bool {
	if left.kind != right.kind {
		return false
	}
	switch left.kind {
	case nullValue:
		return true
	case booleanValue:
		return left.boolean == right.boolean
	case stringValue:
		return left.string == right.string
	case numberValue:
		return compareDecimal(left.number, right.number) == 0
	case objectValue:
		if len(left.object) != len(right.object) {
			return false
		}
		for key, leftChild := range left.object {
			rightChild, exists := right.object[key]
			if !exists || !equalValue(leftChild, rightChild) {
				return false
			}
		}
		return true
	case arrayValue:
		if len(left.array) != len(right.array) {
			return false
		}
		for index := range left.array {
			if !equalValue(left.array[index], right.array[index]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func compareDecimal(left, right decimal) int {
	if left.digits == "" || right.digits == "" {
		if left.digits == right.digits {
			return 0
		}
		if left.digits == "" {
			return signedMagnitudeCompare(-1, right.negative)
		}
		return signedMagnitudeCompare(1, left.negative)
	}
	if left.negative != right.negative {
		if left.negative {
			return -1
		}
		return 1
	}
	comparison := compareMagnitude(left, right)
	if left.negative {
		return -comparison
	}
	return comparison
}

func signedMagnitudeCompare(magnitude int, negative bool) int {
	if negative {
		return -magnitude
	}
	return magnitude
}

func compareMagnitude(left, right decimal) int {
	leftPosition := int64(len(left.digits)) + left.exponent
	rightPosition := int64(len(right.digits)) + right.exponent
	if leftPosition < rightPosition {
		return -1
	}
	if leftPosition > rightPosition {
		return 1
	}
	maxDigits := len(left.digits)
	if len(right.digits) > maxDigits {
		maxDigits = len(right.digits)
	}
	for index := 0; index < maxDigits; index++ {
		leftDigit, rightDigit := byte('0'), byte('0')
		if index < len(left.digits) {
			leftDigit = left.digits[index]
		}
		if index < len(right.digits) {
			rightDigit = right.digits[index]
		}
		if leftDigit < rightDigit {
			return -1
		}
		if leftDigit > rightDigit {
			return 1
		}
	}
	return 0
}

func writeValue(output *bytes.Buffer, value Value) error {
	switch value.kind {
	case nullValue:
		output.WriteString("null")
	case booleanValue:
		if value.boolean {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case stringValue:
		encoded, err := json.Marshal(value.string)
		if err != nil {
			return ErrInvalid
		}
		output.Write(encoded)
	case numberValue:
		output.WriteString(canonicalDecimal(value.number))
	case objectValue:
		keys := make([]string, 0, len(value.object))
		for key := range value.object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			encoded, err := json.Marshal(key)
			if err != nil {
				return ErrInvalid
			}
			output.Write(encoded)
			output.WriteByte(':')
			if err := writeValue(output, value.object[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	case arrayValue:
		output.WriteByte('[')
		for index, child := range value.array {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeValue(output, child); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	default:
		return ErrInvalid
	}
	return nil
}

func canonicalDecimal(value decimal) string {
	if value.digits == "" {
		return "0"
	}
	prefix := ""
	if value.negative {
		prefix = "-"
	}
	exponent := value.exponent
	if exponent == 0 {
		return prefix + value.digits
	}
	return prefix + value.digits + "e" + strconv.FormatInt(exponent, 10)
}
