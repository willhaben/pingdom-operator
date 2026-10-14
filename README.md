# pingdom-operator

A Kubernetes operator that manages [Pingdom](https://www.pingdom.com/) uptime checks as native
Kubernetes resources. You declare a `Check`, and the operator keeps the matching check in Pingdom in
sync: it creates it once, updates it when the spec changes, and deletes it when the resource is
deleted.

```yaml
apiVersion: pingdom.willhaben.at/v1alpha1
kind: Check
metadata:
  name: www-example-com
spec:
  host: www.example.com
  type: http
  url: /
  encryption: true
  port: 443
  resolution: 5
  tags:
    - managed-by-operator
```

## How it works

The `Check` spec mirrors the Pingdom API 3.1 `POST /checks` body, so most Pingdom fields are
available directly (`type`, `host`, `resolution`, `tags`, `probeFilters`, HTTP specifics, DNS/TCP/UDP
options, notification settings, and so on). See
[`api/v1alpha1/check_types.go`](api/v1alpha1/check_types.go) for the full field list and validation
rules.

- The reconciler is idempotent: it creates the Pingdom check only once and records the returned
  `status.checkID`. Later spec changes become `PUT /checks/{id}` updates.
- `status.observedGeneration` tracks the last spec generation applied to Pingdom. A `Ready`
  condition reports `True` when the check matches the spec, or `False` with a reason
  (`InvalidSpec`, `Unauthorized`, `RateLimited`, `PingdomUnavailable`, …).
- The check `type` is immutable and a finalizer deletes the Pingdom check on resource deletion.
- Target basic auth for `http`/`httpcustom`/`smtp` checks is read from a Kubernetes `Secret`
  named by `spec.authSecretRef` (keys `username` and `password`).
- Request headers are plain `"Name:value"` strings in `spec.requestHeaders`. Header values that are
  secrets (a token, say) come from a `Secret` named by `spec.requestHeadersSecretRef`, whose keys are
  header names. They are merged with `requestHeaders` and win on a name clash.
- `spec.urlTmpl` is a Go template for the url, for a value that is a secret. The variables are the keys
  of the `Secret` named by `spec.urlTmplSecretRef`, e.g. `/search?key={{ .key }}`. It is mutually
  exclusive with `spec.url`.
- Changing a referenced `Secret` updates the check in Pingdom (`status.appliedHash`).
- Every 30 minutes the check is sent to Pingdom again, which reverts edits made in the Pingdom UI.
  A check deleted in the Pingdom UI is reported as `NotFound`, not recreated.
- Fields removed from the spec are cleared in Pingdom: `postData`, `customMessage`, `requestHeaders`,
  `tags`, `probeFilters`, `userIDs`, `teamIDs` and `integrationIDs`. Numbers, booleans, `url`,
  `shouldContain` and `shouldNotContain` keep their last value, so set them explicitly.
- To take over a check that already exists in Pingdom (for example one created by Terraform), set the
  annotation `pingdom.willhaben.at/check-id: "<id>"` before applying. The check is updated in place
  instead of creating a duplicate, and is deleted from Pingdom when the resource is deleted.

To avoid deleting and recreating checks on every change, edit `spec` in place and let the operator
update Pingdom.

## Prerequisites

- go version v1.24.6+
- docker version 17.03+
- kubectl version v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster
- A Pingdom API token

## Configuration

The operator authenticates to Pingdom with the `PINGDOM_API_TOKEN` environment variable. In the
default deployment it is read from a Secret named `pingdom-api-token`:

```sh
kubectl create secret generic pingdom-api-token \
  --namespace pingdom-operator-system \
  --from-literal=token=<your-pingdom-api-token>
```

## Getting started

### Run locally

```sh
make install   # install the CRDs into the cluster
make run       # run the manager against the current kubeconfig context
```

`make run` reads `PINGDOM_API_TOKEN` from your shell, so export it first:

```sh
export PINGDOM_API_TOKEN=<your-pingdom-api-token>
```

### Develop

```sh
make manifests generate   # regenerate CRDs, RBAC and DeepCopy after changing types/markers
make lint-fix             # auto-fix code style
make test                 # unit tests (envtest: real API server + etcd)
```

## Deploy on a cluster

**Build and push the image** to the location specified by `IMG`:

```sh
make docker-build docker-push IMG=<some-registry>/pingdom-operator:tag
```

**Install the CRDs:**

```sh
make install
```

**Deploy the manager with that image:**

```sh
make deploy IMG=<some-registry>/pingdom-operator:tag
```

> If you encounter RBAC errors, you may need to grant yourself cluster-admin privileges or be
> logged in as admin.

**Apply the sample Check:**

```sh
kubectl apply -k config/samples/
```

### Uninstall

```sh
kubectl delete -k config/samples/   # delete the Check resources (and their Pingdom checks)
make uninstall                      # delete the CRDs from the cluster
make undeploy                       # remove the controller from the cluster
```

## Distribution

### YAML bundle

```sh
make build-installer IMG=<some-registry>/pingdom-operator:tag
```

This generates `dist/install.yaml` with all resources (CRDs, RBAC, Deployment) needed to install the
project without extra tooling. Users can apply it directly:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/pingdom-operator/<tag or branch>/dist/install.yaml
```

### Helm chart

```sh
kubebuilder edit --plugins=helm/v2-alpha   # generates dist/chart
```

If you change the project afterwards, re-run the same command to sync the chart; when webhooks are
involved, use `--force` and manually re-apply any customizations you made to `dist/chart/values.yaml`
or `dist/chart/manager/manager.yaml`.

## Contributing

Run `make help` for the full list of make targets. More background on operator patterns is in the
[Kubebuilder documentation](https://book.kubebuilder.io/introduction.html).

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
