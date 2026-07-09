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

package budget

import (
	"encoding/json"
	"math/big"
)

// LiteLLMCostHeader is the response header the LiteLLM gateway sets with the USD
// cost it computed for a completion (its own pricing — the M3 cost-span source).
// When present and parseable we PREFER it: pricing a real provider call is
// LiteLLM's job, and reusing its number keeps the budget's spend identical to
// the cost shown in the trace. It is absent/zero for the deterministic mock
// (mock_response short-circuits the provider), so the token-table fallback below
// makes the mock trip reproducibly.
const LiteLLMCostHeader = "x-litellm-response-cost"

// usagePricePerToken is the deterministic fallback price applied to the total
// token count from a completion's usage block when LiteLLM reports no cost
// (mock, or a provider LiteLLM can't price). 0.000001 USD/token = $1 per million
// tokens — a plausible small-model price that keeps mock arithmetic clean:
// usage.total_tokens × $0.000001. Mock pricing is deterministic (LiteLLM returns
// a fixed usage for mock_response), so a fixed price makes the e2e cross soft
// then hard at a predictable call count.
//
// Expressed as an exact rational (1/1_000_000) so accumulation never drifts.
var usagePricePerToken = new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(1_000_000))

// chatUsage is the OpenAI-compatible usage block returned by the gateway. Only
// the token counts are read; the platform never parses the message payload.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatResponseEnvelope is the minimal shape needed to price a completion: its
// usage. Everything else in the response is passed through untouched.
type chatResponseEnvelope struct {
	Usage *chatUsage `json:"usage"`
}

// PriceCall computes the exact USD cost of a completed gateway call. It prefers
// the LiteLLM-reported cost header (litellmCost, the raw header value; "" when
// absent); on absence or a parse failure it falls back to the deterministic
// token-table price over the response body's usage block.
//
// A body with no usage and no cost header prices to $0 — an unpriceable call
// costs nothing rather than an arbitrary guess (the conservative pre-call
// estimate, not this, is what guards against a huge un-booked call).
func PriceCall(litellmCost string, respBody []byte) Money {
	if litellmCost != "" {
		if m, err := ParseMoney(litellmCost); err == nil {
			return m
		}
	}
	return priceFromUsage(respBody)
}

// priceFromUsage prices a completion from its usage.total_tokens using the
// deterministic per-token rate. total_tokens falls back to prompt+completion
// when the gateway omitted the total. A missing/invalid usage block prices to $0.
func priceFromUsage(respBody []byte) Money {
	var env chatResponseEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil || env.Usage == nil {
		return Zero()
	}
	total := env.Usage.TotalTokens
	if total == 0 {
		total = env.Usage.PromptTokens + env.Usage.CompletionTokens
	}
	if total <= 0 {
		return Zero()
	}
	cost := new(big.Rat).Mul(usagePricePerToken, new(big.Rat).SetInt64(int64(total)))
	return MoneyFromRat(cost)
}
