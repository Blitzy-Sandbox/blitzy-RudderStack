//go:generate mockgen -destination=../../mocks/processor/transformer/mock_transformer_clients.go -package=mocks_transformer_clients github.com/rudderlabs/rudder-server/processor/transformer TransformerClients

package transformer

import (
	"context"

	"github.com/rudderlabs/rudder-server/processor/internal/transformer/sourcehydration"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	"github.com/rudderlabs/rudder-go-kit/stats"
	"github.com/rudderlabs/rudder-server/processor/internal/transformer/destination_transformer"
	"github.com/rudderlabs/rudder-server/processor/internal/transformer/trackingplan_validation"
	"github.com/rudderlabs/rudder-server/processor/internal/transformer/user_transformer"
	"github.com/rudderlabs/rudder-server/processor/types"
	transformerfs "github.com/rudderlabs/rudder-server/services/transformer"
)

type DestinationClient interface {
	Transform(ctx context.Context, events []types.TransformerEvent) types.Response
}

type UserClient interface {
	Transform(ctx context.Context, events []types.TransformerEvent) types.Response
}

type TrackingPlanClient interface {
	Validate(ctx context.Context, events []types.TransformerEvent) types.Response
}

type SrcHydrationClient interface {
	Hydrate(ctx context.Context, req types.SrcHydrationRequest) (types.SrcHydrationResponse, error)
}

// FunctionsClient communicates with the Functions runtime for Source, Destination, and Insert Functions.
// Source Functions execute onRequest handlers for custom webhook ingestion (E-015).
// Destination Functions execute typed event handlers (onTrack, onIdentify, etc.) (E-016).
// Insert Functions execute pre-destination transformation hooks (E-017).
type FunctionsClient interface {
	// ExecuteSourceFunction invokes the onRequest handler for a Source Function, processing
	// an incoming HTTP webhook request and returning generated events.
	ExecuteSourceFunction(ctx context.Context, functionID string, request types.FunctionRequest) (types.FunctionResponse, error)
	// ExecuteDestinationFunction invokes the typed event handler (onTrack, onIdentify, onGroup,
	// onPage, onScreen, onAlias, onDelete, onBatch) for a Destination Function.
	ExecuteDestinationFunction(ctx context.Context, functionID string, eventType string, events []types.TransformerEvent) types.Response
	// ExecuteInsertFunction invokes a pre-destination transformation hook that runs between
	// user transformations and destination transformations in the pipeline.
	ExecuteInsertFunction(ctx context.Context, functionID string, events []types.TransformerEvent) types.Response
}

type Clients struct {
	user         UserClient
	userMirror   UserClient
	destination  DestinationClient
	trackingplan TrackingPlanClient
	srcHydration SrcHydrationClient
	functions    FunctionsClient // Functions runtime client (E-015/E-016/E-017)
}

type TransformerClients interface {
	User() UserClient
	UserMirror() UserClient
	Destination() DestinationClient
	TrackingPlan() TrackingPlanClient
	SrcHydration() SrcHydrationClient
	Functions() FunctionsClient // Functions runtime accessor (E-015/E-016/E-017)
}

// WithFeatureService is used to set the feature service for the destination transformer.
func WithFeatureService(featuresService transformerfs.FeaturesService) func(*opts) {
	return func(o *opts) {
		o.destinationOpts = append(o.destinationOpts, destination_transformer.WithFeatureService(featuresService))
	}
}

// WithFunctionsClient is used to inject a Functions runtime client into the transformer clients.
// When not provided, the Functions client defaults to nil and all Functions-related pipeline stages
// are no-op pass-throughs, maintaining backward compatibility with existing pipeline behavior.
func WithFunctionsClient(fc FunctionsClient) func(*opts) {
	return func(o *opts) {
		o.functionsClient = fc
	}
}

// NewClients creates a new instance of TransformerClients.
func NewClients(conf *config.Config, log logger.Logger, statsFactory stats.Stats, options ...func(*opts)) TransformerClients {
	var opts opts
	for _, option := range options {
		option(&opts)
	}
	return &Clients{
		user:         user_transformer.New(conf, log, statsFactory),
		userMirror:   user_transformer.New(conf, log, statsFactory, user_transformer.ForMirroring()),
		destination:  destination_transformer.New(conf, log, statsFactory, opts.destinationOpts...),
		trackingplan: trackingplan_validation.New(conf, log, statsFactory),
		srcHydration: sourcehydration.New(conf, log, statsFactory),
		functions:    opts.functionsClient, // nil when Functions runtime is not available (E-015/E-016/E-017)
	}
}

func (c *Clients) User() UserClient { return c.user }

func (c *Clients) UserMirror() UserClient { return c.userMirror }

func (c *Clients) Destination() DestinationClient { return c.destination }

func (c *Clients) TrackingPlan() TrackingPlanClient { return c.trackingplan }

func (c *Clients) SrcHydration() SrcHydrationClient {
	return c.srcHydration
}

// Functions returns the Functions runtime client for Source, Destination, and Insert Functions.
// Returns nil when no Functions runtime is configured, indicating Functions pipeline stages
// should be no-op pass-throughs (E-015/E-016/E-017).
func (c *Clients) Functions() FunctionsClient { return c.functions }

type opts struct {
	destinationOpts []destination_transformer.Opt
	functionsClient FunctionsClient // Functions runtime client injection (E-015/E-016/E-017)
}
