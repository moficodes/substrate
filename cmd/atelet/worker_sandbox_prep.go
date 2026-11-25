// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	atev1 "go.ate.dev/substrate/api/infra/k8s/ate/v1"
)

// maxConcurrentPreps bounds how many sandbox versions download, extract, and
// probe concurrently in the background. Four lets healthy uncached configs
// make progress alongside a stalled or slow config without saturating node
// disk or network bandwidth on cold multi-version configs.
const maxConcurrentPreps = 4

// errVersionPreparing reports that a sandbox version needed by a worker pod
// is being prepared in the background. The preparer re-enqueues the pod once
// the background work finishes, so finish neither emits a failure event nor
// schedules a rate-limited retry.
var errVersionPreparing = errors.New("sandbox version preparing")

// versionPrepKey identifies one background version preparation: its sandbox
// class and the exact asset names, digests, and URLs to fetch. Including URLs
// ensures editing a failing asset URL drops the stale recorded failure and
// frees the old preparation slot. An already in-flight filecache download for
// the same SHA256 continues detached until --sandbox-asset-fetch-timeout.
func versionPrepKey(class atev1.SandboxClass, rec *sandboxAssetsRecord) string {
	lines := make([]string, 0, len(rec.Assets))
	for name, a := range rec.Assets {
		lines = append(lines, name+"\x00"+a.SHA256+"\x00"+a.URL)
	}
	slices.Sort(lines)
	return string(class) + "\n" + strings.Join(lines, "\n")
}

// prepFlight tracks one in-flight background version preparation and the local
// worker pod UIDs waiting on it.
type prepFlight struct {
	waiters map[string]struct{}
	cancel  context.CancelFunc
}

// versionPreparer runs slow per-version asset fetches and probes off the pod
// sync workers, deduplicating concurrent pods that need the same version and
// re-enqueuing waiting pod UIDs on completion.
type versionPreparer struct {
	sem     chan struct{}
	enqueue func(uid string)
	wg      sync.WaitGroup

	mu      sync.Mutex
	flights map[string]*prepFlight
	// failed holds per-pod errors from completed flights that pod waited on.
	// Entries are peeked while sibling flights for the pod are still running,
	// and cleared by reconcilePod once a sync pass has no pending flights.
	failed map[string]map[string]error
}

func newVersionPreparer(concurrency int, enqueue func(uid string)) *versionPreparer {
	return &versionPreparer{
		sem:     make(chan struct{}, concurrency),
		enqueue: enqueue,
		flights: make(map[string]*prepFlight),
		failed:  make(map[string]map[string]error),
	}
}

// ensure returns any recorded error for uid from a completed preparation of
// key without clearing it, so sibling flights can finish before the pod
// settles and retries. Otherwise it attaches uid to an in-flight preparation
// (starting one if needed) and returns errVersionPreparing.
func (p *versionPreparer) ensure(ctx context.Context, uid, key string, run func(parentCtx, prepCtx context.Context) error) error {
	p.mu.Lock()
	if errs := p.failed[uid]; errs != nil {
		if err, ok := errs[key]; ok {
			p.mu.Unlock()
			return err
		}
	}
	if f, ok := p.flights[key]; ok {
		f.waiters[uid] = struct{}{}
		p.mu.Unlock()
		return errVersionPreparing
	}
	prepCtx, cancel := context.WithCancel(ctx)
	f := &prepFlight{
		waiters: map[string]struct{}{uid: {}},
		cancel:  cancel,
	}
	p.flights[key] = f
	p.wg.Add(1)
	p.mu.Unlock()

	go p.execute(ctx, prepCtx, key, f, run)
	return errVersionPreparing
}

func (p *versionPreparer) execute(parentCtx, prepCtx context.Context, key string, f *prepFlight, run func(parentCtx, prepCtx context.Context) error) {
	defer p.wg.Done()
	defer f.cancel()

	err := p.runBounded(parentCtx, prepCtx, run)

	p.mu.Lock()
	if p.flights[key] != f {
		// Canceled and removed because all waiters left.
		p.mu.Unlock()
		return
	}
	delete(p.flights, key)
	if parentCtx.Err() != nil {
		// Syncer is shutting down; skip failure recording and re-enqueuing.
		p.mu.Unlock()
		return
	}
	waiters := make([]string, 0, len(f.waiters))
	for uid := range f.waiters {
		waiters = append(waiters, uid)
		if err != nil {
			if p.failed[uid] == nil {
				p.failed[uid] = make(map[string]error)
			}
			p.failed[uid][key] = err
		}
	}
	p.mu.Unlock()

	for _, uid := range waiters {
		p.enqueue(uid)
	}
}

func (p *versionPreparer) runBounded(parentCtx, prepCtx context.Context, run func(parentCtx, prepCtx context.Context) error) error {
	if err := prepCtx.Err(); err != nil {
		return err
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
		return run(parentCtx, prepCtx)
	case <-prepCtx.Done():
		return prepCtx.Err()
	}
}

// reconcilePod removes uid from every in-flight preparation outside
// keepFlights (canceling any flight that loses its last waiter) and removes
// recorded failures for uid outside keepFlights and keepFailed. Passing nil
// for both maps clears every wait and recorded failure for uid.
func (p *versionPreparer) reconcilePod(uid string, keepFlights, keepFailed map[string]struct{}) {
	var toCancel []context.CancelFunc
	p.mu.Lock()
	for key, f := range p.flights {
		if _, ok := keepFlights[key]; ok {
			continue
		}
		if _, waiting := f.waiters[uid]; !waiting {
			continue
		}
		delete(f.waiters, uid)
		if len(f.waiters) == 0 {
			delete(p.flights, key)
			toCancel = append(toCancel, f.cancel)
		}
	}
	if errs := p.failed[uid]; errs != nil {
		for key := range errs {
			_, inFlights := keepFlights[key]
			_, inFailed := keepFailed[key]
			if !inFlights && !inFailed {
				delete(errs, key)
			}
		}
		if len(errs) == 0 {
			delete(p.failed, uid)
		}
	}
	p.mu.Unlock()
	for _, cancel := range toCancel {
		cancel()
	}
}

// wait blocks until every background preparation goroutine has exited. Callers
// must stop starting new preparations before calling wait.
func (p *versionPreparer) wait() {
	p.wg.Wait()
}
