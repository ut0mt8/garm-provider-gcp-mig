// SPDX-License-Identifier: Apache-2.0

package geo

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var live = []string{
	"africa-south1", "asia-east1", "asia-northeast1", "australia-southeast1",
	"europe-north1", "europe-west1", "europe-west2", "europe-west6", "me-central1",
	"northamerica-northeast1", "northamerica-south1", "southamerica-east1",
	"us-central1", "us-east1", "us-west1",
}

var countryCodes = map[string]string{
	"europe-north1": "FI", "europe-west1": "BE", "europe-west2": "GB", "europe-west6": "CH",
	"northamerica-northeast1": "CA", "us-central1": "US", "us-east1": "US", "us-west1": "US",
}

func countries(calls *int) func() (map[string]string, error) {
	return func() (map[string]string, error) {
		*calls++
		return countryCodes, nil
	}
}

func TestResolve(t *testing.T) {
	for name, tc := range map[string]struct {
		selectors []string
		home      string
		want      []string
		countries int // expected calls
	}{
		"explicit regions keep their order":        {[]string{"us-west1", "europe-west1"}, "us-central1", []string{"us-west1", "europe-west1"}, 0},
		"geography: home first, then alphabetical": {[]string{"us"}, "us-east1", []string{"us-east1", "us-central1", "us-west1"}, 0},
		"geography without home":                   {[]string{"us"}, "europe-west1", []string{"us-central1", "us-east1", "us-west1"}, 0},
		"northamerica includes us":                 {[]string{"northamerica"}, "", []string{"northamerica-northeast1", "northamerica-south1", "us-central1", "us-east1", "us-west1"}, 0},
		"other geographies": {[]string{"southamerica", "asia", "australia", "me", "africa"}, "", []string{
			"southamerica-east1", "asia-east1", "asia-northeast1", "australia-southeast1", "me-central1", "africa-south1"}, 0},
		"europe":                      {[]string{"europe"}, "", []string{"europe-north1", "europe-west1", "europe-west2", "europe-west6"}, 0},
		"eu excludes GB and CH":       {[]string{"eu"}, "europe-west1", []string{"europe-west1", "europe-north1"}, 1},
		"country":                     {[]string{"country:CA"}, "", []string{"northamerica-northeast1"}, 1},
		"countries fetched once":      {[]string{"country:CA", "eu"}, "", []string{"northamerica-northeast1", "europe-north1", "europe-west1"}, 1},
		"dedupe keeps first position": {[]string{"us-west1", "us", "us-central1"}, "us-central1", []string{"us-west1", "us-central1", "us-east1"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			got, err := Resolve(tc.selectors, live, tc.home, countries(&calls))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.countries, calls, "countries looked up only for eu/country: selectors")
		})
	}
}

func TestResolveErrors(t *testing.T) {
	calls := 0
	_, err := Resolve([]string{"us-south9"}, live, "", countries(&calls))
	assert.ErrorContains(t, err, "us-south9")

	_, err = Resolve([]string{"country:JP"}, live, "", countries(&calls))
	assert.ErrorContains(t, err, "matches no available region")

	_, err = Resolve([]string{"africa"}, []string{"us-central1"}, "", countries(&calls))
	assert.ErrorContains(t, err, "matches no available region")

	_, err = Resolve([]string{"eu"}, live, "", func() (map[string]string, error) { return nil, errors.New("403 denied") })
	assert.ErrorContains(t, err, "403 denied")

	_, err = Resolve([]string{"mars"}, live, "", countries(&calls))
	assert.Error(t, err)
}

func TestValidate(t *testing.T) {
	for _, ok := range []string{"us-central1", "us", "northamerica", "southamerica", "europe", "asia", "australia", "me", "africa", "eu", "country:CA"} {
		assert.NoError(t, Validate(ok), ok)
	}
	for _, bad := range []string{"", "US", "us-central1-a", "country:ca", "country:CAN", "mars", "EU"} {
		assert.Error(t, Validate(bad), bad)
	}
}
