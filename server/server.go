package server

import (
	"context"
	"net"
	"strings"
	"time"

	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	ilogger "github.com/meateam/elasticsearch-logger"
	"github.com/meateam/permission-service/permission"
	pb "github.com/meateam/permission-service/proto"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

const (
	configPort                         = "port"
	configHealthCheckInterval          = "health_check_interval"
	configMongoConnectionString        = "mongo_host"
	configMongoClientConnectionTimeout = "mongo_client_connection_timeout"
	configMongoClientPingTimeout       = "mongo_client_ping_timeout"
	configElasticAPMIgnoreURLS         = "elastic_apm_ignore_urls"
)

func init() {
	viper.SetDefault(configPort, "8080")
	viper.SetDefault(configHealthCheckInterval, 3)
	viper.SetDefault(configElasticAPMIgnoreURLS, "/grpc.health.v1.Health/Check")
	viper.SetDefault(configMongoConnectionString, "mongodb://localhost:27017")
	viper.SetDefault(configMongoClientConnectionTimeout, 10)
	viper.SetDefault(configMongoClientPingTimeout, 10)
	viper.AutomaticEnv()
}

// PermissionServer is a structure that holds the permission grpc server
// and its services and configuration.
type PermissionServer struct {
	*grpc.Server
	logger              *logrus.Logger
	port                string
	healthCheckInterval int
	permissionService   *permission.Service
}

// Serve accepts incoming connections on the listener `lis`, creating a new
// ServerTransport and service goroutine for each. The service goroutines
// read gRPC requests and then call the registered handlers to reply to them.
// Serve returns when `lis.Accept` fails with fatal errors. `lis` will be closed when
// this method returns.
// If `lis` is nil then Serve creates a `net.Listener` with "tcp" network listening
// on the configured `TCP_PORT`, which defaults to "8080".
// Serve will return a non-nil error unless Stop or GracefulStop is called.
func (s PermissionServer) Serve(lis net.Listener) {
	listener := lis
	if lis == nil {
		l, err := net.Listen("tcp", ":"+s.port)
		if err != nil {
			s.logger.Fatalf("failed to listen: %v", err)
		}

		listener = l
	}

	s.logger.Infof("listening and serving grpc server on port %s", s.port)
	if err := s.Server.Serve(listener); err != nil {
		s.logger.Fatalf(err.Error())
	}
}

// NewServer configures and creates a grpc.Server instance with the download service
// health check service.
// Configure using environment variables.
// `HEALTH_CHECK_INTERVAL`: Interval to update serving state of the health check server.
// `PORT`: TCP port on which the grpc server would serve on.
func NewServer(logger *logrus.Logger) *PermissionServer {
	// If no logger is given, create a new default logger for the server.
	if logger == nil {
		logger = ilogger.NewLogger()
	}

	// Create mongodb client.
	connectionString := viper.GetString(configMongoConnectionString)
	mongoOptions := options.Client().ApplyURI(connectionString)
	mongoClient, err := mongo.NewClient(mongoOptions)
	if err != nil {
		logger.Fatalf("failed creating mongodb client with connection string %s: %v", connectionString, err.Error())
	}

	// Connect client to mongodb.
	mongoClientConnectionTimout := viper.GetDuration(configMongoClientConnectionTimeout)
	connectionTimeoutCtx, cancelConn := context.WithTimeout(context.TODO(), mongoClientConnectionTimout*time.Second)
	defer cancelConn()
	err = mongoClient.Connect(connectionTimeoutCtx)
	if err != nil {
		logger.Fatalf("failed connecting to mongodb with connection string %s: %v", connectionString, err.Error())
	}

	// Check the connection.
	mongoClientPingTimeout := viper.GetDuration(configMongoClientPingTimeout)
	pingTimeoutCtx, cancelPing := context.WithTimeout(context.TODO(), mongoClientPingTimeout*time.Second)
	defer cancelPing()
	err = mongoClient.Ping(pingTimeoutCtx, readpref.Primary())
	if err != nil {
		logger.Fatalf("failed pinging to mongodb with connection string %s: %v", connectionString, err.Error())
	}
	logger.Infof("connected to mongodb with connection string %s", connectionString)

	// Set up grpc server opts with logger interceptor.
	serverOpts := append(
		serverLoggerInterceptor(logger),
		grpc.MaxRecvMsgSize(16<<20),
	)

	// Create a new grpc server.
	grpcServer := grpc.NewServer(
		serverOpts...,
	)

	// Create a download service and register it on the grpc server.
	permissionService := permission.NewService(mongoClient, logger)
	pb.RegisterPermissionServer(grpcServer, permissionService)

	// Create a health server and register it on the grpc server.
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	permissionServer := &PermissionServer{
		Server:              grpcServer,
		logger:              logger,
		port:                viper.GetString(configPort),
		healthCheckInterval: viper.GetInt(configHealthCheckInterval),
		permissionService:   permissionService,
	}

	// Health check validation goroutine worker.
	go permissionServer.healthCheckWorker(healthServer)

	return permissionServer
}

// serverLoggerInterceptor configures the logger interceptor for the download server.
func serverLoggerInterceptor(logger *logrus.Logger) []grpc.ServerOption {
	// Create new logrus entry for logger interceptor.
	logrusEntry := logrus.NewEntry(logger)

	ignorePayload := ilogger.IgnoreServerMethodsDecider(
		append(
			strings.Split(viper.GetString(configElasticAPMIgnoreURLS), ","),
		)...,
	)

	ignoreInitialRequest := ilogger.IgnoreServerMethodsDecider(
		strings.Split(viper.GetString(configElasticAPMIgnoreURLS), ",")...,
	)

	// Shared options for the logger, with a custom gRPC code to log level function.
	loggerOpts := []grpc_logrus.Option{
		grpc_logrus.WithDecider(func(fullMethodName string, err error) bool {
			return ignorePayload(fullMethodName)
		}),
		grpc_logrus.WithLevels(grpc_logrus.DefaultCodeToLevel),
	}

	return ilogger.ElasticsearchLoggerServerInterceptor(
		logrusEntry,
		ignorePayload,
		ignoreInitialRequest,
		loggerOpts...,
	)
}

// healthCheckWorker is running an infinite loop that sets the serving status once
// in s.healthCheckInterval seconds.
func (s PermissionServer) healthCheckWorker(healthServer *health.Server) {
	mongoClientPingTimeout := viper.GetDuration(configMongoClientPingTimeout)
	for {
		if s.permissionService.HealthCheck(mongoClientPingTimeout * time.Second) {
			healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		} else {
			healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		}

		time.Sleep(time.Second * time.Duration(s.healthCheckInterval))
	}
}