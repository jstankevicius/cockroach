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
	Pool  *quotapool.IntPool
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

func (c *Controller) acquire(ctx context.Context, ba *roachpb.BatchRequest) (*quotapool.IntAlloc, error) {

	// Batch requests going to system ranges and admin requests should not be
	// handled by the interceptor at all.
	const maxCertainSystemRangeID = 20
	if ba.IsAdmin() || ba.Header.RangeID < maxCertainSystemRangeID {
		return nil, nil
	}

	// TODO: switch to metric.Gauge
	atomic.AddInt64(&c.stats.numWaiting, 1)
	defer atomic.AddInt64(&c.stats.numWaiting, -1)
	alloc, err := c.Pool.Acquire(ctx, 1)

	if err != nil {
		return nil, err
	}

	atomic.AddInt64(&c.stats.numAcquired, 1)
	return alloc, nil
}

func (c *Controller) release(alloc *quotapool.IntAlloc) {
	if alloc == nil {
		return
	}
	alloc.Release()
	atomic.AddInt64(&c.stats.numAcquired, -1)
}

// Interceptor returns a UnaryServerInterceptor with parameters
// taken from a Controller.
func Interceptor(c *Controller) grpc.UnaryServerInterceptor {

	admissionInterceptor := func(ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if ba, ok := req.(*roachpb.BatchRequest); ok {

			alloc, err := c.acquire(ctx, ba)
			if err != nil {
				return nil, err
			}
			defer c.release(alloc)
		}

		return handler(ctx, req)
	}

	return admissionInterceptor
}
