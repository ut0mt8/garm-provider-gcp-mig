# garm-provider-gcp-mig

A [GARM](https://github.com/cloudbase/garm) external provider that creates GitHub Actions runners through **regional managed instance groups** (MIGs) on Google Cloud.

Each runner is one VM created by a MIG, so a pool or scale set gets everything a MIG and an instance template offer:

- **several machine types** behind one label (MIG instance flexibility, with preference ranks);
- **any zone of the region** with capacity, and optionally **other regions** (`mig.regions`);
- **Spot VMs**, GPUs, local SSDs, minimum CPU platform: anything an instance template supports.

It runs **next to** the official [`garm-provider-gcp`](https://github.com/cloudbase/garm-provider-gcp), which keeps handling standalone VMs. Each pool or scale set picks one with `--provider-name`.

## How it works

| GARM command | What the provider does |
|---|---|
| CreateInstance | checks the MIG is safe (below); `createInstances` with the runner's name and per-instance metadata (`user-data` = GARM's cloud-init, `runner_name`); waits for the MIG to create the VM; labels it `garmpoolid`, `garmcontrollerid`, `ostype`, `garmmig` |
| GetInstance | `instances.get`; a missing VM is reported as *not found* (exit code 30) so GARM treats the runner as gone |
| DeleteInstance | `deleteInstances` on the MIG found in the VM's `created-by` metadata, then deletes the per-instance config. Never deletes a VM that no regional MIG owns |
| ListInstances | VMs labelled with the pool ID and `garmmig` |
| RemoveAllInstances | deletes every runner labelled with this controller's ID |
| Stop / Start | on the VM |
| ValidatePoolInfo (v0.1.1) | when a pool or scale set is created/updated: validates the extra specs **and** checks the MIG exists (in at least one of its regions) and is safe |
| Get*JSONSchema (v0.1.1) | publishes the config and extra specs schemas |

Provider ID: `<zone>/<instance-name>`. Supports GARM provider interfaces **v0.1.0 and v0.1.1**.

If the MIG can't create the VM within `create_timeout_minutes` (no capacity for any allowed machine type in any zone), the provider removes the pending instance and its per-instance config and returns the MIG's last error; GARM retries later.

With `mig.regions`, that happens per region: a region that doesn't create the VM within `region_timeout_minutes` is cleaned up the same way and the next region is tried; the last one gets `create_timeout_minutes`. Attempt errors such as `ZONE_RESOURCE_POOL_EXHAUSTED` alone don't move on: a MIG with a flexibility policy reports them while it falls back to other machine types and zones.

## Provider config

```toml
project_id = "my-project"
region = "us-central1"     # default MIG region, overridable per pool
credentials_file = ""      # empty = application default credentials
```

## Extra specs

```json
{
  "mig": {
    "name": "production-shr-large-grp",
    "region": "us-central1",
    "create_timeout_minutes": 10
  },
  "disable_updates": true,
  "custom_labels": {"runner-pool": "production-shr-large"},
  "pre_install_scripts": {"10-setup.sh": "<base64>"}
}
```

| Key | |
|---|---|
| `mig.name` | **required**, regional MIG name |
| `mig.region` | defaults to the provider config `region` |
| `mig.regions` | ordered location selectors, instead of `mig.region` (see below) |
| `mig.create_timeout_minutes` | 1–120, default 10 (with `mig.regions`: for the last region) |
| `mig.region_timeout_minutes` | 1–120, default 3: wait in one region before trying the next |
| `mig.exclude_regions` | location selectors (same forms as `mig.regions`) never tried |
| `custom_labels` | extra VM labels (the four provider labels are reserved) |
| `ssh_keys`, `enable_boot_debug`, `disable_updates` | as in the official provider |
| `runner_install_template`, `pre_install_scripts`, `extra_context` | cloud-init options from `garm-provider-common` |

### Several regions

```json
{"mig": {"name": "production-shr-large-grp", "regions": ["us-east1", "northamerica"], "region_timeout_minutes": 3}}
```

Each selector is one of:

| Selector | Regions |
|---|---|
| `us-central1` | that region |
| `us` | `us-*` |
| `northamerica` | `us-*`, `northamerica-*` (US, Canada, Mexico) |
| `southamerica`, `europe`, `asia`, `australia`, `me`, `africa` | `southamerica-*`, `europe-*`, … (`me` = Middle East) |
| `eu` | regions in an EU member state (as Cloud Storage's EU multi-region: not London `europe-west2` nor Zurich `europe-west6`) |
| `country:CA` | regions in that country (ISO 3166-1 alpha-2, uppercase) |

Regions are tried in selector order, without duplicates. A geography, `eu` or `country:` expands to the matching regions that are `UP`, the provider config `region` first if it matches, then alphabetically. **Regions without the MIG are skipped**, so a geography can be used while the MIG (same name) exists only in some regions; every MIG found must be safe (below). An explicit region that doesn't exist, or a selector that matches nothing, is an error.

`mig.exclude_regions` takes the same selectors and removes their regions, e.g. `"regions": ["us"], "exclude_regions": ["us-east4"]` for a region where the project can't get quota, or `["country:CA"]`. An exclusion that matches nothing is ignored; excluding every region is an error.

`eu` and `country:` need the Cloud Location Finder API (`cloudlocationfinder.googleapis.com`) enabled on the project and the `cloudlocationfinder.cloudLocations.list` permission.

GARM's provider `exec_timeout_seconds` must exceed the worst case: (number of regions with the MIG − 1) × `region_timeout_minutes` + `create_timeout_minutes`.

Anything about the VM itself (machine type, image, disks, network, service account, Spot) is **not** an extra spec: it comes from the MIG. The pool/scale set `flavor` and `image` are required by GARM but **ignored**: set descriptive values.

## MIG requirements

The provider refuses (at pool creation with v0.1.1, and before each runner) a MIG that could act on its own and fight GARM:

| Requirement | Why |
|---|---|
| Regional MIG | instance flexibility needs one; zonal MIG members are never touched |
| No autoscaler | GARM decides how many runners exist |
| Default action on VM failure `DO_NOTHING` | otherwise a crashed or preempted VM is recreated with the same per-instance config: cloud-init reruns an already-used registration |
| Instance redistribution `NONE` | never delete a busy runner to rebalance zones |
| Update policy not `PROACTIVE` | a new template must only apply to new runners |
| No autohealing health checks | same as repair |

Example:

```bash
gcloud compute instance-templates create shr-large-v1 \
  --machine-type=n2d-standard-8 \
  --image-family=shr-runner --image-project=my-project \
  --provisioning-model=SPOT --instance-termination-action=STOP \
  --service-account=<RUNNER_SA> --scopes=cloud-platform \
  --network=<NETWORK> --subnet=<SUBNET> --labels=managed-by=garm

gcloud compute instance-groups managed create production-shr-large-grp \
  --region=us-central1 --zones=us-central1-a,us-central1-b,us-central1-c,us-central1-f \
  --template=shr-large-v1 --size=0 \
  --target-distribution-shape=ANY \
  --instance-redistribution-type=NONE \
  --default-action-on-vm-failure=do-nothing \
  --instance-selection=name=preferred,machine-type=n2d-standard-8,machine-type=n2-standard-8,rank=1 \
  --instance-selection=name=fallback,machine-type=e2-standard-8,rank=2
```

- Image: must include **cloud-init** (Ubuntu GCE images do). Put your tooling in the image, not the runner registration.
- Template: no `user-data` metadata (set per runner).
- Spot: `--instance-termination-action=STOP` keeps a preempted runner stopped; GARM sees it stopped and deletes it through the MIG.
- Keep machine types with the same vCPU count across selections.
- `gcloud` flexibility flags changed across versions: check `gcloud compute instance-groups managed create --help`.

## Using it in GARM

```toml
[[provider]]
name = "gcp"                       # official provider, standalone VMs
provider_type = "external"
  [provider.external]
  provider_executable = "/opt/garm/providers.d/garm-provider-gcp"
  config_file = "/etc/garm-provider/garm-provider-gcp.toml"

[[provider]]
name = "gcp-mig"                   # this provider
provider_type = "external"
  [provider.external]
  provider_executable = "/opt/garm/providers.d/garm-provider-gcp-mig"
  config_file = "/etc/garm-provider-mig/garm-provider-gcp-mig.toml"
  interface_version = "v0.1.1"     # check the key name for your GARM version
```

```bash
garm-cli pool add --org <ORG_ID> --provider-name gcp-mig \
  --tags production-shr-large --flavor from-mig --image from-mig \
  --os-type linux --os-arch amd64 --min-idle-runners 0 --max-runners 40 \
  --extra-specs '{"mig": {"name": "production-shr-large-grp"}, "disable_updates": true}' \
  --enabled=true
```

Scale sets: same extra specs. GARM changed how scale sets are identified towards providers in September 2026 (the pool ID stored in `garmpoolid`); **pin the GARM version and drain scale sets before upgrading GARM**.

IAM for GARM's service account: `roles/compute.instanceAdmin.v1` on the project, `roles/iam.serviceAccountUser` on the runner service account; for `eu`/`country:` selectors, also `cloudlocationfinder.cloudLocations.list`.

## Build and release

```bash
make test        # go test -race ./...
make build       # bin/garm-provider-gcp-mig
make release VERSION=v0.1.0   # dist/*.tar.gz + SHA256SUMS (linux amd64/arm64)
```

Pushing a `v*` tag runs `.github/workflows/release.yml`: tests, GitHub release with the archives, and a `ghcr.io/ut0mt8/garm-provider-gcp-mig:<tag>` image containing only the binary (`/garm-provider-gcp-mig`), to copy into a GARM image:

```dockerfile
COPY --from=ghcr.io/ut0mt8/garm-provider-gcp-mig:v0.1.0 /garm-provider-gcp-mig /opt/garm/providers.d/garm-provider-gcp-mig
```

Requires Go 1.26. Install without cloning: `go install github.com/ut0mt8/garm-provider-gcp-mig@latest` (after the first tag).

## Tests

Unit tests use an in-memory fake of the Compute API (`internal/mig/migtest`) and drive the provider through GARM's own command dispatcher (env vars + stdin), for both interface versions. **Not yet verified against real GCP**, to check in a proof of concept:

1. `createInstances` on a MIG with a flexibility policy picks machine types from the selections (`gcloud compute instance-groups managed list-instances <mig> --region <region> --format="value(name,propertiesFromFlexibilityPolicy.machineType)"`);
2. labels set on MIG members are not reverted by the MIG;
3. Spot preemption: VM stays `TERMINATED`, GARM deletes it, the per-instance config disappears;
4. 20 parallel runners: no quota or rate-limit errors (each creating runner polls `listManagedInstances` every 5 s).

## License

Apache-2.0. Includes code adapted from `cloudbase/garm-provider-gcp`, see `NOTICE`.
