// SPDX-License-Identifier: Apache-2.0
//
// Parts adapted from github.com/cloudbase/garm-provider-gcp (Apache-2.0).

// Package spec parses a pool's (or scale set's) extra specs and builds what
// is needed to create one runner: the target MIG and the cloud-init user data.
package spec

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/cloudbase/garm-provider-common/cloudconfig"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/cloudbase/garm-provider-common/util"
	"github.com/invopop/jsonschema"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/geo"
	"github.com/xeipuuv/gojsonschema"
)

const (
	LabelPoolID       = "garmpoolid"
	LabelControllerID = "garmcontrollerid"
	LabelOSType       = "ostype"
	LabelMIG          = "garmmig"

	defaultCreateTimeoutMinutes = 10
	defaultRegionTimeoutMinutes = 3
	maxCustomLabels             = 59 // GCE allows 64 labels; we set 4, the template may set some
)

var (
	customLabelKeyRegex   = regexp.MustCompile(`^\p{Ll}[\p{Ll}0-9_-]{0,62}$`)
	customLabelValueRegex = regexp.MustCompile(`^[\p{Ll}0-9_-]{0,63}$`)
	migNameRegex          = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	regionRegex           = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	reservedLabels        = map[string]bool{LabelPoolID: true, LabelControllerID: true, LabelOSType: true, LabelMIG: true}
)

// Test hooks (same pattern as upstream).
var (
	DefaultToolFetch               = util.GetTools
	DefaultCloudConfigFunc         = cloudconfig.GetCloudConfig
	DefaultRunnerInstallScriptFunc = cloudconfig.GetRunnerInstallScript
)

// MIG designates the regional managed instance group that creates the runners.
type MIG struct {
	Name                 string   `json:"name" jsonschema:"description=Name of the regional managed instance group that creates the runners."`
	Region               string   `json:"region,omitempty" jsonschema:"description=Region of the MIG. Defaults to the provider config region."`
	Regions              []string `json:"regions,omitempty" jsonschema:"description=Ordered location selectors tried in turn (exclusive with region): a region (us-central1)\\, a geography (us\\, northamerica\\, southamerica\\, europe\\, asia\\, australia\\, me\\, africa)\\, eu (EU member states) or country:XX (ISO code). A geography expands to its regions\\, the provider config region first. Regions without the MIG are skipped."`
	CreateTimeoutMinutes int      `json:"create_timeout_minutes,omitempty" jsonschema:"minimum=1,maximum=120,description=How long to wait for the MIG to create the VM before giving up (in the last region). Default 10."`
	RegionTimeoutMinutes int      `json:"region_timeout_minutes,omitempty" jsonschema:"minimum=1,maximum=120,description=How long to wait for the VM in one region before trying the next one. Default 3."`
}

// ExtraSpecs accepted by this provider. Everything about the VM itself comes
// from the MIG's instance template and flexibility policy.
type ExtraSpecs struct {
	MIG             *MIG              `json:"mig" jsonschema:"required,description=Regional managed instance group that creates the runners."`
	CustomLabels    map[string]string `json:"custom_labels,omitempty" jsonschema:"description=Extra labels set on each runner VM (in addition to the template's labels)."`
	SSHKeys         []string          `json:"ssh_keys,omitempty" jsonschema:"description=SSH keys added to each runner (USERNAME:KEY)."`
	EnableBootDebug *bool             `json:"enable_boot_debug,omitempty" jsonschema:"description=Enable boot debug output of the runner install script."`
	DisableUpdates  *bool             `json:"disable_updates,omitempty" jsonschema:"description=Skip OS package updates at boot (recommended with prebuilt images)."`
	// runner_install_template, pre_install_scripts, extra_context (from garm-provider-common)
	cloudconfig.CloudConfigSpec
}

func (m *MIG) CreateTimeout() int {
	if m.CreateTimeoutMinutes == 0 {
		return defaultCreateTimeoutMinutes
	}
	return m.CreateTimeoutMinutes
}

func (m *MIG) RegionTimeout() int {
	if m.RegionTimeoutMinutes == 0 {
		return defaultRegionTimeoutMinutes
	}
	return m.RegionTimeoutMinutes
}

// JSONSchema returns the extra specs JSON schema (published to GARM, used for validation).
func JSONSchema() *jsonschema.Schema {
	r := jsonschema.Reflector{AllowAdditionalProperties: false, ExpandedStruct: true}
	return r.Reflect(ExtraSpecs{})
}

// ParseExtraSpecs validates raw extra specs against the schema and business rules.
func ParseExtraSpecs(raw json.RawMessage) (*ExtraSpecs, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	result, err := gojsonschema.Validate(gojsonschema.NewGoLoader(JSONSchema()), gojsonschema.NewBytesLoader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to validate extra specs: %w", err)
	}
	if !result.Valid() {
		return nil, fmt.Errorf("invalid extra specs: %s", result.Errors())
	}
	es := &ExtraSpecs{}
	if err := json.Unmarshal(raw, es); err != nil {
		return nil, fmt.Errorf("failed to unmarshal extra specs: %w", err)
	}
	return es, es.Validate()
}

