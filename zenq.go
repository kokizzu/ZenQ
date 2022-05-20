// A minimalist thread-safe queue implemented using a lock-free ringbuffer which is even faster
// and more resource friendly than golang's native channels

// Known Limitations:-
//
// 1. Current max queue_size = 2^63
// 2. The size of the queue must be a power of 2

// Suggestions:-
//
// 1. If you have enough cores you can change from runtime.Gosched() to a busy loop
// 2. Use runtime.LockOSThread() on the goroutine calling ZenQ.Read() for best performance provided you have > 1 cpu cores
//

package zenq

import (
	"fmt"
	"runtime"
	"sync/atomic"
)

const (
	power = 12

	// The queue size, should be a power of 2
	queueSize = 1 << power

	// Masking is faster than division, only works with numbers which are powers of 2
	indexMask = queueSize - 1
)

// ZenQ global state enums
const (
	StateOpen = iota
	StateClosed
)

// ZenQ Slot state enums
const (
	SlotEmpty = iota
	SlotBusy
	SlotCommitted
)

type (
	// Most modern CPUs have cache line size of 64 bytes
	cacheLinePadding [8]uint64

	// Slot represents a single slot in ZenQ each one having its own state
	Slot[T any] struct {
		State  uint32
		Parker *ThreadParker
		Item   T
	}

	// ZenQ is the CPU cache optimized ringbuffer implementation
	ZenQ[T any] struct {
		// The padding members 1 to 5 below are here to ensure each item is on a separate cache line.
		// This prevents false sharing and hence improves performance.
		_p1         cacheLinePadding
		writerIndex uint64
		_p2         cacheLinePadding
		readerIndex uint64
		_p3         cacheLinePadding
		globalState uint32
		_p4         cacheLinePadding
		// arrays have faster access speed than slices for single elements
		contents [queueSize]Slot[T]
		_p5      cacheLinePadding
	}
)

// New returns a new queue given its payload type passed as a generic parameter
func New[T any]() *ZenQ[T] {
	var contents [queueSize]Slot[T]
	for idx := range contents {
		contents[idx].Parker = NewThreadParker()
	}
	return &ZenQ[T]{contents: contents}
}

// Write writes a value to the queue
func (self *ZenQ[T]) Write(value T) {
	if atomic.LoadUint32(&self.globalState) == StateClosed {
		return
	}
	// Get writer slot index
	idx := (atomic.AddUint64(&self.writerIndex, 1) - 1) & indexMask
	slotState := &self.contents[idx].State
	parker := self.contents[idx].Parker

	// CAS -> change slot_state to busy if slot_state == empty
	for !atomic.CompareAndSwapUint32(slotState, SlotEmpty, SlotBusy) {
		// The body of this for loop will never be invoked in case of SPSC (Single-Producer-Single-Consumer) mode
		// guaranteening low latency unless the user's reader thread is blocked for some reason
		parker.Park()
		if atomic.LoadUint32(&self.globalState) == StateClosed {
			return
		}
	}
	self.contents[idx].Item = value
	// commit write into the slot
	atomic.StoreUint32(slotState, SlotCommitted)
}

// consume consumes a slot and marks it ready for writing
func consume(slotState *uint32, parker *ThreadParker) {
	atomic.StoreUint32(slotState, SlotEmpty)
	parker.Ready()
}

// Read reads a value from the queue, you can once read once per object
func (self *ZenQ[T]) Read() (data T, open bool) {
	// Get reader slot index
	idx := (atomic.AddUint64(&self.readerIndex, 1) - 1) & indexMask
	slotState := &self.contents[idx].State
	parker := self.contents[idx].Parker

	// change slot_state to empty after this function returns, via defer thereby preventing race conditions
	// Note:- Although defer adds around 50ns of latency, this is required for preventing race conditions
	defer consume(slotState, parker)

	iter := 0
	// CAS -> change slot_state to busy if slot_state == committed
	for !atomic.CompareAndSwapUint32(slotState, SlotCommitted, SlotBusy) {
		switch atomic.LoadUint32(slotState) {
		case SlotBusy:
			if runtime_canSpin(iter) {
				iter++
				runtime_doSpin()
			} else {
				runtime.Gosched()
			}
		case SlotEmpty:
			if parker.Ready() && runtime_canSpin(iter) {
				iter++
				runtime_doSpin()
			} else if atomic.LoadUint32(&self.globalState) == StateOpen {
				runtime.Gosched()
			} else {
				return getDefault[T](), false
			}
		case SlotCommitted:
			continue
		}
	}
	return self.contents[idx].Item, atomic.LoadUint32(&self.globalState) == StateOpen
}

// TryRead is used for reading from a single channel by multiple selectors
func (self *ZenQ[T]) TryRead() (data T, open bool) {
	idx := atomic.LoadUint64(&self.readerIndex)
	if atomic.LoadUint32(&self.contents[idx&indexMask].State) == SlotCommitted &&
		atomic.CompareAndSwapUint64(&self.readerIndex, idx, idx+1) {
		idx = idx & indexMask
		defer consume(&self.contents[idx].State, self.contents[idx].Parker)
		return self.contents[idx].Item, atomic.LoadUint32(&self.globalState) == StateOpen
	} else {
		return getDefault[T](), false
	}
}

// Close closes the ZenQ for further read/writes
func (self *ZenQ[T]) Close() {
	atomic.StoreUint32(&self.globalState, StateClosed)
}

// Check and Poll implement the Selectable interface

// Check returns the number of reads committed to the queue and whether the queue is ready for reading or not
func (self *ZenQ[T]) Check() (uint32, bool) {
	idx := atomic.LoadUint64(&self.readerIndex) & indexMask
	return atomic.LoadUint32(&self.globalState), atomic.LoadUint32(&self.contents[idx].State) == SlotCommitted
}

// Poll polls
func (self *ZenQ[T]) Poll() (any, any) {
	atomic.AddUint32(&self.globalState, 1)
	return self.Read()
}

// Dump dumps the current queue state
// Unsafe to be called from multiple goroutines
func (self *ZenQ[T]) Dump() {
	fmt.Printf("writerIndex: %3d, readerIndex: %3d, content:", self.writerIndex, self.readerIndex)
	for index := range self.contents {
		fmt.Printf("%5v : State -> %5v, Item -> %5v", index, self.contents[index].State, self.contents[index].Item)
	}
	fmt.Print("\n")
}

// Reset resets the queue state
// Unsafe to be called from multiple goroutines
func (self *ZenQ[T]) Reset() {
	self.Close()
	for idx := range self.contents {
		atomic.StoreUint32(&self.contents[idx].State, SlotCommitted)
		self.contents[idx].Parker.Release()
		atomic.StoreUint32(&self.contents[idx].State, SlotEmpty)
	}
	atomic.StoreUint64(&self.writerIndex, 0)
	atomic.StoreUint64(&self.readerIndex, 0)
	atomic.StoreUint32(&self.globalState, StateOpen)
}

// returns a default value of any type
func getDefault[T any]() T {
	return *new(T)
}
