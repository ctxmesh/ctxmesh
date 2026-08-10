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

package costrollup

import "time"

// LinearForecast extrapolates the month-end spend from day-ASC month-to-date
// cumulative-spend rollup rows for the current month (M70, ADR 0063 D3).
//
// Algorithm (v1 — linear run-rate, no ML):
//
//	projected = latestMTD / daysElapsed * daysInMonth
//
// where daysElapsed is the fractional number of days between the start of the month
// (midnight UTC on the 1st) and `now`, and daysInMonth is the total number of
// calendar days in the month (28–31).
//
// Returns (projected, true) on success. Returns (0, false) when:
//   - rollups is empty (no data to extrapolate from)
//   - daysElapsed ≤ 0 (now is at or before the start of the month, or a bad clock)
//
// NOTE: Both the BFF forecast endpoint AND the forecastExceeded AlertPolicy condition
// call this function so the two planes cannot drift apart. Any change to the formula
// here is the single source of truth for both.
func LinearForecast(rollups []Rollup, now time.Time) (projected float64, ok bool) {
	if len(rollups) == 0 {
		return 0, false
	}

	// The rows are day-ASC; the LAST row is the newest (highest) MTD cumulative spend.
	latestMTD := rollups[len(rollups)-1].SpendUSD

	// daysElapsed = fractional days from month-start midnight UTC to now.
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	elapsed := now.UTC().Sub(monthStart)
	daysElapsed := elapsed.Hours() / 24.0
	if daysElapsed <= 0 {
		return 0, false
	}

	// daysInMonth: number of calendar days in now's month (correct for Feb/31-day months).
	// Go's time package wraps day=0 of the NEXT month to the last day of THIS month.
	nextMonthStart := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	daysInMonth := nextMonthStart.Sub(monthStart).Hours() / 24.0

	projected = latestMTD / daysElapsed * daysInMonth
	return projected, true
}
