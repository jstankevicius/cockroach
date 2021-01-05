package admission

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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
	pool *quotapool.IntPool

	mu struct {
		syncutil.RWMutex

		numAcquired int64
		numWaiting  int64
	}
}

// NewController constructs a new Controller struct from a given Config.
func NewController(conf Config) *Controller {
	fmt.Println("initializing new controller")
	c := Controller{

		// Should this have a better name?
		pool: quotapool.NewIntPool("controller intpool", conf.Limit),
	}

	// Run some goroutine w/ stopper to log stats somewhere.
	fmt.Printf("intpool cap: %d\n", c.pool.Capacity())
	return &c
}

// NumWaiting returns the current number of requests that have been intercepted
// by admission.Interceptor but have not been acquired.
func (c *Controller) NumWaiting() int64 {
	return atomic.LoadInt64(&c.mu.numWaiting)
}

// NumAcquired returns the number of requests we have acquired quota for.
// This is functionally identical to computing
// c.pool.Capacity() - c.pool.ApproximateQuota().
func (c *Controller) NumAcquired() int64 {
	return atomic.LoadInt64(&c.mu.numAcquired)
}

func (c *Controller) handleRelease(alloc *quotapool.IntAlloc) {
	atomic.AddInt64(&c.mu.numAcquired, -1)
	c.pool.Release(alloc)
}

// Interceptor returns a UnaryServerInterceptor with parameters
// taken from a Controller.
func Interceptor(c *Controller) grpc.UnaryServerInterceptor {

	admissionInterceptor := func(ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		atomic.AddInt64(&c.mu.numWaiting, 1)

		if info.FullMethod == "/cockroach.roachpb.Internal/Batch" {

			// Attempt to acquire a single unit from the IntPool.
			// We're not going to handle the error for now.
			alloc, _ := c.pool.Acquire(ctx, 1)
			atomic.AddInt64(&c.mu.numAcquired, 1)
			atomic.AddInt64(&c.mu.numWaiting, -1)
			defer c.handleRelease(alloc)

			// Take request timestamp, add it to context's metadata, and return
			// this new context.
			start := time.Now().UnixNano()
			ctx = metadata.AppendToOutgoingContext(ctx, "start-time", strconv.Itoa(int(start)))

			return handler(ctx, req)
		}

		// Otherwise just handle normally
		return handler(ctx, req)
	}

	return admissionInterceptor
}
