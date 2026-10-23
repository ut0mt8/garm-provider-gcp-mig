// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/execution"
	"github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ut0mt8/garm-provider-gcp-mig/config"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/mig"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/mig/migtest"
	"github.com/ut0mt8/garm-provider-gcp-mig/internal/spec"
	"google.golang.org/protobuf/proto"
)

const migName = "production-shr-default-grp"

func setup(t *testing.T) (*migtest.Fake, string) {
	t.Helper()
	mig.PollInterval = time.Millisecond
	spec.DefaultToolFetch = func(params.OSType, params.OSArch, []params.RunnerApplicationDownload) (params.RunnerApplicationDownload, error) {
		return params.RunnerApplicationDownload{
			OS: proto.String("linux"), Architecture: proto.String("x64"),
			DownloadURL: proto.String("https://example.com/r.tgz"), Filename: proto.String("r.tgz"),
		}, nil
	}
	fake := migtest.New(migName)
	NewAPIFunc = func(context.Context, *config.Config) (mig.API, error) { return fake, nil }
	cfg := filepath.Join(t.TempDir(), "gcp-mig.toml")
	require.NoError(t, os.WriteFile(cfg, []byte("project_id = \"p\"\nregion = \"us-central1\"\n"), 0o600))
	return fake, cfg
}

func bootstrapJSON(t *testing.T) []byte {
	b, err := json.Marshal(params.BootstrapInstance{
		Name: "Garm-RUN1", OSType: params.Linux, OSArch: params.Amd64, PoolID: "pool-1",
		Flavor: "ignored", Image: "ignored",
		ExtraSpecs: json.RawMessage(`{"mig": {"name": "` + migName + `"}, "disable_updates": true}`),
	})
	require.NoError(t, err)
	return b
}

// runCommand executes one GARM command exactly like GARM does: env vars + stdin.
func runCommand(t *testing.T, iface, cmd, cfg, instanceID string, stdin []byte) (string, error) {
	t.Helper()
	t.Setenv("GARM_INTERFACE_VERSION", iface)
	t.Setenv("GARM_COMMAND", cmd)
	t.Setenv("GARM_CONTROLLER_ID", "ctrl-1")
	t.Setenv("GARM_POOL_ID", "pool-1")
	t.Setenv("GARM_PROVIDER_CONFIG_FILE", cfg)
	t.Setenv("GARM_INSTANCE_ID", instanceID)
	t.Setenv("GARM_POOL_EXTRASPECS", `{"mig": {"name": "`+migName+`"}}`)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, _ = w.Write(stdin)
	_ = w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	env, err := execution.GetEnvironment()
	if err != nil {
		return "", err
	}
	return env.Run(context.Background(), New(env.ProviderConfigFile, env.ControllerID))
}

func TestLifecycleThroughGARMDispatcher(t *testing.T) {
	for _, iface := range []string{common.Version010, common.Version011} {
		t.Run(iface, func(t *testing.T) {
			fake, cfg := setup(t)

			out, err := runCommand(t, iface, "CreateInstance", cfg, "", bootstrapJSON(t))
			require.NoError(t, err)
			var created params.ProviderInstance
			require.NoError(t, json.Unmarshal([]byte(out), &created))
			assert.Equal(t, "us-central1-b/garm-run1", created.ProviderID)
			assert.Equal(t, "Garm-RUN1", created.Name)
			assert.Equal(t, params.InstanceRunning, created.Status)

			out, err = runCommand(t, iface, "ListInstances", cfg, "", nil)
			require.NoError(t, err)
			assert.Contains(t, out, "us-central1-b/garm-run1")

			out, err = runCommand(t, iface, "GetInstance", cfg, created.ProviderID, nil)
			require.NoError(t, err)
			assert.Contains(t, out, `"name":"Garm-RUN1"`)

			_, err = runCommand(t, iface, "DeleteInstance", cfg, created.ProviderID, nil)
			require.NoError(t, err)
			assert.Empty(t, fake.Instances)
			assert.Empty(t, fake.Configs, "per-instance config removed")

			_, err = runCommand(t, iface, "GetInstance", cfg, created.ProviderID, nil)
			require.Error(t, err)
			assert.Equal(t, common.ExitCodeNotFound, common.ResolveErrorToExitCode(err), "GARM sees the runner as gone")
		})
	}
}

func TestV011SchemasAndValidation(t *testing.T) {
	fake, cfg := setup(t)
	p := New(cfg, "ctrl-1")
	ctx := context.Background()

	assert.Equal(t, []string{"v0.1.0", "v0.1.1"}, p.GetSupportedInterfaceVersions(ctx))

	cfgSchema, err := p.GetConfigJSONSchema(ctx)
	require.NoError(t, err)
	assert.Contains(t, cfgSchema, "project_id")

	esSchema, err := p.GetExtraSpecsJSONSchema(ctx)
	require.NoError(t, err)
	assert.Contains(t, esSchema, "create_timeout_minutes")

	require.NoError(t, p.ValidatePoolInfo(ctx, "img", "flavor", cfg, `{"mig": {"name": "`+migName+`"}}`))
	assert.ErrorContains(t, p.ValidatePoolInfo(ctx, "", "", cfg, `{"mig": {"name": "missing-grp"}}`), "not found")
	assert.Error(t, p.ValidatePoolInfo(ctx, "", "", cfg, `{"disksize": 10}`))

	fake.MIGs["us-central1/"+migName].InstanceLifecyclePolicy.DefaultActionOnFailure = proto.String("REPAIR")
	assert.ErrorContains(t, p.ValidatePoolInfo(ctx, "", "", cfg, `{"mig": {"name": "`+migName+`"}}`), "DO_NOTHING")
}

func TestValidatePoolInfoMultiRegion(t *testing.T) {
	fake, cfg := setup(t)
	fake.RegionList = []string{"us-central1", "us-east1", "europe-west1"}
	fake.AddMIG("us-east1", "east-only-grp", migtest.Region{Zone: "us-east1-b"})
	p := New(cfg, "ctrl-1")
	ctx := context.Background()

	require.NoError(t, p.ValidatePoolInfo(ctx, "", "", cfg, `{"mig": {"name": "east-only-grp", "regions": ["us"]}}`), "regions without the MIG are skipped")
	assert.ErrorContains(t, p.ValidatePoolInfo(ctx, "", "", cfg, `{"mig": {"name": "east-only-grp", "regions": ["europe"]}}`), "not found")
	assert.ErrorContains(t, p.ValidatePoolInfo(ctx, "", "", cfg, `{"mig": {"name": "east-only-grp", "regions": ["us-south9"]}}`), "us-south9")

	fake.MIGs["us-east1/east-only-grp"].UpdatePolicy.Type = proto.String("PROACTIVE")
	assert.ErrorContains(t, p.ValidatePoolInfo(ctx, "", "", cfg, `{"mig": {"name": "east-only-grp", "regions": ["us"]}}`), "PROACTIVE")
}

func TestSchemaCommandsNeedNoCredentials(t *testing.T) {
	NewAPIFunc = func(context.Context, *config.Config) (mig.API, error) {
		t.Fatal("schema commands must not create API clients")
		return nil, nil
	}
	p := New("/nonexistent.toml", "c")
	_, err := p.GetExtraSpecsJSONSchema(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Version, p.GetVersion(context.Background()))
}
