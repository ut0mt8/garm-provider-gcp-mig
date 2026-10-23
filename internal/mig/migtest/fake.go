// SPDX-License-Identifier: Apache-2.0

// Package migtest provides an in-memory fake of the Compute API used by the
// MIG manager, simulating a regional MIG.
package migtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/mig"
	"google.golang.org/protobuf/proto"
)

var _ mig.API = (*Fake)(nil)

// Fake: createInstances registers a pending managed instance; after
// CreateAfterPolls calls to ListManaged the VM appears in Zone.
type Fake struct {
	mu               sync.Mutex
	Zone             string
	CreateAfterPolls int    // -1 = never (no capacity)
	AttemptError     string // reported as the MIG's last attempt error
	SetLabelsFails   int    // number of SetLabels calls that fail before succeeding
	MIGs             map[string]*computepb.InstanceGroupManager

	polls           int
	CreatedMetadata map[string]map[string]string
	Managed         map[string]*computepb.ManagedInstance
	Instances       map[string]*computepb.Instance
	Configs         map[string]bool
	DeletedManaged  []string
	DeletedConfigs  []string
}

// SafeMIG returns a MIG configured as the provider requires.
func SafeMIG(name string) *computepb.InstanceGroupManager {
	return &computepb.InstanceGroupManager{
		Name:                    proto.String(name),
		InstanceLifecyclePolicy: &computepb.InstanceGroupManagerInstanceLifecyclePolicy{DefaultActionOnFailure: proto.String("DO_NOTHING")},
		UpdatePolicy: &computepb.InstanceGroupManagerUpdatePolicy{
			InstanceRedistributionType: proto.String("NONE"),
			Type:                       proto.String("OPPORTUNISTIC"),
		},
		Status: &computepb.InstanceGroupManagerStatus{},
	}
}

func New(migNames ...string) *Fake {
	f := &Fake{
		Zone:            "us-central1-b",
		MIGs:            map[string]*computepb.InstanceGroupManager{},
		CreatedMetadata: map[string]map[string]string{},
		Managed:         map[string]*computepb.ManagedInstance{},
		Instances:       map[string]*computepb.Instance{},
		Configs:         map[string]bool{},
	}
	for _, n := range migNames {
		f.MIGs[n] = SafeMIG(n)
	}
	return f
}

func (f *Fake) GetMIG(_ context.Context, project, region, name string) (*computepb.InstanceGroupManager, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	igm, ok := f.MIGs[name]
	if !ok {
		return nil, mig.ErrNotFound
	}
	return igm, nil
}

func (f *Fake) CreateInstance(_ context.Context, project, region, migName, name string, metadata map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.Managed[name]; exists {
		return fmt.Errorf("409 instance %s already exists", name)
	}
	f.CreatedMetadata[name] = metadata
	f.Configs[name] = true
	f.Managed[name] = &computepb.ManagedInstance{
		Name:          proto.String(name),
		CurrentAction: proto.String("CREATING"),
		Version:       &computepb.ManagedInstanceVersion{Name: proto.String(region + "/" + migName)},
	}
	return nil
}

func (f *Fake) ListManaged(_ context.Context, project, region, migName string) ([]*computepb.ManagedInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	var out []*computepb.ManagedInstance
	for name, mi := range f.Managed {
		url := fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s/instances/%s", project, f.Zone, name)
		if f.AttemptError != "" {
			mi.Instance = proto.String(url)
			mi.LastAttempt = &computepb.ManagedInstanceLastAttempt{Errors: &computepb.ManagedInstanceLastAttemptErrors{
				Errors: []*computepb.Errors{{Code: proto.String("ZONE_RESOURCE_POOL_EXHAUSTED"), Message: proto.String(f.AttemptError)}},
			}}
		}
		if f.CreateAfterPolls >= 0 && f.polls > f.CreateAfterPolls && f.Instances[name] == nil {
			mi.Instance = proto.String(url)
			mi.InstanceStatus = proto.String("PROVISIONING")
			mi.CurrentAction = proto.String("NONE")
			f.Instances[name] = &computepb.Instance{
				Name:             proto.String(name),
				Zone:             proto.String(fmt.Sprintf("https://www.googleapis.com/compute/v1/projects/%s/zones/%s", project, f.Zone)),
				Status:           proto.String("PROVISIONING"),
				Labels:           map[string]string{"managed-by": "garm"}, // from the template
				LabelFingerprint: proto.String("fp"),
				Disks:            []*computepb.AttachedDisk{{Architecture: proto.String("X86_64")}},
				Metadata: &computepb.Metadata{Items: []*computepb.Items{
					{Key: proto.String("created-by"), Value: proto.String(fmt.Sprintf("projects/123/regions/%s/instanceGroupManagers/%s", region, migName))},
					{Key: proto.String("runner_name"), Value: proto.String(f.CreatedMetadata[name]["runner_name"])},
				}},
			}
		}
		out = append(out, mi)
	}
	return out, nil
}

func (f *Fake) DeleteManaged(_ context.Context, project, region, migName string, urls []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeletedManaged = append(f.DeletedManaged, urls...)
	for _, u := range urls {
		name := u[strings.LastIndex(u, "/")+1:]
		delete(f.Instances, name)
		delete(f.Managed, name)
	}
	return nil
}

func (f *Fake) DeletePerInstanceConfigs(_ context.Context, project, region, migName string, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DeletedConfigs = append(f.DeletedConfigs, names...)
	for _, n := range names {
		if !f.Configs[n] {
			return mig.ErrNotFound
		}
		delete(f.Configs, n)
	}
	return nil
}

func (f *Fake) GetInstance(_ context.Context, project, zone, name string) (*computepb.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.Instances[name]
	if !ok || !strings.HasSuffix(inst.GetZone(), "/"+zone) {
		return nil, mig.ErrNotFound
	}
	return proto.Clone(inst).(*computepb.Instance), nil
}

func (f *Fake) SetLabels(_ context.Context, project, zone, name, fingerprint string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SetLabelsFails > 0 {
		f.SetLabelsFails--
		return errors.New("412 label fingerprint mismatch")
	}
	inst := f.Instances[name]
	if fingerprint != inst.GetLabelFingerprint() {
		return errors.New("412 label fingerprint mismatch")
	}
	inst.Labels = labels
	inst.LabelFingerprint = proto.String(fingerprint + "+")
	return nil
}

func (f *Fake) ListInstances(_ context.Context, project, filter string) ([]*computepb.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byLabel := strings.HasPrefix(filter, "labels.")
	key, value, _ := strings.Cut(strings.TrimPrefix(filter, "labels."), "=")
	var out []*computepb.Instance
	for _, inst := range f.Instances {
		if (byLabel && inst.Labels[key] == value) || (!byLabel && key == "name" && inst.GetName() == value) {
			out = append(out, proto.Clone(inst).(*computepb.Instance))
		}
	}
	return out, nil
}

func (f *Fake) StopInstance(_ context.Context, project, zone, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Instances[name].Status = proto.String("TERMINATED")
	return nil
}

func (f *Fake) StartInstance(_ context.Context, project, zone, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Instances[name].Status = proto.String("RUNNING")
	return nil
}
