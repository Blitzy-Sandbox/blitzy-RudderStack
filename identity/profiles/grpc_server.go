// Package profiles provides the Profiles API for identity resolution (E-027).
//
// This file implements the gRPC ProfilesService server, complementing the REST API
// defined in api.go. Both the REST and gRPC servers delegate to the same
// graph.Service business logic layer, ensuring consistent behavior regardless
// of the access protocol.
//
// The gRPC server is registered and started by the embedded application handler
// (app/apphandlers/embeddedAppHandler.go) alongside the REST API. It listens on
// a configurable port (Identity.Profiles.gRPC.port, default 50051) and provides
// high-performance inter-service communication for the 5 ProfilesService RPCs
// defined in proto/identity/profiles.proto:
//
//   - GetProfile: Resolve a profile by external identifier
//   - GetProfileTraits: List traits for a profile
//   - GetProfileEvents: List derived events for a profile
//   - GetProfileExternalIds: List external identifiers for a profile
//   - GetProfileMetadata: Get merge and timing metadata for a profile
//
// Thread-safe for concurrent use via gRPC's per-request goroutine model.
package profiles

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/rudderlabs/rudder-go-kit/logger"
	obskit "github.com/rudderlabs/rudder-observability-kit/go/labels"

	"github.com/rudderlabs/rudder-server/identity/graph"
	"github.com/rudderlabs/rudder-server/identity/storage"
	identityproto "github.com/rudderlabs/rudder-server/proto/identity"
)

// defaultGRPCPort is the default TCP port for the Profiles gRPC server.
// Configurable via "Identity.Profiles.gRPC.port".
const defaultGRPCPort = 50051

// defaultPageLimit is the default pagination limit for list operations
// when the client does not specify a limit.
const defaultPageLimit int32 = 100

// GRPCServer implements the ProfilesServiceServer gRPC interface defined in
// proto/identity/profiles.proto. It delegates all business logic to the
// graph.Service, reusing the same identity resolution and profile assembly
// that powers the REST API.
//
// The server is created via NewGRPCServer and started via Start(ctx).
// It stops gracefully when the provided context is cancelled.
type GRPCServer struct {
	identityproto.UnimplementedProfilesServiceServer

	graphService graph.Service
	conf         *config.Config
	log          logger.Logger
	server       *grpc.Server
}

// NewGRPCServer creates a new gRPC server for the ProfilesService.
//
// Parameters:
//   - graphService: The identity graph service for profile resolution. Must not be nil.
//   - conf: Configuration provider. If nil, config.Default is used.
//   - log: Structured logger. If nil, the package-level pkgLogger is used.
//
// Returns an error if graphService is nil, since the server cannot function
// without a business logic backend.
func NewGRPCServer(graphService graph.Service, conf *config.Config, log logger.Logger) (*GRPCServer, error) {
	if graphService == nil {
		return nil, fmt.Errorf("profiles gRPC server: graph service is required")
	}
	if conf == nil {
		conf = config.Default
	}
	if log == nil {
		log = pkgLogger
	}
	return &GRPCServer{
		graphService: graphService,
		conf:         conf,
		log:          log.Child("grpc"),
	}, nil
}

// Start creates the gRPC server, registers the ProfilesService, and begins
// serving on the configured TCP port. It blocks until ctx is cancelled,
// at which point it performs a graceful shutdown.
//
// The port is read from "Identity.Profiles.gRPC.port" configuration key
// with a default of 50051.
//
// This method is intended to be called in an errgroup goroutine:
//
//	g.Go(func() error { return grpcServer.Start(ctx) })
func (s *GRPCServer) Start(ctx context.Context) error {
	s.server = grpc.NewServer()
	identityproto.RegisterProfilesServiceServer(s.server, s)

	port := s.conf.GetIntVar(defaultGRPCPort, 1, "Identity.Profiles.gRPC.port")
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("profiles gRPC: failed to listen on port %d: %w", port, err)
	}
	s.log.Infon("Profiles gRPC server listening",
		logger.NewIntField("port", int64(port)),
	)

	// Graceful shutdown when context is cancelled.
	go func() {
		<-ctx.Done()
		s.log.Infon("Profiles gRPC server shutting down gracefully")
		s.server.GracefulStop()
	}()

	if err := s.server.Serve(lis); err != nil {
		return fmt.Errorf("profiles gRPC: serve failed: %w", err)
	}
	return nil
}

