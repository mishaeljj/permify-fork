package servers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/ratelimit"

	grpcAuth "github.com/grpc-ecosystem/go-grpc-middleware/auth"

	grpcRecovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpcValidator "github.com/grpc-ecosystem/go-grpc-middleware/validator"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/rs/cors"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/encoding/protojson"

	health "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/Permify/permify/internal/authn/oidc"
	"github.com/Permify/permify/internal/authn/preshared"
	"github.com/Permify/permify/internal/config"
	"github.com/Permify/permify/internal/invoke"
	"github.com/Permify/permify/internal/middleware"
	"github.com/Permify/permify/internal/storage"
	grpcV1 "github.com/Permify/permify/pkg/pb/base/v1"
)

var tracer = otel.Tracer("servers")

// Container is a struct that holds the invoker and various storage storage
// for permission-related operations. It serves as a central point of access
// for interacting with the underlying data and services.
type Container struct {
	// Invoker for performing permission-related operations
	Invoker invoke.Invoker
	// DataReader for reading data from storage
	DR storage.DataReader
	// DataWriter for writing data to storage
	DW storage.DataWriter
	// SchemaReader for reading schemas from storage
	SR storage.SchemaReader
	// SchemaWriter for writing schemas to storage
	SW storage.SchemaWriter
	// TenantReader for reading tenant information from storage
	TR storage.TenantReader
	// TenantWriter for writing tenant information to storage
	TW storage.TenantWriter

	W storage.Watcher
}

// NewContainer is a constructor for the Container struct.
// It takes an Invoker, RelationshipReader, RelationshipWriter, SchemaReader, SchemaWriter,
// TenantReader, and TenantWriter as arguments, and returns a pointer to a Container instance.
func NewContainer(
	invoker invoke.Invoker,
	dr storage.DataReader,
	dw storage.DataWriter,
	sr storage.SchemaReader,
	sw storage.SchemaWriter,
	tr storage.TenantReader,
	tw storage.TenantWriter,
	w storage.Watcher,
) *Container {
	return &Container{
		Invoker: invoker,
		DR:      dr,
		DW:      dw,
		SR:      sr,
		SW:      sw,
		TR:      tr,
		TW:      tw,
		W:       w,
	}
}

