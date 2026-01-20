# Remote Module Pull Policy

This guide explains how to use the `remotePullPolicy` feature to control when provider-terraform downloads remote Terraform modules, significantly reducing network costs and improving reconciliation performance.

## Overview

By default, provider-terraform downloads remote Terraform modules on every reconciliation. For workspaces that reconcile frequently (every 1-10 minutes), this can result in substantial network egress costs:

- **Without pull policy control**: A 50MB module reconciling 6 times per hour = 300MB/hour = 7.2GB/day per workspace
- **With IfNotPresent policy**: Same module downloads once = 50MB/day per workspace
- **Network reduction**: 98.6% savings per workspace

The `remotePullPolicy` field gives you control over when modules are downloaded, allowing you to optimize for either cost or freshness.

## Pull Policy Options

### Always (Default)

Downloads the remote module on every reconciliation.

**Use cases:**
- Development workspaces where you need the latest module changes
- Modules without pinned versions (e.g., `ref=main` instead of `ref=v1.0.0`)
- When module content changes frequently without version updates

**Example:**
```yaml
apiVersion: tf.upbound.io/v1beta1
kind: Workspace
metadata:
  name: example-always
spec:
  forProvider:
    source: Remote
    module: git::https://github.com/org/repo?ref=main
    remotePullPolicy: Always  # Explicit (default behavior)
```

**Network impact:**
- Downloads on every reconciliation
- Highest network costs
- Ensures latest module content

### IfNotPresent

Downloads the remote module only once, reusing it on subsequent reconciliations until the module URL changes.

**Use cases:**
- Production workspaces with pinned module versions
- Cost-sensitive environments
- Large modules (>10MB) that are expensive to download repeatedly
- High reconciliation frequency (every 1-5 minutes)

**Example:**
```yaml
apiVersion: tf.upbound.io/v1beta1
kind: Workspace
metadata:
  name: example-if-not-present
spec:
  forProvider:
    source: Remote
    module: git::https://github.com/org/repo?ref=v1.0.0  # Pinned version
    remotePullPolicy: IfNotPresent
```

**Network impact:**
- Downloads once on first reconciliation
- 98.6% network reduction per workspace
- Faster reconciliation (no download time)

**Automatic re-download triggers:**
- Module URL changes (including git ref)
- Workspace pod restart (without persistent volume)
- `.terraform` directory is deleted or missing

## How It Works

### Detection Mechanism

The `IfNotPresent` policy checks for the presence of a `.terraform` directory in the workspace:

```
if .terraform directory exists:
    if module URL == previously downloaded URL:
        → Skip download (reuse existing module)
    else:
        → Download (module URL changed)
else:
    → Download (module not present)
```

### Status Tracking

The provider tracks the last downloaded module URL in the workspace status:

```yaml
status:
  atProvider:
    remoteSource: git::https://github.com/org/repo?ref=v1.0.0
```

This allows automatic detection of module URL changes, triggering a re-download when needed.

## Migration Guide

### Migrating Existing Workspaces

Existing workspaces without `remotePullPolicy` will continue using the default `Always` behavior. To opt-in to cost savings:

1. **Ensure your module versions are pinned:**
   ```yaml
   # Good - pinned version
   module: git::https://github.com/org/repo?ref=v1.0.0

   # Avoid - floating ref
   module: git::https://github.com/org/repo?ref=main
   ```

2. **Add the remotePullPolicy field:**
   ```yaml
   spec:
     forProvider:
       source: Remote
       module: git::https://github.com/org/repo?ref=v1.0.0
       remotePullPolicy: IfNotPresent  # Add this line
   ```

3. **Apply the change:**
   ```bash
   kubectl apply -f workspace.yaml
   ```

The first reconciliation after the change will download the module normally. Subsequent reconciliations will skip the download.

### Updating Module Versions

When you need to update to a new module version:

1. **Update the module reference:**
   ```yaml
   spec:
     forProvider:
       module: git::https://github.com/org/repo?ref=v2.0.0  # Changed from v1.0.0
       remotePullPolicy: IfNotPresent
   ```

2. **Apply the change:**
   ```bash
   kubectl apply -f workspace.yaml
   ```

The provider will automatically detect the URL change and download the new version.

## Best Practices

### 1. Pin Module Versions in Production

Always use specific version tags or commit SHAs in production:

```yaml
# Recommended - specific version tag
module: git::https://github.com/terraform-aws-modules/terraform-aws-vpc?ref=v5.1.2

# Recommended - specific commit
module: git::https://github.com/org/repo?ref=abc123def

# Avoid - floating ref
module: git::https://github.com/org/repo?ref=main
```

### 2. Use IfNotPresent with Pinned Versions

Combine version pinning with IfNotPresent for maximum cost savings:

