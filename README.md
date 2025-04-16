# go-pool

[github.com/michaellenaghan/go-pool](https://github.com/michaellenaghan/go-pool)  provides a concurrent generic object pool that efficiently manages expensive-to-create objects. It handles object lifecycle from creation to destruction, implements configurable (and optional) idle time management, and optimizes resource usage through carefully considered reuse patterns.

- The pool maintains between `Min` and `Max` busy and idle objects
- Idle objects are stored in a ring buffer and ordered by their last-used times
- Idle objects are reused on a LIFO (last in, first out) basis; in other words, the most recently used object is reused first
- Idle objects are destroyed on a FIFO (first in, first out) basis; in other words, the least recently used object is destroyed first
- A background goroutine checks the idle objects every `IdleTimeout`/2 seconds
- The background goroutine destroys idle objects that exceed the configured `IdleTimeout`
- When there are no idle objects and the pool is at capacity, `Get()` calls wait for an object to be returned by `Put()`
- `Put()` calls hand off directly to waiting `Get()` calls; in other words, objects move directly from `Put()` to `Get()` without passing through the ring buffer
- Waiting `Get()` calls are served on a FIFO (first in, first out) basis

## Installation

```bash
go get github.com/michaellenaghan/go-pool
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/michaellenaghan/go-pool"
)

func main() {
	pool, err := pool.New(
		pool.Config[int]{
			Min:         2,
			Max:         10,
			IdleTimeout: 500 * time.Millisecond,
			NewFunc:     func() (int, error) { return 0, nil },
			CheckFunc:   func(int) error { return nil }, // this is optional, actually
			DestroyFunc: func(int) {},                   // this is optional, actually
		},
	)
	if err != nil {
		fmt.Printf("Failed to create pool: %v\n", err)
		return
	}

	err = pool.Start()
	if err != nil {
		fmt.Printf("Failed to start pool: %v\n", err)
		return
	}
	defer pool.Stop()

	var wg sync.WaitGroup
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
	fmt.Printf("CreatedTotal: %d\n", stats.CreatedTotal)     // Expect "10"
	fmt.Printf("WaitedTotal: %d\n", stats.WaitedTotal)       // Expect "90"
	fmt.Printf("DestroyedTotal: %d\n", stats.DestroyedTotal) // Expect "0"
	fmt.Printf("CountNow: %d\n", stats.CountNow)             // Expect "10"
	fmt.Printf("BusyNow: %d\n", stats.BusyNow)               // Expect "0"
	fmt.Printf("IdleNow: %d\n", stats.IdleNow)               // Expect "10"
	fmt.Printf("WaitingNow: %d\n", stats.WaitingNow)         // Expect "0"
	fmt.Print("===\n")

	time.Sleep(1 * time.Second)

	stats = pool.Stats()
	fmt.Printf("CreatedTotal: %d\n", stats.CreatedTotal)     // Expect "10"
	fmt.Printf("WaitedTotal: %d\n", stats.WaitedTotal)       // Expect "90"
	fmt.Printf("DestroyedTotal: %d\n", stats.DestroyedTotal) // Expect "8"
	fmt.Printf("CountNow: %d\n", stats.CountNow)             // Expect "2"
	fmt.Printf("BusyNow: %d\n", stats.BusyNow)               // Expect "0"
	fmt.Printf("IdleNow: %d\n", stats.IdleNow)               // Expect "2"
	fmt.Printf("WaitingNow: %d\n", stats.WaitingNow)         // Expect "0"
	fmt.Print("===\n")
}
```

## License

MIT License