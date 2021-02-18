package main

import (
	"context"
	"net/http"
	"strings"

	grpcmw "github.com/grpc-ecosystem/go-grpc-middleware"
	grpczap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpcprom "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/jzelinskie/stringz"
	"github.com/mwitkow/grpc-proxy/proxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

var rootCmd = &cobra.Command{
	Use:   "grpcwebproxy",
	Short: "A proxy that converts grpc-web into grpc.",
	Long:  "A proxy that converts grpc-web into grpc.",
	Run:   rootRun,
}

func main() {
	viper.SetDefault("UPSTREAM_ADDR", "127.0.0.1:50051")
	viper.SetDefault("UPSTREAM_CERT_PATH", "")
	viper.SetDefault("WEB_ADDR", "127.0.0.1:80")
	viper.SetDefault("WEB_KEY_PATH", "")
	viper.SetDefault("WEB_CERT_PATH", "")
	viper.SetDefault("WEB_ALLOWED_ORIGINS", "")
	viper.SetDefault("METRICS_ADDR", "127.0.0.1:9090")
	viper.SetDefault("DEBUG", false)

	rootCmd.Flags().String("upstream-addr", viper.GetString("UPSTREAM_ADDR"), "address of the upstream gRPC service")
	rootCmd.Flags().String("upstream-cert-path", viper.GetString("UPSTREAM_CERT_PATH"), "local path to the TLS certificate of the upstream gRPC service")
	rootCmd.Flags().String("web-addr", viper.GetString("WEB_ADDR"), "address to listen on for grpc-web requests")
	rootCmd.Flags().String("web-key-path", viper.GetString("WEB_KEY_PATH"), "local path to the TLS key of the grpc-web server")
	rootCmd.Flags().String("web-cert-path", viper.GetString("WEB_CERT_PATH"), "local path to the TLS certificate of the grpc-web server")
	rootCmd.Flags().String("web-allowed-origins", viper.GetString("WEB_ALLOWED_ORIGINS"), "CORS allowed origins for grpc-web (comma-separated, defaults to all)")
	rootCmd.Flags().String("metrics-addr", viper.GetString("METRICS_ADDR"), "address to listen on for the metrics server")
	rootCmd.Flags().Bool("debug", viper.GetBool("debug"), "debug log verbosity")

	viper.BindPFlag("UPSTREAM_ADDR", rootCmd.Flags().Lookup("upstream-addr"))
	viper.BindPFlag("UPSTREAM_CERT_PATH", rootCmd.Flags().Lookup("upstream-cert-path"))
	viper.BindPFlag("WEB_ADDR", rootCmd.Flags().Lookup("web-addr"))
	viper.BindPFlag("WEB_KEY_PATH", rootCmd.Flags().Lookup("web-key-path"))
	viper.BindPFlag("WEB_CERT_PATH", rootCmd.Flags().Lookup("web-cert-path"))
	viper.BindPFlag("WEB_ALLOWED_ORIGINS", rootCmd.Flags().Lookup("web-allowed-origins"))
	viper.BindPFlag("METRICS_ADDR", rootCmd.Flags().Lookup("metrics-addr"))
	viper.BindPFlag("DEBUG", rootCmd.Flags().Lookup("debug"))

	viper.SetEnvPrefix("GRPCWEBPROXY")
	viper.AutomaticEnv()

	rootCmd.Execute()
}

func rootRun(cmd *cobra.Command, args []string) {
	logger, _ := zap.NewProduction()
	if viper.GetBool("debug") {
		logger, _ = zap.NewDevelopment()
	}
	defer logger.Sync()

	upstream, err := NewUpstreamConnection(viper.GetString("UPSTREAM_ADDR"), viper.GetString("UPSTREAM_CERT_PATH"))
	if err != nil {
		logger.Fatal("failed to connect to upstream", zap.String("error", err.Error()))
	}

	srv, err := NewGrpcProxyServer(logger, upstream)
	if err != nil {
		logger.Fatal("failed to init grpc server", zap.String("error", err.Error()))
	}

	origins := strings.Split(viper.GetString("WEB_ALLOWED_ORIGINS"), ",")
	grpcwebsrv, err := NewGrpcWebServer(srv, origins)
	if err != nil {
		logger.Fatal("failed to init grpcweb server", zap.String("error", err.Error()))
	}

	go func() {
		certPath := viper.GetString("WEB_CERT_PATH")
		keyPath := viper.GetString("WEB_KEY_PATH")
		websrv := &http.Server{
			Addr:    viper.GetString("WEB_ADDR"),
			Handler: grpcwebsrv,
		}

		if certPath != "" && keyPath != "" {
			logger.Info(
				"grpc-web server listening over HTTPS",
				zap.String("addr", viper.GetString("WEB_ADDR")),
				zap.String("certPath", certPath),
				zap.String("keyPath", keyPath),
			)
			websrv.ListenAndServeTLS(certPath, keyPath)
		} else {
			logger.Info(
				"grpc-web server listening over HTTP",
				zap.String("addr", viper.GetString("WEB_ADDR")),
			)
			websrv.ListenAndServe()
		}
	}()

	logger.Info("metrics server listening over HTTP", zap.String("addr", viper.GetString("METRICS_ADDR")))
	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(viper.GetString("METRICS_ADDR"), nil)
}

func NewGrpcWebServer(srv *grpc.Server, allowedOrigins []string) (*grpcweb.WrappedGrpcServer, error) {
	return grpcweb.WrapServer(srv,
		grpcweb.WithCorsForRegisteredEndpointsOnly(false),
		grpcweb.WithOriginFunc(NewAllowedOriginsFunc(allowedOrigins)),
	), nil
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
