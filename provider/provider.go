// SPDX-License-Identifier: Apache-2.0

// Package provider implements the GARM external provider interface
// (v0.1.0 and v0.1.1) on top of regional managed instance groups.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	garmErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/execution/common"
	executionv010 "github.com/cloudbase/garm-provider-common/execution/v0.1.0"
	executionv011 "github.com/cloudbase/garm-provider-common/execution/v0.1.1"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/invopop/jsonschema"
	"github.com/ut0mt8/garm-provider-gcp-mig/config"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/mig"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/spec"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/util"
)

var (
	_ executionv010.ExternalProvider = (*Provider)(nil)
	_ executionv011.ExternalProvider = (*Provider)(nil)
)

// Version is set at build time (-ldflags "-X .../provider.Version=v1.2.3").
var Version = "v0.0.0-dev"

// NewAPIFunc builds the Compute API; replaced in tests.
var NewAPIFunc = mig.NewAPI

// Provider loads its config and API clients lazily, so that commands that
// don't need GCP (schemas, versions) work without credentials.
type Provider struct {
	cfgFile      string
	controllerID string

	once sync.Once
	cfg  *config.Config
	mgr  *mig.Manager
	err  error
}

func New(cfgFile, controllerID string) *Provider {
	return &Provider{cfgFile: cfgFile, controllerID: controllerID}
}

func (p *Provider) init(ctx context.Context) error {
	p.once.Do(func() {
		cfg, err := config.NewConfig(p.cfgFile)
		if err != nil {
			p.err = err
			return
		}
		api, err := NewAPIFunc(ctx, cfg)
		if err != nil {
			p.err = fmt.Errorf("failed to create Compute API client: %w", err)
			return
		}
		p.cfg, p.mgr = cfg, mig.NewManager(api, cfg.ProjectID)
	})
	return p.err
}

func (p *Provider) CreateInstance(ctx context.Context, bootstrapParams params.BootstrapInstance) (params.ProviderInstance, error) {
	if err := p.init(ctx); err != nil {
		return params.ProviderInstance{}, err
	}
	rs, err := spec.NewRunnerSpec(bootstrapParams, p.controllerID, p.cfg.Region)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("invalid runner spec: %w", err)
	}
	inst, err := p.mgr.Create(ctx, rs)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	return util.ToProviderInstance(inst)
}

func (p *Provider) GetInstance(ctx context.Context, instance string) (params.ProviderInstance, error) {
	if err := p.init(ctx); err != nil {
		return params.ProviderInstance{}, err
	}
	inst, err := p.mgr.Get(ctx, instance)
	if errors.Is(err, mig.ErrNotFound) {
		// GARM recognises this error (exit code 30) and treats the runner as gone.
		return params.ProviderInstance{}, fmt.Errorf("instance %s: %w", instance, garmErrors.ErrNotFound)
	}
	if err != nil {
		return params.ProviderInstance{}, err
	}
	return util.ToProviderInstance(inst)
}

func (p *Provider) DeleteInstance(ctx context.Context, instance string) error {
	if err := p.init(ctx); err != nil {
		return err
	}
	return p.mgr.Delete(ctx, instance)
}

func (p *Provider) ListInstances(ctx context.Context, poolID string) ([]params.ProviderInstance, error) {
	if err := p.init(ctx); err != nil {
		return nil, err
	}
	instances, err := p.mgr.List(ctx, poolID)
	if err != nil {
		return nil, err
	}
	out := make([]params.ProviderInstance, 0, len(instances))
	for _, inst := range instances {
		pi, err := util.ToProviderInstance(inst)
		if err != nil {
			return nil, err
		}
		out = append(out, pi)
	}
	return out, nil
}

func (p *Provider) RemoveAllInstances(ctx context.Context) error {
	if err := p.init(ctx); err != nil {
		return err
	}
	return p.mgr.RemoveAll(ctx, p.controllerID)
}

func (p *Provider) Stop(ctx context.Context, instance string, force bool) error {
	if err := p.init(ctx); err != nil {
		return err
	}
	return p.mgr.Stop(ctx, instance)
}

func (p *Provider) Start(ctx context.Context, instance string) error {
	if err := p.init(ctx); err != nil {
		return err
	}
	return p.mgr.Start(ctx, instance)
}

func (p *Provider) GetVersion(ctx context.Context) string {
	return Version
}

// --- interface v0.1.1 ---

func (p *Provider) GetSupportedInterfaceVersions(ctx context.Context) []string {
	return []string{common.Version010, common.Version011}
}

// ValidatePoolInfo runs when a pool/scale set is created or updated: it
// checks the extra specs and that the MIG exists in at least one of its
// regions and is safe to drive in all of them.
// Image and flavor are ignored: they come from the MIG.
func (p *Provider) ValidatePoolInfo(ctx context.Context, image, flavor, providerConfig, extraspecs string) error {
	es, err := spec.ParseExtraSpecs(json.RawMessage(extraspecs))
	if err != nil {
		return err
	}
	if providerConfig != "" {
		p.cfgFile = providerConfig
	}
	if err := p.init(ctx); err != nil {
		return err
	}
	regions, err := es.Regions(p.cfg.Region)
	if err != nil {
		return err
	}
	_, err = p.mgr.MIGRegions(ctx, regions, es.MIG.ExcludeRegions, p.cfg.Region, es.MIG.Name)
	return err
}

func (p *Provider) GetConfigJSONSchema(ctx context.Context) (string, error) {
	r := jsonschema.Reflector{AllowAdditionalProperties: false, ExpandedStruct: true, FieldNameTag: "toml"}
	b, err := json.Marshal(r.Reflect(config.Config{}))
	return string(b), err
}

func (p *Provider) GetExtraSpecsJSONSchema(ctx context.Context) (string, error) {
	b, err := json.Marshal(spec.JSONSchema())
	return string(b), err
}
