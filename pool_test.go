package pool_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"slices"

	"github.com/michaellenaghan/go-pool"
)

func ExamplePool_concurrentGetAndPut() {
	pool, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         10,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc:     func() (int, error) { return 0, nil },
			CheckFunc:   func(int) error { return nil }, // optional
			DestroyFunc: func(int) {},                   // optional
		},
	)
	if err != nil {
		fmt.Printf("Failed to create pool: %v\n", err)
		return
	}
	defer pool.Stop()

	wg := sync.WaitGroup{}
	for range 100 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			obj, err := pool.Get(context.Background())
			if err != nil {
				fmt.Printf("Failed to get object: %v\n", err)
				return
			}
			defer pool.Put(obj)

			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()

	stats := pool.Stats()
	fmt.Printf("CreatedTotal: %d\n", stats.CreatedTotal)
	fmt.Printf("WaitedTotal: %d\n", stats.WaitedTotal)
	fmt.Printf("DestroyedTotal: %d\n", stats.DestroyedTotal)
	fmt.Printf("CountNow: %d\n", stats.CountNow)
	fmt.Printf("BusyNow: %d\n", stats.BusyNow)
	fmt.Printf("IdleNow: %d\n", stats.IdleNow)
	fmt.Printf("WaitingNow: %d\n", stats.WaitingNow)
	fmt.Print("===\n")

	time.Sleep(1 * time.Second)

	stats = pool.Stats()
	fmt.Printf("CreatedTotal: %d\n", stats.CreatedTotal)
	fmt.Printf("WaitedTotal: %d\n", stats.WaitedTotal)
	fmt.Printf("DestroyedTotal: %d\n", stats.DestroyedTotal)
	fmt.Printf("CountNow: %d\n", stats.CountNow)
	fmt.Printf("BusyNow: %d\n", stats.BusyNow)
	fmt.Printf("IdleNow: %d\n", stats.IdleNow)
	fmt.Printf("WaitingNow: %d\n", stats.WaitingNow)
	fmt.Print("===\n")

	// Output:
	// CreatedTotal: 10
	// WaitedTotal: 90
	// DestroyedTotal: 0
	// CountNow: 10
	// BusyNow: 0
	// IdleNow: 10
	// WaitingNow: 0
	// ===
	// CreatedTotal: 10
	// WaitedTotal: 90
	// DestroyedTotal: 8
	// CountNow: 2
	// BusyNow: 0
	// IdleNow: 2
	// WaitingNow: 0
	// ===
}

