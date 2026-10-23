// SPDX-License-Identifier: Apache-2.0

// Package mig creates GARM runners through regional managed instance groups.
//
// Each runner is one managed instance created with createInstances, with its
// own name and per-instance metadata (the cloud-init user data carrying the
// runner's single-use registration). The MIG's instance template and instance
// flexibility policy decide machine type, zone, image, disks and scheduling.
//
// With several candidate regions (mig.regions), the runner is created in the
// first region whose MIG has capacity: a region that doesn't create the VM
// within region_timeout_minutes is cleaned up and the next one is tried.
//
// The MIG must never act on its own (see MIGRegions): no autoscaler, default
// action on VM failure DO_NOTHING, instance redistribution NONE, no
// PROACTIVE updates.
package mig

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/geo"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/spec"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/util"
)

const (
	linuxUserData     = "user-data"
	windowsStartup    = "sysprep-specialize-script-ps1"
	metadataCreatedBy = "created-by"
)

var (
	// PollInterval is how often the MIG is polled while a VM is being created.
	PollInterval = 5 * time.Second
	// TimeoutUnit is the unit of the extra specs' *_timeout_minutes (shortened in tests).
	TimeoutUnit = time.Minute
	// ErrNotMIGManaged means the instance exists but no regional MIG owns it.
	ErrNotMIGManaged = errors.New("instance is not managed by a regional MIG")
)

type Manager struct {
	api     API
	project string
}

func NewManager(api API, project string) *Manager {
	return &Manager{api: api, project: project}
}

// MIGRegions resolves the location selectors (home = provider config region)
// and returns, in order, the regions where the MIG exists. Regions without the
// MIG are skipped; a MIG that could act on its own and fight GARM is refused.
func (m *Manager) MIGRegions(ctx context.Context, selectors []string, home, name string) ([]string, error) {
	candidates := selectors
	if len(selectors) != 1 || !geo.IsRegion(selectors[0]) {
		live, err := m.api.ListRegions(ctx, m.project)
		if err != nil {
			return nil, fmt.Errorf("failed to list regions: %w", err)
		}
		countries := func() (map[string]string, error) { return m.api.RegionCountries(ctx, m.project) }
		if candidates, err = geo.Resolve(selectors, live, home, countries); err != nil {
			return nil, err
		}
	}
	var found []string
	for _, region := range candidates {
		igm, err := m.api.GetMIG(ctx, m.project, region, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to get MIG %s in %s: %w", name, region, err)
		}
		if err := checkMIGSettings(igm); err != nil {
			return nil, fmt.Errorf("%w (region %s)", err, region)
		}
		found = append(found, region)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("regional MIG %s not found in %s/%s", name, m.project, strings.Join(candidates, ","))
	}
	return found, nil
}

func checkMIGSettings(igm *computepb.InstanceGroupManager) error {
	var problems []string
	if igm.GetStatus().GetAutoscaler() != "" {
		problems = append(problems, "an autoscaler is attached (GARM must decide the size)")
	}
	if a := igm.GetInstanceLifecyclePolicy().GetDefaultActionOnFailure(); a != "DO_NOTHING" {
		problems = append(problems, fmt.Sprintf("default action on VM failure is %q, must be DO_NOTHING (a repaired runner reruns a used registration)", a))
	}
	if r := igm.GetUpdatePolicy().GetInstanceRedistributionType(); r != "NONE" {
		problems = append(problems, fmt.Sprintf("instance redistribution is %q, must be NONE (could delete busy runners)", r))
	}
	if t := igm.GetUpdatePolicy().GetType(); t == "PROACTIVE" {
		problems = append(problems, "update policy is PROACTIVE, must be OPPORTUNISTIC (could replace busy runners)")
	}
	if len(igm.GetAutoHealingPolicies()) > 0 {
		problems = append(problems, "autohealing health checks are configured (could recreate runners)")
	}
	if len(problems) > 0 {
		return fmt.Errorf("MIG %s is not safe for GARM: %s", igm.GetName(), strings.Join(problems, "; "))
	}
	return nil
}

