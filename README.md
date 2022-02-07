# provider-terraform

An __experimental__ Crossplane provider for Terraform. Use this provider to
define new Crossplane Composite Resources (XRs) that are composed of a mix of
'native' Crossplane managed resources and your existing Terraform modules.

The Terraform provider adds support for a `Workspace` managed resource that
represents a Terraform workspace. The configuration of each workspace may be
either fetched from a remote source (e.g. git), or simply specified inline.

```yaml
apiVersion: tf.crossplane.io/v1alpha1
kind: Workspace
metadata:
  name: example-inline
  annotations:
    # The terraform workspace will be named 'coolbucket'. If you omitted this
    # annotation it would be derived from metadata.name - i.e. 'example-inline'.
    crossplane.io/external-name: coolbucket
spec:
  forProvider:
    # For simple cases you can use an inline source to specify the content of
    # main.tf as opaque, inline HCL.
    source: Inline
    module: |
      // Outputs are written to the connection secret.
      output "url" {
        value       = google_storage_bucket.example.self_link
      }

      resource "random_id" "example" {
        byte_length = 4
      }

      // The google provider and remote state are configured by the provider
      // config - see examples/providerconfig.yaml.
      resource "google_storage_bucket" "example" {
        name = "crossplane-example-${terraform.workspace}-${random_id.example.hex}"
      }
  writeConnectionSecretToRef:
    namespace: default
    name: terraform-workspace-example-inline
```

```yaml
apiVersion: tf.crossplane.io/v1alpha1
kind: Workspace
metadata:
  name: example-remote
  annotations:
    crossplane.io/external-name: myworkspace
spec:
  forProvider:
    # Use any module source supported by terraform init -from-module.
    source: Remote
    module: https://github.com/crossplane/tf
    # Variables can be specified inline, or loaded from a ConfigMap or Secret.
    vars:
    - key: region
      value: us-west-1
    varFiles:
    - source: SecretKey
      secretKeyRef:
        namespace: default
        name: terraform
        key: example.tfvar.json
  # All Terraform outputs are written to the connection secret.
  writeConnectionSecretToRef:
    namespace: default
    name: terraform-workspace-example-inline
```

## Private Git repository support

To securely propagate git credentials create a `git-credentials` secret in [git credentials store] format.

```sh
cat .git-credentials
https://<user>:<token>@github.com

kubectl create secret generic git-credentials --from-file=.git-credentials
```

Reference it in ProviderConfig.

```yaml
apiVersion: tf.crossplane.io/v1alpha1
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

In order to use ssh keys for modules create a secret with `id_rsa` key and specify it in `ProviderConfig`

Here is an example how to use existing ssh keys from Flux.

```yaml
    - filename: .ssh/id_rsa
      source: Secret
      secretRef:
        namespace: flux-system
        name: flux-system
        key: identity
    - filename: .ssh/id_rsa.pub
      source: Secret
      secretRef:
        namespace: flux-system
        name: flux-system
        key: identity.pub
    - filename: .ssh/known_hosts
      source: Secret
      secretRef:
        namespace: flux-system
        name: flux-system
        key: known_hosts
```

## Known limitations:

* You must either use remote state or ensure the provider container's `/tf`
  directory is not lost. `provider-terraform` __does not persist state__;
  consider using the [Kubernetes] remote state backend.
* If the module takes longer than the supplied `--timeout` to apply the
  underlying `terraform` process will be killed. You will potentially lose state
  and leak resources.
* The provider won't emit an event until _after_ it has successfully applied the
  Terraform module, which can take a long time.

[Kubernetes]: https://www.terraform.io/docs/language/settings/backends/kubernetes.html
[git credentials store]: https://git-scm.com/docs/git-credential-store
