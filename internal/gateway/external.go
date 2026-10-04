package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ExternalBackends forwards calls for gRPC services api-gateway doesn't
// implement itself -- registered by operator config as "service-name
// prefix -> backend address" -- to that backend, byte for byte, without
// knowing the backend's proto (see docs/specs/external-integration.md
// 「外部バックエンドの登録」). Installed as the gateway server's
// grpc.UnknownServiceHandler, so it runs behind the same stream
// interceptors (authn, authz, audit) and stats handler (metrics, traces)
// as every built-in service: those see one generic bidi stream per call,
// whatever the method's real shape (unary, server streaming, ...).
//
// authz needs the request's tenant_id. Frame.GetTenantId provides it by
// decoding the frame against the method's input message descriptor,
// fetched from the backend via gRPC server reflection (or a configured
// FileDescriptorSet) and cached per service, reading the field *named*
// tenant_id -- its number varies between messages. A method whose
// descriptor can't be resolved, or whose request has no tenant_id, reads
// as an empty tenant_id: authorized for cross-tenant roles only, the same
// rule as a built-in request without one.
type ExternalBackends struct {
	routes []*externalRoute
}

type externalRoute struct {
	prefix string // e.g. "kyuusha.vpc.v1."
	conn   *grpc.ClientConn

	mu    sync.Mutex
	files *protoregistry.Files // nil until the first successful resolve, unless preloaded
}

// AddRoute registers conn as the backend for every gRPC service whose full
// name starts with prefix. files, if non-nil, preloads descriptors (from a
// FileDescriptorSet) instead of using reflection.
func (e *ExternalBackends) AddRoute(prefix string, conn *grpc.ClientConn, files *protoregistry.Files) {
	e.routes = append(e.routes, &externalRoute{prefix: prefix, conn: conn, files: files})
}

func (e *ExternalBackends) route(service string) *externalRoute {
	var best *externalRoute
	for _, r := range e.routes {
		if strings.HasPrefix(service, r.prefix) && (best == nil || len(r.prefix) > len(best.prefix)) {
			best = r
		}
	}
	return best
}

// ParseExternalBackends parses -external-backends:
// "<service prefix>=<address>[,<service prefix>=<address>...]".
func ParseExternalBackends(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		prefix, addr, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || prefix == "" || addr == "" {
			return nil, fmt.Errorf("external backends: %q: want <service prefix>=<address>", entry)
		}
		if strings.HasPrefix(prefix, "kyuusha.") && !strings.HasSuffix(prefix, ".") {
			return nil, fmt.Errorf("external backends: %q: a prefix inside kyuusha.* must end at a package boundary (\".\")", prefix)
		}
		out[prefix] = addr
	}
	return out, nil
}

// Handler is the grpc.UnknownServiceHandler.
func (e *ExternalBackends) Handler() grpc.StreamHandler {
	return func(_ any, ss grpc.ServerStream) error {
		fullMethod, ok := grpc.MethodFromServerStream(ss)
		if !ok {
			return status.Error(codes.Internal, "external backend: no method in stream")
		}
		service, method := splitMethod(fullMethod)
		r := e.route(service)
		if r == nil {
			return status.Errorf(codes.Unimplemented, "unknown service %s", service)
		}
		return r.proxy(ss, fullMethod, service, method)
	}
}

func splitMethod(fullMethod string) (service, method string) {
	s := strings.TrimPrefix(fullMethod, "/")
	service, method, _ = strings.Cut(s, "/")
	return service, method
}

// proxy pumps one call: client->backend frames until the client half-
// closes, backend->client header, frames and trailer until the backend
// ends the call. The first client frame is received (and so authorized,
// by authz's RecvMsg wrapper) before the backend call is even opened.
func (r *externalRoute) proxy(ss grpc.ServerStream, fullMethod, service, method string) error {
	first := &Frame{tenantID: r.tenantIDFunc(ss.Context(), service, method)}
	if err := ss.RecvMsg(first); err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "external backend: no request message")
		}
		return err
	}

	// Only the trusted caller metadata (authn.PropagateCaller*, installed
	// on conn) goes to the backend -- never the client's own headers,
	// bearer token included, same as the built-in proxies.
	ctx, cancel := context.WithCancel(metadata.NewOutgoingContext(ss.Context(), metadata.MD{}))
	defer cancel()
	cs, err := r.conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, fullMethod, grpc.ForceCodecV2(frameCodec{}))
	if err != nil {
		return err
	}

	// Upstream pump. A client-side failure cancels the backend call, which
	// then surfaces below as the backend stream's own error.
	go func() {
		f := first
		for {
			if err := cs.SendMsg(f); err != nil {
				return // the backend ended the call; its status comes from RecvMsg below
			}
			f = &Frame{}
			if err := ss.RecvMsg(f); err != nil {
				if errors.Is(err, io.EOF) {
					_ = cs.CloseSend()
				} else {
					cancel()
				}
				return
			}
		}
	}()

	if header, err := cs.Header(); err == nil {
		if err := ss.SendHeader(header); err != nil {
			return err
		}
	}
	for {
		f := &Frame{}
		err := cs.RecvMsg(f)
		if err != nil {
			ss.SetTrailer(cs.Trailer())
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := ss.SendMsg(f); err != nil {
			return err
		}
	}
}

