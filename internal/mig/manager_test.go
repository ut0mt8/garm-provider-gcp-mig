// SPDX-License-Identifier: Apache-2.0

package mig_test

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/mig"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/mig/migtest"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/spec"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/util"
	"google.golang.org/protobuf/proto"
)

const migName = "production-shr-large-grp"

func init() { mig.PollInterval = time.Millisecond }

func runnerSpec() *spec.RunnerSpec {
	return &spec.RunnerSpec{
		BootstrapParams: params.BootstrapInstance{Name: "Garm-ABCD1234", OSType: params.Linux, OSArch: params.Amd64, PoolID: "pool-1"},
		Tools: params.RunnerApplicationDownload{
			OS: proto.String("linux"), Architecture: proto.String("x64"),
			DownloadURL: proto.String("https://example.com/runner.tgz"), Filename: proto.String("runner.tgz"),
		},
		MIG:    spec.MIG{Name: migName, CreateTimeoutMinutes: 1},
		Region: "us-central1",
		Labels: map[string]string{"garmpoolid": "pool-1", "garmcontrollerid": "ctrl-1", "ostype": "linux", "garmmig": migName},
	}
}

func TestCreateWaitsLabelsAndReturnsInstance(t *testing.T) {
	f := migtest.New(migName)
	f.CreateAfterPolls = 2
	m := mig.NewManager(f, "p")

	inst, err := m.Create(context.Background(), runnerSpec())
	require.NoError(t, err)

	assert.Equal(t, "garm-abcd1234", inst.GetName(), "VM named after the runner (lowercase)")
	md := f.CreatedMetadata["garm-abcd1234"]
	assert.Contains(t, md["user-data"], "#cloud-config", "GARM cloud-init in per-instance metadata")
	assert.Equal(t, "Garm-ABCD1234", md["runner_name"])

	labels := inst.GetLabels()
	assert.Equal(t, "garm", labels["managed-by"], "template labels kept")
	assert.Equal(t, "pool-1", labels["garmpoolid"])
	assert.Equal(t, "ctrl-1", labels["garmcontrollerid"])
	assert.Equal(t, migName, labels["garmmig"])

	pi, err := util.ToProviderInstance(inst)
	require.NoError(t, err)
	assert.Equal(t, "us-central1-b/garm-abcd1234", pi.ProviderID, "provider ID carries the zone the MIG picked")
	assert.Equal(t, "Garm-ABCD1234", pi.Name)
	assert.Equal(t, params.InstanceRunning, pi.Status)
}

func TestCreateRefusesUnsafeMIG(t *testing.T) {
	for name, mutate := range map[string]func(*computepb.InstanceGroupManager){
		"autoscaler": func(g *computepb.InstanceGroupManager) { g.Status.Autoscaler = proto.String("autoscalers/x") },
		"repair": func(g *computepb.InstanceGroupManager) {
			g.InstanceLifecyclePolicy.DefaultActionOnFailure = proto.String("REPAIR")
		},
		"redistribution": func(g *computepb.InstanceGroupManager) {
			g.UpdatePolicy.InstanceRedistributionType = proto.String("PROACTIVE")
		},
		"proactive update": func(g *computepb.InstanceGroupManager) { g.UpdatePolicy.Type = proto.String("PROACTIVE") },
		"autohealing": func(g *computepb.InstanceGroupManager) {
			g.AutoHealingPolicies = []*computepb.InstanceGroupManagerAutoHealingPolicy{{HealthCheck: proto.String("hc")}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := migtest.New(migName)
			mutate(f.MIGs[migName])
			_, err := mig.NewManager(f, "p").Create(context.Background(), runnerSpec())
			assert.ErrorContains(t, err, "not safe for GARM")
			assert.Empty(t, f.CreatedMetadata, "nothing created")
		})
	}
}

func TestCreateMissingMIG(t *testing.T) {
	f := migtest.New()
	_, err := mig.NewManager(f, "p").Create(context.Background(), runnerSpec())
	assert.ErrorContains(t, err, "not found")
}

func TestCreateRetriesLabelFingerprintConflicts(t *testing.T) {
	f := migtest.New(migName)
	f.SetLabelsFails = 2
	inst, err := mig.NewManager(f, "p").Create(context.Background(), runnerSpec())
	require.NoError(t, err)
	assert.Equal(t, migName, inst.GetLabels()["garmmig"])
}

