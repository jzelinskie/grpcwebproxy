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
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	viper.SetEnvPrefix("GRPCWEBPROXY")
	viper.AutomaticEnv()

	viper.SetDefault("UPSTREAM_ADDR", "127.0.0.1:50051")
	viper.SetDefault("UPSTREAM_CERT_PATH", "")
	viper.SetDefault("WEB_ALLOWED_ORIGINS", "")
	viper.SetDefault("WEB_ADDR", "127.0.0.1:80")
	viper.SetDefault("WEB_CERT_PATH", "")
	viper.SetDefault("WEB_KEY_PATH", "")
	viper.SetDefault("DEBUG_ADDR", "127.0.0.1:9090")

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
			websrv.ListenAndServeTLS(certPath, keyPath)
		} else {
			websrv.ListenAndServe()
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(viper.GetString("DEBUG_ADDR"), nil)
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
