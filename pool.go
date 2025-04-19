// Package `pool` provides a concurrent, generic, variable-capacity object
// pool. It maintains a variable number of objects throughout the pool's
// lifetime, adjusting based on demand and idle timeouts. For a more static
// fixed-capacity alternative, consider
// https://github.com/michaellenaghan/go-simplepool.
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrNew               = errors.New("failed to make new pool object")
	ErrStoppingOrStopped = errors.New("pool is stopping or has stopped")
)

// Pool is a generic object pool that manages a collection of objects of type T.
type Pool[T any] struct {
	min         int               // must be >= 0
	max         int               // must be >= min
	idleTimeout time.Duration     // must be >= 0; 0 == "never idle out"
	newFunc     func() (T, error) // required
	checkFunc   func(T) error     // optional
	destroyFunc func(T)           // optional

	mu sync.Mutex

	count int // min <= count <= max

	idle    ring[T]               // idle object ring buffer, ordered by lastUsed
	waiting chan waitingResult[T] // "Get"ers waiting for "Put"ers

	stoppingOrStopped bool
	stoppingCh        chan struct{}
	stoppingWg        sync.WaitGroup

	createdTotal   uint // created an object (stats only)
	waitedTotal    uint // waited for an object (stats only)
	destroyedTotal uint // destroyed an object (stats only)
}

type waitingResult[T any] struct {
	object T
	retry  bool
}

// New creates a new object pool.
//
// New checks the provided config by calling config.Check(). If there's an
// error, New returns it.
//
// Otherwise, New immediately creates the minimum number of pool objects. If
// there's an error creating one of those objects, New destroys the objects it
// created and returns the error.
//
// Finally, New starts a goroutine that periodically checks for and removes
// idle objects from the pool. The checks are performed every idleTimeout/2
// seconds. If an object's idle time exceeds the config's idle timeout, it's
// destroyed. Objects are checked and destroyed in FIFO order. In other words,
// the least recently used object is checked and destroyed first.
func New[T any](config Config[T]) (*Pool[T], error) {
	err := config.Check()
	if err != nil {
		return nil, err
	}

	p := &Pool[T]{
		min:         config.Min,
		max:         config.Max,
		idleTimeout: config.IdleTimeout,
		newFunc:     config.NewFunc,
		checkFunc:   config.CheckFunc,
		destroyFunc: config.DestroyFunc,
		idle:        newRing[T](config.Max, config.IdleTimeout),
		waiting:     make(chan waitingResult[T]),
		stoppingCh:  make(chan struct{}),
		stoppingWg:  sync.WaitGroup{},
	}

	for range p.min {
		object, err := p.newFunc()
		if err != nil {
			for p.idle.count > 0 {
				object := p.idle.popOldest()
				if p.destroyFunc != nil {
					p.destroyFunc(object)
				}
				p.muObjectWasDestroyed()
			}

			return nil, fmt.Errorf("%w: %v", ErrNew, err)
		}
		p.idle.pushNewest(object)
		p.muObjectWasCreated()
	}

	go p.cleanupTick()

	return p, nil
}

// Stop stops the pool.
//
// If the pool is already stopping, or has already stopped, Stop does nothing.
//
// Otherwise, Stop destroys all idle objects and then waits for all busy
// objects to be destroyed before returning.
func (p *Pool[T]) Stop() {
	p.mu.Lock()

	if p.stoppingOrStopped {
		p.mu.Unlock()

		return
	}

	p.stoppingOrStopped = true
	close(p.stoppingCh)
	p.stoppingWg.Add(p.count)

	for p.idle.count > 0 {
		object := p.idle.popOldest()
		if p.destroyFunc != nil {
			p.destroyFunc(object)
		}
		p.muObjectWasDestroyed()
	}

	p.mu.Unlock()

	p.stoppingWg.Wait()
}

// Get returns an object from the pool, and an error.
//
// (If the error is not nil the object will be the zero value of the type T.)
//
// If the pool is stopping or stopped, Get returns an error.
//
// Otherwise, if there are idle objects, Get returns the most recently used
// idle object (LIFO).
//
// Otherwise, if there's capacity, Get creates and returns a new object.
//
// Otherwise, Get waits for an object to be returned to the pool by Put.
//
// (Waiting Get calls are served in FIFO order.)
//
// Get stops waiting when the provided context is cancelled or when Stop is
// called.
func (p *Pool[T]) Get(ctx context.Context) (T, error) {
	for {
		p.mu.Lock()

		// 1. pool is stopping or stopped
		if p.stoppingOrStopped {
			p.mu.Unlock()

			var zero T
			return zero, ErrStoppingOrStopped
		}

		// 2. pool has idle objects
		if p.idle.count > 0 {
			// popOldest() makes the oldest connections less likely to idle out
			// since they get used more frequently.
			//
			// popNewest() makes the oldest connections more likely to idle out
			// since they get used less frequently.
			object := p.idle.popNewest()
			p.mu.Unlock()

			return object, nil
		}

		// 3. pool has capacity
		if p.count < p.max {
			// Don't hold the lock while we call newFunc().
			p.muObjectWasCreated()
			p.mu.Unlock()

			object, err := p.newFunc()
			if err != nil {
				p.mu.Lock()
				p.muObjectWasDestroyed()
				p.mu.Unlock()

				var zero T
				return zero, fmt.Errorf("%w: %v", ErrNew, err)
			}

			return object, nil
		}

		// 4. pool has no idle objects and no capacity; wait for an object
		p.muObjectWasWaitedFor()
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case waitingResult := <-p.waiting:
			// An object was destroyed after failing a CheckFunc() call. There
			// might be capacity to create a new object now. More importantly,
			// though, we don't want to be stuck waiting for an object that
			// might never arrive.
			if waitingResult.retry {
				continue
			}

			return waitingResult.object, nil
		case <-p.stoppingCh:
			var zero T
			return zero, ErrStoppingOrStopped
		}
	}
}

