package money

import (
	"math"
)

const MicroUSDPerCent int64 = 10_000

const MicroUSDPerUSD int64 = 1_000_000

func CentsToMicroUSD(cents int64) int64 {
	if cents > math.MaxInt64/MicroUSDPerCent {
		return math.MaxInt64
	}
	if cents < math.MinInt64/MicroUSDPerCent {
		return math.MinInt64
	}
	return cents * MicroUSDPerCent
}

func MicroUSDToCents(micros int64) int64 {
	if micros == 0 {
		return 0
	}
	c := micros / MicroUSDPerCent

	if micros%MicroUSDPerCent != 0 {
		if micros < 0 {
			c--
		} else {
			c++
		}
	}
	return c
}

func MicroUSDtoDollars(micros int64) float64 {
	return float64(micros) / float64(MicroUSDPerUSD)
}

func CostMicros(priceIn, priceOut float64, promptTokens, completionTokens int) int64 {
	if priceIn == 0 && priceOut == 0 {
		return 0
	}
	usd := (float64(promptTokens)*priceIn + float64(completionTokens)*priceOut) / 1e6
	return int64(math.Round(usd * float64(MicroUSDPerUSD)))
}
