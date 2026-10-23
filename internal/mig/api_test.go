// SPDX-License-Identifier: Apache-2.0

package mig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegionCountriesPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/projects/p/locations/global/cloudLocations", r.URL.Path)
		assert.Equal(t, "cloud_provider=CLOUD_PROVIDER_GCP", r.URL.Query().Get("filter"))
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"cloudLocations":[
				{"displayName":"us-east1","cloudLocationType":"CLOUD_LOCATION_TYPE_REGION","territoryCode":"US"},
				{"displayName":"us-east1-b","cloudLocationType":"CLOUD_LOCATION_TYPE_ZONE","territoryCode":"US"}
			],"nextPageToken":"two"}`))
			return
		}
		assert.Equal(t, "two", r.URL.Query().Get("pageToken"))
		_, _ = w.Write([]byte(`{"cloudLocations":[{"displayName":"europe-west2","cloudLocationType":"CLOUD_LOCATION_TYPE_REGION","territoryCode":"GB"}]}`))
	}))
	defer srv.Close()
	old := locationFinderURL
	locationFinderURL = srv.URL
	defer func() { locationFinderURL = old }()

	got, err := (&gcpAPI{http: srv.Client()}).RegionCountries(context.Background(), "p")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"us-east1": "US", "europe-west2": "GB"}, got)
}

func TestRegionCountriesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Cloud Location Finder API has not been used", http.StatusForbidden)
	}))
	defer srv.Close()
	old := locationFinderURL
	locationFinderURL = srv.URL
	defer func() { locationFinderURL = old }()

	_, err := (&gcpAPI{http: srv.Client()}).RegionCountries(context.Background(), "p")
	assert.ErrorContains(t, err, "403")
	assert.ErrorContains(t, err, "has not been used")
}