// This test verifies the pool's behavior when a context is cancelled while
// waiting for an object. It creates a scenario where:
//
//  1. The pool has only one object (min=max=1)
//  2. That object is obtained by a Get() call
//  3. A second Get() call is made with a cancelled context
//
// This ensures the pool correctly responds to context cancellation by
// returning the appropriate error rather than blocking indefinitely.
func TestPoolCancelContext(t *testing.T) {
	t.Parallel()

	p, err := pool.New(
		pool.Config[int]{
			Min:         1,
			Max:         1,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc:     func() (int, error) { return 0, nil },
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	// Get the only available object
	obj, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}

	// Try to get another object with a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Get(ctx)
	if err != context.Canceled {
		t.Fatalf("Expected context.Canceled error, got: %v", err)
	}

	p.Put(obj)
}

// This test verifies the behavior when CheckFunc rejects an object.
// It specifically tests the scenario where:
//
//  1. The pool has min=max=1 (only one object)
//  2. One goroutine gets the object
//  3. A second goroutine waits for an object (blocks in Get)
//  4. The first goroutine returns the object, but it fails the check
//  5. The second goroutine is unblocked and gets a new object
//
// This ensures proper handling of failed checks and waiting goroutines.
func TestPoolCheckFuncError(t *testing.T) {
	t.Parallel()

	mu := sync.Mutex{} // Cleanup happens in a background goroutine
	shouldFailCheck := false
	idCounter := 0

	p, err := pool.New(
		pool.Config[int]{
			Min:         1,
			Max:         1,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc: func() (int, error) {
				mu.Lock()
				defer mu.Unlock()

				idCounter++

				return idCounter, nil
			},
			CheckFunc: func(obj int) error {
				mu.Lock()
				defer mu.Unlock()

				if shouldFailCheck && obj == 1 {
					return fmt.Errorf("Failed object check")
				}

				return nil
			},
			DestroyFunc: func(obj int) {
				// Do nothing, just for tracking
			},
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	// Get the only object in the pool
	obj1, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	if obj1 != 1 {
		t.Fatalf("Expected object with ID 1, got: %d", obj1)
	}

	// Start a goroutine that will try to get an object
	// It should initially block because max=1 and the object is in use
	wg := sync.WaitGroup{}
	wg.Add(1)

	var obj2 int
	var obj2Err error

	go func() {
		defer wg.Done()
		obj2, obj2Err = p.Get(context.Background())
	}()

	// Give the goroutine time to start waiting
	time.Sleep(100 * time.Millisecond)

	// Enable check to fail for obj1
	mu.Lock()
	shouldFailCheck = true
	mu.Unlock()

	// Return obj1, which should fail the check
	p.Put(obj1)

	// The goroutine should be able to continue and get a new object
	wg.Wait()

	if obj2Err != nil {
		t.Fatalf("Waiting goroutine failed to get object: %v", obj2Err)
	}

	// Verify we got a new object (ID should be 2 since the first one failed check and was destroyed)
	if obj2 != 2 {
		t.Fatalf("Expected object with ID 2, got: %d", obj2)
	}

	// Return the object to the pool
	p.Put(obj2)

	// Verify the pool statistics
	stats := p.Stats()
	if stats.CreatedTotal != 2 {
		t.Errorf("Expected 2 created objects, got: %d", stats.CreatedTotal)
	}
	if stats.DestroyedTotal != 1 {
		t.Errorf("Expected 1 destroyed object, got: %d", stats.DestroyedTotal)
	}
	if stats.CountNow != 1 {
		t.Errorf("Expected 1 object, got: %d", stats.CountNow)
	}
}

// This test verifies the pool's ability to clean up idle objects.
// It creates a pool with min=2 and max=5, gets all 5 objects,
// returns them to the pool, then waits for the idle timeout.
// After the timeout, the pool should have reduced the object
// count back to min (2).
func TestPoolCleanupIdleObjects(t *testing.T) {
	t.Parallel()

	const min = 2
	const max = 5

	p, err := pool.New(
		pool.Config[int]{
			Min:         min,
			Max:         max,
			IdleTimeout: 100 * time.Millisecond,
			NewFunc:     func() (int, error) { return 0, nil },
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	// Get 5 and then Put 5 objects
	ctx := context.Background()
	obj1, _ := p.Get(ctx)
	obj2, _ := p.Get(ctx)
	obj3, _ := p.Get(ctx)
	obj4, _ := p.Get(ctx)
	obj5, _ := p.Get(ctx)
	p.Put(obj1)
	p.Put(obj2)
	p.Put(obj3)
	p.Put(obj4)
	p.Put(obj5)

	// Wait for cleanup
	time.Sleep(200 * time.Millisecond)

	// Check that min objects are left after cleanup
	stats := p.Stats()
	if stats.CountNow != min {
		t.Errorf("Expected %d objects after cleanup, got: %d", min, stats.CountNow)
	}
}

// This test verifies that objects are properly destroyed after idling out,
// and that subsequent Get() operations create fresh objects rather than
// returning destroyed ones. It specifically tests:
//
//  1. An object is obtained and returned to the pool
//  2. The object idles out and is destroyed by the cleanup mechanism
//  3. A subsequent Get() creates a new object rather than returning the
//     destroyed one
//
// This ensures the pool properly manages object lifecycle and doesn't reuse
// destroyed objects.
func TestPoolCleanupIdleObjectsAndThenGet(t *testing.T) {
	t.Parallel()

	mu := sync.Mutex{} // Cleanup happens in a background goroutine
	destroyedIDs := make([]int, 0)
	idCounter := 0

	p, err := pool.New(
		pool.Config[int]{
			Min:         0,
			Max:         3,
			IdleTimeout: 100 * time.Millisecond,
			NewFunc: func() (int, error) {
				mu.Lock()
				defer mu.Unlock()

				idCounter++

				return idCounter, nil
			},
			DestroyFunc: func(id int) {
				mu.Lock()
				defer mu.Unlock()

				destroyedIDs = append(destroyedIDs, id)
			},
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	// Get an object
	obj1, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}

	// Return it to the pool
	p.Put(obj1)

	// Wait for the idle timeout to pass so cleanup happens
	time.Sleep(200 * time.Millisecond)

	// Check if object was destroyed
	mu.Lock()
	obj1WasDestroyed := slices.Contains(destroyedIDs, obj1)
	mu.Unlock()
	if !obj1WasDestroyed {
		t.Errorf("Expected destroyed object with ID %d", obj1)
	}

	// Get a new object
	obj2, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}

	// Check if we got a destroyed object
	mu.Lock()
	obj2WasDestroyed := slices.Contains(destroyedIDs, obj2)
	mu.Unlock()
	if obj1 == obj2 || obj2WasDestroyed {
		t.Errorf("Expected created object, got destroyed object with ID %d", obj2)
	}

	// Return it to the pool
	p.Put(obj2)
}

// This test verifies the pool's behavior under concurrent load.
// It launches multiple goroutines that simultaneously get objects,
// hold them briefly, and then return them to the pool.
// This tests thread safety and proper object management when
// multiple goroutines interact with the pool simultaneously.
func TestPoolConcurrentGetAndPut(t *testing.T) {
	t.Parallel()

	p, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         10,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc:     func() (int, error) { return 0, nil },
			CheckFunc:   func(int) error { return nil },
			DestroyFunc: func(int) {},
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	wg := sync.WaitGroup{}
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obj, err := p.Get(context.Background())
			if err != nil {
				t.Errorf("Failed to get object: %v", err)
				return
			}
			defer p.Put(obj)

			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()

	stats := pool.Stats{}
	uwant := uint(0)
	ugot := uint(0)
	want := 0
	got := 0

	stats = p.Stats()
	uwant = 10
	ugot = stats.CreatedTotal
	if ugot != uwant {
		t.Errorf("Expected CreatedTotal to be %d, got: %d", uwant, ugot)
	}
	uwant = 90
	ugot = stats.WaitedTotal
	if ugot != uwant {
		t.Errorf("Expected WaitedTotal to be %d, got: %d", uwant, ugot)
	}
	uwant = 0
	ugot = stats.DestroyedTotal
	if ugot != uwant {
		t.Errorf("Expected DestroyedTotal to be %d, got: %d", uwant, ugot)
	}
	want = 10
	got = stats.CountNow
	if got != want {
		t.Errorf("Expected CountNow to be %d, got: %d", want, got)
	}
	want = 0
	got = stats.BusyNow
	if got != want {
		t.Errorf("Expected BusyNow to be %d, got: %d", want, got)
	}
	want = 10
	got = stats.IdleNow
	if got != want {
		t.Errorf("Expected IdleNow to be %d, got: %d", want, got)
	}
	want = 0
	got = stats.WaitingNow
	if got != want {
		t.Errorf("Expected WaitingNow to be %d, got: %d", want, got)
	}

	time.Sleep(1 * time.Second)

	stats = p.Stats()
	uwant = 10
	ugot = stats.CreatedTotal
	if ugot != uwant {
		t.Errorf("Expected CreatedTotal to be %d, got: %d", uwant, ugot)
	}
	uwant = 90
	ugot = stats.WaitedTotal
	if ugot != uwant {
		t.Errorf("Expected WaitedTotal to be %d, got: %d", uwant, ugot)
	}
	uwant = 8
	ugot = stats.DestroyedTotal
	if ugot != uwant {
		t.Errorf("Expected DestroyedTotal to be %d, got: %d", uwant, ugot)
	}
	want = 2
	got = stats.CountNow
	if got != want {
		t.Errorf("Expected CountNow to be %d, got: %d", want, got)
	}
	want = 0
	got = stats.BusyNow
	if got != want {
		t.Errorf("Expected BusyNow to be %d, got: %d", want, got)
	}
	want = 2
	got = stats.IdleNow
	if got != want {
		t.Errorf("Expected IdleNow to be %d, got: %d", want, got)
	}
	want = 0
	got = stats.WaitingNow
	if got != want {
		t.Errorf("Expected WaitingNow to be %d, got: %d", want, got)
	}
}

// This test verifies the pool's behavior with different edge case configurations:
//
//  1. Fixed-size pool (min=max) with idle timeout (idleTimeout>0):
//     Should maintain exactly that many objects
//  2. Fixed-size pool (min=max) no idle timeout (idleTimeout=0):
//     Objects should never be destroyed due to idling
//  3. On-demand creation (min=0):
//     Objects created only when needed
//
// Each configuration is tested for proper object creation, reuse, and cleanup.
func TestPoolEdgeCaseConfigurations(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		min         int
		max         int
		idleTimeout time.Duration
	}{
		{
			name:        "FixedSizePoolWithIdleTimeout",
			min:         5,
			max:         5,
			idleTimeout: 500 * time.Millisecond,
		},
		{
			name:        "FixedSizePoolWithNoIdleTimeout",
			min:         3,
			max:         3,
			idleTimeout: 0,
		},
		{
			name:        "OnDemandCreation",
			min:         0,
			max:         10,
			idleTimeout: 500 * time.Millisecond,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mu := sync.Mutex{}
			created := 0
			destroyed := 0

			p, err := pool.New(
				pool.Config[int]{
					Min:         tc.min,
					Max:         tc.max,
					IdleTimeout: tc.idleTimeout,
					NewFunc: func() (int, error) {
						mu.Lock()
						defer mu.Unlock()

						created++
						id := created

						return id, nil
					},
					DestroyFunc: func(int) {
						mu.Lock()
						defer mu.Unlock()

						destroyed++
					},
				},
			)
			if err != nil {
				t.Fatalf("Failed to create pool: %v", err)
			}
			defer p.Stop()

			// Verify initial pool state
			mu.Lock()
			initialCreated := created
			mu.Unlock()

			stats := p.Stats()
			if stats.CountNow != tc.min {
				t.Errorf("Expected %d objects, got: %d", tc.min, stats.CountNow)
			}
			if initialCreated != tc.min {
				t.Errorf("Expected %d objects, got: %d", tc.min, initialCreated)
			}

			// Test basic Get/Put operations
			// For fixed size pools or no idle timeout, don't try to get more than min
			extraObjs := 0
			if tc.min < tc.max && tc.idleTimeout > 0 {
				extraObjs = 2 // Only get extra objects if we can exceed min and have idle timeout
			}

			totalToGet := min(tc.min+extraObjs, tc.max)

			objs := make([]int, 0, totalToGet)
			for range totalToGet {
				obj, err := p.Get(context.Background())
				if err != nil {
					t.Fatalf("Failed to get object: %v", err)
				}
				objs = append(objs, obj)
			}

			// Return all objects
			for _, obj := range objs {
				p.Put(obj)
			}

			// Only for configurations with idle timeout and min < max, wait for cleanup
			if tc.idleTimeout > 0 && tc.min < tc.max {
				// Wait long enough for idle cleanup
				time.Sleep(tc.idleTimeout * 2)

				stats = p.Stats()
				if stats.CountNow != tc.min {
					t.Errorf("Expected %d objects after idle timeout, got: %d",
						tc.min, stats.CountNow)
				}

				mu.Lock()
				destroyedCount := destroyed
				mu.Unlock()

				expectedDestroyed := extraObjs
				if destroyedCount != expectedDestroyed {
					t.Errorf("Expected %d destroyed objects after idle timeout, got: %d",
						expectedDestroyed, destroyedCount)
				}
			}

			// For fixed size or no idle timeout, verify no objects were destroyed
			if tc.min == tc.max || tc.idleTimeout == 0 {
				mu.Lock()
				destroyedCount := destroyed
				mu.Unlock()

				if destroyedCount != 0 {
					t.Errorf("Expected 0 destroyed objects for fixed pool or no idle timeout, got: %d",
						destroyedCount)
				}
			}
		})
	}
}

// This test verifies error handling when NewFunc fails.
// It tests two scenarios:
//
//  1. Failure during pool initialization (New)
//  2. Failure during normal operation when trying to create a new object
//
// The test ensures proper error propagation and pool state management in both
// cases.
func TestPoolNewFuncError(t *testing.T) {
	t.Parallel()

	mu := sync.Mutex{} // Cleanup happens in a background goroutine
	shouldFailNew := true

	// New should fail because NewFunc returns an error
	_, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         5,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc: func() (int, error) {
				mu.Lock()
				defer mu.Unlock()

				if shouldFailNew {
					return 0, fmt.Errorf("Failed to create object")
				}

				return 42, nil
			},
		},
	)
	if err == nil {
		t.Fatal("Expected error, got: nil")
	}
	if !errors.Is(err, pool.ErrNew) {
		t.Fatalf("Expected ErrNew error, got: %v", err)
	}

	mu.Lock()
	shouldFailNew = false
	mu.Unlock()

	// Now make NewFunc succeed
	p, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         5,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc: func() (int, error) {
				mu.Lock()
				defer mu.Unlock()

				if shouldFailNew {
					return 0, fmt.Errorf("Failed to create object")
				}

				return 42, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	// Pool should be usable now
	obj, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	if obj != 42 {
		t.Fatalf("Expected object value 42, got: %d", obj)
	}
	p.Put(obj)

	// Test Get when NewFunc fails during normal operation
	mu.Lock()
	shouldFailNew = true
	mu.Unlock()

	// Drain the pool first
	objs := make([]int, 0, p.Stats().CountNow)
	for range p.Stats().IdleNow {
		obj, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("Failed to get object: %v", err)
		}
		objs = append(objs, obj)
	}

	// Now try to get one more - NewFunc should be called and fail
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = p.Get(ctx)
	if err == nil {
		t.Fatal("Expected error, got: nil")
	}
	if !errors.Is(err, pool.ErrNew) {
		t.Fatalf("Expected ErrNew error, got: %v", err)
	}

	// Return objects to the pool
	for _, obj := range objs {
		p.Put(obj)
	}
}

// This test verifies that objects are properly reused by the pool.
// It tests:
//
//  1. Basic reuse: Objects obtained, returned, and then obtained again
//     should be the same objects
//  2. LIFO (Last-In-First-Out) behavior: Objects returned last should
//     be retrieved first
//
// This ensures the core object reuse functionality works correctly.
func TestPoolObjectReuse(t *testing.T) {
	t.Parallel()

	mu := sync.Mutex{} // Cleanup happens in a background goroutine
	createdIDs := make([]int, 0)
	idCounter := 0

	p, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         5,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc: func() (int, error) {
				mu.Lock()
				defer mu.Unlock()

				idCounter++
				createdIDs = append(createdIDs, idCounter)

				return idCounter, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	// Get all objects (and remember them)
	initialObjs := make([]int, 0, p.Stats().CountNow)
	for range p.Stats().CountNow {
		obj, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("Failed to get object: %v", err)
		}

		initialObjs = append(initialObjs, obj)
	}

	// Return all objects
	for _, obj := range initialObjs {
		p.Put(obj)
	}

	// Get all objects again (and verify they're the same ones)
	for range initialObjs {
		obj, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("Failed to get object: %v", err)
		}
		defer p.Put(obj)

		// Check that the object is one we've seen before
		found := slices.Contains(initialObjs, obj)
		if !found {
			t.Errorf("Expected a reused object, got a new one: %v", obj)
		}
	}

	// Verify we didn't create more objects than expected
	mu.Lock()
	totalCreated := len(createdIDs)
	mu.Unlock()

	// We expect only the initial min objects to be created
	if totalCreated != p.Stats().CountNow {
		t.Errorf("Expected %d created objects, got: %d", p.Stats().CountNow, totalCreated)
	}

	// Test LIFO behavior (Last-In-First-Out)
	// Get three objects and track their order
	obj1, _ := p.Get(context.Background())
	obj2, _ := p.Get(context.Background())
	obj3, _ := p.Get(context.Background())

	// Return in reverse order
	p.Put(obj3)
	p.Put(obj2)
	p.Put(obj1)

	// If LIFO is working, we should get objects in order 1, 2, 3
	retrievedObj, _ := p.Get(context.Background())
	if retrievedObj != obj1 {
		t.Errorf("Expected obj1 (%d) from LIFO behavior, got: %d", obj1, retrievedObj)
	}

	retrievedObj, _ = p.Get(context.Background())
	if retrievedObj != obj2 {
		t.Errorf("Expected obj2 (%d) from LIFO behavior, got: %d", obj2, retrievedObj)
	}

	retrievedObj, _ = p.Get(context.Background())
	if retrievedObj != obj3 {
		t.Errorf("Expected obj3 (%d) from LIFO behavior, got: %d", obj3, retrievedObj)
	}

	// Return all objects
	p.Put(obj3)
	p.Put(obj2)
	p.Put(obj1)
}

