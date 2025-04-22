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

	mu   sync.Mutex
	cond *sync.Cond // signal waiting "Get"ers

	count int // min <= count <= max

	idle ring[T] // idle object ring buffer, ordered by lastUsed

	stoppingOrStopped bool
	stoppingCh        chan struct{}
	stoppingWg        sync.WaitGroup

	createdTotal   uint // created an object (stats only)
	destroyedTotal uint // destroyed an object (stats only)

	waitingMax int // waiting for an object max (stats only)
	waitingNow int // waiting for an object now (stats only)
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
		stoppingCh:  make(chan struct{}),
		stoppingWg:  sync.WaitGroup{},
	}
	p.cond = sync.NewCond(&p.mu)

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
	p.cond.Broadcast()

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
// Get stops waiting when the provided context is cancelled or when Stop is
// called.
func (p *Pool[T]) Get(ctx context.Context) (T, error) {
	p.mu.Lock()

	for {
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
			p.muObjectWasCreated()
			p.mu.Unlock()

			// Don't hold the lock while we call newFunc().
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
		select {
		case <-ctx.Done():
			p.mu.Unlock()

			var zero T
			return zero, ctx.Err()
		default:
			done := make(chan struct{})

			go func() {
				select {
				case <-ctx.Done():
					// Broadcast the condition to ensure that the `Wait()`
					// below is signaled. `Broadcast()` will almost certainly
					// result in spurious wakeups, but it's our only choice;
					// `Signal()` will only signal *one* waiter, and it may not
					// be *our* waiter.
					p.mu.Lock()
					p.cond.Broadcast()
					p.mu.Unlock()
				case <-done:
				}
			}()

			p.muObjectWaitingStart()
			p.cond.Wait()
			p.muObjectWaitingFinish()

			close(done)
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
// Otherwise, the object is added to the idle pool.
func (p *Pool[T]) Put(object T) {
	// Don't hold the lock while we call checkFunc().
	checkFuncErr := p.checkFunc != nil && p.checkFunc(object) != nil

	p.mu.Lock()

	// 1. destroy the object if checkFunc() returned an error
	if checkFuncErr {
		p.muObjectWasDestroyed()
		p.mu.Unlock()

		p.cond.Signal()

		// Don't hold the lock while we call destroyFunc().
		if p.destroyFunc != nil {
			p.destroyFunc(object)
		}

		return
	}

	// 2. destroy the object if the pool is stopping or stopped
	if p.stoppingOrStopped {
		p.muObjectWasDestroyed()
		p.mu.Unlock()

		p.cond.Signal()

		// Don't hold the lock while we call destroyFunc().
		if p.destroyFunc != nil {
			p.destroyFunc(object)
		}

		return
	}

	// 3. return the object to the pool.
	p.idle.pushNewest(object)
	p.mu.Unlock()

	p.cond.Signal()
}

// muObjectWasCreated is an internal method that manipulates some counters.
// This method should be called while holding the pool's mutex.
func (p *Pool[T]) muObjectWasCreated() {
	p.createdTotal++
	p.count++
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

// muObjectWaitingStart is an internal method that manipulates some counters.
// This method should be called while holding the pool's mutex.
func (p *Pool[T]) muObjectWaitingStart() {
	p.waitingNow++
	p.waitingMax = max(p.waitingNow, p.waitingMax)
}

// muObjectWaitingFinish is an internal method that manipulates some counters.
// This method should be called while holding the pool's mutex.
func (p *Pool[T]) muObjectWaitingFinish() {
	p.waitingNow--
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
		object := p.idle.popOldest()
		p.muObjectWasDestroyed()

		// Don't hold the lock while we call destroyFunc().
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
		DestroyedTotal: p.destroyedTotal,

		CountNow: p.count,
		BusyNow:  p.count - p.idle.count,
		IdleNow:  p.idle.count,

		WaitingNow: p.waitingNow,
		WaitingMax: p.waitingMax,
	}
}
