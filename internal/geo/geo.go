// SPDX-License-Identifier: Apache-2.0

// Package geo turns location selectors (a region, a geography, "eu" or
// "country:XX") into an ordered list of GCP regions.
package geo

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	regionRegex  = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	countryRegex = regexp.MustCompile(`^country:[A-Z]{2}$`)

	// geographies match regions by name prefix.
	geographies = map[string][]string{
		"us":           {"us-"},
		"northamerica": {"us-", "northamerica-"},
		"southamerica": {"southamerica-"},
		"europe":       {"europe-"},
		"asia":         {"asia-"},
		"australia":    {"australia-"},
		"me":           {"me-"},
		"africa":       {"africa-"},
	}

	// euMembers are the EU-27 (ISO 3166-1 alpha-2), as in Cloud Storage's EU multi-region.
	euMembers = map[string]bool{
		"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true, "DK": true,
		"EE": true, "FI": true, "FR": true, "DE": true, "GR": true, "HU": true, "IE": true,
		"IT": true, "LV": true, "LT": true, "LU": true, "MT": true, "NL": true, "PL": true,
		"PT": true, "RO": true, "SK": true, "SI": true, "ES": true, "SE": true,
	}
)

// IsRegion reports whether the selector is a plain region name.
func IsRegion(sel string) bool {
	return regionRegex.MatchString(sel)
}

// Validate checks the syntax of one selector.
func Validate(sel string) error {
	if IsRegion(sel) || geographies[sel] != nil || sel == "eu" || countryRegex.MatchString(sel) {
		return nil
	}
	return fmt.Errorf("%q is not a region (e.g. us-central1), a geography (us, northamerica, southamerica, europe, asia, australia, me, africa, eu) or country:XX (ISO code, uppercase)", sel)
}

// Resolve expands selectors against the live regions into an ordered,
// de-duplicated list, minus the regions matched by exclude. Explicit regions
// keep their position; a geography expands to its regions, home first if it
// matches, then alphabetically. countries (region -> ISO code) is only called
// for "eu" and "country:" selectors. An exclusion that matches nothing is not
// an error (a region may have been retired).
func Resolve(selectors, exclude, regions []string, home string, countries func() (map[string]string, error)) ([]string, error) {
	sorted := slices.Sorted(slices.Values(regions))
	var codes map[string]string
	// matches returns the live regions a selector stands for, in order.
	matches := func(sel string) ([]string, error) {
		if IsRegion(sel) {
			if !slices.Contains(regions, sel) {
				return nil, nil
			}
			return []string{sel}, nil
		}
		var match func(string) bool
		switch {
		case geographies[sel] != nil:
			match = func(r string) bool {
				return slices.ContainsFunc(geographies[sel], func(p string) bool { return strings.HasPrefix(r, p) })
			}
		case sel == "eu" || countryRegex.MatchString(sel):
			if codes == nil {
				var err error
				if codes, err = countries(); err != nil {
					return nil, fmt.Errorf("failed to get region countries (for %s): %w", sel, err)
				}
			}
			want := strings.TrimPrefix(sel, "country:")
			match = func(r string) bool {
				if sel == "eu" {
					return euMembers[codes[r]]
				}
				return codes[r] == want
			}
		default:
			return nil, Validate(sel)
		}
		var matched []string
		for _, r := range sorted {
			if match(r) {
				matched = append(matched, r)
			}
		}
		if slices.Contains(matched, home) {
			matched = append([]string{home}, slices.DeleteFunc(matched, func(r string) bool { return r == home })...)
		}
		return matched, nil
	}

	excluded := map[string]bool{}
	for _, sel := range exclude {
		matched, err := matches(sel)
		if err != nil {
			return nil, err
		}
		for _, r := range matched {
			excluded[r] = true
		}
	}
	var out []string
	for _, sel := range selectors {
		matched, err := matches(sel)
		if err != nil {
			return nil, err
		}
		if len(matched) == 0 {
			if IsRegion(sel) {
				return nil, fmt.Errorf("region %s is not an available region", sel)
			}
			return nil, fmt.Errorf("%s matches no available region", sel)
		}
		for _, r := range matched {
			if !excluded[r] && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("exclude_regions %v excludes every region of %v", exclude, selectors)
	}
	return out, nil
}
