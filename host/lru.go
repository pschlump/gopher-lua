package host

// A minimal mutex-guarded LRU — the shape of glasm host's pluto
// lru_ts usage, in-package so the module carries no dependency for it.

import "sync"

type lruEntry[K comparable, V any] struct {
	key  K
	val  V
	prev, next *lruEntry[K, V]
}

type lru[K comparable, V any] struct {
	mu   sync.Mutex
	cap  int
	m    map[K]*lruEntry[K, V]
	head *lruEntry[K, V] // most recently used
	tail *lruEntry[K, V] // least recently used
}

func newLru[K comparable, V any](capacity int) *lru[K, V] {
	return &lru[K, V]{cap: capacity, m: make(map[K]*lruEntry[K, V], capacity+1)}
}

func (l *lru[K, V]) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// get looks up key and re-marks recency.
func (l *lru[K, V]) get(key K) (V, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[key]
	if !ok {
		var zero V
		return zero, false
	}
	l.moveToFront(e)
	return e.val, true
}

// take is the atomic get+delete (an entry leaves the cache on hit so
// two takers can never hold one value).
func (l *lru[K, V]) take(key K) (V, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[key]
	if !ok {
		var zero V
		return zero, false
	}
	l.remove(e)
	delete(l.m, key)
	return e.val, true
}

// put inserts/refreshes key: a key already present refreshes its own
// slot (no victim); a new key at capacity pops the LRU victim, which is
// returned for the caller to close OUTSIDE the lock.
func (l *lru[K, V]) put(key K, val V) (V, bool) {
	l.mu.Lock()
	victim, popped := l.putLocked(key, val)
	l.mu.Unlock()
	return victim, popped
}

func (l *lru[K, V]) putLocked(key K, val V) (V, bool) {
	var zero V
	if e, ok := l.m[key]; ok {
		e.val = val
		l.moveToFront(e)
		return zero, false
	}
	e := &lruEntry[K, V]{key: key, val: val}
	l.m[key] = e
	l.pushFront(e)
	if len(l.m) > l.cap {
		if victim := l.popOldestLocked(); victim != nil && victim.key != key {
			delete(l.m, victim.key)
			return victim.val, true
		}
	}
	return zero, false
}

// popOldest removes and returns the least recently used entry.
func (l *lru[K, V]) popOldest() (K, V, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var zeroK K
	var zeroV V
	e := l.popOldestLocked()
	if e == nil {
		return zeroK, zeroV, false
	}
	delete(l.m, e.key)
	return e.key, e.val, true
}

func (l *lru[K, V]) popOldestLocked() *lruEntry[K, V] {
	e := l.tail
	if e == nil {
		return nil
	}
	l.remove(e)
	return e
}

// drain removes every entry and returns the values.
func (l *lru[K, V]) drain() []V {
	l.mu.Lock()
	defer l.mu.Unlock()
	vals := make([]V, 0, len(l.m))
	for e := l.head; e != nil; e = e.next {
		vals = append(vals, e.val)
	}
	l.m = make(map[K]*lruEntry[K, V], l.cap+1)
	l.head, l.tail = nil, nil
	return vals
}

func (l *lru[K, V]) pushFront(e *lruEntry[K, V]) {
	e.prev = nil
	e.next = l.head
	if l.head != nil {
		l.head.prev = e
	}
	l.head = e
	if l.tail == nil {
		l.tail = e
	}
}

func (l *lru[K, V]) remove(e *lruEntry[K, V]) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

func (l *lru[K, V]) moveToFront(e *lruEntry[K, V]) {
	if l.head == e {
		return
	}
	l.remove(e)
	l.pushFront(e)
}
