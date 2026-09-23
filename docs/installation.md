# Installation

vk-cocoon is a host-level binary, not a Kubernetes Deployment. The
recommended path is the supplied systemd unit.

```bash
sudo install -m 0755 ./vk-cocoon /usr/local/bin/vk-cocoon
sudo install -m 0644 packaging/vk-cocoon.service /etc/systemd/system/vk-cocoon.service
sudo install -m 0644 packaging/vk-cocoon.env.example /etc/cocoon/vk-cocoon.env

# edit /etc/cocoon/vk-cocoon.env to your environment (see Configuration)

sudo systemctl daemon-reload
sudo systemctl enable --now vk-cocoon
```

The unit reads `/etc/cocoon/kubeconfig` for cluster credentials and
`/etc/cocoon/vk-cocoon.env` for the [configuration](configuration.md)
variables. It grants `AmbientCapabilities=CAP_NET_RAW` so the readiness
probe can open an ICMP raw socket (see [Readiness probing](probes.md)).
The unit runs as root with the full capability bounding set: the cocoon CLI it
execs creates network namespaces, TAP devices and TC redirects. On Linux,
transfer pipes therefore grow to 8 MiB; a process without `CAP_SYS_RESOURCE`
falls back to `/proc/sys/fs/pipe-max-size` without failing the transfer.

## Building from source

```bash
make all            # full pipeline: deps + fmt + lint + test + build
make build          # build vk-cocoon binary
make test           # vet + race-detected tests
make lint           # golangci-lint on linux + darwin
make fmt            # gofumpt + goimports
make help           # show all targets
```

The Makefile detects Go workspace mode (`go env GOWORK`) and skips `go
mod tidy` when active so cross-module references resolve through
`go.work` without forcing a release of cocoon-common.
