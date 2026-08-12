package gate

import (
	"math/big"
	"regexp"
	"strings"
)

var decimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

type coverageOperator string

const (
	coverageGreaterEqual coverageOperator = ">="
	coverageGreater      coverageOperator = ">"
	coverageEqual        coverageOperator = "=="
	coverageLessEqual    coverageOperator = "<="
	coverageLess         coverageOperator = "<"
)

func coverageSatisfies(requirement, observed string) bool {
	operator, expected, ok := parseCoverageRequirement(requirement)
	if !ok {
		return false
	}
	actual, ok := parseDecimal(observed)
	if !ok {
		return false
	}
	comparison := actual.Cmp(expected)
	switch operator {
	case coverageGreaterEqual:
		return comparison >= 0
	case coverageGreater:
		return comparison > 0
	case coverageEqual:
		return comparison == 0
	case coverageLessEqual:
		return comparison <= 0
	case coverageLess:
		return comparison < 0
	default:
		return false
	}
}

func validCoverageRequirement(value string) bool {
	_, _, ok := parseCoverageRequirement(value)
	return ok
}

func parseCoverageRequirement(value string) (coverageOperator, *big.Rat, bool) {
	if len(value) == 0 || len(value) > 64 {
		return "", nil, false
	}
	value = strings.TrimSpace(value)
	operators := []struct {
		prefix string
		value  coverageOperator
	}{
		{">=", coverageGreaterEqual},
		{"<=", coverageLessEqual},
		{"==", coverageEqual},
		{">", coverageGreater},
		{"<", coverageLess},
	}
	for _, item := range operators {
		if strings.HasPrefix(value, item.prefix) {
			number := strings.TrimSpace(strings.TrimPrefix(value, item.prefix))
			parsed, ok := parseDecimal(number)
			if !ok {
				return "", nil, false
			}
			return item.value, parsed, true
		}
	}
	return "", nil, false
}

func parseDecimal(value string) (*big.Rat, bool) {
	if len(value) == 0 || len(value) > 64 || !decimalPattern.MatchString(value) {
		return nil, false
	}
	ratio, ok := new(big.Rat).SetString(value)
	return ratio, ok && ratio != nil
}

// normalizeDecimal canonicalizes a bounded decimal without introducing
// floating-point rounding. It is used in signed evidence reports.
func normalizeDecimal(value string) (string, bool) {
	if _, ok := parseDecimal(value); !ok {
		return "", false
	}
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}
	parts := strings.SplitN(value, ".", 2)
	integer := strings.TrimLeft(parts[0], "0")
	if integer == "" {
		integer = "0"
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = strings.TrimRight(parts[1], "0")
	}
	if fraction == "" {
		if integer == "0" {
			return "0", true
		}
		if negative {
			return "-" + integer, true
		}
		return integer, true
	}
	result := integer + "." + fraction
	if negative && result != "0" {
		result = "-" + result
	}
	return result, true
}
