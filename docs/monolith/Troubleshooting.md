---
title: Troubleshooting Reconciliation Issues
weight: 3
---
# Troubleshooting Reconciliation Issues

This document provides guidance for troubleshooting common reconciliation and performance issues with the Terraform provider, based on real-world scenarios and their resolutions.

## Reconciliations Blocking Unexpectedly

### Problem Description

You may experience reconciliations backing up behind long-running terraform apply/destroy operations, where workspaces appear to get "stuck" even when CPU resources are available. This manifests as:

- Reconciliations not making progress despite available CPU capacity
- Long queues of pending reconciliations
- Underutilization of configured `--max-reconcile-rate` settings

### Root Cause

This issue is caused by the provider's locking mechanism used to prevent terraform plugin cache corruption:

1. **Read-Write Lock Behavior**: The provider uses RWMutex for workspace operations:
   - Multiple `terraform plan` operations can run concurrently (RLock)
   - Only one `terraform init` operation can run at a time (Lock)
   - When a Lock is requested, it blocks all new RLock requests until completed

2. **Blocking Scenario**: When a new Workspace is created requiring `terraform init`:
   - The Lock request waits for all current RLock (plan) operations to finish
   - Meanwhile, all new RLock requests are blocked
   - This effectively makes the provider single-threaded until the init completes

### Solutions and Workarounds

#### 1. Use Persistent Storage (Recommended)

Mount a persistent volume to `/tf` to eliminate the need for frequent `terraform init` operations:

```yaml
apiVersion: pkg.crossplane.io/v1alpha1
kind: DeploymentRuntimeConfig
metadata:
  name: provider-terraform-with-pv
spec:
  deploymentTemplate:
    spec:
      template:
        spec:
          containers:
          - name: package-runtime
            volumeMounts:
            - name: tf-workspace
              mountPath: /tf
          volumes:
          - name: tf-workspace
            persistentVolumeClaim:
              claimName: provider-terraform-pvc
```

**Benefits:**
- Workspaces persist across pod restarts
- Eliminates need to re-run `terraform init` on restart
- Reduces plugin download traffic
- Significantly improves performance with many workspaces

#### 2. Disable Plugin Cache for High Concurrency

If persistent storage is not available, consider disabling the terraform plugin cache to avoid locking entirely:

```yaml
apiVersion: pkg.crossplane.io/v1alpha1
kind: ControllerConfig
metadata:
  name: provider-terraform-no-cache
spec:
  args:
  - --debug
  env:
  - name: TF_PLUGIN_CACHE_DIR
    value: ""
```

**Trade-offs:**
- Eliminates blocking issues
- Increases network traffic (providers downloaded per workspace)
- Higher NAT gateway costs in cloud environments
- Still better than single-threaded performance

#### 3. Optimize Concurrency Settings

Align your `--max-reconcile-rate` with available CPU resources:

```yaml
apiVersion: pkg.crossplane.io/v1alpha1
kind: ControllerConfig
metadata:
  name: provider-terraform-optimized
spec:
  args:
  - --max-reconcile-rate=4  # Match your CPU allocation
  resources:
    requests:
      cpu: 4
    limits:
      cpu: 4
```

### Monitoring and Diagnosis

Use these Prometheus queries to monitor reconciliation performance:

```promql
# Maximum concurrent reconciles configured
sum by (controller)(controller_runtime_max_concurrent_reconciles{controller="managed/workspace.tf.upbound.io"})

# Active workers currently processing
sum by (controller)(controller_runtime_active_workers{controller="managed/workspace.tf.upbound.io"})

# Reconciliation rate
sum by (controller)(rate(controller_runtime_reconcile_total{controller="managed/workspace.tf.upbound.io"}[5m]))

# CPU usage
sum by ()(rate(container_cpu_usage_seconds_total{container!="",namespace="crossplane-system",pod=~"upbound-provider-terraform.*"}[5m]))

# Memory usage
sum by ()(container_memory_working_set_bytes{container!="",namespace="crossplane-system",pod=~"upbound-provider-terraform.*"})
```

## Remote Git Repository Issues

### Problem Description

When using remote git repositories as workspace sources, you may experience:

- Excessive network traffic
- Providers being re-downloaded on every reconciliation
- "text file busy" errors even with persistent volumes

### Root Cause

The provider removes and recreates the entire workspace directory for each reconciliation when using remote repositories due to limitations in the go-getter library.

### Current Limitations

- Remote repositories are re-cloned on every reconciliation
- `terraform init` runs on every reconciliation for git-backed workspaces
- Plugin cache conflicts can still occur during rapid workspace creation

### Recommendations

1. **Use Inline Workspaces**: When possible, embed terraform configuration directly in the Workspace spec rather than referencing remote repositories.
2. **Disable Plugin Cache**: For remote repositories with high reconciliation rates, disable the plugin cache to avoid conflicts.
3. **Monitor Traffic Costs**: Be aware of increased network egress costs when using remote repositories with disabled plugin cache.

## Error Messages and Recovery

### "text file busy" Errors

```
Error: Failed to install provider
Error while installing hashicorp/aws v5.44.0: open
/tf/plugin-cache/registry.terraform.io/hashicorp/aws/5.44.0/linux_arm64/terraform-provider-aws_v5.44.0_x5:
text file busy
```

**Resolution**: These errors typically resolve automatically due to built-in retry logic, but indicate plugin cache conflicts. Consider:
- Using persistent volumes with plugin cache disabled
- Reducing `--max-reconcile-rate` during initial workspace creation

### CLI Configuration Warnings

```
Warning: Unable to open CLI configuration file
The CLI configuration file at "./.terraformrc" does not exist.
```

**Resolution**: This is typically harmless but can be resolved by:
- Mounting a custom `.terraformrc` configuration
- Setting appropriate terraform CLI environment variables

## Best Practices

1. **Start Conservative**: Begin with `--max-reconcile-rate=1` and increase gradually while monitoring performance.
2. **Match Resources**: Ensure CPU requests/limits align with your concurrency settings.
3. **Use Persistent Storage**: Always use persistent volumes in production environments with multiple workspaces.
4. **Monitor Actively**: Set up monitoring for reconciliation rates, error rates, and resource utilization.
5. **Plan for Scale**: Consider the total number of workspaces and their reconciliation patterns when designing your deployment.