// Stop gracefully stops the gRPC server if it is running.
// Safe to call multiple times or on a nil/unstarted server.
func (s *GRPCServer) Stop() {
	if s.server != nil {
		s.server.GracefulStop()
	}
}

// ---------------------------------------------------------------------------
// ProfilesService RPC implementations
// ---------------------------------------------------------------------------

// GetProfile resolves a profile by external identifier (e.g., user_id, email)
// and returns the full profile including traits, external IDs, and metadata.
//
// This is the primary lookup method for identity resolution — it first resolves
// the external identifier to an identity graph segment, then assembles the
// complete profile from that segment.
func (s *GRPCServer) GetProfile(ctx context.Context, req *identityproto.GetProfileRequest) (*identityproto.Profile, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace_id is required")
	}
	if req.GetIdentifierType() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "identifier_type is required")
	}
	if req.GetIdentifierValue() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "identifier_value is required")
	}

	segment, err := s.graphService.ResolveIdentity(ctx, req.GetWorkspaceId(), req.GetIdentifierType(), req.GetIdentifierValue())
	if err != nil {
		s.log.Errorn("GetProfile: failed to resolve identity",
			obskit.Error(err),
			logger.NewStringField("workspace_id", req.GetWorkspaceId()),
			logger.NewStringField("identifier_type", req.GetIdentifierType()),
		)
		return nil, status.Errorf(codes.Internal, "failed to resolve identity: %v", err)
	}
	if segment == nil {
		return nil, status.Errorf(codes.NotFound, "no profile found for %s=%s", req.GetIdentifierType(), req.GetIdentifierValue())
	}

	profileData, err := s.graphService.GetProfileData(ctx, segment.ID)
	if err != nil {
		s.log.Errorn("GetProfile: failed to get profile data",
			obskit.Error(err),
			logger.NewIntField("segment_id", segment.ID),
		)
		return nil, status.Errorf(codes.Internal, "failed to retrieve profile data")
	}
	if profileData == nil {
		return nil, status.Errorf(codes.NotFound, "profile data not found for segment %d", segment.ID)
	}

	return storageProfileToProto(profileData), nil
}

// GetProfileTraits returns the traits (key-value attributes) for a profile.
// The profile_id in the request is the identity graph segment ID (int64).
// Supports pagination via limit and offset fields.
func (s *GRPCServer) GetProfileTraits(ctx context.Context, req *identityproto.GetProfileTraitsRequest) (*identityproto.ProfileTraits, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace_id is required")
	}
	segmentID, err := parseSegmentID(req.GetProfileId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid profile_id: %v", err)
	}

	traits, err := s.graphService.GetSegmentTraits(ctx, segmentID)
	if err != nil {
		s.log.Errorn("GetProfileTraits: failed to get traits",
			obskit.Error(err),
			logger.NewIntField("segment_id", segmentID),
		)
		return nil, status.Errorf(codes.Internal, "failed to retrieve traits")
	}

	// Build traits map.
	traitsMap := make(map[string]string, len(traits))
	var latestUpdate time.Time
	for _, t := range traits {
		traitsMap[t.Key] = t.Value
		if t.UpdatedAt.After(latestUpdate) {
			latestUpdate = t.UpdatedAt
		}
	}

	result := &identityproto.ProfileTraits{
		Traits: traitsMap,
	}
	if !latestUpdate.IsZero() {
		result.UpdatedAt = timestamppb.New(latestUpdate)
	}
	return result, nil
}

