# vk-cocoon Production Deployment on GKE

Deploy vk-cocoon on bare-metal (or nested-virt) GCE instances registered as virtual nodes in a GKE cluster.

## Architecture

```
GKE Cluster (managed control plane)
├── GKE node pool (e2-standard-4 x3)
│   └── cocoon-operator, cocoon-webhook, glance
│
├── cocoon-pool      ← virtual node (cocoonset-node-1)
│   └── MicroVMs via Cloud Hypervisor + KVM
│
└── cocoon-pool-2    ← virtual node (cocoonset-node-2)
    └── MicroVMs via Cloud Hypervisor + KVM
```

## Prerequisites

- GKE Standard cluster (not Autopilot) with `--enable-ip-alias`
- GCE instances with `--enable-nested-virtualization` and `--can-ip-forward`
- Same VPC and subnet as the GKE cluster
- Go 1.25+ (for building from source)

## 1. Prepare GCE Instances

```bash
gcloud compute instances create cocoonset-node-1 \
  --zone=asia-southeast1-b \
  --machine-type=n2-standard-32 \
  --network=default --subnet=default \
  --can-ip-forward \
  --enable-nested-virtualization \
  --image-family=ubuntu-2204-lts --image-project=ubuntu-os-cloud \
  --boot-disk-size=200GB --boot-disk-type=pd-ssd
```

Verify KVM:

```bash
ls -la /dev/kvm
```

## 2. Install Cocoon Stack Binaries

Download from releases or build from source:

```bash
# cocoon CLI
curl -sL https://github.com/cocoonstack/cocoon/releases/download/v0.2.5/cocoon_0.2.5_Linux_x86_64.tar.gz | tar xz
sudo mv cocoon /usr/local/bin/

# Cloud Hypervisor
sudo curl -sL https://github.com/cocoonstack/cloud-hypervisor/releases/download/dev/cloud-hypervisor -o /usr/local/bin/cloud-hypervisor

# Firmware
sudo curl -sL https://github.com/cocoonstack/rust-hypervisor-firmware/releases/download/dev/hypervisor-fw -o /usr/local/bin/hypervisor-fw

# Doctor scripts
sudo curl -sL https://raw.githubusercontent.com/cocoonstack/cocoon/master/doctor/check.sh -o /usr/local/bin/check.sh
sudo curl -sL https://raw.githubusercontent.com/cocoonstack/cocoon/master/doctor/vm-inspect.sh -o /usr/local/bin/vm-inspect.sh

sudo chmod +x /usr/local/bin/{cocoon,cloud-hypervisor,hypervisor-fw,check.sh,vm-inspect.sh}
```

Install Docker and containerd:

```bash
sudo apt-get install -y docker.io containerd
sudo systemctl enable --now docker containerd
```

## 3. CNI Networking

Install CNI plugins:

```bash
sudo mkdir -p /opt/cni/bin /etc/cni/net.d
curl -sL https://github.com/containernetworking/plugins/releases/download/v1.6.2/cni-plugins-linux-amd64-v1.6.2.tgz \
  | sudo tar xz -C /opt/cni/bin/
```

### 3.1 Default bridge (host-local IPAM)

```bash
sudo tee /etc/cni/net.d/10-cocoon.conflist > /dev/null <<'EOF'
{
  "cniVersion": "1.0.0",
  "name": "cocoon",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cocoon0",
      "isGateway": true,
      "ipMasq": true,
      "hairpinMode": true,
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "10.88.0.0/24", "gateway": "10.88.0.1"}]],
        "routes": [{"dst": "0.0.0.0/0"}]
      }
    }
  ]
}
EOF
```

### 3.2 dnsmasq-dhcp bridge (for Windows VMs)

Windows VMs run their own DHCP client, so the CNI IPAM must be empty to avoid IP conflicts. dnsmasq serves as the DHCP server on the bridge.

```bash
sudo tee /etc/cni/net.d/30-dnsmasq-dhcp.conflist > /dev/null <<'EOF'
{
  "cniVersion": "1.0.0",
  "name": "dnsmasq-dhcp",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cni0",
      "isGateway": false,
      "ipMasq": false,
      "ipam": {}
    }
  ]
}
EOF
```

> **Important**: CNI IPAM is intentionally empty (`{}`). Windows guests obtain their IP directly from dnsmasq. Using `"type": "dhcp"` would cause a dual-DHCP conflict where the CNI plugin and the Windows guest get different IPs.