// tenantIDFunc returns the lazy tenant_id extractor for this method's
// request frames.
func (r *externalRoute) tenantIDFunc(ctx context.Context, service, method string) func([]byte) string {
	return func(payload []byte) string {
		in, err := r.inputDescriptor(ctx, service, method)
		if err != nil {
			slog.Warn("external backend: cannot resolve request type, treating tenant_id as empty", "service", service, "method", method, "err", err)
			return ""
		}
		fd := in.Fields().ByName("tenant_id")
		if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
			return ""
		}
		msg := dynamicpb.NewMessage(in)
		if err := proto.Unmarshal(payload, msg); err != nil {
			return ""
		}
		return msg.Get(fd).String()
	}
}

func (r *externalRoute) inputDescriptor(ctx context.Context, service, method string) (protoreflect.MessageDescriptor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files == nil || !hasService(r.files, service) {
		files, err := resolveByReflection(ctx, r.conn, service)
		if err != nil {
			return nil, err
		}
		r.files = files
	}
	d, err := r.files.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil, err
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s is not a service", service)
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil, fmt.Errorf("%s has no method %s", service, method)
	}
	return md.Input(), nil
}

func hasService(files *protoregistry.Files, service string) bool {
	_, err := files.FindDescriptorByName(protoreflect.FullName(service))
	return err == nil
}

// resolveByReflection asks the backend (grpc.reflection.v1) for the file
// defining service plus its transitive dependencies.
func resolveByReflection(ctx context.Context, conn *grpc.ClientConn, service string) (*protoregistry.Files, error) {
	ctx = metadata.NewOutgoingContext(ctx, metadata.MD{})
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("reflection: %w", err)
	}
	defer stream.CloseSend()
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: service},
	}); err != nil {
		return nil, fmt.Errorf("reflection: %w", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("reflection: %w", err)
	}
	if e := resp.GetErrorResponse(); e != nil {
		return nil, fmt.Errorf("reflection: %s", e.GetErrorMessage())
	}
	set := &descriptorpb.FileDescriptorSet{}
	for _, raw := range resp.GetFileDescriptorResponse().GetFileDescriptorProto() {
		fdp := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(raw, fdp); err != nil {
			return nil, fmt.Errorf("reflection: decode file descriptor: %w", err)
		}
		set.File = append(set.File, fdp)
	}
	return filesFromSet(set)
}

// FilesFromDescriptorSet builds a registry from a FileDescriptorSet (e.g.
// `buf build -o x.binpb`), for routes configured without reflection.
func FilesFromDescriptorSet(raw []byte) (*protoregistry.Files, error) {
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, set); err != nil {
		return nil, err
	}
	return filesFromSet(set)
}

// filesFromSet resolves set's files, falling back to the gateway's own
// global registry for well-known dependencies (google/protobuf/*.proto) a
// backend's reflection response may leave out.
func filesFromSet(set *descriptorpb.FileDescriptorSet) (*protoregistry.Files, error) {
	byName := map[string]*descriptorpb.FileDescriptorProto{}
	for _, f := range set.File {
		byName[f.GetName()] = f
	}
	files := new(protoregistry.Files)
	var add func(name string) error
	add = func(name string) error {
		if _, err := files.FindFileByPath(name); err == nil {
			return nil
		}
		fdp, ok := byName[name]
		if !ok {
			global, err := protoregistry.GlobalFiles.FindFileByPath(name)
			if err != nil {
				return fmt.Errorf("missing dependency %s", name)
			}
			return files.RegisterFile(global)
		}
		for _, dep := range fdp.GetDependency() {
			if err := add(dep); err != nil {
				return err
			}
		}
		fd, err := protodesc.NewFile(fdp, files)
		if err != nil {
			return err
		}
		return files.RegisterFile(fd)
	}
	for name := range byName {
		if err := add(name); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// Frame is one message of a forwarded call, kept as raw wire bytes.
type Frame struct {
	payload  []byte
	tenantID func([]byte) string
}

// GetTenantId satisfies authz.TenantIDGetter -- see ExternalBackends.
func (f *Frame) GetTenantId() string {
	if f.tenantID == nil {
		return ""
	}
	return f.tenantID(f.payload)
}

// frameCodec passes *Frame through untouched and hands every other
// message to the regular proto codec, so built-in services on the same
// server are unaffected.
type frameCodec struct{}

func (frameCodec) Name() string { return "proto" }

func (frameCodec) Marshal(v any) (mem.BufferSlice, error) {
	if f, ok := v.(*Frame); ok {
		return mem.BufferSlice{mem.SliceBuffer(f.payload)}, nil
	}
	return encoding.GetCodecV2("proto").Marshal(v)
}

func (frameCodec) Unmarshal(data mem.BufferSlice, v any) error {
	if f, ok := v.(*Frame); ok {
		f.payload = data.Materialize()
		return nil
	}
	return encoding.GetCodecV2("proto").Unmarshal(data, v)
}

// ServerCodec is the codec api-gateway's server must use
// (grpc.ForceServerCodecV2) for ExternalBackends to see raw frames.
func ServerCodec() encoding.CodecV2 { return frameCodec{} }
