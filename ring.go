package pool

import (
	"errors"
	"time"
)

var (
	errRingIsEmpty = errors.New("ring is empty") // internal programming error
	errRingIsFull  = errors.New("ring is full")  // internal programming error
)

// ring is a generic ring buffer that stores objects along with their last
// used time.
type ring[T any] struct {
	buffer      []ringObject[T] // The actual ring buffer storage
	head        int             // Index of the first object (0 <= head < cap(buffer))
	tail        int             // Index of the next object (0 <= tail < cap(buffer))
	count       int             // Current number of items in the buffer (0 <= count <= cap(buffer))
	idleTimeout time.Duration   // Maximum time an object can be idle (>= 0; 0 == "never idle out")
}

// ringObject represents a generic object in the ring buffer along with its
// last used time.
type ringObject[T any] struct {
	object   T
	lastUsed time.Time
}

// newRing creates a new generic ring buffer with the given capacity.
func newRing[T any](max int, idleTimeout time.Duration) ring[T] {
	return ring[T]{
		buffer:      make([]ringObject[T], max),
		idleTimeout: idleTimeout,
	}
}

// oldestIdleTooLong returns true if the oldest object has been idle too long.
func (r *ring[T]) oldestIdleTooLong() bool {
	if r.count > 0 {
		return r.idleTimeout > 0 && time.Since(r.buffer[r.head].lastUsed) >= r.idleTimeout
	} else {
		panic(errRingIsEmpty)
	}
}

// popOldest removes and returns an object from the head of the ring buffer.
// This implements FIFO (First-In-First-Out) behavior.
func (r *ring[T]) popOldest() T {
	if r.count > 0 {
		var zero T
		object := r.buffer[r.head].object
		r.buffer[r.head].object = zero

		if r.head < cap(r.buffer)-1 {
			r.head++
		} else {
			r.head = 0
		}
		r.count--

		return object
	} else {
		panic(errRingIsEmpty)
	}
}

// popNewest removes and returns an object from the tail of the ring buffer.
// This implements LIFO (Last-In-First-Out) behavior.
func (r *ring[T]) popNewest() T {
	if r.count > 0 {
		if r.tail > 0 {
			r.tail--
		} else {
			r.tail = cap(r.buffer) - 1
		}
		r.count--

		var zero T
		object := r.buffer[r.tail].object
		r.buffer[r.tail].object = zero

		return object
	} else {
		panic(errRingIsEmpty)
	}
}

// pushNewest adds an object to the tail of the ring buffer.
func (r *ring[T]) pushNewest(object T) {
	if r.count < cap(r.buffer) {
		r.buffer[r.tail].object = object
		// Don't record lastUsed time if we don't need it.
		// (Recording time takes time.)
		if r.idleTimeout > 0 {
			r.buffer[r.tail].lastUsed = time.Now()
		}

		if r.tail < cap(r.buffer)-1 {
			r.tail++
		} else {
			r.tail = 0
		}
		r.count++
	} else {
		panic(errRingIsFull)
	}
}
