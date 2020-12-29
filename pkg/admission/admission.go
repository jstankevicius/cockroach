package admission

import (
	"context"
	"strconv"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
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
}

// NewController constructs a new Controller struct from a given Config.
func NewController(conf Config) *Controller {
	c := Controller{

		// Should this have a better name?
		pool: quotapool.NewIntPool("controller intpool", conf.Limit),
	}

	return &c
}

// Interceptor returns a UnaryServerInterceptor with parameters
// taken from a Controller.
func Interceptor(c *Controller) grpc.UnaryServerInterceptor {

	admissionInterceptor := func(ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if info.FullMethod == "/cockroach.roachpb.Internal/Batch" {

			// Attempt to acquire a single unit from the IntPool.
			// We're not going to handle the error for now.
			alloc, _ := c.pool.Acquire(ctx, 1)
			defer c.pool.Release(alloc)

			// Take request timestamp, add it to context's metadata, and return
			// this new context.
			start := time.Now().UnixNano()
			ctx = metadata.AppendToOutgoingContext(ctx, "start-time", strconv.Itoa(int(start)))

			// outMd, _ := metadata.FromOutgoingContext(ctx)
			// log.Infof(ctx, "%s\n", outMd.Get("start-time"))
			// fmt.Printf("%s\n", outMd.Get("start-time"))

			return handler(ctx, req)
		}

		// Otherwise just handle normally
		return handler(ctx, req)
	}

	return admissionInterceptor
}