```yaml
spec:
  forProvider:
    source: Remote
    module: git::https://github.com/org/repo?ref=v1.0.0
    remotePullPolicy: IfNotPresent
```

### 3. Use Always for Development

Keep the Always policy for development workspaces where you need the latest changes:

```yaml
spec:
  forProvider:
    source: Remote
    module: git::https://github.com/org/repo?ref=develop
    remotePullPolicy: Always  # Get latest changes
```

### 4. Monitor Network Costs

After enabling IfNotPresent, monitor your cloud provider's network egress metrics to verify cost savings:

```bash
# Example: Check workspace reconciliation logs
kubectl logs -n upbound-system deploy/provider-terraform-* | grep "Remote module"

# Expected with IfNotPresent:
# First reconciliation: "Remote module downloaded"
# Subsequent reconciliations: "Remote module already present, skipping download"
```

## Troubleshooting

### Module Not Updating After URL Change

**Symptoms:**
- Module URL changed but old module content is still being used
- Status field shows old URL

**Solution:**
Check that the workspace reconciliation completed successfully:

```bash
kubectl describe workspace <name>
kubectl logs -n upbound-system deploy/provider-terraform-* | grep "Remote module URL changed"
```

### Module Downloaded Every Time Despite IfNotPresent

**Possible causes:**

1. **Workspace pods are restarting frequently**
   - Module is downloaded to pod's ephemeral storage
   - Pod restart = module is lost
   - Solution: Use persistent volumes for `/tf` directory (future enhancement)

2. **.terraform directory is being deleted**
   - Check if any process is cleaning up the workspace directory
   - Verify workspace directory permissions

3. **Module URL is changing on every reconciliation**
   - Check if dynamic refs are being used
   - Verify status.atProvider.remoteSource matches spec.forProvider.module

### Status Field Not Populated

**Symptoms:**
- `status.atProvider.remoteSource` is empty
- Module downloads every time even with IfNotPresent

**Solution:**
Wait for one successful reconciliation. The status field is populated after the first download:

```bash
kubectl get workspace <name> -o jsonpath='{.status.atProvider.remoteSource}'
```

## Cost Analysis

### Network Savings Example

**Scenario:**
- 100 workspaces
- 50MB module size
- 6 reconciliations per hour
- $0.12/GB network egress (AWS example)

**Without IfNotPresent:**
- Per workspace: 50MB × 6 × 24 = 7.2GB/day
- 100 workspaces: 720GB/day
- Monthly cost: 720GB × 30 × $0.12 = $2,592/month

**With IfNotPresent:**
- Per workspace: 50MB/day (one download)
- 100 workspaces: 5GB/day
- Monthly cost: 5GB × 30 × $0.12 = $18/month

**Savings: $2,574/month (99.3% reduction)**

### Performance Improvement

**Download time savings:**
- 50MB module at 100Mbps = 4 seconds download time
- 6 reconciliations/hour × 4 seconds = 24 seconds/hour wasted
- With IfNotPresent: 4 seconds once, then <1 second for subsequent reconciliations
- **Reconciliation speedup: 75%+ after first reconciliation**

## Limitations

### Current Limitations

1. **No cross-workspace deduplication**
   - Each workspace downloads its own copy of the module
   - 100 workspaces using the same module = 100 downloads (one per workspace)
   - Mitigated by: Each workspace only downloads once with IfNotPresent

2. **Module lost on pod restart**
   - Modules are stored in pod's ephemeral storage
   - Pod restart requires re-download
   - Mitigation: Use persistent volumes (manual setup)

3. **No content-based detection**
   - Detection is based on URL comparison, not module content hash
   - Changing module content without changing URL is not detected
   - Best practice: Always update version tags when changing modules

### Future Enhancements

Potential improvements under consideration:

1. **Provider-level cache**: Share modules across all workspaces
2. **Content hashing**: Detect module changes without URL changes
3. **Persistent storage**: Recommend PVC for `/tf` directory
4. **Cache warming**: Pre-download popular modules

## Related Configuration

### Works with Flux Source

The pull policy also applies to Flux-sourced modules:

```yaml
spec:
  forProvider:
    source: Flux
    module: GitRepository::my-namespace/my-repo
    remotePullPolicy: IfNotPresent
```

### Compatible with All Module Sources

The `remotePullPolicy` field is supported for:
- Remote sources (git, http, S3, etc.)
- Flux sources (GitRepository, Bucket, etc.)
- Not applicable to Inline sources (module is already in the spec)

## Summary

- Use `remotePullPolicy: IfNotPresent` with pinned module versions for 98%+ network cost savings
- Use `remotePullPolicy: Always` (default) for development or when module freshness is critical
- The provider automatically re-downloads modules when URLs change
- Monitor logs and status fields to verify expected behavior
- Combine with persistent volumes for maximum module reuse across pod restarts
