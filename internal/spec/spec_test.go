// SPDX-License-Identifier: Apache-2.0

package spec

import (
	"encoding/json"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func fakeTools(params.OSType, params.OSArch, []params.RunnerApplicationDownload) (params.RunnerApplicationDownload, error) {
	return params.RunnerApplicationDownload{
		OS: proto.String("linux"), Architecture: proto.String("x64"),
		DownloadURL: proto.String("https://example.com/runner.tgz"), Filename: proto.String("runner.tgz"),
	}, nil
}

func bootstrap(specs string) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name: "Garm-X", OSType: params.Linux, OSArch: params.Amd64, PoolID: "pool-1",
		ExtraSpecs: json.RawMessage(specs),
	}
}

func TestNewRunnerSpec(t *testing.T) {
	DefaultToolFetch = fakeTools
	rs, err := NewRunnerSpec(bootstrap(`{
		"mig": {"name": "production-shr-large-grp", "create_timeout_minutes": 15},
		"custom_labels": {"team": "ci"},
		"ssh_keys": ["a:ssh-ed25519 AAA", "b:ssh-ed25519 BBB"],
		"disable_updates": true,
		"pre_install_scripts": {"10-docker-login.sh": "IyEvYmluL3NoCg=="}
	}`), "ctrl-1", "us-central1")
	require.NoError(t, err)

	assert.Equal(t, "production-shr-large-grp", rs.MIG.Name)
	assert.Equal(t, 15, rs.MIG.CreateTimeout())
	assert.Equal(t, []string{"us-central1"}, rs.Regions, "region from provider config")
	assert.Equal(t, "us-central1", rs.HomeRegion)
	assert.Equal(t, 3, rs.MIG.RegionTimeout(), "default region timeout")
	assert.Equal(t, map[string]string{
		"team": "ci", "garmpoolid": "pool-1", "garmcontrollerid": "ctrl-1",
		"ostype": "linux", "garmmig": "production-shr-large-grp",
	}, rs.Labels)
	assert.Equal(t, "a:ssh-ed25519 AAA\nb:ssh-ed25519 BBB", rs.SSHKeys)
	assert.True(t, rs.DisableUpdates)

	udata, err := rs.ComposeUserData()
	require.NoError(t, err)
	assert.Contains(t, udata, "#cloud-config")
	assert.Contains(t, udata, "10-docker-login.sh", "pre_install_scripts handled by garm-provider-common")
}

func TestRegionResolution(t *testing.T) {
	DefaultToolFetch = fakeTools
	rs, err := NewRunnerSpec(bootstrap(`{"mig": {"name": "g", "region": "europe-west1"}}`), "c", "us-central1")
	require.NoError(t, err)
	assert.Equal(t, []string{"europe-west1"}, rs.Regions, "extra spec overrides provider default")

	rs, err = NewRunnerSpec(bootstrap(`{"mig": {"name": "g", "regions": ["us-east1", "eu", "country:CA"], "region_timeout_minutes": 5}}`), "c", "us-central1")
	require.NoError(t, err)
	assert.Equal(t, []string{"us-east1", "eu", "country:CA"}, rs.Regions, "selectors resolved later, against live regions")
	assert.Equal(t, "us-central1", rs.HomeRegion)
	assert.Equal(t, 5, rs.MIG.RegionTimeout())

	rs, err = NewRunnerSpec(bootstrap(`{"mig": {"name": "g", "regions": ["us"]}}`), "c", "")
	require.NoError(t, err, "a geography needs no default region")
	assert.Equal(t, []string{"us"}, rs.Regions)

	rs, err = NewRunnerSpec(bootstrap(`{"mig": {"name": "g", "regions": ["us"], "exclude_regions": ["us-east4", "country:CA"]}}`), "c", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"us-east4", "country:CA"}, rs.MIG.ExcludeRegions)

	_, err = NewRunnerSpec(bootstrap(`{"mig": {"name": "g"}}`), "c", "")
	assert.ErrorContains(t, err, "no region")
}

func TestExtraSpecsRejected(t *testing.T) {
	for name, specs := range map[string]string{
		"empty":                   ``,
		"no mig":                  `{"disable_updates": true}`,
		"bad mig name":            `{"mig": {"name": "Bad_Name"}}`,
		"zone instead of region":  `{"mig": {"name": "g", "region": "us-central1-a"}}`,
		"timeout too long":        `{"mig": {"name": "g", "create_timeout_minutes": 500}}`,
		"region and regions":      `{"mig": {"name": "g", "region": "us-east1", "regions": ["us"]}}`,
		"bad selector":            `{"mig": {"name": "g", "regions": ["mars"]}}`,
		"bad exclude selector":    `{"mig": {"name": "g", "regions": ["us"], "exclude_regions": ["us-east4-a"]}}`,
		"zone selector":           `{"mig": {"name": "g", "regions": ["us-central1-a"]}}`,
		"lowercase country":       `{"mig": {"name": "g", "regions": ["country:ca"]}}`,
		"uppercase geography":     `{"mig": {"name": "g", "regions": ["EU"]}}`,
		"region timeout too long": `{"mig": {"name": "g", "region_timeout_minutes": 121}}`,
		"negative region timeout": `{"mig": {"name": "g", "region_timeout_minutes": -1}}`,
		"unknown field":           `{"mig": {"name": "g"}, "disksize": 100}`,
		"unknown mig field":       `{"mig": {"name": "g", "zones": ["a"]}}`,
		"reserved label":          `{"mig": {"name": "g"}, "custom_labels": {"garmpoolid": "x"}}`,
		"bad label":               `{"mig": {"name": "g"}, "custom_labels": {"Team": "x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseExtraSpecs(json.RawMessage(specs))
			assert.Error(t, err)
		})
	}
}

func TestJSONSchemaPublishesMIG(t *testing.T) {
	b, err := json.Marshal(JSONSchema())
	require.NoError(t, err)
	assert.Contains(t, string(b), `"mig"`)
	assert.Contains(t, string(b), `"pre_install_scripts"`)
	assert.Contains(t, string(b), `"required":["mig"]`)
}
