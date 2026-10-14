// Package gds implements GreedyDual-Size eviction, which chooses what to evict
// from a cache whose entries cost different amounts to replace.
//
// Each entry has a value: the cost of losing it, divided by the space it takes
// up. An entry's priority is L plus its value, where L starts at 0, and Evict
// removes the entry with the lowest priority and raises L to that priority. So
// entries with a high value survive longer, but an entry that isn't re-added
// eventually falls below the rising L and is evicted. Re-adding an entry
// refreshes its priority to the current L plus its value.
//
// When every entry has the same value, entries are evicted in the order they
// were added, which is LRU.
//
// See Cao and Irani, "Cost-Aware WWW Proxy Caching Algorithms" (1997).
package gds

import (
	"container/heap"
)

// Pool tracks the eviction priorities of a set of entries. It is not safe for
// concurrent use.
type Pool[K comparable] struct {
	// The priority of the most recently evicted entry.
	inflation int64
	// Counts additions, to evict entries with equal priorities in the order
	// they were added.
	clock   int64
	entries map[K]*entry[K]
	heap    entryHeap[K]
}

type entry[K comparable] struct {
	key      K
	priority int64
	added    int64
	index    int
}

// New returns an empty pool.
func New[K comparable]() *Pool[K] {
	return &Pool[K]{entries: map[K]*entry[K]{}}
}

// Add adds an entry with the given value, or refreshes the priority of an
// entry that is already in the pool.
func (p *Pool[K]) Add(key K, value int64) {
	p.clock++
	priority := p.inflation + value
	if e, ok := p.entries[key]; ok {
		e.priority, e.added = priority, p.clock
		heap.Fix(&p.heap, e.index)
		return
	}
	e := &entry[K]{key: key, priority: priority, added: p.clock}
	p.entries[key] = e
	heap.Push(&p.heap, e)
}

// Remove removes an entry without evicting it, for example because the
// entry is being used, and returns whether it was in the pool.
func (p *Pool[K]) Remove(key K) bool {
	e, ok := p.entries[key]
	if !ok {
		return false
	}
	heap.Remove(&p.heap, e.index)
	delete(p.entries, key)
	return true
}

// Evict removes and returns the entry with the lowest priority, and returns
// false if the pool is empty.
func (p *Pool[K]) Evict() (K, bool) {
	if len(p.heap) == 0 {
		var zero K
		return zero, false
	}
	e := heap.Pop(&p.heap).(*entry[K])
	delete(p.entries, e.key)
	p.inflation = e.priority
	return e.key, true
}

// Len returns the number of entries in the pool.
func (p *Pool[K]) Len() int {
	return len(p.heap)
}

type entryHeap[K comparable] []*entry[K]

func (h entryHeap[K]) Len() int { return len(h) }

func (h entryHeap[K]) Less(i, j int) bool {
	if h[i].priority != h[j].priority {
		return h[i].priority < h[j].priority
	}
	return h[i].added < h[j].added
}

func (h entryHeap[K]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *entryHeap[K]) Push(x any) {
	e := x.(*entry[K])
	e.index = len(*h)
	*h = append(*h, e)
}

func (h *entryHeap[K]) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return e
}
