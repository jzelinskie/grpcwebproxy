package main

import (
	"context"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"

	grpcmw "github.com/grpc-ecosystem/go-grpc-middleware"
	grpczap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpcprom "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/jzelinskie/cobrautil"
	"github.com/jzelinskie/stringz"
	"github.com/mwitkow/grpc-proxy/proxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func main() {
	var rootCmd = &cobra.Command{
		Use:               "grpcwebproxy",
		Short:             "A proxy that converts grpc-web into grpc.",
		Long:              "A proxy that converts grpc-web into grpc.",
		PersistentPreRunE: cobrautil.SyncViperPreRunE("GRPCWEBPROXY"),
		Run:               rootRun,
	}

	rootCmd.Flags().String("upstream-addr", "127.0.0.1:50051", "address of the upstream gRPC service")
	rootCmd.Flags().String("upstream-cert-path", "", "local path to the TLS certificate of the upstream gRPC service")
	rootCmd.Flags().String("web-addr", ":80", "address to listen on for grpc-web requests")
	rootCmd.Flags().String("web-key-path", "", "local path to the TLS key of the grpc-web server")
	rootCmd.Flags().String("web-cert-path", "", "local path to the TLS certificate of the grpc-web server")
	rootCmd.Flags().String("web-allowed-origins", "", "CORS allowed origins for grpc-web (comma-separated)")
	rootCmd.Flags().String("metrics-addr", ":9090", "address to listen on for the metrics server")
	rootCmd.Flags().Bool("debug", false, "debug log verbosity")

	rootCmd.Execute()
}

func rootRun(cmd *cobra.Command, args []string) {
	logger, _ := zap.NewProduction()
	if cobrautil.MustGetBool(cmd, "debug") {
		logger, _ = zap.NewDevelopment()
	}
	defer logger.Sync()

	upstream, err := NewUpstreamConnection(
		cobrautil.MustGetString(cmd, "upstream-addr"),
		cobrautil.MustGetStringExpanded(cmd, "upstream-cert-path"),
	)
	if err != nil {
		logger.Fatal("failed to connect to upstream", zap.String("error", err.Error()))
	}

	srv, err := NewGrpcProxyServer(logger, upstream)
	if err != nil {
		logger.Fatal("failed to init grpc server", zap.String("error", err.Error()))
	}

	origins := strings.Split(cobrautil.MustGetString(cmd, "web-allowed-origins"), ",")
	grpcwebsrv := NewGrpcWebServer(srv, cobrautil.MustGetString(cmd, "web-addr"), origins)
	go func() {
		ListenMaybeTLS(
			logger,
			grpcwebsrv,
			cobrautil.MustGetStringExpanded(cmd, "web-cert-path"),
			cobrautil.MustGetStringExpanded(cmd, "web-key-path"),
		)
	}()

	metricsrv := NewMetricsServer(cobrautil.MustGetString(cmd, "metrics-addr"))
	go func() {
		if err := metricsrv.ListenAndServe(); err != http.ErrServerClosed {
			logger.Fatal("failed while serving metrics", zap.Error(err))
		}
	}()

	signalctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)
	for {
		select {
		case <-signalctx.Done():
			if err := grpcwebsrv.Close(); err != nil {
				logger.Fatal("failed while shutting down metrics server", zap.Error(err))
			}
			if err := metricsrv.Close(); err != nil {
				logger.Fatal("failed while shutting down metrics server", zap.Error(err))
			}
			return
		}
	}
}

func NewMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return &http.Server{
		Addr:    addr,
		Handler: mux,
	}
}

func ListenMaybeTLS(logger *zap.Logger, srv *http.Server, certPath, keyPath string) {
	if certPath != "" && keyPath != "" {
		logger.Info(
			"grpc-web server listening over HTTPS",
			zap.String("addr", srv.Addr),
			zap.String("certPath", certPath),
			zap.String("keyPath", keyPath),
		)
		srv.ListenAndServeTLS(certPath, keyPath)
	} else {
		logger.Info(
			"grpc-web server listening over HTTP",
			zap.String("addr", srv.Addr),
		)
		srv.ListenAndServe()
	}
}

func NewGrpcWebServer(srv *grpc.Server, addr string, allowedOrigins []string) *http.Server {
	return &http.Server{
		Addr: addr,
		Handler: grpcweb.WrapServer(srv,
			grpcweb.WithCorsForRegisteredEndpointsOnly(false),
			grpcweb.WithOriginFunc(NewAllowedOriginsFunc(allowedOrigins)),
		),
	}
}

func NewGrpcProxyServer(logger *zap.Logger, upstream *grpc.ClientConn) (*grpc.Server, error) {
	grpc.EnableTracing = true
	grpczap.ReplaceGrpcLogger(logger)

	// If the connection header is present in the request from the web client,
	// the actual connection to the backend will not be established.
	// https://github.com/improbable-eng/grpc-web/issues/568
	director := func(ctx context.Context, _ string) (context.Context, *grpc.ClientConn, error) {
		metadataIn, _ := metadata.FromIncomingContext(ctx)
		md := metadataIn.Copy()
		delete(md, "user-agent")
		delete(md, "connection")
		return metadata.NewOutgoingContext(ctx, md), upstream, nil
	}

	return grpc.NewServer(
		grpc.CustomCodec(proxy.Codec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
		grpcmw.WithUnaryServerChain(
			grpczap.UnaryServerInterceptor(logger),
			grpcprom.UnaryServerInterceptor,
		),
		grpcmw.WithStreamServerChain(
			grpczap.StreamServerInterceptor(logger),
			grpcprom.StreamServerInterceptor,
		),
	), nil
}

func NewUpstreamConnection(addr string, certPath string) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption
	if certPath != "" {
		creds, err := credentials.NewClientTLSFromFile(certPath, "")
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	opts = append(opts, grpc.WithCodec(proxy.Codec()))
	return grpc.Dial(addr, opts...)
}

func NewAllowedOriginsFunc(urls []string) func(string) bool {
	if stringz.SliceEqual(urls, []string{""}) {
		return func(string) bool {
			return true
		}
	}

	return func(origin string) bool {
		return stringz.SliceContains(urls, origin)
	}
}
