// replicator the replication logic for an OrbitDB store
package replicator

import (
	"context"
	"fmt"
	"sync"
	"time"

	ipfslog "berty.tech/go-ipfs-log"
	"berty.tech/go-orbit-db/events"
	"github.com/ipfs/go-cid"
	"github.com/pkg/errors"
	"github.com/prometheus/common/log"
)

var batchSize = 1

type replicator struct {
	events.EventEmitter

	cancelFunc          context.CancelFunc
	store               storeInterface
	fetching            map[string]cid.Cid
	statsTasksRequested uint
	statsTasksStarted   uint
	statsTasksProcessed uint
	buffer              []ipfslog.Log
	concurrency         uint
	queue               map[string]cid.Cid
	lock                sync.RWMutex
}

func (r *replicator) GetBufferLen() int {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return len(r.buffer)
}

func (r *replicator) Stop() {
	r.cancelFunc()
}

func (r *replicator) GetQueue() []cid.Cid {
	var queue []cid.Cid

	r.lock.RLock()
	for _, c := range r.queue {
		queue = append(queue, c)
	}
	r.lock.RUnlock()

	return queue
}

func (r *replicator) Load(ctx context.Context, cids []cid.Cid) {
	for _, h := range cids {
		inLog := r.store.OpLog().GetEntries().UnsafeGet(h.String()) != nil
		r.lock.RLock()
		_, fetching := r.fetching[h.String()]
		_, queued := r.queue[h.String()]
		r.lock.RUnlock()

		if fetching || queued || inLog {
			continue
		}

		r.addToQueue(h)
	}

	r.processQueue(ctx)
}

// NewReplicator Creates a new Replicator instance
func NewReplicator(ctx context.Context, store storeInterface, concurrency uint) Replicator {
	ctx, cancelFunc := context.WithCancel(ctx)

	if concurrency == 0 {
		concurrency = 128
	}

	r := replicator{
		cancelFunc:  cancelFunc,
		concurrency: concurrency,
		store:       store,
		queue:       map[string]cid.Cid{},
		fetching:    map[string]cid.Cid{},
	}

	go func() {
		for {
			select {
			case <-time.After(time.Second * 3):
				r.lock.RLock()
				qLen := len(r.queue)
				r.lock.RUnlock()

				if r.tasksRunning() == 0 && qLen > 0 {
					logger().Debug(fmt.Sprintf("Had to flush the queue! %d items in the queue, %d %d tasks requested/finished", qLen, r.tasksRequested(), r.tasksFinished()))
					r.processQueue(ctx)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return &r
}

func (r *replicator) tasksRunning() uint {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.statsTasksStarted - r.statsTasksProcessed
}

func (r *replicator) tasksRequested() uint {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.statsTasksRequested
}

func (r *replicator) tasksFinished() uint {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.statsTasksProcessed
}

func (r *replicator) queueSlice() []cid.Cid {
	var slice []cid.Cid

	r.lock.RLock()
	for _, v := range r.queue {
		slice = append(slice, v)
	}
	r.lock.RUnlock()

	return slice
}

func (r *replicator) processOne(ctx context.Context, h cid.Cid) ([]cid.Cid, error) {
	r.lock.RLock()
	_, isFetching := r.fetching[h.String()]
	_, hasEntry := r.store.OpLog().Values().Get(h.String())
	r.lock.RUnlock()

	if hasEntry || isFetching {
		return nil, nil
	}

	r.lock.Lock()
	r.fetching[h.String()] = h
	r.lock.Unlock()

	r.Emit(NewEventLoadAdded(h))

	r.lock.Lock()
	r.statsTasksStarted++
	r.lock.Unlock()

	l, err := ipfslog.NewFromEntryHash(ctx, r.store.IPFS(), r.store.Identity(), h, &ipfslog.LogOptions{
		ID:               r.store.OpLog().GetID(),
		AccessController: r.store.AccessController(),
	}, &ipfslog.FetchOptions{
		Length: &batchSize,
	})

	if err != nil {
		return nil, errors.Wrap(err, "unable to fetch log")
	}

	var logToAppend ipfslog.Log = l

	r.lock.Lock()
	r.buffer = append(r.buffer, logToAppend)

	latest := l.Values().At(0)

	delete(r.queue, h.String())

	// Mark this task as processed
	r.statsTasksProcessed++
	r.lock.Unlock()

	r.lock.RLock()
	b := r.buffer
	r.lock.RUnlock()

	// Notify subscribers that we made progress
	r.Emit(NewEventLoadProgress("", h, latest, nil, len(b))) // TODO JS: this._id should be undefined

	var nextValues []cid.Cid

	for _, e := range l.Values().Slice() {
		for _, n := range e.GetNext() {
			nextValues = append(nextValues, n)
		}
	}

	// Return all next pointers
	return nextValues, nil
}

func (r *replicator) processQueue(ctx context.Context) {
	if r.tasksRunning() >= r.concurrency {
		return
	}

	var hashesList [][]cid.Cid
	capacity := r.concurrency - r.tasksRunning()
	slicedQueue := r.queueSlice()
	if uint(len(slicedQueue)) < capacity {
		capacity = uint(len(slicedQueue))
	}

	items := map[string]cid.Cid{}
	for _, h := range slicedQueue[:capacity] {
		items[h.String()] = h
	}

	for _, e := range items {
		r.lock.Lock()
		delete(r.queue, e.String())
		r.lock.Unlock()

		hashes, err := r.processOne(ctx, e)
		if err != nil {
			log.Errorf("unable to get data to process %v", err)
			return
		}

		hashesList = append(hashesList, hashes)
	}

	for _, hashes := range hashesList {
		r.lock.RLock()
		b := r.buffer
		r.lock.RUnlock()

		if (len(items) > 0 && len(b) > 0) || (r.tasksRunning() == 0 && len(b) > 0) {
			r.lock.Lock()
			r.buffer = []ipfslog.Log{}
			r.lock.Unlock()

			logger().Debug(fmt.Sprintf("load end logs, logs found :%d", len(b)))

			r.Emit(NewEventLoadEnd(b))
		}

		if len(hashes) > 0 {
			r.Load(ctx, hashes)
		}
	}
}

func (r *replicator) addToQueue(h cid.Cid) {
	r.lock.Lock()
	r.statsTasksRequested++
	r.queue[h.String()] = h
	r.lock.Unlock()
}

var _ Replicator = &replicator{}