// This test verifies the basic functionality of Get and Put operations
// in a sequential (non-concurrent) context. It ensures that a simple
// get-then-put operation works correctly.
func TestPoolSequentialGetAndPut(t *testing.T) {
	t.Parallel()

	p, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         5,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc:     func() (int, error) { return 0, nil },
			CheckFunc:   func(int) error { return nil },
			DestroyFunc: func(int) {},
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	obj, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Failed to get object: %v", err)
	}
	p.Put(obj)
}

// This test verifies the pool's shutdown behavior when Stop is called
// while objects are still in use. It ensures:
//
//  1. Stop marks the pool as stopping, making Get() return an error
//  2. Objects returned after Stop is called are properly destroyed
//  3. Stop waits for all busy objects to be returned and destroyed
//
// This tests proper resource cleanup during shutdown.
func TestPoolStopWithBusyObjects(t *testing.T) {
	t.Parallel()

	mu := sync.Mutex{} // Cleanup happens in a background goroutine
	destroyedIDs := make([]int, 0)

	p, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         5,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc: func() (int, error) {
				return rand.Int(), nil
			},
			DestroyFunc: func(obj int) {
				mu.Lock()
				defer mu.Unlock()

				destroyedIDs = append(destroyedIDs, obj)
			},
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}

	// Get some objects and keep track of them
	busyObjs := make([]int, 0, 3)
	for range 3 {
		obj, err := p.Get(context.Background())
		if err != nil {
			t.Fatalf("Failed to get object: %v", err)
		}
		busyObjs = append(busyObjs, obj)
	}

	// Verify initial stats
	stats := p.Stats()
	if stats.CountNow != 3 {
		t.Errorf("Expected 3 objects, got: %d", stats.CountNow)
	}
	if stats.BusyNow != 3 {
		t.Errorf("Expected 3 busy objects, got: %d", stats.BusyNow)
	}

	// Call Stop in a goroutine since it will block until all objects are returned
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.Stop() // This will block until all objects are returned and destroyed
	}()

	// Try to get another object after Stop - should fail immediately
	// Give a small delay for Stop to mark the pool as stopping
	time.Sleep(100 * time.Millisecond)

	_, err = p.Get(context.Background())
	if err == nil {
		t.Fatal("Expected error, got: nil")
	}
	if !errors.Is(err, pool.ErrStoppingOrStopped) {
		t.Fatalf("Expected error ErrStoppingOrStopped, got: %v", err)
	}

	// Return busy objects, which should get destroyed
	for _, obj := range busyObjs {
		p.Put(obj)
	}

	// Wait for Stop to complete
	wg.Wait()

	// Verify all objects were destroyed
	mu.Lock()
	destroyedCount := len(destroyedIDs)
	mu.Unlock()
	if destroyedCount != 3 {
		t.Errorf("Expected 3 destroyed objects after returning busy objects, got: %d", destroyedCount)
	}

	// Final stats check
	stats = p.Stats()
	if stats.CountNow != 0 {
		t.Errorf("Expected 0 objects, got: %d", stats.CountNow)
	}
}

