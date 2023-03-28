# Shared Scrape Endpoint Configs
Sometimes you might want to share some scrape configs, for example HTTP-related
configs, across a multiple scrape endpoints within a single or multiple
`PodMonitoring` or `ClusterPodMonitoring` resources.

## Prerequisites
This example shows you how to do that. To run this example, `kustomize` version
`5.0` or greater is required. You can check your `kustomize` version with:
```bash
kustomize version
```

Or within kubectl:
```bash
kubectl version --output=yaml | grep kustomizeVersion
```

## Running
The structure is as follows:
`podmonitorings.yaml`: Your `PodMonitoring` or `ClusterPodMonitoring` resources.
`common.yaml`: This example shows you how to setup a common `tls` configuration.
`kustomization.yaml`: Applies `common.yaml` over `podmonitorings.yaml`.

To view the final YAML, run from this directory:
```bash
kustomize build .
```

Or with kubectl:
```
kubectl kustomize .
```
