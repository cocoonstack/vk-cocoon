package cocoon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/ociutil"
)

const snapshotReclaimInterval = 10 * time.Minute

func (p *Provider) StartSnapshotReclaimer() {
	p.goBackground(func() {
		p.reclaimLocalSnapshots(p.lifecycleCtx)
		commonk8s.RunTicker(p.lifecycleCtx, snapshotReclaimInterval, p.reclaimLocalSnapshots)
	})
}

func (p *Provider) reclaimLocalSnapshots(ctx context.Context) {
	if p.Registry == nil {
		return
	}
	logger := log.WithFunc("Provider.reclaimLocalSnapshots")
	snapshots, err := p.Runtime.SnapshotList(ctx)
	if err != nil {
		logger.Warnf(ctx, "list local snapshots: %v", err)
		return
	}
	inUse := p.snapshotNamesInUse()
	for _, s := range snapshots {
		vmName, isImport := strings.CutSuffix(s.Name, meta.HibernateImportSuffix)
		if !strings.HasPrefix(vmName, "vk-") || inUse[vmName] {
			continue
		}
		if isImport {
			logger.Infof(ctx, "reclaim local snapshot %s: no pod on this node wakes %s", s.Name, vmName)
			p.removeSnapshotDetached(ctx, s.Name)
			continue
		}
		if _, _, verifyErr := p.verifyLocalSnapshot(ctx, vmName, &s); errors.Is(verifyErr, errStaleLocalSnapshot) {
			logger.Infof(ctx, "reclaim local snapshot %s: %v", s.Name, verifyErr)
			p.removeLocalSnapshots(ctx, vmName)
		}
	}
}

func (p *Provider) snapshotNamesInUse() map[string]bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	names := make(map[string]bool, 2*len(p.pods))
	for _, pod := range p.pods {
		spec := meta.ParseVMSpec(pod)
		names[spec.VMName] = true
		names[localSnapshotName(ociutil.ParseRef(spec.Image))] = true
	}
	return names
}