// GetProfileEvents returns derived events for a profile assembled from the
// profile's external identifiers and traits.
//
// Event types produced:
//   - "identify": One per external identifier, representing the initial identification
//   - "merge": One per external identifier that was merged from another segment
//   - "trait_update": One per trait, representing the most recent trait value change
//
// Events are sorted by timestamp descending (most recent first) and support
// pagination via limit and offset fields.
func (s *GRPCServer) GetProfileEvents(ctx context.Context, req *identityproto.GetProfileEventsRequest) (*identityproto.ProfileEvents, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace_id is required")
	}
	segmentID, err := parseSegmentID(req.GetProfileId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid profile_id: %v", err)
	}

	profileData, err := s.graphService.GetProfileData(ctx, segmentID)
	if err != nil {
		s.log.Errorn("GetProfileEvents: failed to get profile data",
			obskit.Error(err),
			logger.NewIntField("segment_id", segmentID),
		)
		return nil, status.Errorf(codes.Internal, "failed to retrieve profile data")
	}
	if profileData == nil {
		return nil, status.Errorf(codes.NotFound, "profile not found")
	}

	// Build events from profile data following the same logic as the REST handler
	// (api.go buildProfileEvents) to ensure consistent behavior across protocols.
	allEvents := buildProtoProfileEvents(profileData)

	// Sort events by timestamp descending (most recent first).
	sort.Slice(allEvents, func(i, j int) bool {
		ti := allEvents[i].GetTimestamp().AsTime()
		tj := allEvents[j].GetTimestamp().AsTime()
		return ti.After(tj)
	})

	// Apply pagination.
	total := int32(len(allEvents))
	limit := req.GetLimit()
	offset := req.GetOffset()
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if offset < 0 {
		offset = 0
	}

	start := int(offset)
	if start > len(allEvents) {
		start = len(allEvents)
	}
	end := start + int(limit)
	if end > len(allEvents) {
		end = len(allEvents)
	}
	page := allEvents[start:end]

	return &identityproto.ProfileEvents{
		Events: page,
		Pagination: &identityproto.Pagination{
			Total:  total,
			Limit:  limit,
			Offset: offset,
		},
	}, nil
}

// GetProfileExternalIds returns all external identifiers associated with a profile.
// Supports pagination via limit and offset fields.
func (s *GRPCServer) GetProfileExternalIds(ctx context.Context, req *identityproto.GetProfileExternalIdsRequest) (*identityproto.ProfileExternalIds, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace_id is required")
	}
	segmentID, err := parseSegmentID(req.GetProfileId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid profile_id: %v", err)
	}

	externalIDs, err := s.graphService.GetSegmentIdentifiers(ctx, segmentID)
	if err != nil {
		s.log.Errorn("GetProfileExternalIds: failed to get identifiers",
			obskit.Error(err),
			logger.NewIntField("segment_id", segmentID),
		)
		return nil, status.Errorf(codes.Internal, "failed to retrieve external identifiers")
	}

	// Convert storage types to proto and apply pagination.
	total := int32(len(externalIDs))
	limit := req.GetLimit()
	offset := req.GetOffset()
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if offset < 0 {
		offset = 0
	}

	start := int(offset)
	if start > len(externalIDs) {
		start = len(externalIDs)
	}
	end := start + int(limit)
	if end > len(externalIDs) {
		end = len(externalIDs)
	}

	protoIDs := make([]*identityproto.ExternalId, 0, end-start)
	for _, eid := range externalIDs[start:end] {
		protoIDs = append(protoIDs, storageExternalIDToProto(&eid))
	}

	return &identityproto.ProfileExternalIds{
		ExternalIds: protoIDs,
		Pagination: &identityproto.Pagination{
			Total:  total,
			Limit:  limit,
			Offset: offset,
		},
	}, nil
}

