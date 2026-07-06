---
title: Configuration
weight: 2
---
# Configuration Options

There are several ways to provide configurations to the Terraform
provider that will propagate to the underlying Terraform workspace. In the
following sections, we will cover the most common ones.

## IAM Roles for Service Accounts (IRSA)

You can setup the Terraform Provider using AWS [IAM Roles for Service Accounts
(IRSA)](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html).
For more information, check out the example
[setup](../examples/aws-eks-irsa-setup.yaml), the process is similar to what you
would use for the
[provider-aws](https://github.com/upbound/provider-aws/blob/main/AUTHENTICATION.md#authentication-using-irsa).

## Provider Performance and Throughput

The performance and throughput of the provider can be tuned using the `--poll`
and `--max-reconcile-rate` arguments in a `ControllerConfig`.

The `poll` option determines how often the provider will compare the desired
`Workspace` configuration with the actual deployed resources (`terraform plan`)
and reconcile any differences (`terraform apply`).  The default value is 10m.
Shorter `poll` intervals increase the load on the provider by reconciling
existing `Workspaces` more often to allow for faster reconciliation of
differences, but this can cause the provider to take longer to process new
`Workspace` objects that are created.  Longer poll intervals will reduce the
load on the provider by reconciling existing `Workspaces` less often, taking a
longer time to identify and reconcile differences, but also shortening the
amount of time required for the provider to respond to new `Workspaces`.

The `max-reconcile-rate` option determines how many `Workspace` objects can be
reconciled in parallel concurrently.  The default value is 1.  Increasing this
value will allow the provider to process more `Workspaces` but will consume
more CPU, as the provider must run `terraform plan` for each `Workspace`.  The
provider could potentially use the same number of CPUs as the value set for
`max-reconcile-rate`, so plan accordingly or use `resources.requests` and
`resources.limits` to control the number of CPUs available to the provider.

For example, to set a polling interval of 5m and process 10 `Workspaces`
concurrently:

```yaml
apiVersion: pkg.crossplane.io/v1alpha1
kind: ControllerConfig
metadata:
  name: terraform
  labels:
    app: crossplane-provider-terraform
spec:
  args:
    - -d
    - --poll=5m
    - --max-reconcile-rate=10
```

and set the `spec.controllerConfigRef.name` in the Provider to `terraform`.

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-terraform
spec:
  package: xpkg.upbound.io/upbound/provider-terraform:<version>
  controllerConfigRef:
    name: terraform
```


## Private Git repository support

To securely propagate git credentials create a `git-credentials` secret in [git
credentials store] format.

```sh
cat .git-credentials
https://<user>:<token>@github.com

kubectl create secret generic git-credentials --from-file=.git-credentials
```

Reference it in ProviderConfig.

```yaml
apiVersion: tf.upbound.io/v1beta1
kind: ProviderConfig
metadata:
  name: default
spec:
  credentials:
  - filename: .git-credentials # use exactly this filename
    source: Secret
    secretRef:
      namespace: crossplane-system
      name: git-credentials
      key: .git-credentials
...
```

Standard `.git-credentials` filename is important to keep so provider-terraform
controller will be able to automatically pick it up.

## `.terraformc` repository support

```yaml
spec:
  credentials:
  - filename: .terraformrc # use exactly this filename by convention
    source: Secret
    secretRef:
      namespace: upbound-system
      name: terraformrc
      key: .terraformrc
```

will enable [Terraform CLI Configuration File](https://developer.hashicorp.com/terraform/cli/config/config-file)
installed from Kubernetes secret

## Terraform Output support

Non-sensitive outputs are mapped to the status.atProvider.outputs section as
strings so they can be referenced by the Composition. Strings, numbers and
booleans can be referenced directly in Compositions and can be used in the
_convert_ transform if type conversion is needed. Tuple and object outputs will
be available in the corresponding JSON form. This is required because undefined
object attributes are not specified in the Workspace CRD and so will be
sanitized before the status is stored in the database.

That means that any output values required for use in the Composition must be
published explicitly and individually, and they cannot be referenced inside a
tuple or object.

For example, the following terraform outputs:
```hcl
      output "string" {
        value = "bar"
        sensitive = false
      }
      output "number" {
        value = 1.9
        sensitive = false
      }
      output "object" {
        // This will be a JSON string - the key/value pairs are not accessible
        value = {"a": 3, "b": 2}
        sensitive = false
      }
      output "tuple" {
        // This will be a JSON string - the elements will not be accessible
        value = ["foo", "bar"]
        sensitive = false
      }
      output "bool" {
        value = false
        sensitive = false
      }
      output "sensitive" {
        value = "SENSITIVE"
        sensitive = true
      }
```
Appear in the corresponding outputs section as:
```yaml
  status:
    atProvider:
      outputs:
        bool: "false"
        number: "1.9"
        object: '{"a":3,"b":2}'
        string: bar
        tuple: '["foo", "bar"]'
```
Note that the "sensitive" output is not included in status.atProvider.outputs

## Terraform CLI Command Arguments
Additional arguments can be passed to the Terraform plan, apply, and destroy
commands by specifying the planArgs, applyArgs and destroyArgs options.

For example:
```yaml
apiVersion: tf.upbound.io/v1beta1
kind: Workspace
metadata:
  name: example-args
spec:
  forProvider:
    # Run the terraform init command with -upgrade=true to upgrade any stored providers
    initArgs:
      - -upgrade=true
    # Run the terraform plan command with the -parallelism=2 argument
    planArgs:
      - -parallelism=2
    # Run the terraform apply command with the -target=specificresource argument
    applyArgs:
      - -target=specificresource
    # Run the terraform destroy command with the -refresh=false argument
    destroyArgs:
      - -refresh=false
    # Use any module source supported by terraform init -from-module.
    source: Remote
    module: https://github.com/crossplane/tf
  # All Terraform outputs are written to the connection secret.
  writeConnectionSecretToRef:
    namespace: default
    name: terraform-workspace-example-inline
```
This will cause the _terraform init_ command to be run with the "-upgrade=true"
argument, the _terraform plan_ command to be run with the -parallelism=2
argument, the _terraform apply_ command to be run with the
-target=specificresource argument, and the _terraform destroy_ command to be run
with the -refresh=false argument.

Note that by default the terraform _init_ command is run with the
"-input=false", and "-no-color" arguments, the terraform _apply_ and _destroy_
commands are run with the "-no-color", "-auto-approve", and "-input=false"
arguments, and the terraform _plan_ command is run with the "-no-color",
"-input=false", and "-detailed-exitcode" arguments.  Arguments specified in
applyArgs, destroyArgs and planArgs will be added to these default arguments.

## Custom Entrypoint for Terraform Invocation

In some cases, you might want to initialize and apply terraform in the
subdirectory of the repository checkout. It is most relevant for the cases when
your terraform modules contain inline [relative paths](#83).

To enable it, the `Workspace` spec has an **optional** `Entrypoint` field.

Consider this example:

```yml
apiVersion: tf.upbound.io/v1beta1
kind: Workspace
metadata:
  name: relative-path-test
spec:
  forProvider:
    module: git::https://github.com/ytsarev/provider-terraform-test-module.git
    source: Remote
    entrypoint: relative-path-iam
    vars:
      - key: iamRole
        value: relative-path-test
```

In this case, the whole repository will be checked out but terraform will be
initialized in the `relative-path-iam` subdirectory with the module that
contains relative path reference to the `iam` module located in the root of the
tree.

```HCL
module "relative-path-iam" {
  source  = "../iam"
  iamRole = var.iamRole
}
```

## Provider Plugin Cache(enabled by default)

[Provider Plugin
Cache](https://developer.hashicorp.com/terraform/cli/config/config-file#provider-plugin-cache)
is enabled by default to speed up reconciliation.

In case you need to disable it, set optional `pluginCache` to `false` in
`ProviderConfig`:

```console
apiVersion: tf.upbound.io/v1beta1
kind: ProviderConfig
metadata:
  name: default
spec:
  pluginCache: false
...
```

## Enable External Secret Support

If you need to store the sensitive output to an external secret store like
Vault, you can specify the `--enable-external-secret-stores` flag to enable it:

```yaml
apiVersion: pkg.crossplane.io/v1alpha1
kind: ControllerConfig
metadata:
  name: terraform-config
  labels:
    app: crossplane-provider-terraform
spec:
  image: crossplane/provider-terraform-controller:v0.3.0
  args:
    - -d
    - --enable-external-secret-stores
  metadata:
    annotations:
      vault.hashicorp.com/agent-inject: "true"
      vault.hashicorp.com/agent-inject-token: "true"
      vault.hashicorp.com/role: "crossplane"
      vault.hashicorp.com/agent-run-as-user: "2000"
```

Prepare a `StoreConfig` for Vault:
```yaml
apiVersion: tf.upbound.io/v1beta1
kind: StoreConfig
metadata:
  name: vault
spec:
  type: Vault
  defaultScope: crossplane-system
  vault:
    server: http://vault.vault-system:8200
    mountPath: secret/
    version: v2
    auth:
      method: Token
      token:
        source: Filesystem
        fs:
          path: /vault/secrets/token
```

Specify it in `spec.publishConnectionDetailsTo`:
```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: ...
  labels:
    feature: ess
spec:
  compositeTypeRef:
    apiVersion: ...
    kind: ...
  resources:
    - name: foo
      base:
        apiVersion: tf.upbound.io/v1beta1
        kind: Workspace
        metadata:
          name: foo
        spec:
          forProvider:
            ...
          publishConnectionDetailsTo:
            name: bar
            configRef:
              name: vault
```

At Vault side configuration is also needed to allow the write operation, see
[example](https://docs.crossplane.io/knowledge-base/integrations/vault-as-secret-store/)
here for inspiration.


## Enable Terraform CLI logs

Terraform CLI output can be written to the container logs to assist with debugging and to view detailed information about Terraform operations.
To enable it, the `Workspace` spec has an **optional** `EnableTerraformCLILogging` field.
```yaml
apiVersion: tf.upbound.io/v1beta1
kind: Workspace
metadata:
  name: example-random-generator
  annotations:
    meta.upbound.io/example-id: tf/v1beta1/workspace
    crossplane.io/external-name: random
spec:
  forProvider:
    source: Inline
    enableTerraformCLILogging: true
...
```

- `enableTerraformCLILogging`: Specifies whether logging is enabled (`true`) or disabled (`false`). When enabled, Terraform CLI command output will be written to the container logs. Default is `false`

## Horizontal Scaling: Sharding Workspaces Across Instances

A single provider-terraform pod reconciles every `Workspace` it can see. When the number of Workspaces on a cluster grows large enough that one pod's `terraform` throughput becomes the bottleneck, you can run **N instances** of the provider, each watching a disjoint subset ("shard") of Workspaces. Every instance is the same binary, deployed as a separate `Provider`/`ControllerConfig` (or equivalent) with different flag values — there is no separate "sharding" binary or CRD.

All three flags below default to today's single-instance behavior, so an existing deployment is unaffected until you opt in.

### `--watch-label-selector`: partition Workspaces by label

Restricts an instance's manager cache to only the Workspaces matching a label selector. A Workspace whose labels don't match never enters that instance's cache — it is structurally invisible, not filtered after the fact.

```yaml
apiVersion: pkg.crossplane.io/v1alpha1
kind: ControllerConfig
metadata:
  name: terraform-shard-1
spec:
  args:
    - --watch-label-selector=sharding.gwcp.guidewire.com/shard=1
```

- **Default:** `""` (empty) — watch every Workspace, exactly like today.
- **At most one running instance may use this default.** An empty selector means *no filtering at all* — it watches every Workspace, including ones explicitly labeled for another shard. Leaving more than one instance at the default (or a flag simply omitted, which is identical to `""`), or leaving an old single-instance deployment's empty-selector instance running alongside a newly-added sharded fleet, means every empty-selector instance reconciles the *entire* Workspace set concurrently with whichever shard instances also match — full overlap on every Workspace, not a partial or edge-case one. This is the same failure class as leaving `--leader-election-id` at its default across instances (below); the provider can't detect it itself since no instance knows what selector any other instance is running — getting this right is a deployment-time responsibility, not something the binary enforces.
- **Assignment contract:** a Workspace is reconciled by instance `k` if and only if it carries the label `sharding.gwcp.guidewire.com/shard=k`. Every shard instance uses a plain equality selector (`shard=1`, `shard=2`, ...). This label is additive and optional — it requires no change to the Workspace CRD schema.
- **The default (catch-all) instance watches unlabelled Workspaces only — nothing else.** There is no "shard 0." A Workspace that carries the `shard` label — even if the value doesn't match any currently-running instance (a typo, a decommissioned shard) — is **not** picked up by the default instance. That's deliberate: an instance should only ever run `terraform` on Workspaces it was actually assigned, not absorb whatever nobody else claims. The default instance's selector is:

  ```
  --watch-label-selector=!sharding.gwcp.guidewire.com/shard
  ```

  `!key` (`DoesNotExist`) is standard Kubernetes selector syntax and matches only Workspaces where the label is **absent entirely**.
- **`shard=""` is not the same as "no label" — do not use it.** The `DoesNotExist` check is pure key-presence: a Workspace labeled `shard=""` (key present, empty value) does **not** match `!shard`, and matches no real shard's equality selector either. It becomes an orphan — reconciled by nobody. If you need to represent "not yet assigned," omit the `shard` label entirely; never set it to an empty string.
- **Operational consequence:** because a mislabeled or orphaned-shard Workspace is now deliberately left unpicked rather than silently absorbed, monitor for it explicitly — e.g. a periodic audit alerting on any Workspace whose `shard` label (including `shard=""`) doesn't match a currently-deployed instance's selector. A Workspace nobody is watching should be a loud signal, not a silent one.
- Selector syntax accepts the full Kubernetes label selector grammar (`=`, `!=`, `in (...)`, `notin (...)`, `key`, `!key`); an invalid selector fails the provider at startup rather than at reconcile time.

### `--leader-election-id`: give each instance its own leader lease

```yaml
    - --leader-election
    - --leader-election-id=crossplane-leader-election-provider-terraform-shard-1
```

- **Default:** `crossplane-leader-election-provider-terraform` — today's hardcoded lease name.
- **Why you must set this per instance:** the lease name has nothing to do with the Deployment's name. If two instances share a namespace and both leave this flag at its default (or set it to the same value), all of their pods race for **one** lease, and only one pod cluster-wide ever becomes leader. Every other instance's pods sit as permanent standbys with no leader, so their entire shard silently stops being reconciled — not degraded throughput, a total and silent outage for that shard.
- **Deployment rule of thumb:** derive `--leader-election-id` from the *same* shard index used in `--watch-label-selector` (e.g. append `-shard-1` to both), so the two settings can never drift apart in your deployment templates.
- Only matters when `--leader-election` (`-l`) is on with `replicas >= 2`; with a single replica per instance the ID is inert.

### `--enable-ownership-claims` (optional): safe handover when a Workspace is relabeled

When a Workspace's shard label changes while a `terraform apply` is still running on the old instance, a bare relabel is already safe by default (no destroy is triggered, and the Terraform state lock serializes any overlap) — this flag is **optional polish**, not a correctness requirement. Turning it on adds:

1. **Noise suppression** — the incoming instance backs off quietly instead of repeatedly failing against the held state lock during the handover window.
2. **Automated crash recovery** — if the old owner crashed mid-apply, the new owner automatically runs `terraform force-unlock` once the old claim goes stale, instead of requiring a manual runbook.

```yaml
    - --enable-ownership-claims
    - --ownership-claim-ttl=90s
    - --ownership-heartbeat-interval=30s
```

- `--enable-ownership-claims`: default `false`. Off = identical to today's behavior.
- `--ownership-claim-ttl`: how long a claim's heartbeat may go stale before another instance is allowed to steal it. Default `90s`.
- `--ownership-heartbeat-interval`: how often the current owner refreshes its claim while `terraform` is running. Default `30s`.
- **Requires downward-API wiring:** the provider resolves its own identity from `$POD_NAME` (falls back to hostname, then to `--leader-election-id`) and its claim namespace from `$POD_NAMESPACE` (falls back to the in-cluster service account namespace). If you enable this flag, set both env vars via the pod's downward API:

  ```yaml
  env:
    - name: POD_NAME
      valueFrom: { fieldRef: { fieldPath: metadata.name } }
    - name: POD_NAMESPACE
      valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
  ```

  The provider fails fast at startup if `--enable-ownership-claims` is set and no namespace can be resolved.
- **RBAC:** the provider's ServiceAccount needs `get`, `list`, `watch`, `create`, `update`, `delete` on `leases.coordination.k8s.io` wherever this flag is enabled (claims are stored in a dedicated `Lease` per Workspace, separate from the leader-election lease). **This must be a `ClusterRole`, not a namespace-scoped `Role`.** A cluster-scoped Workspace's claim Lease lives in the provider's own namespace, but a *namespaced* Workspace's claim lives in that Workspace's own (tenant) namespace — so the grant can't be limited to the provider's namespace alone; it needs to cover every namespace a namespaced Workspace could exist in.