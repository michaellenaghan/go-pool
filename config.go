package pool

import (
	"errors"
	"time"
)

// Config configures a new pool.
type Config[T any] struct {
	// Min is the minimum number of objects in the pool.
	// Must be >= 0.
	Min int
	// Max is the maximum number of objects in the pool.
	// Must be >= min.
	Max int
	// IdleTimeout is the maximum time an object can be idle before being
	// destroyed. If IdleTimeout is 0, objects never idle out (and min
	// should equal max). Otherwise, idle objects are checked every
	// IdleTimeout/2 seconds. If an object's idle time exceeds the idle
	// timeout, it's destroyed.
	// Must be >= 0.
	IdleTimeout time.Duration
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

// Check checks the configuration.
//
// If the configuration is invalid, Check returns an error.
func (c *Config[T]) Check() error {
	if c.Min < 0 {
		return errors.New("min must be greater than or equal to zero")
	}
	if c.Min > c.Max {
		return errors.New("min must be less than or equal to max")
	}
	if c.IdleTimeout < 0 {
		return errors.New("idle timeout must be greater than or equal to zero")
	}
	// This isn't *really* an error, it's just an indication that someone's
	// mental model may not be quite right. Once more than min objects exist,
	// they'll continue to exist until `Stop()` is called. Again, not *really*
	// an error, but probably not what was intended?
	if c.IdleTimeout == 0 && c.Min != c.Max {
		return errors.New("when idle timeout equals zero min should equal max")
	}
	if c.NewFunc == nil {
		return errors.New("newFunc is required")
	}
	return nil
}
