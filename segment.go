package rw_freecache

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	ENTRY_HDR_SIZE = 24
)

var ErrLargeKey = errors.New("The key is larger than 65535")
var ErrLargeEntry = errors.New("The entry size is larger than 1/1024 of cache size")
var ErrNotFound = errors.New("Entry not found")

// entry pointer struct points to an entry in ring buffer
type entryPtr struct {
	offset   int64  // entry offset in ring buffer
	hash16   uint16 // entries are ordered by hash16 in a slot.
	keyLen   uint16 // used to compare a key
	reserved uint32
}

// entry header struct in ring buffer, followed by key and value.
type entryHdr struct {
	expireAt uint32
	keyLen   uint16
	hash16   uint16
	valLen   uint32
	valCap   uint32
	deleted  bool
	slotId   uint8
	reserved uint16
	_        uint32 //占位 4 bytes
}

// a segment contains 256 slots, a slot is an array of entry pointers ordered by hash16 value
// the entry can be looked up by hash value of the key.
type segment struct {
	rb            RingBuf       // ring buffer that stores data
	lock          *sync.RWMutex //address is 32bit (4 bytes)
	segId         int
	hitCount      int64
	missCount     int64
	entryCount    int64
	totalCount    int64      // number of entries in ring buffer, including deleted entries.
	totalEvacuate int64      // used for debug
	totalExpired  int64      // used for debug
	overwrites    int64      // used for debug
	vacuumLen     int64      // up to vacuumLen, new data can be written without overwriting old data.
	slotLens      [256]int32 // The actual length for every slot.
	slotCap       int32      // max number of entry pointers a slot can hold.
	slotsData     []entryPtr // shared by all 256 slots
}

func newSegment(bufSize int, segId int) (seg segment) {
	seg.rb = NewRingBuf(bufSize, 0)
	seg.lock = new(sync.RWMutex)
	seg.segId = segId
	seg.vacuumLen = int64(bufSize)
	seg.slotCap = 1
	seg.slotsData = make([]entryPtr, 256*seg.slotCap)
	return
}

func (seg *segment) set(key, value []byte, hashVal uint64, expireSeconds int) (err error) {
	seg.lock.Lock()
	if len(key) > 65535 {
		err = ErrLargeKey
		seg.lock.Unlock()
		return
	}
	maxKeyValLen := len(seg.rb.data)/4 - ENTRY_HDR_SIZE
	if len(key)+len(value) > maxKeyValLen {
		// Do not accept large entry.
		err = ErrLargeEntry
		seg.lock.Unlock()
		return
	}
	now := uint32(time.Now().Unix())
	expireAt := uint32(0)
	if expireSeconds > 0 {
		expireAt = now + uint32(expireSeconds)
	}

	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)

	var hdrBuf [ENTRY_HDR_SIZE]byte
	hdr := (*entryHdr)(unsafe.Pointer(&hdrBuf[0]))

	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if match {
		matchedPtr := &slot[idx]
		seg.rb.ReadAt(hdrBuf[:], matchedPtr.offset)
		hdr.slotId = slotId
		hdr.hash16 = hash16
		hdr.keyLen = uint16(len(key))
		hdr.expireAt = expireAt
		hdr.valLen = uint32(len(value))
		if hdr.valCap >= hdr.valLen {
			//in place overwrite
			seg.rb.WriteAt(hdrBuf[:], matchedPtr.offset)
			seg.rb.WriteAt(value, matchedPtr.offset+ENTRY_HDR_SIZE+int64(hdr.keyLen))
			atomic.AddInt64(&seg.overwrites, 1)
			seg.lock.Unlock()
			return
		}
		// avoid unnecessary memory copy.
		seg.delEntryPtr(slotId, hash16, slot[idx].offset)
		match = false
		// increase capacity and limit entry len.
		for hdr.valCap < hdr.valLen {
			hdr.valCap *= 2
		}
		if hdr.valCap > uint32(maxKeyValLen-len(key)) {
			hdr.valCap = uint32(maxKeyValLen - len(key))
		}
	} else {
		hdr.slotId = slotId
		hdr.hash16 = hash16
		hdr.keyLen = uint16(len(key))
		hdr.expireAt = expireAt
		hdr.valLen = uint32(len(value))
		hdr.valCap = uint32(len(value))
		if hdr.valCap == 0 {
			// avoid infinite loop when increasing capacity.
			hdr.valCap = 1
		}
	}

	entryLen := ENTRY_HDR_SIZE + int64(len(key)) + int64(hdr.valCap)
	slotModified := seg.evacuate(entryLen, slotId, now)
	if slotModified {
		// the slot has been modified during evacuation, we need to looked up for the 'idx' again.
		// otherwise there would be index out of bound error.
		slot = seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
		idx, _ = seg.lookup(slot, hash16, key)
		// assert(match == false)
	}
	newOff := seg.rb.End()
	seg.insertEntryPtr(slotId, hash16, newOff, idx, hdr.keyLen)
	seg.rb.Write(hdrBuf[:])
	seg.rb.Write(key)
	seg.rb.Write(value)
	seg.rb.Skip(int64(hdr.valCap - hdr.valLen))
	atomic.AddInt64(&seg.totalCount, 1)
	seg.vacuumLen -= entryLen
	seg.lock.Unlock()
	err = nil
	return
}

