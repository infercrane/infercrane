// Package managedbilling contains money-safe calculations for InferCrane's
// prepaid managed Model API boundary. All values are integer micro-US dollars;
// floating point never participates in authorization or settlement.
package managedbilling

import (
	"errors"
	"math"
)

const tokensPerMillion int64 = 1_000_000

func TokenCostMicrousd(inputTokens, outputTokens int, inputPrice, outputPrice int64) (int64, error) {
	if inputTokens < 0 || outputTokens < 0 || inputPrice < 0 || outputPrice < 0 {
		return 0, errors.New("tokens and prices must be non-negative")
	}
	input, err := roundedUpProduct(int64(inputTokens), inputPrice)
	if err != nil {
		return 0, err
	}
	output, err := roundedUpProduct(int64(outputTokens), outputPrice)
	if err != nil || input > int64(^uint64(0)>>1)-output {
		return 0, errors.New("token cost overflows int64")
	}
	return input + output, nil
}

// MinimumRetailPriceMicrousd returns the smallest integer retail price whose
// gross margin is at least marginBPS. It deliberately rounds up: money safety
// must never depend on a favorable truncation.
func MinimumRetailPriceMicrousd(costBasis int64, marginBPS int) (int64, error) {
	if costBasis < 0 || marginBPS < 0 || marginBPS >= 10_000 {
		return 0, errors.New("cost basis must be non-negative and gross margin must be between 0 and 9999 basis points")
	}
	if costBasis == 0 {
		return 0, nil
	}
	denominator := int64(10_000 - marginBPS)
	if costBasis > (math.MaxInt64-(denominator-1))/10_000 {
		return 0, errors.New("minimum retail price overflows int64")
	}
	return (costBasis*10_000 + denominator - 1) / denominator, nil
}

func roundedUpProduct(tokens, price int64) (int64, error) {
	if tokens == 0 || price == 0 {
		return 0, nil
	}
	if tokens > (int64(^uint64(0)>>1)-(tokensPerMillion-1))/price {
		return 0, errors.New("token cost overflows int64")
	}
	return (tokens*price + tokensPerMillion - 1) / tokensPerMillion, nil
}
