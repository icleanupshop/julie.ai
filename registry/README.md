# k3s Registry

Standalone Helm chart for Distribution (`registry:3.0.0`). Defaults to one
Deployment replica on the local cluster's control-plane node, `rp4-01`, with a
20Gi `local-path` PVC. The `Recreate` update strategy avoids overlapping writers
and ReadWriteOnce volume conflicts. This is not a highly available registry.

## Install

From the repository root, with Helm 3 or newer installed:

```sh
helm upgrade --install registry ./registry --namespace registry --create-namespace
kubectl rollout status deployment/registry -n registry
```

The default Service is cluster-internal and has no authentication or TLS. Do not
expose it to untrusted clients without enabling both. Cluster-internal access is
not an authentication boundary: other workloads can push and pull images.

For temporary access from this host:

```sh
kubectl port-forward -n registry service/registry 5000:5000
```

In another terminal:

```sh
curl --fail http://localhost:5000/v2/
docker tag julie:0.1.0 localhost:5000/julie:0.1.0
docker push localhost:5000/julie:0.1.0
```

Port-forwarding is for local access, not a stable endpoint for cluster image pulls.

## Persistent Node Access

For a trusted LAN only, enable an unauthenticated HTTP NodePort:

```sh
helm upgrade --install registry ./registry --namespace registry --create-namespace \
  --set service.type=NodePort
kubectl get node rp4-01 -o wide
```

Use `<control-plane-ip>:30500` for image tags and pushes. NodePort exposes the
Service through all eligible node addresses, not only the node hosting the pod.
Restrict access with your host/network firewall. Docker must explicitly allow
this endpoint as an insecure registry in its daemon configuration; restart Docker
after changing that configuration.

To let k3s pull images over this HTTP endpoint, merge the following entry into
`/etc/rancher/k3s/registries.yaml` on **every node that pulls these images**,
substituting the actual control-plane IP in both places:

```yaml
mirrors:
  "<control-plane-ip>:30500":
    endpoint:
      - "http://<control-plane-ip>:30500"
```

Restart `k3s` on server nodes and `k3s-agent` on agent nodes after updating their
configuration. This interrupts node services, so schedule it appropriately.
Then use `<control-plane-ip>:30500/julie:0.1.0` as your application's image.
The registry's own image is pulled from Docker Hub; do not make it depend on itself.

## TLS And Authentication

Create Secrets in the release namespace before enabling these options:

- `auth.existingSecret`: Secret containing a bcrypt-formatted `htpasswd` file.
  Generate it interactively with `htpasswd -Bc htpasswd <username>`, then create
  the Secret with `kubectl create secret generic registry-auth -n registry
  --from-file=htpasswd=./htpasswd`. Delete the local file when no longer needed.
- `tls.existingSecret`: Kubernetes TLS Secret containing `tls.crt` and `tls.key`.
  The certificate must cover the hostname or IP clients use. Create it with
  `kubectl create secret tls registry-tls -n registry --cert=<cert-file>
  --key=<key-file>`.

```sh
helm upgrade --install registry ./registry --namespace registry --create-namespace \
  --set service.type=NodePort \
  --set auth.existingSecret=registry-auth \
  --set tls.existingSecret=registry-tls
```

Use HTTPS and `docker login <registry-host>:30500`. Configure k3s registry
endpoints to use HTTPS and distribute any private CA to each pulling node.
For authenticated pulls, create an image-pull Secret in each application's
namespace and reference it in that application's `imagePullSecrets`. Never
commit credentials or private keys. Restart the Deployment after rotating mounted
Secrets so the registry reloads them.

## Storage And Scheduling

- Override `nodeSelector.kubernetes\.io/hostname` if the control-plane hostname
  changes. The defaults also require the control-plane role label to be `"true"`.
- `persistence.size` controls the initial claim size. The local `local-path`
  StorageClass does not support expansion; increasing this value on an existing
  claim will not resize it.
- Set `persistence.existingClaim` to reuse a PVC in the release namespace.
- Set `persistence.storageClass` to another provisioner, to `""` for the cluster
  default, or to `"-"` for no StorageClass.
- The PVC is retained on Helm uninstall by default (`persistence.keep: true`).
  To reinstall using retained data, set `persistence.existingClaim=registry`.
  Deleting a local-path PVC normally deletes its underlying data. Back up the
  data independently; loss of this node or its disk can lose the registry images.

## Validate

```sh
helm lint ./registry
helm template registry ./registry --namespace registry | kubectl apply --dry-run=server -f -
```

Server-side validation requires the target namespace to exist and does not verify
image startup, volume provisioning, or image push/pull behavior.

## Local Cluster Configuration

The local deployment uses `cluster-values.yaml` to expose the registry at
`http://100.0.0.2:30500` on control-plane node `rp4-01`:

```sh
helm upgrade --install registry ./registry --namespace registry --create-namespace \
  -f ./registry/cluster-values.yaml --wait
```

`registries.yaml` is the node-side configuration for this cluster. Install it as
`/etc/rancher/k3s/registries.yaml` on every node, including cordoned nodes, and
restart the appropriate k3s service one node at a time. Its quoted wildcard makes
the local registry the first mirror for all registry names. Upstream default
endpoints remain available as fallback; this is not an air-gapped setup.
Existing explicit mirror entries override the wildcard and must also put this
endpoint first if any are added later. Locally cached images need not contact
any registry.

The registry does not automatically cache upstream pulls. Push images under
their original repository paths to make transparent mirroring work, for example
`100.0.0.2:30500/library/alpine:<tag>` for `docker.io/library/alpine:<tag>`.
Different upstream registries with identical repository paths share the same
local repository, so manage those collisions deliberately.

This endpoint is unauthenticated HTTP. Restrict it to a trusted network; enable
TLS and authentication before extending access. Update every node's mirror
endpoint and client trust settings when changing the registry address or TLS.

To roll back registry-first pulls, restore the previous node configuration (or
remove the file if none existed), then restart `k3s-agent` on agents and `k3s`
on the control plane. Helm uninstall alone does not revert node configuration.