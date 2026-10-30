// Copyright 2016 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Go adaptation of Google Cloud CLI 591.0.0 core/util/iso_duration.py:
// Duration.Parse and Duration._Normalize, used by submit_util._GetBuildTimeout.
// Changes: Go floats/strings replace Python numeric objects/lists; the result
// is bounded by time.Duration and must contain at least one whole second.
package localbuild

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type sdkDuration struct {
	years, months, days, hours, minutes, seconds, microseconds float64
}

func sdkTimeout(s string) (time.Duration, error) {
	value := s
	if n, err := parseSDKInteger(s); err == nil {
		value = strconv.FormatInt(n, 10) + "s"
	}
	parts, err := parseSDKDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid build timeout %q: %w", s, err)
	}
	seconds := parts.normalizedSeconds()
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > float64(math.MaxInt64/int64(time.Second)) {
		return 0, fmt.Errorf("invalid or out-of-range build timeout %q", s)
	}
	// _GetBuildTimeout uses int(total_seconds), truncating fractional seconds.
	if seconds < 1 {
		return 0, fmt.Errorf("build timeout %q is less than one whole second", s)
	}
	return time.Duration(math.Trunc(seconds)) * time.Second, nil
}

func parseSDKDuration(input string) (sdkDuration, error) {
	var d sdkDuration
	s := strings.ToUpper(input)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	tSeparator := false
	tImplied := !strings.HasPrefix(s, "P")
	s = strings.TrimPrefix(s, "P")
	amount := sign
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			amount += string(c)
			continue
		case c == '.' || c == ',':
			amount += "."
			continue
		case c == 'T':
			if tSeparator {
				return d, fmt.Errorf("at most one T separator is allowed")
			}
			tSeparator, tImplied = true, true
			continue
		case amount == sign:
			return d, fmt.Errorf("duration unit must follow a number")
		}
		number, err := strconv.ParseFloat(amount, 64)
		if err != nil || math.IsInf(number, 0) {
			return d, fmt.Errorf("invalid duration number %q", amount)
		}
		amount = sign
		switch {
		case c == 'Y':
			d.years += number
		case c == 'W':
			d.days += number * 7
		case c == 'D':
			d.days += number
		case (c == 'M' || c == 'U' || c == 'N') && i+2 == len(s) && s[i+1] == 'S':
			divisor := map[byte]float64{'M': 1e3, 'U': 1e6, 'N': 1e9}[c]
			d.seconds += number / divisor
			return d, nil
		case c == 'M' && !tImplied:
			tImplied = true
			d.months += number
		default:
			tImplied = true
			switch c {
			case 'H':
				d.hours += number
			case 'M':
				d.minutes += number
			case 'S':
				d.seconds += number
			default:
				return d, fmt.Errorf("unknown duration character %q", c)
			}
		}
	}
	if amount != sign && strings.TrimLeft(strings.ToUpper(input), "+-") != "P0" {
		return d, fmt.Errorf("duration must end with a unit")
	}
	return d, nil
}

func (d sdkDuration) normalizedSeconds() float64 {
	const daysPerYear = 365.2422
	const secondsPerDay = 86400
	const secondsPerYear = secondsPerDay * daysPerYear
	const secondsPerMonth = secondsPerYear / 12
	// Match Python's decimal rounding before passing fractions down the units.
	round4 := func(n float64) float64 {
		value, _ := strconv.ParseFloat(strconv.FormatFloat(n, 'f', 4, 64), 64)
		return value
	}
	percolate := func(n float64) (float64, float64) {
		whole := math.Trunc(n)
		return whole, round4(round4(n) - whole)
	}
	var fraction float64
	d.years, fraction = percolate(d.years)
	d.days += daysPerYear * fraction
	d.months, fraction = percolate(d.months)
	d.days += math.Trunc(daysPerYear * fraction / 12)
	d.days, fraction = percolate(d.days)
	d.hours += 24 * fraction
	d.hours, fraction = percolate(d.hours)
	d.minutes += 60 * fraction
	d.minutes, fraction = percolate(d.minutes)
	d.seconds += 60 * fraction
	d.seconds, fraction = percolate(d.seconds)
	if fraction != 0 {
		d.microseconds = math.Trunc(1e6 * fraction)
	}
	carry := math.Trunc(d.microseconds / 1e6)
	d.microseconds -= math.Trunc(carry * 1e6)
	total := d.microseconds / 1e6
	d.seconds += carry
	carry = math.Trunc(d.seconds / 60)
	d.seconds -= carry * 60
	total += d.seconds
	d.minutes += carry
	carry = math.Trunc(d.minutes / 60)
	d.minutes -= carry * 60
	total += d.minutes * 60
	d.hours += carry
	if d.days == 0 && d.months == 0 && d.years == 0 {
		return total + d.hours*3600
	}
	carry = math.Trunc(d.hours / 24)
	d.hours -= carry * 24
	total += d.hours * 3600
	d.days += carry
	if d.days >= 366 {
		d.days -= 366
		d.years++
	} else if d.days <= -366 {
		d.days += 366
		d.years--
	}
	carry = math.Trunc(d.days / daysPerYear)
	d.days -= math.Trunc(carry * daysPerYear)
	total += d.days * secondsPerDay
	d.years += carry
	carry = math.Trunc(d.months / 12)
	d.months -= carry * 12
	total += d.months * secondsPerMonth
	d.years += carry
	total += d.years * secondsPerYear
	return math.RoundToEven(total) + d.microseconds/1e6
}
