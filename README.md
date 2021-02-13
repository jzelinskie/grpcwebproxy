# grpcwebproxy

This is a fork of [Improbable's grpcwebproxy][grpcwebproxy].

Alterations include:

- Less than 1/3 the LoC
- Manages dependencies with Go modules
- Dropped support for websockets
- Logs with [Zap] instead of [Logrus]
- Uses [Viper] configuration
- Separate server for serving [Prometheus] metrics

[grpcwebproxy]: https://github.com/improbable-eng/grpc-web/tree/master/go/grpcwebproxy
[Zap]: https://github.com/uber-go/zap
[Logrus]: https://github.com/sirupsen/logrus
[Viper]: https://github.com/spf13/viper
[Prometheus]: https://prometheus.io
