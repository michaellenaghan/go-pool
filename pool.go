// Package pool provides a concurrent generic object pool that efficiently
// manages expensive-to-create objects. It handles object lifecycle from
// creation to destruction, implements configurable (and optional) idle time
// management, and optimizes resource usage through carefully considered reuse
// patterns.
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
	ErrStoppingOrStopped = errors.New("pool is stopping or stopped")
	errRingIsEmpty       = errors.New("ring is empty") // internal programming error
	errRingIsFull        = errors.New("ring is full")  // internal programming error
)

// Config configures a new pool.
type Config[T any] struct {
	// Min is the minimum number of objects in the pool.
	// Must be >= 0.
	Min int
	// Max is the maximum number of objects in the pool.
	// Must be >= min.
	Max int
	// IdleTime is the maximum time an object can be idle before being
	// destroyed and recreated. If IdleTime is 0, objects never idle out
	// (and min should equal max since objects between min and max would
	// never idle out).
	// Must be >= 0.
	IdleTime time.Duration
	// NewFunc is a function that creates a new object.
	// This function is required.
	NewFunc func() (T, error)
	// CheckFunc is a function that checks an object when it's returned to
	// the pool. If CheckFunc returns an error, the object is destroyed (via
	// DestroyFunc) and not returned.
	// This function is optional.
	CheckFunc func(T) error
	// DestroyFunc is a function that destroys an object when it's no longer
	// needed.
	// This function is optional.
	DestroyFunc func(T)
}

// Pool is a generic object pool that manages a collection of objects of type T.
// It maintains a minimum and maximum number of objects, handles object creation
// and destruction, and manages object availability and idle time.
type Pool[T any] struct {
	min         int               // must be >= 0
	max         int               // must be >= min
	idleTime    time.Duration     // must be >= 0; 0 == "never idle out"
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

// Stats represents statistical information about the pool's performance.
type Stats struct {
	// Number of created objects
	CreatedTotal uint
	// Number of objects waited for
	// (i.e. number of "Get"ers that had to wait for a "Put"er)
	WaitedTotal uint
	// Number of destroyed objects
	DestroyedTotal uint

	// Number of objects right now (count == busy + idle)
	CountNow int
	// Number of busy objects right now (busy == count - idle)
	BusyNow int
	// Number of idle objects right now (idle == count - busy)
	IdleNow int
	// Number of objects being waited for right now
	// (i.e. number of "Get"ers currently waiting for a "Put"er)
	WaitingNow int
}

// New creates a new pool of objects of type T.
//
// The pool will maintain somewhere between the minimum (min) and maximum (max)
// number of objects.
//
// Objects will be reused until they idle out — that is, until they've been
// idle longer than idleTime. When they've been idle for longer than idleTime
// they'll eventually be destroyed.
//
// An idleTime of zero means that objects never idle out.
//
// (When idleTime is zero, min should equal max since objects between min and
// max would never idle out anyway.)
//
// The newFunc function is required and used to create new objects.
//
// The checkFunc function is optional and used to check objects when they're
// returned to the pool. If checkFunc returns an error the object is destroyed
// (via destroyFunc, if provided) and not returned.
//
// The destroyFunc function is optional and used to destroy objects when
// they're no longer needed.
func New[T any](config Config[T]) (*Pool[T], error) {
	if config.Min < 0 {
		return nil, errors.New("min must be greater than or equal to zero")
	}
	if config.Min > config.Max {
		return nil, errors.New("min must be less than or equal to max")
	}
	if config.IdleTime < 0 {
		return nil, errors.New("idle time must be greater than or equal to zero")
	}
	// This isn't *really* an error, it's just an indication that someone's
	// mental model may not be quite right. Once more than min objects exist,
	// they'll continue to exist until `Stop()` is called. Again, not *really*
	// an error, but probably not what was intended?
	if config.IdleTime == 0 && config.Min != config.Max {
		return nil, errors.New("when idle time equals zero min should equal max")
	}
	if config.NewFunc == nil {
		return nil, errors.New("newFunc is required")
	}
	p := &Pool[T]{
		min:         config.Min,
		max:         config.Max,
		idleTime:    config.IdleTime,
		newFunc:     config.NewFunc,
		checkFunc:   config.CheckFunc,
		destroyFunc: config.DestroyFunc,
		idle:        newRing[T](config.Max, config.IdleTime),
		waiting:     make(chan waitingResult[T]),
		stoppingCh:  make(chan struct{}),
		stoppingWg:  sync.WaitGroup{},
	}
	return p, nil
}

// Start initializes the pool and prepares it for use.
//
// If the pool is already stopping or stopped, Start returns an error.
//
// Otherwise, Start creates the minimum number of pool objects. If there's an
// error creating one of the initial objects, Start destroys any objects it
// already created and returns an error.
//
// Start should be called after New to ensure the pool is ready to use.
func (p *Pool[T]) Start() error {
	// log.Println("> Start")
	// defer log.Println("< Start")
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stoppingOrStopped {
		return ErrStoppingOrStopped
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

			return fmt.Errorf("%w: %v", ErrNew, err)
		}
		p.idle.pushNewest(object)
		p.muObjectWasCreated()
	}

	go p.cleanupTick()

	return nil
}

// Stop stops the pool.
//
// If the pool is already stopping or stopped, Stop does nothing.
//
// Otherwise, Stop marks the pool as stopping or stopped and destroys all idle
// objects. (Busy objects will be destroyed by Put when they're eventually
// returned to the pool.)
//
// Stop waits for all objects to be destroyed before returning.
func (p *Pool[T]) Stop() {
	// log.Println("> Stop")
	// defer log.Println("< Stop")
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

// Get returns an object from the pool and an error.
//
// If an error is returned, the object will be the zero value of the type.
//
// If the pool is stopping or stopped, Get returns an error.
//
// If there are idle objects, Get returns the most recently used idle object
// (LIFO).
//
// If there are no idle objects and the pool has capacity, Get creates and
// returns a new object.
//
// If there are no idle objects and the pool is at capacity, Get waits for an
// object to be returned to the pool by Put. Waiting Get calls are served in
// FIFO order.
//
// Get stops waiting when Stop is called or when the provided context is
// cancelled.
func (p *Pool[T]) Get(ctx context.Context) (T, error) {
	// log.Println("> Get")
	// defer log.Println("< Get")
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
			// An object was destroyed after failing a checkFunc() call. There
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
// If checkFunc was provided in the pool configuration, it is called before
// the object is processed further. If checkFunc returns an error, the
// object is destroyed and not returned to the pool.
//
// If the pool is stopping or stopped, Put destroys the object rather than
// returning it to the pool.
//
// If any Get() calls are waiting for an object, Put will directly hand off
// the object to the longest-waiting Get() call.
//
// If no Get() calls are waiting, the object is added to the idle pool.
func (p *Pool[T]) Put(object T) {
	// log.Println("> Put")
	// defer log.Println("< Put")
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
	if p.max > p.min && p.idleTime > 0 {
		ticker := time.NewTicker(p.idleTime / 2)
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
