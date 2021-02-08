package admission

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"google.golang.org/grpc"
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Config configures a Controller.
type Config struct {
	Limit       uint64
	Alpha       float64
	Beta        float64
	DelayTarget time.Duration
}

// Default config settings:
const (
	defaultConcurrency = 50
	defaultAlpha       = 0.001                  // 0.1% from Breakwater
	defaultBeta        = 0.02                   // 2% from Breakwater
	defaultDelayTarget = 100 * time.Millisecond // rough estimate using ping avg
)

// DefaultConfig returns a Config with preset values.
func DefaultConfig() Config {
	c := Config{
		Limit:       defaultConcurrency,
		Alpha:       defaultAlpha,
		Beta:        defaultBeta,
		DelayTarget: defaultDelayTarget,
	}

	return c
}

// Controller determines the current admission parameters.
type Controller struct {
	Pool *quotapool.IntPool

	// Parameters:
	delayTarget time.Duration
	alpha       float64
	beta        float64

	slo time.Duration

	stats struct {

		// These should only be accessed using atomics.
		// These should also probably turn into metric.Gauges.
		numAcquired int64
		numWaiting  int64
	}

	mu struct {
		syncutil.Mutex

		// For tracking credits per upstream client
		credits map[roachpb.NodeID]int

		creditsTotal         int
		creditsOvercommitted int
		creditsIssued        int
		delayMeasured        time.Duration
	}
}

// NewController constructs a new Controller struct from a given Config.
func NewController(conf Config) *Controller {
	c := &Controller{

		Pool:        quotapool.NewIntPool("controller intpool", conf.Limit),
		delayTarget: 100 * time.Millisecond,
		alpha:       0.001,
		beta:        0.02,
	}

	c.mu.credits = make(map[roachpb.NodeID]int)
	fmt.Printf("creating new controller \n")

	go func() {
		for {
			// sleep for 1 network RTT (1 second for debug)
			time.Sleep(1 * time.Second)
			c.mu.Lock()

			nClients := len(c.mu.credits)
			if nClients > 0 {

				// it's time for a typecasting nightmare
				dm := float64(c.mu.delayMeasured.Nanoseconds())
				dt := float64(c.delayTarget.Nanoseconds())

				if dm < dt {
					newCredits := math.Max(float64(nClients)*c.alpha, 1)
					c.mu.creditsTotal = c.mu.creditsTotal + int(newCredits)
				} else {
					overloadLevel := 1 - c.beta*(dm-dt)/dt
					c.mu.creditsTotal = int(float64(c.mu.creditsTotal) * math.Max(overloadLevel, 0.5))
				}

				c.mu.creditsOvercommitted = max((c.mu.creditsTotal-c.mu.creditsIssued)/nClients, 1)

			}

			c.mu.Unlock()
		}
	}()

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

	// TODO: stuff this into a function
	// Now that we have acquired quota, we can determine how many credits
	// to issue back to the client. To simplify, we'll assume that demand = 1.

	ts, ok := rpc.BatchRPCStartTimestampFromContext(ctx)
	if !ok {
		return nil, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	id := ba.GatewayNodeID
	demand := 1 // maybe len(ba.Requests)?
	creditsAvailable := c.mu.creditsTotal - c.mu.creditsIssued
	oldClientTotal, exists := c.mu.credits[id]
	c.mu.delayMeasured = time.Now().Sub(ts)

	// If we don't have an entry for this node, we add one
	if !exists {
		c.mu.credits[id] = 0
	}

	// Compute a new credit total for the client:
	if c.mu.creditsIssued < c.mu.creditsTotal {
		c.mu.credits[id] = min(demand+c.mu.creditsOvercommitted, oldClientTotal+creditsAvailable)
	} else {
		c.mu.credits[id] = min(demand+c.mu.creditsOvercommitted, oldClientTotal-1)
	}
	newlyIssued := c.mu.credits[id] - oldClientTotal
	c.mu.creditsIssued += newlyIssued

	// TODO: piggyback newlyIssued onto a response.

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
