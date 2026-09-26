// Package money defines the single unit-of-account for cost inside Tallow.
//
// Per-request LLM spend is a fraction of a cent: a typical call at
// $0.50/1M input and $1.50/1M output over 1000+1000 tokens costs $0.002. When
// cost was accumulated in integer cents that value truncated to 0, so every
// cost budget, every cost total, and every cost rollup read zero regardless of
// real spend.
//
// Tallow therefore accumulates cost in micro-USD (int64) everywhere inside the
// gateway, and converts to cents only at the operator-facing boundary: the
// `cost_limit_cents` config key and the human-readable dollar displays. That
// keeps the operator's existing config meaning stable while no longer losing
// precision on the hot path.
package money

import (
	"math"
)

// MicroUSDPerCent is micro-USD in one US cent. 1 cent = $0.01 = 10,000
// micro-USD.
const MicroUSDPerCent int64 = 10_000

// MicroUSDPerUSD is micro-USD in one US dollar.
const MicroUSDPerUSD int64 = 1_000_000

// CentsToMicroUSD converts an operator-supplied cent amount (such as
// `cost_limit_cents`) into the internal micro-USD unit.
func CentsToMicroUSD(cents int64) int64 {
	return cents * MicroUSDPerCent
}

// MicroUSDToCents converts internal micro-USD into whole cents, rounding away
// from zero so a non-zero spend never displays as exactly $0.00.
func MicroUSDToCents(micros int64) int64 {
	if micros == 0 {
		return 0
	}
	c := micros / MicroUSDPerCent
	// Round away from zero on the remainder.
	if micros%MicroUSDPerCent != 0 {
		if micros < 0 {
			c--
		} else {
			c++
		}
	}
	return c
}

// MicroUSDtoDollars renders micro-USD as a float for display.
func MicroUSDtoDollars(micros int64) float64 {
	return float64(micros) / float64(MicroUSDPerUSD)
}

// CostMicros computes the cost in micro-USD of a request from manually
// declared per-1M-token pricing. A zero price on either axis means unknown
// pricing, which the project reports as zero cost.
//
// The result is rounded to the nearest micro-USD with ties away from zero, so
// accumulation never biases downward. Truncating instead would reintroduce the
// original defect at a smaller scale: 0.5 micro-USD of real spend would be
// discarded rather than rounded.
func CostMicros(priceIn, priceOut float64, promptTokens, completionTokens int) int64 {
	if priceIn == 0 && priceOut == 0 {
		return 0
	}
	usd := (float64(promptTokens)*priceIn + float64(completionTokens)*priceOut) / 1e6
	return int64(math.Round(usd * float64(MicroUSDPerUSD)))
}
