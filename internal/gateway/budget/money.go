/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package budget implements the M8 gateway cost-budget enforcement core
// (specs/cost-governance.md). It is a pure, dependency-light library — money
// arithmetic, per-conversation/per-agent spend accounting, and the soft/hard
// threshold decision — reused by the launcher's outbound gateway proxy
// (cmd/launcher). It performs NO I/O and holds NO knowledge of HTTP: the caller
// (the proxy) reads identity/budget from request headers, asks this package for
// a decision before forwarding, and reports the actual priced cost afterwards.
package budget

import (
	"fmt"
	"math/big"
)

// Money is an EXACT decimal USD amount. It wraps *big.Rat so accumulation is
// lossless: adding many small fractional-cent costs never drifts the way
// repeated float64 addition does (0.1 + 0.2 != 0.3). A budget ceiling is a
// promise about real dollars, so the accounting must be exact — never float.
//
// The zero value is NOT usable (nil rat); construct via ParseMoney, MoneyFromRat,
// or Zero(). Money values are immutable — Add returns a new Money.
type Money struct {
	r *big.Rat
}

// Zero returns an exact $0.00.
func Zero() Money {
	return Money{r: new(big.Rat)}
}

// MoneyFromRat wraps a *big.Rat as Money. The rat is copied so the caller cannot
// mutate the Money's internal value. A nil rat yields Zero.
func MoneyFromRat(r *big.Rat) Money {
	if r == nil {
		return Zero()
	}
	return Money{r: new(big.Rat).Set(r)}
}

// ParseMoney parses an exact-decimal USD string (e.g. "0.50", "10", "1.234567")
// into Money. It accepts a plain decimal — an optional integer part, an optional
// fractional part, no sign, no exponent, no currency symbol — matching the CRD's
// budget pattern ^[0-9]+(\.[0-9]{1,6})?$. An empty string is an error (the
// caller decides whether "unset" means "no cap"); a malformed string is an error.
//
// big.Rat.SetString would also accept forms we do not want (fractions "1/3",
// signs, exponents), so we validate the surface first, then convert exactly.
func ParseMoney(s string) (Money, error) {
	if s == "" {
		return Money{}, fmt.Errorf("empty money string")
	}
	// Reject anything big.Rat would over-accept; we want a plain non-negative
	// decimal only. Validate char-by-char: digits, at most one dot.
	dots := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			// ok
		case c == '.':
			dots++
			if dots > 1 {
				return Money{}, fmt.Errorf("invalid money %q: multiple decimal points", s)
			}
		default:
			return Money{}, fmt.Errorf("invalid money %q: unexpected character %q", s, string(c))
		}
	}
	if s == "." {
		return Money{}, fmt.Errorf("invalid money %q: no digits", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Money{}, fmt.Errorf("invalid money %q", s)
	}
	return Money{r: r}, nil
}

// Add returns m + other as a new Money. Neither operand is mutated.
func (m Money) Add(other Money) Money {
	return Money{r: new(big.Rat).Add(m.rat(), other.rat())}
}

// Cmp compares m to other: -1 if m<other, 0 if equal, +1 if m>other.
func (m Money) Cmp(other Money) int {
	return m.rat().Cmp(other.rat())
}

// GreaterThan reports whether m > other.
func (m Money) GreaterThan(other Money) bool {
	return m.Cmp(other) > 0
}

// AtLeast reports whether m >= other.
func (m Money) AtLeast(other Money) bool {
	return m.Cmp(other) >= 0
}

// IsZero reports whether the amount is exactly $0.
func (m Money) IsZero() bool {
	return m.rat().Sign() == 0
}

// MulPercent returns m * pct/100 as a new Money — the soft-threshold amount for
// a cap and percentage. Exact: pct is an integer, so no rounding occurs.
func (m Money) MulPercent(pct int) Money {
	factor := new(big.Rat).SetFrac(big.NewInt(int64(pct)), big.NewInt(100))
	return Money{r: new(big.Rat).Mul(m.rat(), factor)}
}

// String renders the amount as a fixed 6-decimal-place USD string (the CRD's max
// precision). This is the canonical form stamped into the budget_exceeded
// response and span attributes so spent/cap are always directly comparable.
func (m Money) String() string {
	return m.rat().FloatString(6)
}

// rat returns the underlying *big.Rat, treating a nil (zero-value Money) as $0
// so a Money that skipped construction still behaves as zero rather than
// panicking. Internal use only.
func (m Money) rat() *big.Rat {
	if m.r == nil {
		return new(big.Rat)
	}
	return m.r
}
