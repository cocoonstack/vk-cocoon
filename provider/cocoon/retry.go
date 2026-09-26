package cocoon

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
)

const (
	opRetryBaseDelay = 15 * time.Second
	opRetryMaxDelay  = 5 * time.Minute
)

type opRetry struct {
	delay time.Duration
	due   time.Time
}

func (p *Provider) podLock(key string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.podLocks[key]
	if l == nil {
		l = &sync.Mutex{}
		p.podLocks[key] = l
	}
	return l
}

func (p *Provider) takeOpRetry(key string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.opRetries[key]
	delete(p.opRetries, key)
	return r.delay
}

func (p *Provider) retryOpLater(ctx context.Context, pod *corev1.Pod, prev time.Duration, err error) {
	delay := cmp.Or(min(prev*2, opRetryMaxDelay), opRetryBaseDelay)
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	tracked := p.pods[key]
	owned := tracked != nil && tracked.UID == pod.UID
	if owned {
		p.opRetries[key] = opRetry{delay: delay, due: time.Now().Add(delay)}
	}
	p.mu.Unlock()
	if owned {
		log.WithFunc("Provider.retryOpLater").Warnf(ctx, "update of %s/%s failed, retrying in %s: %v", pod.Namespace, pod.Name, delay, err)
	}
}

func (p *Provider) retryDueOps(ctx context.Context) {
	now := time.Now()
	p.mu.RLock()
	var due []string
	for key, r := range p.opRetries {
		if !now.Before(r.due) {
			due = append(due, key)
		}
	}
	p.mu.RUnlock()
	for _, key := range due {
		p.goBackground(func() { p.retryOp(ctx, key) })
	}
}

func (p *Provider) retryOp(ctx context.Context, key string) {
	l := p.podLock(key)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	p.mu.RLock()
	r, owed := p.opRetries[key]
	tracked := p.pods[key].DeepCopy()
	p.mu.RUnlock()
	if !owed || time.Now().Before(r.due) || tracked == nil {
		return
	}
	if err := p.updatePod(ctx, tracked); err != nil {
		log.WithFunc("Provider.retryOp").Warnf(ctx, "retry update of %s: %v", key, err)
	}
}
