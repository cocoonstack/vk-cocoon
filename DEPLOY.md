# Deployment

This repository does not ship a one-size-fits-all node bootstrap script in the public export. Worker bootstrap tends to be environment-specific and depends on:

- your Kubernetes distribution
- your Cocoon installation layout
- networking and DHCP choices
- snapshot distribution strategy

What the provider expects:

## Worker prerequisites

- `cocoon` installed on the worker
- default Cocoon root at `/data01/cocoon`
- network access to the Kubernetes API server
- the `vk-cocoon` binary running with access to a kubeconfig or in-cluster service account
- optional access to `epoch` when using remote snapshots

## Required environment variables

| Variable | Description |
|---|---|
| `KUBECONFIG` | kubeconfig path when running outside the cluster |
| `VK_NODE_NAME` | virtual node name, defaults to `cocoon-pool` |
| `VK_NODE_IP` | virtual node IP; auto-detected when unset |
| `COCOON_BIN` | Cocoon binary path, defaults to `/usr/local/bin/cocoon` |
| `VK_TLS_CERT` | optional kubelet API cert path |
| `VK_TLS_KEY` | optional kubelet API key path |

## Example

```bash
export KUBECONFIG=$HOME/.kube/config
export VK_NODE_NAME=cocoon-pool
export COCOON_BIN=/usr/local/bin/cocoon

./vk-cocoon
```

## Recommended Companion Components

- `epoch` for remote snapshots
- `cocoon-operator` for `CocoonSet` and hibernation workflows
- `cocoon-webhook` for sticky scheduling in multi-node setups
- `glance` for browser access to guests
