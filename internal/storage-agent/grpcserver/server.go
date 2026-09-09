// Package grpcserver adapts internal/storage-agent's Backend to
// StorageBackendService (proto/kyuusha/storageagent/v1).
package grpcserver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	storageagent "gitlab.com/ki.yuta1230/kyuusha/internal/storage-agent"

	storageagentv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/storageagent/v1"
)

type Server struct {
	storageagentv1.UnimplementedStorageBackendServiceServer
	backend *storageagent.Backend
}

func NewServer(backend *storageagent.Backend) *Server {
	return &Server{backend: backend}
}

func (s *Server) CreateVolume(ctx context.Context, req *storageagentv1.CreateVolumeRequest) (*storageagentv1.CreateVolumeResponse, error) {
	if err := s.backend.CreateVolume(ctx, req.GetVolumeId(), req.GetSizeGb()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &storageagentv1.CreateVolumeResponse{}, nil
}

func (s *Server) DeleteVolume(ctx context.Context, req *storageagentv1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	if err := s.backend.DeleteVolume(ctx, req.GetVolumeId()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) ExportVolume(ctx context.Context, req *storageagentv1.ExportVolumeRequest) (*storageagentv1.ExportVolumeResponse, error) {
	iqn, portal, err := s.backend.ExportVolume(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &storageagentv1.ExportVolumeResponse{TargetIqn: iqn, Portal: portal}, nil
}

func (s *Server) UnexportVolume(ctx context.Context, req *storageagentv1.UnexportVolumeRequest) (*emptypb.Empty, error) {
	if err := s.backend.UnexportVolume(ctx, req.GetVolumeId()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &emptypb.Empty{}, nil
}