// Create asks the MIG for one VM named after the runner, waits until it
// exists, labels it for GARM and returns it. With several regions, each but
// the last gets region_timeout_minutes before the next one is tried.
func (m *Manager) Create(ctx context.Context, rs *spec.RunnerSpec) (*computepb.Instance, error) {
	regions, err := m.MIGRegions(ctx, rs.Regions, rs.HomeRegion, rs.MIG.Name)
	if err != nil {
		return nil, err
	}
	name := util.InstanceName(rs.BootstrapParams.Name)

	udata, err := rs.ComposeUserData()
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{
		startupKey(rs.BootstrapParams.OSType): udata,
		util.MetadataRunnerName:               rs.BootstrapParams.Name,
	}
	if rs.SSHKeys != "" {
		metadata["ssh-keys"] = rs.SSHKeys
	}

	var errs []error
	for i, region := range regions {
		timeout := rs.MIG.RegionTimeout()
		if i == len(regions)-1 {
			timeout = rs.MIG.CreateTimeout()
		}
		inst, err := m.createIn(ctx, rs, region, name, metadata, time.Duration(timeout)*TimeoutUnit)
		if err == nil {
			return inst, nil
		}
		if len(regions) == 1 {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", region, err))
		if ctx.Err() != nil {
			break
		}
		if i < len(regions)-1 {
			fmt.Fprintf(os.Stderr, "create %s in %s failed, trying %s: %v\n", name, region, regions[i+1], err)
		}
	}
	return nil, fmt.Errorf("failed to create %s in any region of MIG %s: %w", name, rs.MIG.Name, errors.Join(errs...))
}

// createIn creates the runner in one region's MIG, cleaning up on failure.
func (m *Manager) createIn(ctx context.Context, rs *spec.RunnerSpec, region, name string, metadata map[string]string, timeout time.Duration) (*computepb.Instance, error) {
	if err := m.api.CreateInstance(ctx, m.project, region, rs.MIG.Name, name, metadata); err != nil {
		// The operation may have stored the per-instance config anyway; the MIG
		// would then create the VM later with the same runner registration.
		m.cleanup(context.WithoutCancel(ctx), region, rs.MIG.Name, "", name)
		return nil, fmt.Errorf("failed to create instance %s in MIG %s: %w", name, rs.MIG.Name, err)
	}

	// Attempt errors (e.g. ZONE_RESOURCE_POOL_EXHAUSTED) are normal while a
	// flexibility policy falls back to other machine types or zones: only the
	// timeout ends the wait.
	zone, err := m.waitForVM(ctx, region, rs.MIG.Name, name, timeout)
	if err != nil {
		// Don't leave a half-created runner (or a pending per-instance config) behind.
		m.cleanup(context.WithoutCancel(ctx), region, rs.MIG.Name, zone, name)
		return nil, err
	}

	inst, err := m.labelInstance(ctx, zone, name, rs.Labels)
	if err != nil {
		m.cleanup(context.WithoutCancel(ctx), region, rs.MIG.Name, zone, name)
		return nil, fmt.Errorf("failed to label instance %s: %w", name, err)
	}
	return inst, nil
}

// waitForVM polls the MIG until the managed instance has a VM, and returns its zone.
func (m *Manager) waitForVM(ctx context.Context, region, migName, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr, zone string
	for {
		managed, err := m.api.ListManaged(ctx, m.project, region, migName)
		if err != nil {
			return zone, fmt.Errorf("failed to list managed instances of %s: %w", migName, err)
		}
		for _, mi := range managed {
			if mi.GetName() != name {
				continue
			}
			if z := util.ZoneFromURL(mi.GetInstance()); z != "" {
				zone = z
			}
			if msg := lastAttemptError(mi); msg != "" {
				lastErr = msg
			}
			switch mi.GetInstanceStatus() {
			case "PROVISIONING", "STAGING", "RUNNING":
				if zone != "" {
					return zone, nil
				}
			}
		}
		if time.Now().After(deadline) {
			if lastErr != "" {
				return zone, fmt.Errorf("MIG %s did not create %s within %s, last error: %s", migName, name, timeout, lastErr)
			}
			return zone, fmt.Errorf("MIG %s did not create %s within %s", migName, name, timeout)
		}
		select {
		case <-ctx.Done():
			return zone, ctx.Err()
		case <-time.After(PollInterval):
		}
	}
}

// labelInstance merges labels into the VM's labels (keeping the template's),
// retrying on fingerprint conflicts, and returns the refreshed instance.
func (m *Manager) labelInstance(ctx context.Context, zone, name string, labels map[string]string) (*computepb.Instance, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(PollInterval)
		}
		inst, err := m.api.GetInstance(ctx, m.project, zone, name)
		if err != nil {
			lastErr = err
			continue
		}
		merged := map[string]string{}
		maps.Copy(merged, inst.GetLabels())
		maps.Copy(merged, labels)
		if err := m.api.SetLabels(ctx, m.project, zone, name, inst.GetLabelFingerprint(), merged); err != nil {
			lastErr = err
			continue
		}
		return m.api.GetInstance(ctx, m.project, zone, name)
	}
	return nil, lastErr
}

// cleanup removes a runner from its MIG: the VM if it exists, then its per-instance config.
func (m *Manager) cleanup(ctx context.Context, region, migName, zone, name string) {
	if zone != "" {
		url := fmt.Sprintf("zones/%s/instances/%s", zone, name)
		if err := m.api.DeleteManaged(ctx, m.project, region, migName, []string{url}); err != nil && !errors.Is(err, ErrNotFound) {
			fmt.Fprintf(os.Stderr, "cleanup: failed to delete %s from MIG %s: %v\n", name, migName, err)
		}
	}
	if err := m.api.DeletePerInstanceConfigs(ctx, m.project, region, migName, []string{name}); err != nil && !errors.Is(err, ErrNotFound) {
		fmt.Fprintf(os.Stderr, "cleanup: failed to delete per-instance config %s in MIG %s: %v\n", name, migName, err)
	}
}