func (e *ExtraSpecs) Validate() error {
	if e.MIG == nil {
		return fmt.Errorf("extra spec 'mig' is required")
	}
	if !migNameRegex.MatchString(e.MIG.Name) {
		return fmt.Errorf("mig.name %q is not a valid instance group manager name", e.MIG.Name)
	}
	if e.MIG.Region != "" && !regionRegex.MatchString(e.MIG.Region) {
		return fmt.Errorf("mig.region %q is not a region (e.g. us-central1)", e.MIG.Region)
	}
	if e.MIG.Region != "" && len(e.MIG.Regions) > 0 {
		return fmt.Errorf("mig.region and mig.regions are mutually exclusive")
	}
	for _, sel := range e.MIG.Regions {
		if err := geo.Validate(sel); err != nil {
			return fmt.Errorf("mig.regions: %w", err)
		}
	}
	if e.MIG.CreateTimeoutMinutes < 0 || e.MIG.CreateTimeoutMinutes > 120 {
		return fmt.Errorf("mig.create_timeout_minutes must be between 1 and 120")
	}
	if e.MIG.RegionTimeoutMinutes < 0 || e.MIG.RegionTimeoutMinutes > 120 {
		return fmt.Errorf("mig.region_timeout_minutes must be between 1 and 120")
	}
	if len(e.CustomLabels) > maxCustomLabels {
		return fmt.Errorf("custom_labels cannot exceed %d items", maxCustomLabels)
	}
	for k, v := range e.CustomLabels {
		if reservedLabels[k] {
			return fmt.Errorf("custom label %q is reserved by the provider", k)
		}
		if !customLabelKeyRegex.MatchString(k) {
			return fmt.Errorf("custom label key %q does not match GCE requirements", k)
		}
		if !customLabelValueRegex.MatchString(v) {
			return fmt.Errorf("custom label value %q does not match GCE requirements", v)
		}
	}
	return nil
}

// Regions returns the MIG location selectors: mig.regions, else mig.region,
// else the provider default region. They are resolved at create time.
func (e *ExtraSpecs) Regions(defaultRegion string) ([]string, error) {
	switch {
	case len(e.MIG.Regions) > 0:
		return e.MIG.Regions, nil
	case e.MIG.Region != "":
		return []string{e.MIG.Region}, nil
	case defaultRegion == "":
		return nil, fmt.Errorf("no region: set mig.region or mig.regions in the extra specs, or region in the provider config")
	}
	return []string{defaultRegion}, nil
}

// RunnerSpec is everything needed to create one runner.
type RunnerSpec struct {
	BootstrapParams params.BootstrapInstance
	Tools           params.RunnerApplicationDownload
	MIG             MIG
	Regions         []string          // location selectors, see geo.Resolve
	HomeRegion      string            // provider config region, tried first within a geography
	Labels          map[string]string // set on the VM after creation
	SSHKeys         string
	EnableBootDebug bool
	DisableUpdates  bool
}

func NewRunnerSpec(data params.BootstrapInstance, controllerID, defaultRegion string) (*RunnerSpec, error) {
	es, err := ParseExtraSpecs(data.ExtraSpecs)
	if err != nil {
		return nil, err
	}
	regions, err := es.Regions(defaultRegion)
	if err != nil {
		return nil, err
	}
	tools, err := DefaultToolFetch(data.OSType, data.OSArch, data.Tools)
	if err != nil {
		return nil, fmt.Errorf("failed to get tools: %w", err)
	}

	labels := map[string]string{}
	for k, v := range es.CustomLabels {
		labels[k] = v
	}
	labels[LabelPoolID] = data.PoolID
	labels[LabelControllerID] = controllerID
	labels[LabelOSType] = string(data.OSType)
	labels[LabelMIG] = es.MIG.Name

	rs := &RunnerSpec{
		BootstrapParams: data,
		Tools:           tools,
		MIG:             *es.MIG,
		Regions:         regions,
		HomeRegion:      defaultRegion,
		Labels:          labels,
	}
	for _, k := range es.SSHKeys {
		if rs.SSHKeys != "" {
			rs.SSHKeys += "\n"
		}
		rs.SSHKeys += k
	}
	if es.EnableBootDebug != nil {
		rs.EnableBootDebug = *es.EnableBootDebug
	}
	if es.DisableUpdates != nil {
		rs.DisableUpdates = *es.DisableUpdates
	}
	return rs, nil
}

// ComposeUserData renders GARM's runner bootstrap (cloud-init on Linux,
// PowerShell on Windows). runner_install_template, pre_install_scripts and
// extra_context are read from the extra specs by garm-provider-common.
func (r RunnerSpec) ComposeUserData() (string, error) {
	bp := r.BootstrapParams
	bp.UserDataOptions.EnableBootDebug = r.EnableBootDebug
	bp.UserDataOptions.DisableUpdatesOnBoot = r.DisableUpdates

	switch bp.OSType {
	case params.Linux:
		udata, err := DefaultCloudConfigFunc(bp, r.Tools, bp.Name)
		if err != nil {
			return "", fmt.Errorf("failed to generate userdata: %w", err)
		}
		return udata, nil
	case params.Windows:
		udata, err := DefaultRunnerInstallScriptFunc(bp, r.Tools, bp.Name)
		if err != nil {
			return "", fmt.Errorf("failed to generate userdata: %w", err)
		}
		return string(udata), nil
	}
	return "", fmt.Errorf("unsupported OS type: %s", bp.OSType)
}