// GetProfileMetadata returns merge and timing metadata for a profile.
// The metadata includes merge count, first-seen, and last-seen timestamps
// derived from the profile's external identifiers and traits.
func (s *GRPCServer) GetProfileMetadata(ctx context.Context, req *identityproto.GetProfileMetadataRequest) (*identityproto.ProfileMetadata, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "workspace_id is required")
	}
	segmentID, err := parseSegmentID(req.GetProfileId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid profile_id: %v", err)
	}

	profileData, err := s.graphService.GetProfileData(ctx, segmentID)
	if err != nil {
		s.log.Errorn("GetProfileMetadata: failed to get profile data",
			obskit.Error(err),
			logger.NewIntField("segment_id", segmentID),
		)
		return nil, status.Errorf(codes.Internal, "failed to retrieve profile metadata")
	}
	if profileData == nil {
		return nil, status.Errorf(codes.NotFound, "profile not found")
	}

	return buildProfileMetadataProto(profileData), nil
}

// ---------------------------------------------------------------------------
// Conversion helpers — storage types to proto types
// ---------------------------------------------------------------------------

// storageProfileToProto converts a complete ProfileData from the storage layer
// into the proto Profile message. Assembles traits, external IDs, and metadata.
func storageProfileToProto(pd *storage.ProfileData) *identityproto.Profile {
	profile := &identityproto.Profile{
		Id:          strconv.FormatInt(pd.Segment.ID, 10),
		WorkspaceId: pd.Segment.WorkspaceID,
		CreatedAt:   timestamppb.New(pd.Segment.CreatedAt),
	}

	// Traits
	if len(pd.Traits) > 0 {
		traitsMap := make(map[string]string, len(pd.Traits))
		var latestTraitUpdate time.Time
		for _, t := range pd.Traits {
			traitsMap[t.Key] = t.Value
			if t.UpdatedAt.After(latestTraitUpdate) {
				latestTraitUpdate = t.UpdatedAt
			}
		}
		profile.Traits = &identityproto.ProfileTraits{
			Traits: traitsMap,
		}
		if !latestTraitUpdate.IsZero() {
			profile.Traits.UpdatedAt = timestamppb.New(latestTraitUpdate)
			// Use the latest trait update as the overall profile update time.
			profile.UpdatedAt = timestamppb.New(latestTraitUpdate)
		}
	}

	// External IDs
	if len(pd.ExternalIDs) > 0 {
		protoIDs := make([]*identityproto.ExternalId, 0, len(pd.ExternalIDs))
		for i := range pd.ExternalIDs {
			protoIDs = append(protoIDs, storageExternalIDToProto(&pd.ExternalIDs[i]))
		}
		profile.ExternalIds = protoIDs
	}

	// Metadata
	profile.Metadata = buildProfileMetadataProto(pd)

	return profile
}

// storageExternalIDToProto converts a single storage.ExternalID to the proto ExternalId.
func storageExternalIDToProto(eid *storage.ExternalID) *identityproto.ExternalId {
	return &identityproto.ExternalId{
		Id:            strconv.FormatInt(eid.ID, 10),
		Type:          eid.ExternalIDType,
		Value:         eid.ExternalIDValue,
		CreatedSource: eid.CreatedSource,
		CreatedAt:     timestamppb.New(eid.CreatedAt),
	}
}