// Run is a method that starts the Container and its services, including the gRPC server,
// an optional HTTP server, and an optional profiler server. It also sets up authentication,
// TLS configurations, and interceptors as needed.
func (s *Container) Run(
	ctx context.Context,
	srv *config.Server,
	dst *config.Distributed,
	authentication *config.Authn,
	profiler *config.Profiler,
	localInvoker invoke.Invoker,
) error {
	var err error

	limiter := middleware.NewRateLimiter(srv.RateLimit) // for example 1000 req/sec

	unaryInterceptors := []grpc.UnaryServerInterceptor{
		grpcValidator.UnaryServerInterceptor(),
		grpcRecovery.UnaryServerInterceptor(),
		ratelimit.UnaryServerInterceptor(limiter),
	}

	streamingInterceptors := []grpc.StreamServerInterceptor{
		grpcValidator.StreamServerInterceptor(),
		grpcRecovery.StreamServerInterceptor(),
		ratelimit.StreamServerInterceptor(limiter),
	}

	// Configure authentication based on the provided method ("preshared" or "oidc").
	// Add the appropriate interceptors to the unary and streaming interceptors.
	if authentication != nil && authentication.Enabled {
		switch authentication.Method {
		case "preshared":
			var authenticator *preshared.KeyAuthn
			authenticator, err = preshared.NewKeyAuthn(ctx, authentication.Preshared)
			if err != nil {
				return err
			}
			unaryInterceptors = append(unaryInterceptors, grpcAuth.UnaryServerInterceptor(middleware.KeyAuthFunc(authenticator)))
			streamingInterceptors = append(streamingInterceptors, grpcAuth.StreamServerInterceptor(middleware.KeyAuthFunc(authenticator)))
		case "oidc":
			var authenticator *oidc.Authn
			authenticator, err = oidc.NewOidcAuthn(ctx, authentication.Oidc)
			if err != nil {
				return err
			}
			unaryInterceptors = append(unaryInterceptors, oidc.UnaryServerInterceptor(authenticator))
			streamingInterceptors = append(streamingInterceptors, oidc.StreamServerInterceptor(authenticator))
		default:
			return fmt.Errorf("unknown authentication method: '%s'", authentication.Method)
		}
	}

	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		grpc.ChainStreamInterceptor(streamingInterceptors...),
	}

	if srv.GRPC.TLSConfig.Enabled {
		var c credentials.TransportCredentials
		c, err = credentials.NewServerTLSFromFile(srv.GRPC.TLSConfig.CertPath, srv.GRPC.TLSConfig.KeyPath)
		if err != nil {
			return err
		}
		opts = append(opts, grpc.Creds(c))
	}

	// Create a new gRPC server instance with the provided options.
	grpcServer := grpc.NewServer(opts...)

	// Register various gRPC services to the server.
	grpcV1.RegisterPermissionServer(grpcServer, NewPermissionServer(s.Invoker))
	grpcV1.RegisterSchemaServer(grpcServer, NewSchemaServer(s.SW, s.SR))
	grpcV1.RegisterDataServer(grpcServer, NewDataServer(s.DR, s.DW, s.SR))
	grpcV1.RegisterTenancyServer(grpcServer, NewTenancyServer(s.TR, s.TW))
	grpcV1.RegisterWatchServer(grpcServer, NewWatchServer(s.W, s.DR))

	// Register health check and reflection services for gRPC.
	health.RegisterHealthServer(grpcServer, NewHealthServer())
	reflection.Register(grpcServer)

	// Create another gRPC server, presumably for invoking permissions.
	invokeServer := grpc.NewServer(opts...)
	grpcV1.RegisterPermissionServer(invokeServer, NewPermissionServer(localInvoker))

	// Register health check and reflection services for the invokeServer.
	health.RegisterHealthServer(invokeServer, NewHealthServer())
	reflection.Register(invokeServer)

	// If profiling is enabled, set up the profiler using the net/http package.
	if profiler.Enabled {
		// Create a new HTTP ServeMux to register pprof routes.
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		// Run the profiler server in a separate goroutine.
		go func() {
			// Log a message indicating the profiler server's start status and port.
			slog.Info(fmt.Sprintf("🚀 profiler server successfully started: %s", profiler.Port))

			// Define the HTTP server with timeouts and the mux handler for pprof routes.
			pprofserver := &http.Server{
				Addr:         ":" + profiler.Port,
				Handler:      mux,
				ReadTimeout:  20 * time.Second,
				WriteTimeout: 20 * time.Second,
				IdleTimeout:  15 * time.Second,
			}

			// Start the profiler server.
			if err := pprofserver.ListenAndServe(); err != nil {
				// Check if the error was due to the server being closed, and log it.
				if errors.Is(err, http.ErrServerClosed) {
					slog.Error("failed to start profiler", err)
				}
			}
		}()
	}

	var lis net.Listener
	lis, err = net.Listen("tcp", ":"+srv.GRPC.Port)
	if err != nil {
		return err
	}

	var invokeLis net.Listener
	invokeLis, err = net.Listen("tcp", ":"+dst.Port)
	if err != nil {
		return err
	}

	// Start the gRPC server.
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("failed to start grpc server", err)
		}
	}()

	go func() {
		if err := invokeServer.Serve(invokeLis); err != nil {
			slog.Error("failed to start invoke grpc server", err)
		}
	}()

	slog.Info(fmt.Sprintf("🚀 grpc server successfully started: %s", srv.GRPC.Port))
	slog.Info(fmt.Sprintf("🚀 invoker grpc server successfully started: %s", dst.Port))

	var httpServer *http.Server

	// Start the optional HTTP server with CORS and optional TLS configurations.
	// Connect to the gRPC server and register the HTTP handlers for each service.
	if srv.HTTP.Enabled {
		options := []grpc.DialOption{
			grpc.WithBlock(),
			grpc.WithUnaryInterceptor(otelgrpc.UnaryClientInterceptor()),
		}
		if srv.GRPC.TLSConfig.Enabled {
			c, err := credentials.NewClientTLSFromFile(srv.GRPC.TLSConfig.CertPath, "")
			if err != nil {
				return err
			}
			options = append(options, grpc.WithTransportCredentials(c))
		} else {
			options = append(options, grpc.WithTransportCredentials(insecure.NewCredentials()))
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		conn, err := grpc.DialContext(timeoutCtx, ":"+srv.GRPC.Port, options...)
		if err != nil {
			return err
		}
		defer func() {
			if err = conn.Close(); err != nil {
				slog.Error("Failed to close gRPC connection: %v", err)
			}
		}()

		healthClient := health.NewHealthClient(conn)
		muxOpts := []runtime.ServeMuxOption{
			runtime.WithHealthzEndpoint(healthClient),
			runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.HTTPBodyMarshaler{
				Marshaler: &runtime.JSONPb{
					MarshalOptions: protojson.MarshalOptions{
						UseProtoNames:   true,
						EmitUnpopulated: true,
					},
					UnmarshalOptions: protojson.UnmarshalOptions{
						DiscardUnknown: true,
					},
				},
			}),
		}

		mux := runtime.NewServeMux(muxOpts...)

		if err = grpcV1.RegisterPermissionHandler(ctx, mux, conn); err != nil {
			return err
		}
		if err = grpcV1.RegisterSchemaHandler(ctx, mux, conn); err != nil {
			return err
		}
		if err = grpcV1.RegisterDataHandler(ctx, mux, conn); err != nil {
			return err
		}
		if err = grpcV1.RegisterTenancyHandler(ctx, mux, conn); err != nil {
			return err
		}

		httpServer = &http.Server{
			Addr: ":" + srv.HTTP.Port,
			Handler: cors.New(cors.Options{
				AllowCredentials: true,
				AllowedOrigins:   srv.HTTP.CORSAllowedOrigins,
				AllowedHeaders:   srv.HTTP.CORSAllowedHeaders,
				AllowedMethods: []string{
					http.MethodGet, http.MethodPost,
					http.MethodHead, http.MethodPatch, http.MethodDelete, http.MethodPut,
				},
			}).Handler(mux),
			ReadHeaderTimeout: 5 * time.Second,
		}

		// Start the HTTP server with TLS if enabled, otherwise without TLS.
		go func() {
			var err error
			if srv.HTTP.TLSConfig.Enabled {
				err = httpServer.ListenAndServeTLS(srv.HTTP.TLSConfig.CertPath, srv.HTTP.TLSConfig.KeyPath)
			} else {
				err = httpServer.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				slog.Error(err.Error())
			}
		}()

		slog.Info(fmt.Sprintf("🚀 http server successfully started: %s", srv.HTTP.Port))
	}

	// Wait for the context to be canceled (e.g., due to a signal).
	<-ctx.Done()

	// Shutdown the servers gracefully.
	ctxShutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if httpServer != nil {
		if err := httpServer.Shutdown(ctxShutdown); err != nil {
			slog.Error(err.Error())
			return err
		}
	}

	// Gracefully stop the gRPC server.
	grpcServer.GracefulStop()
	invokeServer.GracefulStop()

	slog.Info("gracefully shutting down")

	return nil
}