### 3.3 cni0 bridge systemd service

```bash
sudo tee /etc/systemd/system/cni0-bridge.service > /dev/null <<'EOF'
[Unit]
Description=Create cni0 bridge for dnsmasq DHCP
Before=dnsmasq-cni.service
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash -c "ip link add cni0 type bridge 2>/dev/null || true; ip addr replace 10.89.0.1/24 dev cni0; ip link set cni0 up; sysctl -w net.ipv4.ip_forward=1; iptables -t nat -C POSTROUTING -s 10.89.0.0/24 ! -o cni0 -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -s 10.89.0.0/24 ! -o cni0 -j MASQUERADE"
ExecStop=/bin/bash -c "ip link set cni0 down; ip link del cni0 2>/dev/null || true"

[Install]
WantedBy=multi-user.target
EOF
```

### 3.4 dnsmasq DHCP server

```bash
sudo apt-get install -y dnsmasq
sudo systemctl disable dnsmasq  # disable system default to avoid conflicts

sudo mkdir -p /etc/dnsmasq-cni.d
sudo tee /etc/dnsmasq-cni.d/cni0.conf > /dev/null <<'EOF'
interface=cni0
bind-interfaces
except-interface=lo
except-interface=ens4
dhcp-range=10.89.0.2,10.89.0.254,255.255.255.0,24h
dhcp-option=option:router,10.89.0.1
dhcp-option=option:dns-server,8.8.8.8,1.1.1.1
dhcp-leasefile=/var/lib/misc/dnsmasq.leases
dhcp-authoritative
port=0
log-dhcp
EOF
```

> **Important**: The lease file must be `/var/lib/misc/dnsmasq.leases` — vk-cocoon reads this path to resolve VM IPs from MAC addresses via `resolveLeaseByMAC()`.

```bash
sudo tee /etc/systemd/system/dnsmasq-cni.service > /dev/null <<'EOF'
[Unit]
Description=dnsmasq DHCP server for CNI bridge (cni0)
Documentation=man:dnsmasq(8)
Requires=cni0-bridge.service
After=cni0-bridge.service network-online.target

[Service]
Type=simple
ExecStart=/usr/sbin/dnsmasq -k -C /etc/dnsmasq-cni.d/cni0.conf --pid-file=/run/dnsmasq-cni.pid
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now cni0-bridge.service
sudo systemctl enable --now dnsmasq-cni.service
```

Service dependency chain:

```
cni0-bridge.service (create bridge + assign 10.89.0.1/24)
  → dnsmasq-cni.service (DHCP server on cni0)
```

## 4. Kubernetes ServiceAccount

Create a `cocoon` ServiceAccount with cluster-admin and a persistent token:

```bash
kubectl create serviceaccount cocoon -n default
kubectl create clusterrolebinding cocoon-admin \
  --clusterrole=cluster-admin --serviceaccount=default:cocoon

kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: cocoon-token
  namespace: default
  annotations:
    kubernetes.io/service-account.name: cocoon
type: kubernetes.io/service-account-token
EOF
```

Generate kubeconfig:

```bash
TOKEN=$(kubectl get secret cocoon-token -o jsonpath='{.data.token}' | base64 -d)
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

python3 -c "
import yaml, sys
kc = {
    'apiVersion': 'v1', 'kind': 'Config',
    'clusters': [{'name': 'gke', 'cluster': {'server': sys.argv[1], 'certificate-authority-data': sys.argv[2]}}],
    'contexts': [{'name': 'cocoon@gke', 'context': {'cluster': 'gke', 'user': 'cocoon'}}],
    'current-context': 'cocoon@gke',
    'users': [{'name': 'cocoon', 'user': {'token': sys.argv[3]}}],
}
with open('/tmp/kubeconfig', 'w') as f:
    yaml.dump(kc, f, default_flow_style=False)
" "$SERVER" "$CA" "$TOKEN"

sudo mkdir -p /etc/cocoon
sudo cp /tmp/kubeconfig /etc/cocoon/kubeconfig
```

## 5. TLS Certificates (Cluster-Signed)

GKE API server requires kubelet TLS certs signed by the cluster CA for `kubectl port-forward`, `exec`, and `logs` to work.

Generate a CSR and have the cluster CA sign it:

