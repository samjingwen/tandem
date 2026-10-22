package api

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
)

// Only complete immutable record encodings are cached, never a full snapshot.
// Eviction changes performance, not readability or cursor semantics. Entries
// above the memory budget use the uncached path rather than being rejected.
const readCacheBytes = 32 << 20

type cachedRead struct {
	key    string
	data   []byte
	sha256 string
}
type readCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	bytes   int
}

func cacheKey(cursor readCursor) string {
	return cursor.InstanceID + "/" + strconv.FormatInt(cursor.Revision.Revision, 10) + "/" + cursor.Collection + "/" + cursor.Key
}

func (cache *readCache) put(cursor readCursor, data []byte) {
	if len(data) > readCacheBytes {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = map[string]*list.Element{}
	}
	key := cacheKey(cursor)
	if entry := cache.entries[key]; entry != nil {
		cache.order.MoveToFront(entry)
		return
	}
	for cache.bytes+len(data) > readCacheBytes || cache.order.Len() >= 128 {
		entry := cache.order.Back()
		value := entry.Value.(cachedRead)
		delete(cache.entries, value.key)
		cache.bytes -= len(value.data)
		cache.order.Remove(entry)
	}
	hash := sha256.Sum256(data)
	cache.entries[key] = cache.order.PushFront(cachedRead{key: key, data: data, sha256: hex.EncodeToString(hash[:])})
	cache.bytes += len(data)
}

func (cache *readCache) get(cursor readCursor) (cachedRead, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[cacheKey(cursor)]
	if entry == nil {
		return cachedRead{}, false
	}
	cache.order.MoveToFront(entry)
	return entry.Value.(cachedRead), true
}
