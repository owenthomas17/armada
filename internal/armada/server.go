package armada

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/nats-io/stan.go"
	"github.com/segmentio/kafka-go"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/G-Research/armada/internal/armada/authorization"
	"github.com/G-Research/armada/internal/armada/configuration"
	"github.com/G-Research/armada/internal/armada/metrics"
	"github.com/G-Research/armada/internal/armada/repository"
	"github.com/G-Research/armada/internal/armada/scheduling"
	"github.com/G-Research/armada/internal/armada/server"
	"github.com/G-Research/armada/internal/common/task"
	"github.com/G-Research/armada/internal/common/util"
	"github.com/G-Research/armada/pkg/api"
)

func Serve(config *configuration.ArmadaConfig) (func(), *sync.WaitGroup) {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	grpcServer := createServer(config)

	taskManager := task.NewBackgroundTaskManager(metrics.MetricPrefix)

	db := createRedisClient(&config.Redis)
	eventsDb := createRedisClient(&config.EventsRedis)

	jobRepository := repository.NewRedisJobRepository(db)
	usageRepository := repository.NewRedisUsageRepository(db)
	queueRepository := repository.NewRedisQueueRepository(db)

	redisEventRepository := repository.NewRedisEventRepository(eventsDb, config.EventRetention)
	var eventStore repository.EventStore

	// TODO: move this to task manager
	stopSubscription := func() {}
	if len(config.EventsKafka.Brokers) > 0 {
		log.Infof("Using Kafka for events (%+v)", config.EventsKafka)
		writer := kafka.NewWriter(kafka.WriterConfig{
			Brokers: config.EventsKafka.Brokers,
			Topic:   config.EventsKafka.Topic,
		})
		reader := kafka.NewReader(kafka.ReaderConfig{
			Brokers:  config.EventsKafka.Brokers,
			GroupID:  config.EventsKafka.ConsumerGroupID,
			Topic:    config.EventsKafka.Topic,
			MaxWait:  500 * time.Millisecond,
			MinBytes: 0,    // 10KB
			MaxBytes: 10e6, // 10MB
		})

		eventStore = repository.NewKafkaEventStore(writer)
		eventProcessor := repository.NewKafkaEventRedisProcessor(reader, redisEventRepository)

		//TODO: Remove this metric, and add one to track event delay
		taskManager.Register(eventProcessor.ProcessEvents, 100*time.Millisecond, "kafka_redis_processor")

	} else if len(config.EventsNats.Servers) > 0 {

		conn, err := stan.Connect(
			config.EventsNats.ClusterID,
			"armada-server-"+util.NewULID(),
			stan.NatsURL(strings.Join(config.EventsNats.Servers, ",")),
		)
		if err != nil {
			panic(err)
		}
		eventStore = repository.NewNatsEventStore(conn, config.EventsNats.Subject)
		eventProcessor := repository.NewNatsEventRedisProcessor(conn, redisEventRepository, config.EventsNats.Subject, config.EventsNats.QueueGroup)
		eventProcessor.Start()

		stopSubscription = func() {
			err := conn.Close()
			if err != nil {
				log.Errorf("failed to close nats connection: %v", err)
			}
		}

	} else {
		eventStore = redisEventRepository
	}

	permissions := authorization.NewPrincipalPermissionChecker(config.PermissionGroupMapping, config.PermissionScopeMapping)

	submitServer := server.NewSubmitServer(permissions, jobRepository, queueRepository, eventStore)
	usageServer := server.NewUsageServer(permissions, config.PriorityHalfTime, usageRepository)
	aggregatedQueueServer := server.NewAggregatedQueueServer(permissions, config.Scheduling, jobRepository, queueRepository, usageRepository, eventStore)
	eventServer := server.NewEventServer(permissions, redisEventRepository, eventStore)
	leaseManager := scheduling.NewLeaseManager(jobRepository, queueRepository, eventStore, config.Scheduling.Lease.ExpireAfter)

	taskManager.Register(leaseManager.ExpireLeases, config.Scheduling.Lease.ExpiryLoopInterval, "lease_expiry")

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", config.GrpcPort))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	metrics.ExposeDataMetrics(queueRepository, jobRepository, usageRepository)

	api.RegisterSubmitServer(grpcServer, submitServer)
	api.RegisterUsageServer(grpcServer, usageServer)
	api.RegisterAggregatedQueueServer(grpcServer, aggregatedQueueServer)
	api.RegisterEventServer(grpcServer, eventServer)

	grpc_prometheus.Register(grpcServer)

	go func() {
		defer log.Println("Stopping server.")

		log.Printf("Grpc listening on %d", config.GrpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}

		wg.Done()
	}()

	return func() {
		stopSubscription()
		taskManager.StopAll(time.Second * 2)
		grpcServer.GracefulStop()
	}, wg
}

func createRedisClient(config *redis.UniversalOptions) redis.UniversalClient {
	return redis.NewUniversalClient(config)
}

func createServer(config *configuration.ArmadaConfig) *grpc.Server {

	unaryInterceptors := []grpc.UnaryServerInterceptor{}
	streamInterceptors := []grpc.StreamServerInterceptor{}

	authServices := []authorization.AuthService{}

	if len(config.BasicAuth.Users) > 0 {
		authServices = append(authServices,
			authorization.NewBasicAuthService(config.BasicAuth.Users))
	}

	if config.OpenIdAuth.ProviderUrl != "" {
		openIdAuthService, err := authorization.NewOpenIdAuthServiceForProvider(context.Background(), &config.OpenIdAuth)
		if err != nil {
			panic(err)
		}
		authServices = append(authServices, openIdAuthService)
	}

	if config.AnonymousAuth {
		authServices = append(authServices, &authorization.AnonymousAuthService{})
	}

	// Kerberos should be the last service as it is adding WWW-Authenticate header for unauthenticated response
	if config.Kerberos.KeytabLocation != "" {
		kerberosAuthService, err := authorization.NewKerberosAuthService(&config.Kerberos)
		if err != nil {
			panic(err)
		}
		authServices = append(authServices, kerberosAuthService)
	}

	authFunction := authorization.CreateMiddlewareAuthFunction(authServices)
	unaryInterceptors = append(unaryInterceptors, grpc_auth.UnaryServerInterceptor(authFunction))
	streamInterceptors = append(streamInterceptors, grpc_auth.StreamServerInterceptor(authFunction))

	grpc_prometheus.EnableHandlingTimeHistogram()
	unaryInterceptors = append(unaryInterceptors, grpc_prometheus.UnaryServerInterceptor)
	streamInterceptors = append(streamInterceptors, grpc_prometheus.StreamServerInterceptor)

	return grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
		}),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(streamInterceptors...)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(unaryInterceptors...)))
}