```bash
NODE_IP=$(hostname -I | awk '{print $1}')
NODE_NAME=cocoon-pool  # or cocoon-pool-2

cat > /tmp/kubelet-csr.conf <<EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
CN = system:node:${NODE_NAME}
[v3_req]
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:${NODE_NAME},IP:${NODE_IP},IP:127.0.0.1
EOF

openssl req -new -newkey rsa:2048 -nodes \
  -keyout /tmp/kubelet.key -out /tmp/kubelet.csr \
  -config /tmp/kubelet-csr.conf \
  -subj "/O=system:nodes/CN=system:node:${NODE_NAME}"
```

Submit and approve the CSR:

```bash
CSR_DATA=$(cat /tmp/kubelet.csr | base64 -w0)

kubectl apply -f - <<EOF
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata:
  name: ${NODE_NAME}-kubelet
spec:
  request: ${CSR_DATA}
  signerName: kubernetes.io/kubelet-serving
  usages: [digital signature, key encipherment, server auth]
EOF

kubectl certificate approve ${NODE_NAME}-kubelet
kubectl get csr ${NODE_NAME}-kubelet -o jsonpath='{.status.certificate}' \
  | base64 -d > /tmp/kubelet-signed.crt
```

Install the signed cert:

```bash
sudo mkdir -p /etc/cocoon/vk/tls
sudo cp /tmp/kubelet-signed.crt /etc/cocoon/vk/tls/vk-kubelet.crt
sudo cp /tmp/kubelet.key /etc/cocoon/vk/tls/vk-kubelet.key
```

## 6. GKE Firewall Rule

Allow GKE master to reach the vk-cocoon kubelet API (port 10250):

```bash
gcloud compute instances add-tags cocoonset-node-1 --zone=asia-southeast1-b --tags=cocoonset-node
gcloud compute instances add-tags cocoonset-node-2 --zone=asia-southeast1-b --tags=cocoonset-node

gcloud compute firewall-rules create allow-gke-master-to-vk \
  --allow=tcp:10250 \
  --source-ranges=0.0.0.0/0 \
  --target-tags=cocoonset-node \
  --description="Allow GKE master to reach vk-cocoon kubelet API"
```

> For production, restrict `--source-ranges` to the GKE master's IP range instead of `0.0.0.0/0`.

## 7. Configure and Start vk-cocoon

```bash
sudo tee /etc/cocoon/vk-cocoon.env > /dev/null <<'EOF'
VK_NODE_NAME=cocoon-pool
COCOON_BIN=/usr/local/bin/cocoon
EPOCH_REGISTRY_TOKEN=<your-epoch-token>
EOF
```

For the second node, set `VK_NODE_NAME=cocoon-pool-2`.

Install the systemd service:

```bash
sudo cp deploy/vk-cocoon.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vk-cocoon
```

Verify the virtual node registered:

```bash
kubectl get nodes
# Should show cocoon-pool (or cocoon-pool-2) with status Ready
```

## 8. Deploy cocoon-operator

Apply CRDs and the operator deployment:

```bash
kubectl apply -f cocoon-operator/deploy/crd.yaml
kubectl apply -f cocoon-operator/deploy/cocoonset-crd.yaml
kubectl apply -f cocoon-operator/deploy/deploy.yaml
```

Ensure the operator pod runs on a GKE node (not a virtual node) by adding node affinity:

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: type
              operator: NotIn
              values: [virtual-kubelet]
```

## 9. Create a CocoonSet

### Windows VM (cloud image from Epoch)

```yaml
apiVersion: cocoon.cis/v1alpha1
kind: CocoonSet
metadata:
  name: win11-set-1
spec:
  nodeName: cocoon-pool
  agent:
    image: https://epoch.simular.cloud/win11
    mode: run
    os: windows
    network: dnsmasq-dhcp
    replicas: 1
    resources:
      cpu: "2"
      memory: "4Gi"
  snapshotPolicy: never
```

### Linux VM (snapshot clone)

```yaml
apiVersion: cocoon.cis/v1alpha1
kind: CocoonSet
metadata:
  name: ubuntu-set-1
spec:
  nodeName: cocoon-pool
  agent:
    image: ubuntu-dev-base
    replicas: 1
    resources:
      cpu: "2"
      memory: "4Gi"
    storage: "20G"
  snapshotPolicy: always