// Put returns an object to the pool.
//
// If CheckFunc was provided in the pool configuration, it's called before
// anything else happens. If CheckFunc returns an error, Put destroys the
// object rather than returning it to the pool.
//
// Otherwise, if the pool is stopping or stopped, Put destroys the object
// rather than returning it to the pool.
//
// Otherwise, if any Get calls are waiting for an object, Put directly hands
// the object off to the longest-waiting Get call.
//
// If no Get calls are waiting, the object is added to the end of the idle
// pool.
func (p *Pool[T]) Put(object T) {
	if p.checkFunc != nil {
		err := p.checkFunc(object)
		if err != nil {
			// Don't hold the lock while we call destroyFunc().
			p.mu.Lock()
			p.muObjectWasDestroyed()
			p.mu.Unlock()

			// We're destroying an object rather than returning it to the
			// pool.
			//
			// Imagine a case where the pool size is 1 and there's at least
			// one waiting Get() call. If we don't send a retry signal to
			// the waiting Get() call, it could end up waiting forever. (The
			// "could" is because the next Get() call to come along will see
			// there's capacity, create a new object, and eventually return
			// it to the pool — waking the waiting Get().)
			//
			// In theory, we could optimize this; we only have a problem when
			// waiting Get() calls are >= busy objects. (If busy > waiting
			// then eventually all of the waiting Get() calls will be woken
			// up when the busy objects return.) The problem is that we compute
			// waiting with len(p.waiting) -- and, at least in theory, that
			// number could change after we check it. I mean, not today it
			// couldn't; we only send to the channel when the mutex is locked.
			// But, hey, maybe someday something changes, and that becomes
			// possible. (Indeed, after writing that I ended up moving *this*
			// send outside of the mutex lock, for, well, reasons!)
			//
			// The downside of not being more conservative is that the Get()
			// call at the front of the waiting queue could end up at the back.
			// On the other hand, it's also possible that the waiting Get()
			// that we just woke up will see that there's now capacity
			// available and create a new object. So, yes, we might end up
			// worse off — and, no, we might not.
			//
			// P.S. We send to the waiting channel before actually destroying
			// the object because there's no point in making the waiting
			// Get() call wait any longer than it has to.

			select {
			case p.waiting <- waitingResult[T]{retry: true}:
			default:
			}

			if p.destroyFunc != nil {
				p.destroyFunc(object)
			}

			return
		}
	}

	p.mu.Lock()

	if p.stoppingOrStopped {
		// Don't hold the lock while we call destroyFunc().
		p.muObjectWasDestroyed()
		p.mu.Unlock()

		if p.destroyFunc != nil {
			p.destroyFunc(object)
		}

		return
	}

	select {
	case p.waiting <- waitingResult[T]{object: object}:
	default:
		p.idle.pushNewest(object)
	}

	p.mu.Unlock()
}

// muObjectWasCreated is an internal method that manipulates some counters.
// This method should be called while holding the pool's mutex.
func (p *Pool[T]) muObjectWasCreated() {
	p.createdTotal++
	p.count++
}

// muObjectWasWaitedFor is an internal method that manipulates some counters.
// This method should be called while holding the pool's mutex.
func (p *Pool[T]) muObjectWasWaitedFor() {
	p.waitedTotal++
}

// muObjectWasDestroyed is an internal method that manipulates some counters.
// This method should be called while holding the pool's mutex.
func (p *Pool[T]) muObjectWasDestroyed() {
	p.destroyedTotal++
	p.count--
	if p.stoppingOrStopped {
		p.stoppingWg.Done()
	}
}

// cleanupTick is an internal method that periodically checks for and removes
// idle objects from the pool.
func (p *Pool[T]) cleanupTick() {
	if p.max > p.min && p.idleTimeout > 0 {
		ticker := time.NewTicker(p.idleTimeout / 2)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				p.cleanupTock()
			case <-p.stoppingCh:
				return
			}
		}
	}
}

// cleanupTock is an internal method that periodically checks for and removes
// idle objects from the pool.
func (p *Pool[T]) cleanupTock() {
	p.mu.Lock()
	for !p.stoppingOrStopped && p.count > p.min && p.idle.count > 0 && p.idle.oldestIdleTooLong() {
		// Don't hold the lock while we call destroyFunc().
		object := p.idle.popOldest()
		p.muObjectWasDestroyed()
		p.mu.Unlock()

		if p.destroyFunc != nil {
			p.destroyFunc(object)
		}

		p.mu.Lock()
	}
	p.mu.Unlock()
}

// Stats returns statistics about the pool.
func (p *Pool[T]) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()

	return Stats{
		CreatedTotal:   p.createdTotal,
		WaitedTotal:    p.waitedTotal,
		DestroyedTotal: p.destroyedTotal,

		CountNow:   p.count,
		BusyNow:    p.count - p.idle.count,
		IdleNow:    p.idle.count,
		WaitingNow: len(p.waiting),
	}
}