func (seg *segment) evacuate(entryLen int64, slotId uint8, now uint32) (slotModified bool) {
	var oldHdrBuf [ENTRY_HDR_SIZE]byte
	consecutiveEvacuate := 0
	for seg.vacuumLen < entryLen {
		oldOff := seg.rb.End() + seg.vacuumLen - seg.rb.Size()
		seg.rb.ReadAt(oldHdrBuf[:], oldOff)
		oldHdr := (*entryHdr)(unsafe.Pointer(&oldHdrBuf[0]))
		oldEntryLen := ENTRY_HDR_SIZE + int64(oldHdr.keyLen) + int64(oldHdr.valCap)
		if oldHdr.deleted {
			consecutiveEvacuate = 0
			atomic.AddInt64(&seg.totalCount, -1)
			seg.vacuumLen += oldEntryLen
			continue
		}
		expired := oldHdr.expireAt != 0 && oldHdr.expireAt <= now //元素超时
		randomKeyDelete := ((oldHdr.expireAt | now) & 3) == 3     //随机25%
		if expired || randomKeyDelete || consecutiveEvacuate > 5 {
			seg.delEntryPtr(oldHdr.slotId, oldHdr.hash16, oldOff)
			if oldHdr.slotId == slotId {
				slotModified = true
			}
			consecutiveEvacuate = 0
			atomic.AddInt64(&seg.totalCount, -1)
			seg.vacuumLen += oldEntryLen
			if expired {
				atomic.AddInt64(&seg.totalExpired, 1)
			}
		} else {
			// evacuate an old entry that has been accessed recently for better cache hit rate.
			newOff := seg.rb.Evacuate(oldOff, int(oldEntryLen))
			seg.updateEntryPtr(oldHdr.slotId, oldHdr.hash16, oldOff, newOff)
			consecutiveEvacuate++
			atomic.AddInt64(&seg.totalEvacuate, 1)
		}
	}
	return
}

func (seg *segment) get(key []byte, hashVal uint64) (value []byte, expireAt uint32, err error) {
	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)
	seg.lock.RLock()
	slotOff := int32(slotId) * seg.slotCap
	var slot = seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if !match {
		err = ErrNotFound
		atomic.AddInt64(&seg.missCount, 1)
		seg.lock.RUnlock()
		return
	}
	ptr := &slot[idx]
	now := uint32(time.Now().Unix())

	var hdrBuf [ENTRY_HDR_SIZE]byte
	seg.rb.ReadAt(hdrBuf[:], ptr.offset)
	hdr := (*entryHdr)(unsafe.Pointer(&hdrBuf[0]))
	expireAt = hdr.expireAt

	if hdr.expireAt != 0 && hdr.expireAt <= now {
		seg.lock.RUnlock()
		seg.lock.Lock()
		seg.delEntryPtr(slotId, hash16, ptr.offset)
		atomic.AddInt64(&seg.totalExpired, 1)
		atomic.AddInt64(&seg.missCount, 1)
		err = ErrNotFound
		seg.lock.Unlock()
		return
	}

	seg.rb.WriteAt(hdrBuf[:], ptr.offset)
	value = make([]byte, hdr.valLen)
	seg.rb.ReadAt(value, ptr.offset+ENTRY_HDR_SIZE+int64(hdr.keyLen))
	atomic.AddInt64(&seg.hitCount, 1)
	seg.lock.RUnlock()
	return
}

func (seg *segment) del(key []byte, hashVal uint64) (affected bool) {
	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)
	seg.lock.Lock()
	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if !match {
		affected = false
		seg.lock.Unlock()
		return
	}
	ptr := &slot[idx]
	seg.delEntryPtr(slotId, hash16, ptr.offset)
	affected = true
	seg.lock.Unlock()
	return
}

func (seg *segment) ttl(key []byte, hashVal uint64) (timeLeft uint32, err error) {
	slotId := uint8(hashVal >> 8)
	hash16 := uint16(hashVal >> 16)
	seg.lock.Lock()
	slotOff := int32(slotId) * seg.slotCap
	var slot = seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookup(slot, hash16, key)
	if !match {
		err = ErrNotFound
		seg.lock.Unlock()
		return
	}
	ptr := &slot[idx]
	now := uint32(time.Now().Unix())

	var hdrBuf [ENTRY_HDR_SIZE]byte
	seg.rb.ReadAt(hdrBuf[:], ptr.offset)
	hdr := (*entryHdr)(unsafe.Pointer(&hdrBuf[0]))

	if hdr.expireAt == 0 {
		timeLeft = 0
		seg.lock.Unlock()
		return
	} else if hdr.expireAt != 0 && hdr.expireAt >= now {
		timeLeft = hdr.expireAt - now
		seg.lock.Unlock()
		return
	}
	err = ErrNotFound
	seg.lock.Unlock()
	return
}