// buildProfileMetadataProto constructs ProfileMetadata from ProfileData by
// computing merge count, first-seen, and last-seen timestamps from the
// profile's external identifiers and traits.
func buildProfileMetadataProto(pd *storage.ProfileData) *identityproto.ProfileMetadata {
	meta := &identityproto.ProfileMetadata{
		Id:          strconv.FormatInt(pd.Segment.ID, 10),
		WorkspaceId: pd.Segment.WorkspaceID,
	}

	// Count merges: external IDs with a non-nil MergedAt were merged from another segment.
	var mergeCount int64
	var firstSeen, lastSeen time.Time

	for _, eid := range pd.ExternalIDs {
		if eid.MergedAt != nil {
			mergeCount++
		}
		if firstSeen.IsZero() || eid.CreatedAt.Before(firstSeen) {
			firstSeen = eid.CreatedAt
		}
		if eid.CreatedAt.After(lastSeen) {
			lastSeen = eid.CreatedAt
		}
	}

	// Also consider trait update times for last-seen.
	for _, t := range pd.Traits {
		if firstSeen.IsZero() || t.UpdatedAt.Before(firstSeen) {
			firstSeen = t.UpdatedAt
		}
		if t.UpdatedAt.After(lastSeen) {
			lastSeen = t.UpdatedAt
		}
	}

	// Include segment creation time as a candidate for first-seen.
	if firstSeen.IsZero() || pd.Segment.CreatedAt.Before(firstSeen) {
		firstSeen = pd.Segment.CreatedAt
	}

	meta.MergeCount = mergeCount
	if !firstSeen.IsZero() {
		meta.FirstSeen = timestamppb.New(firstSeen)
	}
	if !lastSeen.IsZero() {
		meta.LastSeen = timestamppb.New(lastSeen)
	}

	return meta
}

// buildProtoProfileEvents constructs derived ProfileEvent messages from a
// ProfileData, following the same logic as the REST handler's buildProfileEvents.
//
// Event types:
//   - "identify": One per external ID representing the initial identification.
//   - "merge": One per external ID that was merged from another segment.
//   - "trait_update": One per trait representing the latest value change.
func buildProtoProfileEvents(pd *storage.ProfileData) []*identityproto.ProfileEvent {
	events := make([]*identityproto.ProfileEvent, 0, len(pd.ExternalIDs)+len(pd.Traits))

	// External IDs produce "identify" events, and optionally "merge" events.
	for _, eid := range pd.ExternalIDs {
		events = append(events, &identityproto.ProfileEvent{
			Id:        strconv.FormatInt(eid.ID, 10),
			Type:      "identify",
			EventName: "identify",
			Properties: fmt.Sprintf(`{"external_id_type":%q,"external_id_value":%q,"source":%q}`,
				eid.ExternalIDType, eid.ExternalIDValue, eid.CreatedSource),
			Timestamp:  timestamppb.New(eid.CreatedAt),
			ReceivedAt: timestamppb.New(eid.CreatedAt),
		})

		// If this external ID was merged from another segment, add a merge event.
		if eid.MergedAt != nil {
			events = append(events, &identityproto.ProfileEvent{
				Id:   fmt.Sprintf("merge-%d", eid.ID),
				Type: "merge",
				Properties: fmt.Sprintf(`{"external_id_type":%q,"external_id_value":%q,"action":"merge"}`,
					eid.ExternalIDType, eid.ExternalIDValue),
				Timestamp:  timestamppb.New(*eid.MergedAt),
				ReceivedAt: timestamppb.New(*eid.MergedAt),
			})
		}
	}

	// Traits produce "trait_update" events.
	for _, t := range pd.Traits {
		events = append(events, &identityproto.ProfileEvent{
			Id:         fmt.Sprintf("trait-%d", t.ID),
			Type:       "trait_update",
			EventName:  "trait_update",
			Properties: fmt.Sprintf(`{"key":%q,"value":%q}`, t.Key, t.Value),
			Timestamp:  timestamppb.New(t.UpdatedAt),
			ReceivedAt: timestamppb.New(t.UpdatedAt),
		})
	}

	return events
}

// parseSegmentID parses a profile/segment ID string into an int64.
// Returns an error if the string is empty or not a valid positive integer.
func parseSegmentID(idStr string) (int64, error) {
	if idStr == "" {
		return 0, fmt.Errorf("profile_id is required")
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("must be a numeric segment ID: %w", err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("must be positive, got %d", id)
	}
	return id, nil
}
