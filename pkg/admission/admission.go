package admission

import (
	"context"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"google.golang.org/grpc"
)

// Config configures a Controller.
type Config struct {
	Limit uint64
}

// Default config settings:
const (
	defaultConcurrency = 50
)

// DefaultConfig returns a Config with preset values.
func DefaultConfig() Config {
	c := Config{
		Limit: defaultConcurrency,
	}

	return c
}

// Controller determines the current admission parameters.
type Controller struct {
	Pool *quotapool.IntPool

	stats struct {

		// These should only be accessed using atomics.
		numAcquired int64
		numWaiting  int64
	}
}

// NewController constructs a new Controller struct from a given Config.
func NewController(conf Config) *Controller {
	c := &Controller{

		// Should this have a better name?
		Pool: quotapool.NewIntPool("controller intpool", conf.Limit),
	}

	return c
}

// NumWaiting returns the current number of requests that have been intercepted
// by admission.Interceptor but have not been acquired.
func (c *Controller) NumWaiting() int64 {
	return atomic.LoadInt64(&c.stats.numWaiting)
}

// NumAcquired returns the number of requests we have acquired quota for.
// This is functionally identical to computing
// c.pool.Capacity() - c.pool.ApproximateQuota().
func (c *Controller) NumAcquired() int64 {
	return atomic.LoadInt64(&c.stats.numAcquired)
}

func (c *Controller) handleRelease(alloc *quotapool.IntAlloc) {
	atomic.AddInt64(&c.stats.numAcquired, -1)
	alloc.Release()
}

// Interceptor returns a UnaryServerInterceptor with parameters
// taken from a Controller.
func Interceptor(c *Controller) grpc.UnaryServerInterceptor {

	admissionInterceptor := func(ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if ba, ok := req.(*roachpb.BatchRequest); ok {

			atomic.AddInt64(&c.stats.numWaiting, 1)

			const maxCertainSystemRangeID = 20

			// We do not wait to acquire quota for admin or system range
			// requests. We still need to decrement numWaiting.
			if ba.IsAdmin() || ba.Header.RangeID < maxCertainSystemRangeID {
				atomic.AddInt64(&c.stats.numWaiting, -1)
				return handler(ctx, req)
			}

			// Attempt to acquire a single unit from the IntPool.
			alloc, err := c.Pool.Acquire(ctx, 1)

			if err != nil {
				atomic.AddInt64(&c.stats.numWaiting, -1)
				return nil, err
			}

			// Defer only when we know for sure that we've acquired quota
			defer c.handleRelease(alloc)

			atomic.AddInt64(&c.stats.numWaiting, -1)
			atomic.AddInt64(&c.stats.numAcquired, 1)

			return handler(ctx, req)
		}

		// Otherwise just handle normally
		return handler(ctx, req)
	}

	return admissionInterceptor
}
