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

import (
	"math"
	"testing"
	"time"
)

// day returns a UTC day boundary at (year, month, d) for rollup fixtures.
func day(year int, month time.Month, d int) time.Time {
	return time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
}

// TestLinearForecast_EmptyInput proves ok=false and projected=0 on empty input.
func TestLinearForecast_EmptyInput(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	p, ok := LinearForecast(nil, now)
	if ok {
		t.Fatalf("empty rollups: want ok=false, got ok=true (projected=%v)", p)
	}
	if p != 0 {
		t.Fatalf("empty rollups: want projected=0, got %v", p)
	}

	p, ok = LinearForecast([]Rollup{}, now)
	if ok {
		t.Fatalf("empty slice: want ok=false, got ok=true")
	}
	if p != 0 {
		t.Fatalf("empty slice: want projected=0, got %v", p)
	}
}

// TestLinearForecast_NowAtMonthStart proves ok=false when now is at or before month start
// (daysElapsed=0 or negative — guard against div-by-zero).
func TestLinearForecast_NowAtMonthStart(t *testing.T) {
	// now == midnight on the 1st → daysElapsed=0
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rollups := []Rollup{{Day: day(2026, 8, 1), SpendUSD: 10.0}}
	p, ok := LinearForecast(rollups, now)
	if ok {
		t.Fatalf("now == month start: want ok=false (div-by-zero guard), got ok=true (projected=%v)", p)
	}
	if p != 0 {
		t.Fatalf("now == month start: want projected=0, got %v", p)
	}
}

// TestLinearForecast_SingleDay proves the single-day run-rate extrapolation.
// If 10 USD spent by day 1 of a 31-day month, projected = 10/1 * 31 = 310.
func TestLinearForecast_SingleDay(t *testing.T) {
	// August 2026 has 31 days. now = 2026-08-02 00:00 UTC → daysElapsed=1 exactly.
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	rollups := []Rollup{
		{Day: day(2026, 8, 1), SpendUSD: 10.0},
	}
	p, ok := LinearForecast(rollups, now)
	if !ok {
		t.Fatalf("single-day: want ok=true, got ok=false")
	}
	want := 10.0 / 1.0 * 31.0
	if math.Abs(p-want) > 1e-9 {
		t.Fatalf("single-day: want projected=%.6f, got %.6f", want, p)
	}
}

// TestLinearForecast_MidMonth proves the extrapolation mid-month with multiple rows.
// 10 days elapsed, 45 USD MTD in a 30-day month → projected = 45/10 * 30 = 135.
func TestLinearForecast_MidMonth(t *testing.T) {
	// September 2026 has 30 days. now = 2026-09-11 00:00 UTC → daysElapsed=10.
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	rollups := []Rollup{
		{Day: day(2026, 9, 1), SpendUSD: 4.5},
		{Day: day(2026, 9, 5), SpendUSD: 22.5},
		{Day: day(2026, 9, 10), SpendUSD: 45.0}, // newest = MTD
	}
	p, ok := LinearForecast(rollups, now)
	if !ok {
		t.Fatalf("mid-month: want ok=true, got ok=false")
	}
	want := 45.0 / 10.0 * 30.0
	if math.Abs(p-want) > 1e-9 {
		t.Fatalf("mid-month: want projected=%.6f, got %.6f", want, p)
	}
}

// TestLinearForecast_FractionalDay proves that a fractional elapsed day (a few hours in)
// produces a sensible run-rate, not a panicked zero or NaN.
// 6 h into a 31-day month (daysElapsed=0.25), 2.5 USD spent → projected = 2.5/0.25 * 31 = 310.
func TestLinearForecast_FractionalDay(t *testing.T) {
	// August 2026, 6 hours in.
	now := time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC)
	rollups := []Rollup{{Day: day(2026, 8, 1), SpendUSD: 2.5}}
	p, ok := LinearForecast(rollups, now)
	if !ok {
		t.Fatalf("fractional day: want ok=true, got ok=false")
	}
	// daysElapsed = 6/24 = 0.25
	want := 2.5 / 0.25 * 31.0
	if math.Abs(p-want) > 1e-6 {
		t.Fatalf("fractional day: want projected=%.6f, got %.6f", want, p)
	}
}

// TestLinearForecast_FebruaryLeapYear proves the correct 29-day month denominator.
// February 2024 is a leap year. 15 days in, 75 USD MTD → projected = 75/15 * 29 = 145.
func TestLinearForecast_FebruaryLeapYear(t *testing.T) {
	now := time.Date(2024, 2, 16, 0, 0, 0, 0, time.UTC) // daysElapsed=15
	rollups := []Rollup{
		{Day: day(2024, 2, 1), SpendUSD: 5.0},
		{Day: day(2024, 2, 15), SpendUSD: 75.0},
	}
	p, ok := LinearForecast(rollups, now)
	if !ok {
		t.Fatalf("leap Feb: want ok=true, got ok=false")
	}
	want := 75.0 / 15.0 * 29.0
	if math.Abs(p-want) > 1e-9 {
		t.Fatalf("leap Feb: want projected=%.6f, got %.6f", want, p)
	}
}

// TestLinearForecast_OnlyNewestRowMatters proves that older rows are ignored — only the
// LAST (newest day-ASC) row's SpendUSD is used as the MTD cumulative.
func TestLinearForecast_OnlyNewestRowMatters(t *testing.T) {
	// August 2026, day 11 midnight → daysElapsed=10
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	rollups := []Rollup{
		{Day: day(2026, 8, 1), SpendUSD: 10.0},
		{Day: day(2026, 8, 5), SpendUSD: 50.0},
		{Day: day(2026, 8, 10), SpendUSD: 100.0}, // newest = MTD
	}
	p, ok := LinearForecast(rollups, now)
	if !ok {
		t.Fatalf("newest-row: want ok=true, got ok=false")
	}
	want := 100.0 / 10.0 * 31.0
	if math.Abs(p-want) > 1e-9 {
		t.Fatalf("newest-row: want projected=%.6f, got %.6f", want, p)
	}
}