// This test subjects the pool to high concurrent load to verify its
// stability and performance under stress. It:
//
//  1. Creates many concurrent goroutines (1000) that each try to get an object
//  2. Uses short timeouts to simulate real-world constraints
//  3. Allows some Get() calls to fail due to timeout
//  4. Collects final statistics to verify the pool maintained proper state
//
// This test helps identify potential deadlocks, race conditions, or resource
// leaks that might only appear under heavy load.
func TestPoolStressTest(t *testing.T) {
	t.Parallel()

	p, err := pool.New(
		pool.Config[int]{
			Min:         5,
			Max:         20,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc:     func() (int, error) { return 0, nil },
		},
	)
	if err != nil {
		t.Fatalf("Failed to create pool: %v", err)
	}
	defer p.Stop()

	wg := sync.WaitGroup{}
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			obj, err := p.Get(ctx)
			if err != nil {
				return // It's okay to fail sometimes due to timeout
			}
			defer p.Put(obj)

			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()

	stats := p.Stats()
	uwant := uint(20)
	ugot := stats.CreatedTotal
	if ugot != uwant {
		t.Errorf("Expected CreatedTotal to be %d, got: %d", uwant, ugot)
	}
	uwant = uint(980)
	ugot = stats.WaitedTotal
	if ugot != uwant {
		t.Errorf("Expected WaitedTotal to be %d, got: %d", uwant, ugot)
	}
	uwant = uint(0)
	ugot = stats.DestroyedTotal
	if ugot != uwant {
		t.Errorf("Expected DestroyedTotal to be %d, got: %d", uwant, ugot)
	}
	want := 20
	got := stats.CountNow
	if got != want {
		t.Errorf("Expected CountNow to be %d, got: %d", want, got)
	}
	want = 0
	got = stats.BusyNow
	if got != want {
		t.Errorf("Expected BusyNow to be %d, got: %d", want, got)
	}
	want = 20
	got = stats.IdleNow
	if got != want {
		t.Errorf("Expected IdleNow to be %d, got: %d", want, got)
	}
	want = 0
	got = stats.WaitingNow
	if got != want {
		t.Errorf("Expected WaitingNow to be %d, got: %d", want, got)
	}
}
