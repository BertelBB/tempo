package distributor

import (
	"context"
	"flag"
	"net/http"
	"sync/atomic"
	"time"

	cortex_distributor "github.com/cortexproject/cortex/pkg/distributor"
	cortex_client "github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/ring"
	cortex_util "github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/limiter"
	cortex_validation "github.com/cortexproject/cortex/pkg/util/validation"

	"github.com/go-kit/kit/log/level"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/joe-elliott/frigg/pkg/friggpb"
	"github.com/joe-elliott/frigg/pkg/ingester/client"
	"github.com/joe-elliott/frigg/pkg/util"
	"github.com/joe-elliott/frigg/pkg/util/validation"
)

var readinessProbeSuccess = []byte("Ready")
var (
	ingesterAppends = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loki",
		Name:      "distributor_ingester_appends_total",
		Help:      "The total number of batch appends sent to ingesters.",
	}, []string{"ingester"})
	ingesterAppendFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "loki",
		Name:      "distributor_ingester_append_failures_total",
		Help:      "The total number of failed batch appends sent to ingesters.",
	}, []string{"ingester"})

	spansIngested = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "frigg",
		Name:      "distributor_spans_received_total",
		Help:      "The total number of spans received per tenant",
	}, []string{"tenant"})
)

// Config for a Distributor.
type Config struct {
	// Distributors ring
	DistributorRing cortex_distributor.RingConfig `yaml:"ring,omitempty"`

	// For testing.
	factory func(addr string) (grpc_health_v1.HealthClient, error) `yaml:"-"`
}

// RegisterFlags registers the flags.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.DistributorRing.RegisterFlags(f)
}

// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
	cfg           Config
	clientCfg     client.Config
	ingestersRing ring.ReadRing
	overrides     *validation.Overrides
	pool          *cortex_client.Pool

	// The global rate limiter requires a distributors ring to count
	// the number of healthy instances.
	distributorsRing *ring.Lifecycler

	// Per-user rate limiter.
	ingestionRateLimiter *limiter.RateLimiter
}

// New a distributor creates.
func New(cfg Config, clientCfg client.Config, ingestersRing ring.ReadRing, overrides *validation.Overrides) (*Distributor, error) {
	factory := cfg.factory
	if factory == nil {
		factory = func(addr string) (grpc_health_v1.HealthClient, error) {
			return client.New(clientCfg, addr)
		}
	}

	// Create the configured ingestion rate limit strategy (local or global).
	var ingestionRateStrategy limiter.RateLimiterStrategy
	var distributorsRing *ring.Lifecycler

	if overrides.IngestionRateStrategy() == validation.GlobalIngestionRateStrategy {
		var err error
		distributorsRing, err = ring.NewLifecycler(cfg.DistributorRing.ToLifecyclerConfig(), nil, "distributor", ring.DistributorRingKey)
		if err != nil {
			return nil, err
		}

		distributorsRing.Start()

		ingestionRateStrategy = newGlobalIngestionRateStrategy(overrides, distributorsRing)
	} else {
		ingestionRateStrategy = newLocalIngestionRateStrategy(overrides)
	}

	d := Distributor{
		cfg:                  cfg,
		clientCfg:            clientCfg,
		ingestersRing:        ingestersRing,
		distributorsRing:     distributorsRing,
		overrides:            overrides,
		pool:                 cortex_client.NewPool(clientCfg.PoolConfig, ingestersRing, factory, cortex_util.Logger),
		ingestionRateLimiter: limiter.NewRateLimiter(ingestionRateStrategy, 10*time.Second),
	}

	return &d, nil
}

func (d *Distributor) Stop() {
	if d.distributorsRing != nil {
		d.distributorsRing.Shutdown()
	}
}

// ReadinessHandler is used to indicate to k8s when the distributor is ready.
// Returns 200 when the distributor is ready, 500 otherwise.
func (d *Distributor) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	_, err := d.ingestersRing.GetAll()
	if err != nil {
		http.Error(w, "Not ready: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(readinessProbeSuccess); err != nil {
		level.Error(cortex_util.Logger).Log("msg", "error writing success message", "error", err)
	}
}

// Push a set of streams.
func (d *Distributor) Push(ctx context.Context, req *friggpb.PushRequest) (*friggpb.PushResponse, error) {
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}

	// Track metrics.
	spanCount := 0
	for _, stream := range req.Spans {
		spanCount++
	}
	if spanCount == 0 {
		return &logproto.PushResponse{}, nil
	}
	spansIngested.WithLabelValues(userID).Add(float64(spanCount))

	// friggtodo: split spans into traces in case trace ids differ
	key := util.TokenFor(req.Spans[0].TraceID)

	now := time.Now()
	if !d.ingestionRateLimiter.AllowN(now, userID, validatedSamplesSize) {
		// Return a 4xx here to have the client discard the data and not retry. If a client
		// is sending too much data consistently we will unlikely ever catch up otherwise.
		validation.DiscardedSamples.WithLabelValues(validation.RateLimited, userID).Add(float64(validatedSamplesCount))
		validation.DiscardedBytes.WithLabelValues(validation.RateLimited, userID).Add(float64(validatedSamplesSize))
		return nil, httpgrpc.Errorf(http.StatusTooManyRequests, "ingestion rate limit (%d bytes) exceeded while adding %d lines for a total size of %d bytes", int(d.ingestionRateLimiter.Limit(now, userID)), validatedSamplesCount, validatedSamplesSize)
	}

	const maxExpectedReplicationSet = 1 // 1.  b/c frigg it
	var descs [maxExpectedReplicationSet]ring.IngesterDesc
	replicationSet, err := d.ingestersRing.Get(key, ring.Write, descs[:0])

	if len(replicationSet) != 1 {
		return &logproto.PushResponse{}, fmt.Errorf("we only need one friggin' replica, but got %d", len(replicationSet))
	}
	ingesterDesc := replicationSet[0]

	// Use a background context to make sure ingester gets samples even if we return early
	localCtx, cancel := context.WithTimeout(context.Background(), d.clientCfg.RemoteTimeout)
	defer cancel()
	localCtx = user.InjectOrgID(localCtx, userID)
	if sp := opentracing.SpanFromContext(ctx); sp != nil {
		localCtx = opentracing.ContextWithSpan(localCtx, sp)
	}
	err = d.sendSamplesErr(ctx, ingesterDesc, req)

	if err {
		return nil, err
	}
	return &logproto.PushResponse{}, nil
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendSamplesErr(ctx context.Context, ingester ring.IngesterDesc, req friggpb.PushRequest) error {
	c, err := d.pool.GetClientFor(ingester.Addr)
	if err != nil {
		return err
	}

	_, err = c.(friggpb.PusherClient).Push(ctx, req)
	ingesterAppends.WithLabelValues(ingester.Addr).Inc()
	if err != nil {
		ingesterAppendFailures.WithLabelValues(ingester.Addr).Inc()
	}
	return err
}

// Check implements the grpc healthcheck
func (*Distributor) Check(_ context.Context, _ *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}