// Get returns a runner by provider ID ("zone/name").
func (m *Manager) Get(ctx context.Context, providerID string) (*computepb.Instance, error) {
	zone, name, ok := util.SplitProviderID(providerID)
	if !ok {
		return nil, fmt.Errorf("invalid provider ID %q, expected zone/name", providerID)
	}
	inst, err := m.api.GetInstance(ctx, m.project, zone, name)
	if err != nil {
		return nil, err
	}
	if _, _, ok := migFromInstance(inst); !ok {
		return nil, ErrNotMIGManaged
	}
	return inst, nil
}

// Delete removes the runner through its MIG (deleting the VM directly would
// make the MIG recreate it). A runner that no longer exists is not an error.
func (m *Manager) Delete(ctx context.Context, providerID string) error {
	zone, name, ok := util.SplitProviderID(providerID)
	if !ok {
		// GARM passes the runner name when a create failed before returning a
		// provider ID; find the VM by name, if it got that far.
		found, err := m.api.ListInstances(ctx, m.project, fmt.Sprintf("name=%s", providerID))
		if err != nil {
			return fmt.Errorf("failed to list instances: %w", err)
		}
		if len(found) == 0 {
			return nil
		}
		zone, name = util.ZoneFromURL(found[0].GetZone()), found[0].GetName()
	}
	inst, err := m.api.GetInstance(ctx, m.project, zone, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get instance %s: %w", providerID, err)
	}
	region, migName, ok := migFromInstance(inst)
	if !ok {
		// Never delete a VM this provider doesn't own.
		return fmt.Errorf("%s: %w", providerID, ErrNotMIGManaged)
	}
	url := fmt.Sprintf("zones/%s/instances/%s", zone, name)
	if err := m.api.DeleteManaged(ctx, m.project, region, migName, []string{url}); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("failed to delete %s from MIG %s: %w", name, migName, err)
	}
	if err := m.api.DeletePerInstanceConfigs(ctx, m.project, region, migName, []string{name}); err != nil && !errors.Is(err, ErrNotFound) {
		// The VM is gone; a leftover config only matters if the name is reused.
		fmt.Fprintf(os.Stderr, "failed to delete per-instance config %s in MIG %s: %v\n", name, migName, err)
	}
	return nil
}

// List returns the runners of a pool (or scale set).
func (m *Manager) List(ctx context.Context, poolID string) ([]*computepb.Instance, error) {
	return m.listByLabel(ctx, spec.LabelPoolID, poolID)
}

// RemoveAll deletes every runner of this controller.
func (m *Manager) RemoveAll(ctx context.Context, controllerID string) error {
	all, err := m.listByLabel(ctx, spec.LabelControllerID, controllerID)
	if err != nil {
		return err
	}
	var errs []error
	for _, inst := range all {
		if err := m.Delete(ctx, util.ProviderID(util.ZoneFromURL(inst.GetZone()), inst.GetName())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) listByLabel(ctx context.Context, key, value string) ([]*computepb.Instance, error) {
	all, err := m.api.ListInstances(ctx, m.project, fmt.Sprintf("labels.%s=%s", key, value))
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}
	var out []*computepb.Instance
	for _, inst := range all {
		if inst.GetLabels()[spec.LabelMIG] != "" { // only runners created by this provider
			out = append(out, inst)
		}
	}
	return out, nil
}

// Stop / Start act on the VM directly: the MIG keeps it as a stopped member.
func (m *Manager) Stop(ctx context.Context, providerID string) error {
	inst, err := m.Get(ctx, providerID)
	if err != nil {
		return err
	}
	return m.api.StopInstance(ctx, m.project, util.ZoneFromURL(inst.GetZone()), inst.GetName())
}

func (m *Manager) Start(ctx context.Context, providerID string) error {
	inst, err := m.Get(ctx, providerID)
	if err != nil {
		return err
	}
	return m.api.StartInstance(ctx, m.project, util.ZoneFromURL(inst.GetZone()), inst.GetName())
}

// migFromInstance reads the "created-by" metadata GCE sets on MIG members:
// projects/<num>/regions/<region>/instanceGroupManagers/<name>
func migFromInstance(inst *computepb.Instance) (region, name string, ok bool) {
	for _, item := range inst.GetMetadata().GetItems() {
		if item.GetKey() != metadataCreatedBy {
			continue
		}
		parts := strings.Split(item.GetValue(), "/")
		for i := 0; i+3 < len(parts); i++ {
			if parts[i] == "regions" && parts[i+2] == "instanceGroupManagers" {
				return parts[i+1], parts[i+3], true
			}
		}
	}
	return "", "", false
}

func lastAttemptError(mi *computepb.ManagedInstance) string {
	var msgs []string
	for _, e := range mi.GetLastAttempt().GetErrors().GetErrors() {
		msgs = append(msgs, fmt.Sprintf("%s: %s", e.GetCode(), e.GetMessage()))
	}
	return strings.Join(msgs, "; ")
}

func startupKey(osType params.OSType) string {
	if osType == params.Windows {
		return windowsStartup
	}
	return linuxUserData
}
