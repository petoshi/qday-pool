// Package amount converts displayed QDAY values to exact consensus atoms.
package amount

import (
	"errors"
	"math/big"
	"strings"
)

// Parse converts a non-negative decimal QDAY amount using the exact unit
// reported by the node. QDAY changes denomination at PQ Day, so callers must
// use the current unit instead of assuming a fixed number of decimals.
func Parse(display, unitAtomic string) (*big.Int, error) {
	display = strings.TrimSpace(display)
	unit, ok := new(big.Int).SetString(unitAtomic, 10)
	if !ok || unit.Sign() <= 0 || unit.String() != unitAtomic {
		return nil, errors.New("invalid QDAY unit")
	}
	decimals := len(unitAtomic) - 1
	if unitAtomic[0] != '1' || strings.Trim(unitAtomic[1:], "0") != "" {
		return nil, errors.New("QDAY unit is not a power of ten")
	}
	if display == "" || strings.HasPrefix(display, "+") || strings.HasPrefix(display, "-") || strings.Count(display, ".") > 1 {
		return nil, errors.New("amount must be a non-negative decimal")
	}
	parts := strings.SplitN(display, ".", 2)
	if parts[0] == "" {
		return nil, errors.New("amount is not canonical")
	}
	if len(parts[0]) > 1 && parts[0][0] == '0' {
		return nil, errors.New("amount is not canonical")
	}
	for _, part := range parts {
		if part == "" {
			return nil, errors.New("amount is not canonical")
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, errors.New("amount must be a non-negative decimal")
			}
		}
	}
	whole, ok := new(big.Int).SetString(parts[0], 10)
	if !ok {
		return nil, errors.New("invalid amount")
	}
	result := new(big.Int).Mul(whole, unit)
	if len(parts) == 2 {
		if len(parts[1]) > decimals {
			return nil, errors.New("amount has more precision than the current QDAY unit")
		}
		fraction, ok := new(big.Int).SetString(parts[1]+strings.Repeat("0", decimals-len(parts[1])), 10)
		if !ok {
			return nil, errors.New("invalid amount")
		}
		result.Add(result, fraction)
	}
	return result, nil
}