```

### Apply and verify

```bash
kubectl apply -f cocoonset.yaml
kubectl get cocoonsets
kubectl get pods -o wide
```

### Scale sub-agents

`replicas` controls the number of sub-agents forked from main (slot-0). Total pods = 1 (main) + replicas.

```bash
# Scale to 3 sub-agents (4 pods total)
kubectl patch cocoonset win11-set-1 --type merge -p '{"spec":{"agent":{"replicas":3}}}'
```

### Delete

```bash
kubectl delete cocoonset win11-set-1
```

With `snapshotPolicy: never`, VMs are destroyed immediately (~10s). With `always`, VMs are snapshotted and pushed to Epoch before destruction (can take minutes depending on VM size).

### CocoonSet spec reference

| Field | Description | Default |
|---|---|---|
| `nodeName` | Target virtual node name | `cocoon-pool` |
| `agent.image` | Epoch URL or local snapshot name | (required) |
| `agent.mode` | `run` for cloud images, `clone` for snapshots | `clone` |
| `agent.os` | `linux` or `windows` | `linux` |
| `agent.network` | CNI conflist name (e.g. `dnsmasq-dhcp`) | first conflist |
| `agent.replicas` | Number of sub-agents (0 = main only) | `0` |
| `agent.resources.cpu` | vCPU count | `2` |
| `agent.resources.memory` | Memory size | linux: `1G`, windows: `4G` |
| `agent.storage` | COW disk size | linux: `100G`, windows: `15G` |
| `snapshotPolicy` | `always`, `main-only`, or `never` | `always` |
| `suspend` | `true` to hibernate all VMs | `false` |

## 10. Access VMs

### RDP via kubectl port-forward (Windows)

Traffic flow:

```
local RDP client → 127.0.0.1:3389
  → kubectl port-forward
    → GKE API server (136.110.49.234)
      → vk-cocoon kubelet API (node-ip:10250)
        → PortForward() TCP proxy
          → VM (10.89.0.x:3389)
```

This requires steps 5 (cluster-signed TLS cert) and 6 (firewall rule for 10250) to be completed. Without them:
- Missing cert → `certificate signed by unknown authority`
- Missing firewall → `dial tcp <ip>:10250: i/o timeout`

```bash
kubectl port-forward pod/win11-set-1-0 3389:3389
# Then connect RDP client to localhost:3389
```

### Console (SAC)

```bash
ssh <node-ip>
sudo cocoon vm console <vm-name>
# Escape: Ctrl+]
```

### VM status

```bash
sudo cocoon vm list
kubectl get cocoonsets
kubectl get pods -o wide
```

## Troubleshooting

### Pod stuck in Pending

- Check vk-cocoon logs: `sudo journalctl -u vk-cocoon -f`
- Verify epoch token: look for `401 UNAUTHORIZED` in logs
- Check if cloud image is cached: `ls /var/lib/cocoon/cloudimg/blobs/`

### kubectl port-forward fails with TLS error

- `certificate signed by unknown authority`: kubelet cert not signed by cluster CA. Re-do step 5.
- `dial tcp <ip>:10250: i/o timeout`: firewall rule missing. Re-do step 6.
- `certificate is valid for 127.0.0.1, not <ip>`: cert SAN missing node IP. Regenerate with correct `subjectAltName`.

### Windows VM IP mismatch

- CNI IPAM must be empty (`{}`) for dnsmasq-dhcp network. If set to `"type": "dhcp"`, the CNI plugin and Windows guest both run DHCP clients and get different IPs.
- dnsmasq lease file must be at `/var/lib/misc/dnsmasq.leases` for vk-cocoon to resolve IPs.

### Slow CocoonSet startup

- vk-cocoon polls dnsmasq leases every 2s with a 120s timeout. If the lease file path is wrong or dnsmasq is not running, each pod waits the full timeout before falling back.
- Sub-agent pods (slot-1+) are created only after slot-0 is Running, so delays compound.

### Stale VMs after vk-cocoon restart

When vk-cocoon is restarted (e.g. after cert rotation), existing VMs may become `stopped (stale)`. These block new VM creation for the same pod names.

Clean up stale VMs:

```bash
# List stale VMs
sudo cocoon vm list

# Remove all stale VMs
for id in $(sudo cocoon vm list --format json | python3 -c "import sys,json; [print(v['id']) for v in json.load(sys.stdin)]"); do
  sudo cocoon vm rm --force $id
done
```

Then delete the affected pods to trigger fresh creation:

```bash
kubectl delete pods -l cocoon.cis/cocoonset --force
```
