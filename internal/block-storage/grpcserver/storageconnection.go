package grpcserver

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	blockstorage "github.com/kiyuta1230/kyuusha/internal/block-storage"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

type StorageConnectionServer struct {
	blockstoragev1.UnimplementedStorageConnectionServiceServer
	svc *blockstorage.Service
}

func NewStorageConnectionServer(svc *blockstorage.Service) *StorageConnectionServer {
	return &StorageConnectionServer{svc: svc}
}

func (s *StorageConnectionServer) Create(ctx context.Context, req *blockstoragev1.CreateStorageConnectionRequest) (*blockstoragev1.StorageConnection, error) {
	sc, err := s.svc.CreateStorageConnection(ctx, req.GetName(), fromStorageConnectionSpec(req.GetSpec()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toStorageConnection(*sc), nil
}

func (s *StorageConnectionServer) Get(ctx context.Context, req *blockstoragev1.GetStorageConnectionRequest) (*blockstoragev1.StorageConnection, error) {
	sc, err := s.svc.GetStorageConnection(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toStorageConnection(*sc), nil
}

func (s *StorageConnectionServer) List(ctx context.Context, req *blockstoragev1.ListStorageConnectionsRequest) (*blockstoragev1.ListStorageConnectionsResponse, error) {
	scs, err := s.svc.ListStorageConnections(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &blockstoragev1.ListStorageConnectionsResponse{}
	for _, sc := range scs {
		out.Items = append(out.Items, toStorageConnection(sc))
	}
	return out, nil
}

func (s *StorageConnectionServer) Delete(ctx context.Context, req *blockstoragev1.DeleteStorageConnectionRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteStorageConnection(ctx, req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *StorageConnectionServer) Watch(req *blockstoragev1.WatchStorageConnectionsRequest, stream blockstoragev1.StorageConnectionService_WatchServer) error {
	events, err := s.svc.WatchStorageConnections(stream.Context(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		if err := stream.Send(toStorageConnectionEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func fromStorageConnectionSpec(s *blockstoragev1.StorageConnectionSpec) blockstorage.StorageConnectionSpec {
	return blockstorage.StorageConnectionSpec{
		Zones:       s.GetZones(),
		Annotations: s.GetAnnotations(),
	}
}

func toStorageConnectionSpec(s blockstorage.StorageConnectionSpec) *blockstoragev1.StorageConnectionSpec {
	return &blockstoragev1.StorageConnectionSpec{
		Zones:       s.Zones,
		Annotations: s.Annotations,
	}
}

func toStorageConnectionStatusProto(st blockstorage.StorageConnectionStatus) *blockstoragev1.StorageConnectionStatus {
	out := &blockstoragev1.StorageConnectionStatus{
		Phase:         string(st.Phase),
		VerifiedZones: st.VerifiedZones,
	}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, toConditionProto(c))
	}
	return out
}

func toStorageConnection(sc blockstorage.StorageConnection) *blockstoragev1.StorageConnection {
	return &blockstoragev1.StorageConnection{
		Meta:   toMetaProto(sc.Meta),
		Spec:   toStorageConnectionSpec(sc.Spec),
		Status: toStorageConnectionStatusProto(sc.Status),
	}
}

func toStorageConnectionEvent(e blockstorage.StorageConnectionEvent) *blockstoragev1.StorageConnectionEvent {
	out := &blockstoragev1.StorageConnectionEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case blockstorage.EventAdded:
		out.Type = blockstoragev1.StorageConnectionEvent_ADDED
	case blockstorage.EventModified:
		out.Type = blockstoragev1.StorageConnectionEvent_MODIFIED
	case blockstorage.EventDeleted:
		out.Type = blockstoragev1.StorageConnectionEvent_DELETED
	case blockstorage.EventBookmark:
		out.Type = blockstoragev1.StorageConnectionEvent_BOOKMARK
	}
	if out.Type != blockstoragev1.StorageConnectionEvent_BOOKMARK {
		out.StorageConnection = toStorageConnection(e.Object)
	}
	return out
}
