package admission

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"google.golang.org/grpc"
)

// Controller is a struct that determines the
// current admission parameters.
type Controller struct {
	pool *quotapool.IntPool

	// more data members?
}

// NewController constructs a new Controller struct.
func NewController() *Controller {
	c := Controller{

		// Right now this just has infinite capacity.
		pool: quotapool.NewIntPool("controller intpool", math.MaxInt64),
	}

	return &c
}

// Interceptor returns a UnaryServerInterceptor with parameters
// taken from a Controller.
func Interceptor(c *Controller) grpc.UnaryServerInterceptor {
	// Right now we don't actually do anything with the
	// Controller, but we'll do stuff later.

	admissionInterceptor := func(ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		// Take request timestamp
		start := time.Now().UnixNano()

		// Call handler
		h, err := handler(ctx, req)

		// Log request reception (and print to console for good
		// measure)
		log.Infof(ctx, "request received at %d\n", start)
		fmt.Printf("admission interceptor: request received at %d\n", start)
		fmt.Printf("info: %s\n", info.FullMethod)

		return h, err
	}

	return admissionInterceptor
}
