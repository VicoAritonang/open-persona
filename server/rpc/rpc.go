// Package rpc serves the OpenPersona API as gRPC services
// (openpersona.v1.PersonaService and openpersona.v1.MemoryService, defined
// in api/proto). It is built on Connect, so one handler speaks three
// protocols on the same port as the HTTP API:
//
//   - gRPC (HTTP/2): any language's generated gRPC client, grpcurl
//   - gRPC-Web: browsers
//   - Connect: plain HTTP/1.1 or HTTP/2 POST with JSON or Protobuf bodies,
//     which also works through serverless platforms that buffer responses
//
// Every RPC calls the same operation of server.Server as the matching HTTP
// endpoint, so behaviour, authorization and errors are identical. The
// standard gRPC health service (grpc.health.v1) and, optionally, server
// reflection are included.
//
//	srv := &server.Server{...}
//	rpc.Mount(srv, rpc.Options{Reflection: true})
//	http.ListenAndServe(":8080", srv.Handler()) // serve HTTP/2 cleartext (h2c) too for plain gRPC
package rpc

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"

	"go.vicoworks.com/open-persona/api/gen/openpersona/v1/openpersonav1connect"
	"go.vicoworks.com/open-persona/server"
)

// Options configures the services.
type Options struct {
	// Reflection serves gRPC server reflection, so tools such as grpcurl
	// and Postman can discover the API without the .proto files. It reveals
	// only the schema, which is public anyway.
	Reflection bool
	// ReadMaxBytes caps a request message (default 8 MiB, as the HTTP API).
	ReadMaxBytes int
	// Interceptors run after authentication, around every RPC: logging,
	// metrics, tracing.
	Interceptors []connect.Interceptor
}

// Handlers returns the gRPC services keyed by the ServeMux pattern each is
// mounted on.
func Handlers(s *server.Server, opt Options) map[string]http.Handler {
	if opt.ReadMaxBytes <= 0 {
		opt.ReadMaxBytes = 8 << 20
	}
	handlerOpts := []connect.HandlerOption{
		connect.WithInterceptors(append([]connect.Interceptor{&authInterceptor{s: s}}, opt.Interceptors...)...),
		connect.WithReadMaxBytes(opt.ReadMaxBytes),
	}
	out := make(map[string]http.Handler)
	add := func(path string, h http.Handler) { out[path] = h }
	add(openpersonav1connect.NewPersonaServiceHandler(&personaService{s: s}, handlerOpts...))
	add(openpersonav1connect.NewMemoryServiceHandler(&memoryService{s: s}, handlerOpts...))
	add(grpchealth.NewHandler(grpchealth.NewStaticChecker(
		openpersonav1connect.PersonaServiceName,
		openpersonav1connect.MemoryServiceName,
	)))
	if opt.Reflection {
		r := grpcreflect.NewStaticReflector(openpersonav1connect.PersonaServiceName, openpersonav1connect.MemoryServiceName)
		add(grpcreflect.NewHandlerV1(r))
		add(grpcreflect.NewHandlerV1Alpha(r))
	}
	return out
}

// Mount adds the gRPC services to s.Mount, so s.Handler serves them next
// to the HTTP API.
func Mount(s *server.Server, opt Options) {
	if s.Mount == nil {
		s.Mount = make(map[string]http.Handler)
	}
	for path, h := range Handlers(s, opt) {
		s.Mount[path] = h
	}
}

// authInterceptor authenticates every RPC with the server's authenticator,
// from "authorization: Bearer <key>" (or "x-api-key") metadata.
type authInterceptor struct{ s *server.Server }

func (a *authInterceptor) authenticate(ctx context.Context, h http.Header) (context.Context, error) {
	p, err := a.s.Authenticate(ctx, server.TokenFrom(h))
	if err != nil {
		return ctx, toConnect(err)
	}
	return server.WithPrincipal(ctx, p), nil
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := a.authenticate(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := a.authenticate(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// codes maps the API's error codes to gRPC status codes.
var codes = map[server.Code]connect.Code{
	server.CodeInvalidArgument:  connect.CodeInvalidArgument,
	server.CodeUnauthenticated:  connect.CodeUnauthenticated,
	server.CodePermissionDenied: connect.CodePermissionDenied,
	server.CodeNotFound:         connect.CodeNotFound,
	server.CodeUnavailable:      connect.CodeUnavailable,
	server.CodeUnimplemented:    connect.CodeUnimplemented,
	server.CodeUpstream:         connect.CodeUnavailable,
	server.CodeInternal:         connect.CodeInternal,
}

func toConnect(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	e := server.AsError(err)
	code, ok := codes[e.Code]
	if !ok {
		code = connect.CodeUnknown
	}
	return connect.NewError(code, errors.New(e.Message))
}