func TestCreateNoCapacityTimesOutAndCleansUp(t *testing.T) {
	f := migtest.New(migName)
	f.CreateAfterPolls = -1
	f.AttemptError = "no n2d/n2/e2 capacity in us-central1"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := mig.NewManager(f, "p").Create(ctx, runnerSpec())
	require.Error(t, err)
	assert.Contains(t, f.DeletedManaged, "zones/us-central1-b/instances/garm-abcd1234", "pending VM removed from the MIG")
	assert.Contains(t, f.DeletedConfigs, "garm-abcd1234", "per-instance config removed")
}

func TestDeleteGoesThroughMIG(t *testing.T) {
	f := migtest.New(migName)
	m := mig.NewManager(f, "p")
	_, err := m.Create(context.Background(), runnerSpec())
	require.NoError(t, err)

	require.NoError(t, m.Delete(context.Background(), "us-central1-b/garm-abcd1234"))
	assert.Equal(t, []string{"zones/us-central1-b/instances/garm-abcd1234"}, f.DeletedManaged)
	assert.Equal(t, []string{"garm-abcd1234"}, f.DeletedConfigs)
	assert.Empty(t, f.Configs)

	require.NoError(t, m.Delete(context.Background(), "us-central1-b/garm-abcd1234"), "already gone is not an error")
}

func TestNeverTouchesForeignVMs(t *testing.T) {
	f := migtest.New(migName)
	f.Instances["standalone"] = &computepb.Instance{Name: proto.String("standalone"), Zone: proto.String("zones/us-central1-a")}
	f.Instances["zonal-mig-member"] = &computepb.Instance{
		Name: proto.String("zonal-mig-member"), Zone: proto.String("zones/us-central1-a"),
		Metadata: &computepb.Metadata{Items: []*computepb.Items{
			{Key: proto.String("created-by"), Value: proto.String("projects/1/zones/us-central1-a/instanceGroupManagers/other")},
		}},
	}
	m := mig.NewManager(f, "p")
	assert.ErrorIs(t, m.Delete(context.Background(), "us-central1-a/standalone"), mig.ErrNotMIGManaged)
	assert.ErrorIs(t, m.Delete(context.Background(), "us-central1-a/zonal-mig-member"), mig.ErrNotMIGManaged)
	_, err := m.Get(context.Background(), "us-central1-a/standalone")
	assert.ErrorIs(t, err, mig.ErrNotMIGManaged)
	assert.Empty(t, f.DeletedManaged)
}

// GARM passes the runner name when a create failed before it got a provider
// ID; the delete must find the VM by name or report it gone, or GARM retries
// it forever.
func TestDeleteByRunnerName(t *testing.T) {
	f := migtest.New(migName)
	m := mig.NewManager(f, "p")
	require.NoError(t, m.Delete(context.Background(), "garm-nevercreated"), "no VM is not an error")
	assert.Empty(t, f.DeletedManaged)

	_, err := m.Create(context.Background(), runnerSpec())
	require.NoError(t, err)
	require.NoError(t, m.Delete(context.Background(), "garm-abcd1234"))
	assert.Equal(t, []string{"zones/us-central1-b/instances/garm-abcd1234"}, f.DeletedManaged)
}

func TestListAndRemoveAll(t *testing.T) {
	f := migtest.New(migName)
	m := mig.NewManager(f, "p")
	_, err := m.Create(context.Background(), runnerSpec())
	require.NoError(t, err)
	// Same pool label but not created by this provider (no garmmig label): ignored.
	f.Instances["other"] = &computepb.Instance{Name: proto.String("other"), Labels: map[string]string{"garmpoolid": "pool-1", "garmcontrollerid": "ctrl-1"}}

	list, err := m.List(context.Background(), "pool-1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "garm-abcd1234", list[0].GetName())

	require.NoError(t, m.RemoveAll(context.Background(), "ctrl-1"))
	assert.Equal(t, []string{"zones/us-central1-b/instances/garm-abcd1234"}, f.DeletedManaged)
	assert.Contains(t, f.Instances, "other")
}

func TestStopStart(t *testing.T) {
	f := migtest.New(migName)
	m := mig.NewManager(f, "p")
	_, err := m.Create(context.Background(), runnerSpec())
	require.NoError(t, err)
	require.NoError(t, m.Stop(context.Background(), "us-central1-b/garm-abcd1234"))
	assert.Equal(t, "TERMINATED", f.Instances["garm-abcd1234"].GetStatus())
	require.NoError(t, m.Start(context.Background(), "us-central1-b/garm-abcd1234"))
	assert.Equal(t, "RUNNING", f.Instances["garm-abcd1234"].GetStatus())
}