func (seg *segment) expand() {
	newSlotData := make([]entryPtr, seg.slotCap*2*256)
	for i := 0; i < 256; i++ {
		off := int32(i) * seg.slotCap
		copy(newSlotData[off*2:], seg.slotsData[off:off+seg.slotLens[i]])
	}
	seg.slotCap *= 2
	seg.slotsData = newSlotData
}

func (seg *segment) updateEntryPtr(slotId uint8, hash16 uint16, oldOff, newOff int64) {
	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookupByOff(slot, hash16, oldOff)
	if !match {
		return
	}
	ptr := &slot[idx]
	ptr.offset = newOff
}

func (seg *segment) insertEntryPtr(slotId uint8, hash16 uint16, offset int64, idx int, keyLen uint16) {
	slotOff := int32(slotId) * seg.slotCap
	if seg.slotLens[slotId] == seg.slotCap {
		seg.expand()
		slotOff *= 2
	}
	seg.slotLens[slotId]++
	atomic.AddInt64(&seg.entryCount, 1)
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	copy(slot[idx+1:], slot[idx:])
	slot[idx].offset = offset
	slot[idx].hash16 = hash16
	slot[idx].keyLen = keyLen
}

func (seg *segment) delEntryPtr(slotId uint8, hash16 uint16, offset int64) {
	slotOff := int32(slotId) * seg.slotCap
	slot := seg.slotsData[slotOff : slotOff+seg.slotLens[slotId] : slotOff+seg.slotCap]
	idx, match := seg.lookupByOff(slot, hash16, offset)
	if !match {
		return
	}
	var entryHdrBuf [ENTRY_HDR_SIZE]byte
	seg.rb.ReadAt(entryHdrBuf[:], offset)
	entryHdr := (*entryHdr)(unsafe.Pointer(&entryHdrBuf[0]))
	entryHdr.deleted = true
	seg.rb.WriteAt(entryHdrBuf[:], offset)
	copy(slot[idx:], slot[idx+1:])
	seg.slotLens[slotId]--
	atomic.AddInt64(&seg.entryCount, -1)
}

func entryPtrIdx(slot []entryPtr, hash16 uint16) (idx int) {
	high := len(slot)
	for idx < high {
		mid := (idx + high) >> 1
		oldEntry := &slot[mid]
		if oldEntry.hash16 < hash16 {
			idx = mid + 1
		} else {
			high = mid
		}
	}
	return
}

func (seg *segment) lookup(slot []entryPtr, hash16 uint16, key []byte) (idx int, match bool) {
	idx = entryPtrIdx(slot, hash16)
	for idx < len(slot) {
		ptr := &slot[idx]
		if ptr.hash16 != hash16 {
			break
		}
		match = int(ptr.keyLen) == len(key) && seg.rb.EqualAt(key, ptr.offset+ENTRY_HDR_SIZE)
		if match {
			return
		}
		idx++
	}
	return
}

func (seg *segment) lookupByOff(slot []entryPtr, hash16 uint16, offset int64) (idx int, match bool) {
	idx = entryPtrIdx(slot, hash16)
	for idx < len(slot) {
		ptr := &slot[idx]
		if ptr.hash16 != hash16 {
			break
		}
		match = ptr.offset == offset
		if match {
			return
		}
		idx++
	}
	return
}

func (seg *segment) resetStatistics() {
	seg.lock.Lock()
	atomic.StoreInt64(&seg.totalEvacuate, 0)
	atomic.StoreInt64(&seg.totalExpired, 0)
	atomic.StoreInt64(&seg.overwrites, 0)
	atomic.StoreInt64(&seg.hitCount, 0)
	atomic.StoreInt64(&seg.missCount, 0)
	seg.lock.Unlock()
}

func (seg *segment) clear() {
	seg.lock.Lock()
	bufSize := len(seg.rb.data)
	seg.rb = NewRingBuf(bufSize, 0)
	seg.vacuumLen = int64(bufSize)
	seg.slotCap = 1
	seg.slotsData = make([]entryPtr, 256*seg.slotCap)
	for i := 0; i < len(seg.slotLens); i++ {
		seg.slotLens[i] = 0
	}
	atomic.StoreInt64(&seg.hitCount, 0)
	atomic.StoreInt64(&seg.missCount, 0)
	atomic.StoreInt64(&seg.entryCount, 0)
	atomic.StoreInt64(&seg.totalCount, 0)
	atomic.StoreInt64(&seg.totalEvacuate, 0)
	atomic.StoreInt64(&seg.totalExpired, 0)
	atomic.StoreInt64(&seg.overwrites, 0)
	seg.lock.Unlock()
}
