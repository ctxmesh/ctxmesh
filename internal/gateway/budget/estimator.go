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
	"math/big"
	"sync"
)

// estimateFloorUSD is the minimum conservative estimate for a pending call's
// cost, used before any call on a route has been observed (and as a lower bound
// afterwards). A single micro-cent — small enough not to reject the very first
// call under any real cap, large enough to be non-zero so an unpriceable route
// cannot let unlimited calls slip the pre-call check.
var estimateFloorUSD = new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(1_000_000))

// Estimator supplies the conservative pre-call cost estimate the hard check adds
// to already-accumulated spend (specs/cost-governance.md "Estimated vs actual
// cost"). Rule: the estimate for a route is the LAST OBSERVED actual cost on
// that route, floored at estimateFloorUSD; before any observation it is the
// floor. Because mock pricing is deterministic (every mock call has the same
// usage → the same cost), the last-observed cost equals the next call's cost, so
// the pre-call check trips reproducibly exactly when spent + oneCallCost > cap —
// the "single huge call can't slip through" guarantee the spec asks for.
//
// Thread-safe: the map is mutex-guarded like the spend maps.
type Estimator struct {
	mu   sync.Mutex
	last map[string]Money // route → last observed actual cost
}

// NewEstimator returns an empty Estimator.
func NewEstimator() *Estimator {
	return &Estimator{last: make(map[string]Money)}
}

// floor returns estimateFloorUSD as Money.
func floor() Money {
	return MoneyFromRat(estimateFloorUSD)
}

// Estimate returns the conservative estimate for the next call on route: the
// last observed cost (floored), or the floor if the route is unseen. An empty
// route uses the global floor/last-seen bucket ("").
func (e *Estimator) Estimate(route string) Money {
	e.mu.Lock()
	defer e.mu.Unlock()
	last, ok := e.last[route]
	f := floor()
	if !ok || last.Cmp(f) < 0 {
		return f
	}
	return last
}

// Observe records the actual cost of a completed call on route so the next
// call's estimate reflects it.
func (e *Estimator) Observe(route string, actual Money) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.last[route] = actual
}
