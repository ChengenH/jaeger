// Copyright (c) 2019,2020 The Jaeger Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jaegertracing/jaeger/cmd/internal/flags"
	"github.com/jaegertracing/jaeger/cmd/query/app/querysvc"
	"github.com/jaegertracing/jaeger/internal/grpctest"
	"github.com/jaegertracing/jaeger/model"
	"github.com/jaegertracing/jaeger/pkg/config/tlscfg"
	"github.com/jaegertracing/jaeger/pkg/healthcheck"
	"github.com/jaegertracing/jaeger/pkg/jtracer"
	"github.com/jaegertracing/jaeger/pkg/tenancy"
	"github.com/jaegertracing/jaeger/ports"
	"github.com/jaegertracing/jaeger/proto-gen/api_v2"
	depsmocks "github.com/jaegertracing/jaeger/storage/dependencystore/mocks"
	spanstoremocks "github.com/jaegertracing/jaeger/storage/spanstore/mocks"
)

var testCertKeyLocation = "../../../pkg/config/tlscfg/testdata"

func TestServerError(t *testing.T) {
	srv := &Server{
		queryOptions: &QueryOptions{
			HTTPHostPort: ":-1",
		},
	}
	require.Error(t, srv.Start())
}

func TestCreateTLSServerSinglePortError(t *testing.T) {
	// When TLS is enabled, and the host-port of both servers are the same, this leads to error, as TLS-enabled server is required to run on dedicated port.
	tlsCfg := tlscfg.Options{
		Enabled:      true,
		CertPath:     testCertKeyLocation + "/example-server-cert.pem",
		KeyPath:      testCertKeyLocation + "/example-server-key.pem",
		ClientCAPath: testCertKeyLocation + "/example-CA-cert.pem",
	}

	_, err := NewServer(zaptest.NewLogger(t), healthcheck.New(), &querysvc.QueryService{}, nil,
		&QueryOptions{HTTPHostPort: ":8080", GRPCHostPort: ":8080", TLSGRPC: tlsCfg, TLSHTTP: tlsCfg},
		tenancy.NewManager(&tenancy.Options{}), jtracer.NoOp())
	require.Error(t, err)
}

func TestCreateTLSGrpcServerError(t *testing.T) {
	tlsCfg := tlscfg.Options{
		Enabled:      true,
		CertPath:     "invalid/path",
		KeyPath:      "invalid/path",
		ClientCAPath: "invalid/path",
	}

	_, err := NewServer(zaptest.NewLogger(t), healthcheck.New(), &querysvc.QueryService{}, nil,
		&QueryOptions{HTTPHostPort: ":8080", GRPCHostPort: ":8081", TLSGRPC: tlsCfg},
		tenancy.NewManager(&tenancy.Options{}), jtracer.NoOp())
	require.Error(t, err)
}

func TestCreateTLSHttpServerError(t *testing.T) {
	tlsCfg := tlscfg.Options{
		Enabled:      true,
		CertPath:     "invalid/path",
		KeyPath:      "invalid/path",
		ClientCAPath: "invalid/path",
	}

	_, err := NewServer(zaptest.NewLogger(t), healthcheck.New(), &querysvc.QueryService{}, nil,
		&QueryOptions{HTTPHostPort: ":8080", GRPCHostPort: ":8081", TLSHTTP: tlsCfg},
		tenancy.NewManager(&tenancy.Options{}), jtracer.NoOp())
	require.Error(t, err)
}

var testCases = []struct {
	name              string
	TLS               tlscfg.Options
	HTTPTLSEnabled    bool
	GRPCTLSEnabled    bool
	clientTLS         tlscfg.Options
	expectError       bool
	expectClientError bool
	expectServerFail  bool
}{
	{
		// this is a cross test for the "dedicated ports" use case without TLS
		name:           "Should pass with insecure connection",
		HTTPTLSEnabled: false,
		GRPCTLSEnabled: false,
		TLS: tlscfg.Options{
			Enabled: false,
		},
		clientTLS: tlscfg.Options{
			Enabled: false,
		},
		expectError:       false,
		expectClientError: false,
		expectServerFail:  false,
	},
	{
		name:           "should fail with TLS client to untrusted TLS server",
		HTTPTLSEnabled: true,
		GRPCTLSEnabled: true,
		TLS: tlscfg.Options{
			Enabled:  true,
			CertPath: testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:  testCertKeyLocation + "/example-server-key.pem",
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			ServerName: "example.com",
		},
		expectError:       true,
		expectClientError: true,
		expectServerFail:  false,
	},
	{
		name:           "should fail with TLS client to trusted TLS server with incorrect hostname",
		HTTPTLSEnabled: true,
		GRPCTLSEnabled: true,
		TLS: tlscfg.Options{
			Enabled:  true,
			CertPath: testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:  testCertKeyLocation + "/example-server-key.pem",
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			CAPath:     testCertKeyLocation + "/example-CA-cert.pem",
			ServerName: "nonEmpty",
		},
		expectError:       true,
		expectClientError: true,
		expectServerFail:  false,
	},
	{
		name:           "should pass with TLS client to trusted TLS server with correct hostname",
		HTTPTLSEnabled: true,
		GRPCTLSEnabled: true,
		TLS: tlscfg.Options{
			Enabled:  true,
			CertPath: testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:  testCertKeyLocation + "/example-server-key.pem",
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			CAPath:     testCertKeyLocation + "/example-CA-cert.pem",
			ServerName: "example.com",
		},
		expectError:       false,
		expectClientError: false,
		expectServerFail:  false,
	},
	{
		name:           "should fail with TLS client without cert to trusted TLS server requiring cert",
		HTTPTLSEnabled: true,
		GRPCTLSEnabled: true,
		TLS: tlscfg.Options{
			Enabled:      true,
			CertPath:     testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:      testCertKeyLocation + "/example-server-key.pem",
			ClientCAPath: testCertKeyLocation + "/example-CA-cert.pem",
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			CAPath:     testCertKeyLocation + "/example-CA-cert.pem",
			ServerName: "example.com",
		},
		expectError:       false,
		expectServerFail:  false,
		expectClientError: true,
	},
	{
		name:           "should pass with TLS client with cert to trusted TLS server requiring cert",
		HTTPTLSEnabled: true,
		GRPCTLSEnabled: true,
		TLS: tlscfg.Options{
			Enabled:      true,
			CertPath:     testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:      testCertKeyLocation + "/example-server-key.pem",
			ClientCAPath: testCertKeyLocation + "/example-CA-cert.pem",
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			CAPath:     testCertKeyLocation + "/example-CA-cert.pem",
			ServerName: "example.com",
			CertPath:   testCertKeyLocation + "/example-client-cert.pem",
			KeyPath:    testCertKeyLocation + "/example-client-key.pem",
		},
		expectError:       false,
		expectServerFail:  false,
		expectClientError: false,
	},
	{
		name:           "should fail with TLS client without cert to trusted TLS server requiring cert from a different CA",
		HTTPTLSEnabled: true,
		GRPCTLSEnabled: true,
		TLS: tlscfg.Options{
			Enabled:      true,
			CertPath:     testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:      testCertKeyLocation + "/example-server-key.pem",
			ClientCAPath: testCertKeyLocation + "/wrong-CA-cert.pem", // NB: wrong CA
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			CAPath:     testCertKeyLocation + "/example-CA-cert.pem",
			ServerName: "example.com",
			CertPath:   testCertKeyLocation + "/example-client-cert.pem",
			KeyPath:    testCertKeyLocation + "/example-client-key.pem",
		},
		expectError:       false,
		expectServerFail:  false,
		expectClientError: true,
	},
	{
		name:           "should pass with TLS client with cert to trusted TLS HTTP server requiring cert and insecure GRPC server",
		HTTPTLSEnabled: true,
		GRPCTLSEnabled: false,
		TLS: tlscfg.Options{
			Enabled:      true,
			CertPath:     testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:      testCertKeyLocation + "/example-server-key.pem",
			ClientCAPath: testCertKeyLocation + "/example-CA-cert.pem",
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			CAPath:     testCertKeyLocation + "/example-CA-cert.pem",
			ServerName: "example.com",
			CertPath:   testCertKeyLocation + "/example-client-cert.pem",
			KeyPath:    testCertKeyLocation + "/example-client-key.pem",
		},
		expectError:       false,
		expectServerFail:  false,
		expectClientError: false,
	},
	{
		name:           "should pass with TLS client with cert to trusted GRPC TLS server requiring cert and insecure HTTP server",
		HTTPTLSEnabled: false,
		GRPCTLSEnabled: true,
		TLS: tlscfg.Options{
			Enabled:      true,
			CertPath:     testCertKeyLocation + "/example-server-cert.pem",
			KeyPath:      testCertKeyLocation + "/example-server-key.pem",
			ClientCAPath: testCertKeyLocation + "/example-CA-cert.pem",
		},
		clientTLS: tlscfg.Options{
			Enabled:    true,
			CAPath:     testCertKeyLocation + "/example-CA-cert.pem",
			ServerName: "example.com",
			CertPath:   testCertKeyLocation + "/example-client-cert.pem",
			KeyPath:    testCertKeyLocation + "/example-client-key.pem",
		},
		expectError:       false,
		expectServerFail:  false,
		expectClientError: false,
	},
}

type fakeQueryService struct {
	qs               *querysvc.QueryService
	spanReader       *spanstoremocks.Reader
	dependencyReader *depsmocks.Reader
	expectedServices []string
}

func makeQuerySvc() *fakeQueryService {
	spanReader := &spanstoremocks.Reader{}
	dependencyReader := &depsmocks.Reader{}
	expectedServices := []string{"test"}
	spanReader.On("GetServices", mock.AnythingOfType("*context.valueCtx")).Return(expectedServices, nil)
	qs := querysvc.NewQueryService(spanReader, dependencyReader, querysvc.QueryServiceOptions{})
	return &fakeQueryService{
		qs:               qs,
		spanReader:       spanReader,
		dependencyReader: dependencyReader,
		expectedServices: expectedServices,
	}
}

func TestServerHTTPTLS(t *testing.T) {
	testlen := len(testCases)

	tests := make([]struct {
		name              string
		TLS               tlscfg.Options
		HTTPTLSEnabled    bool
		GRPCTLSEnabled    bool
		clientTLS         tlscfg.Options
		expectError       bool
		expectClientError bool
		expectServerFail  bool
	}, testlen)
	copy(tests, testCases)

	tests[testlen-1].clientTLS = tlscfg.Options{Enabled: false}
	tests[testlen-1].name = "Should pass with insecure HTTP Client and insecure HTTP server with secure GRPC Server"
	tests[testlen-1].TLS = tlscfg.Options{
		Enabled: false,
	}

	disabledTLSCfg := tlscfg.Options{
		Enabled: false,
	}
	enabledTLSCfg := tlscfg.Options{
		Enabled:      true,
		CertPath:     testCertKeyLocation + "/example-server-cert.pem",
		KeyPath:      testCertKeyLocation + "/example-server-key.pem",
		ClientCAPath: testCertKeyLocation + "/example-CA-cert.pem",
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			TLSGRPC := disabledTLSCfg
			if test.GRPCTLSEnabled {
				TLSGRPC = enabledTLSCfg
			}

			serverOptions := &QueryOptions{
				GRPCHostPort: ports.GetAddressFromCLIOptions(ports.QueryGRPC, ""),
				HTTPHostPort: ports.GetAddressFromCLIOptions(ports.QueryHTTP, ""),
				TLSHTTP:      test.TLS,
				TLSGRPC:      TLSGRPC,
				QueryOptionsBase: QueryOptionsBase{
					BearerTokenPropagation: true,
				},
			}
			flagsSvc := flags.NewService(ports.QueryAdminHTTP)
			flagsSvc.Logger = zaptest.NewLogger(t)

			querySvc := makeQuerySvc()
			server, err := NewServer(flagsSvc.Logger, flagsSvc.HC(), querySvc.qs,
				nil, serverOptions, tenancy.NewManager(&tenancy.Options{}),
				jtracer.NoOp())
			require.NoError(t, err)
			require.NoError(t, server.Start())
			t.Cleanup(func() {
				require.NoError(t, server.Close())
			})

			var clientError error
			var clientClose func() error
			var clientTLSCfg *tls.Config

			if serverOptions.TLSHTTP.Enabled {
				var err0 error
				clientTLSCfg, err0 = test.clientTLS.Config(flagsSvc.Logger)
				defer test.clientTLS.Close()

				require.NoError(t, err0)
				dialer := &net.Dialer{Timeout: 2 * time.Second}
				conn, err1 := tls.DialWithDialer(dialer, "tcp", "localhost:"+fmt.Sprintf("%d", ports.QueryHTTP), clientTLSCfg)
				clientError = err1
				clientClose = nil
				if conn != nil {
					clientClose = conn.Close
				}

			} else {

				conn, err1 := net.DialTimeout("tcp", "localhost:"+fmt.Sprintf("%d", ports.QueryHTTP), 2*time.Second)
				clientError = err1
				clientClose = nil
				if conn != nil {
					clientClose = conn.Close
				}
			}

			if test.expectError {
				require.Error(t, clientError)
			} else {
				require.NoError(t, clientError)
			}
			if clientClose != nil {
				require.NoError(t, clientClose())
			}

			if test.HTTPTLSEnabled && test.TLS.ClientCAPath != "" {
				client := &http.Client{
					Transport: &http.Transport{
						TLSClientConfig: clientTLSCfg,
					},
				}
				querySvc.spanReader.On("FindTraces", mock.Anything, mock.Anything).Return([]*model.Trace{mockTrace}, nil).Once()
				queryString := "/api/traces?service=service&start=0&end=0&operation=operation&limit=200&minDuration=20ms"
				req, err := http.NewRequest(http.MethodGet, "https://localhost:"+fmt.Sprintf("%d", ports.QueryHTTP)+queryString, nil)
				require.NoError(t, err)
				req.Header.Add("Accept", "application/json")

				resp, err2 := client.Do(req)
				if err2 == nil {
					resp.Body.Close()
				}

				if test.expectClientError {
					require.Error(t, err2)
				} else {
					require.NoError(t, err2)
				}
			}
		})
	}
}

func newGRPCClientWithTLS(t *testing.T, addr string, creds credentials.TransportCredentials) *grpcClient {
	var conn *grpc.ClientConn
	var err error

	if creds != nil {
		conn, err = grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	} else {
		conn, err = grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	require.NoError(t, err)
	return &grpcClient{
		QueryServiceClient: api_v2.NewQueryServiceClient(conn),
		conn:               conn,
	}
}

func TestServerGRPCTLS(t *testing.T) {
	testlen := len(testCases)

	tests := make([]struct {
		name              string
		TLS               tlscfg.Options
		HTTPTLSEnabled    bool
		GRPCTLSEnabled    bool
		clientTLS         tlscfg.Options
		expectError       bool
		expectClientError bool
		expectServerFail  bool
	}, testlen)
	copy(tests, testCases)
	tests[testlen-2].clientTLS = tlscfg.Options{Enabled: false}
	tests[testlen-2].name = "should pass with insecure GRPC Client and insecure GRPC server with secure HTTP Server"
	tests[testlen-2].TLS = tlscfg.Options{
		Enabled: false,
	}

	disabledTLSCfg := tlscfg.Options{
		Enabled: false,
	}
	enabledTLSCfg := tlscfg.Options{
		Enabled:      true,
		CertPath:     testCertKeyLocation + "/example-server-cert.pem",
		KeyPath:      testCertKeyLocation + "/example-server-key.pem",
		ClientCAPath: testCertKeyLocation + "/example-CA-cert.pem",
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			TLSHTTP := disabledTLSCfg
			if test.HTTPTLSEnabled {
				TLSHTTP = enabledTLSCfg
			}
			serverOptions := &QueryOptions{
				GRPCHostPort: ports.GetAddressFromCLIOptions(ports.QueryGRPC, ""),
				HTTPHostPort: ports.GetAddressFromCLIOptions(ports.QueryHTTP, ""),
				TLSHTTP:      TLSHTTP,
				TLSGRPC:      test.TLS,
				QueryOptionsBase: QueryOptionsBase{
					BearerTokenPropagation: true,
				},
			}
			flagsSvc := flags.NewService(ports.QueryAdminHTTP)
			flagsSvc.Logger = zaptest.NewLogger(t)

			querySvc := makeQuerySvc()
			server, err := NewServer(flagsSvc.Logger, flagsSvc.HC(), querySvc.qs,
				nil, serverOptions, tenancy.NewManager(&tenancy.Options{}),
				jtracer.NoOp())
			require.NoError(t, err)
			require.NoError(t, server.Start())
			t.Cleanup(func() {
				require.NoError(t, server.Close())
			})

			var client *grpcClient
			if serverOptions.TLSGRPC.Enabled {
				clientTLSCfg, err0 := test.clientTLS.Config(flagsSvc.Logger)
				require.NoError(t, err0)
				defer test.clientTLS.Close()
				creds := credentials.NewTLS(clientTLSCfg)
				client = newGRPCClientWithTLS(t, ports.PortToHostPort(ports.QueryGRPC), creds)
			} else {
				client = newGRPCClientWithTLS(t, ports.PortToHostPort(ports.QueryGRPC), nil)
			}
			t.Cleanup(func() {
				require.NoError(t, client.conn.Close())
			})

			// using generous timeout since grpc.NewClient no longer does a handshake.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			flagsSvc.Logger.Info("calling client.GetServices()")
			res, clientError := client.GetServices(ctx, &api_v2.GetServicesRequest{})
			flagsSvc.Logger.Info("returned from GetServices()")

			if test.expectClientError {
				require.Error(t, clientError)
			} else {
				require.NoError(t, clientError)
				assert.Equal(t, querySvc.expectedServices, res.Services)
			}
		})
	}
}

func TestServerBadHostPort(t *testing.T) {
	_, err := NewServer(zaptest.NewLogger(t), healthcheck.New(), &querysvc.QueryService{}, nil,
		&QueryOptions{
			HTTPHostPort: "8080", // bad string, not :port
			GRPCHostPort: "127.0.0.1:8081",
			QueryOptionsBase: QueryOptionsBase{
				BearerTokenPropagation: true,
			},
		},
		tenancy.NewManager(&tenancy.Options{}),
		jtracer.NoOp())
	require.Error(t, err)

	_, err = NewServer(zaptest.NewLogger(t), healthcheck.New(), &querysvc.QueryService{}, nil,
		&QueryOptions{
			HTTPHostPort: "127.0.0.1:8081",
			GRPCHostPort: "9123", // bad string, not :port
			QueryOptionsBase: QueryOptionsBase{
				BearerTokenPropagation: true,
			},
		},
		tenancy.NewManager(&tenancy.Options{}),
		jtracer.NoOp())

	require.Error(t, err)
}

func TestServerInUseHostPort(t *testing.T) {
	const availableHostPort = "127.0.0.1:0"
	conn, err := net.Listen("tcp", availableHostPort)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	testCases := []struct {
		name         string
		httpHostPort string
		grpcHostPort string
	}{
		{"HTTP host port clash", conn.Addr().String(), availableHostPort},
		{"GRPC host port clash", availableHostPort, conn.Addr().String()},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server, err := NewServer(
				zaptest.NewLogger(t),
				healthcheck.New(),
				&querysvc.QueryService{},
				nil,
				&QueryOptions{
					HTTPHostPort: tc.httpHostPort,
					GRPCHostPort: tc.grpcHostPort,
					QueryOptionsBase: QueryOptionsBase{
						BearerTokenPropagation: true,
					},
				},
				tenancy.NewManager(&tenancy.Options{}),
				jtracer.NoOp(),
			)
			require.NoError(t, err)
			require.Error(t, server.Start())
			server.Close()
		})
	}
}

func TestServerSinglePort(t *testing.T) {
	flagsSvc := flags.NewService(ports.QueryAdminHTTP)
	flagsSvc.Logger = zaptest.NewLogger(t)
	hostPort := ports.GetAddressFromCLIOptions(ports.QueryHTTP, "")
	querySvc := makeQuerySvc()
	server, err := NewServer(flagsSvc.Logger, flagsSvc.HC(), querySvc.qs, nil,
		&QueryOptions{
			GRPCHostPort: hostPort,
			HTTPHostPort: hostPort,
			QueryOptionsBase: QueryOptionsBase{
				BearerTokenPropagation: true,
			},
		},
		tenancy.NewManager(&tenancy.Options{}),
		jtracer.NoOp())
	require.NoError(t, err)
	require.NoError(t, server.Start())
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})

	client := newGRPCClient(t, hostPort)
	t.Cleanup(func() {
		require.NoError(t, client.conn.Close())
	})

	// using generous timeout since grpc.NewClient no longer does a handshake.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := client.GetServices(ctx, &api_v2.GetServicesRequest{})
	require.NoError(t, err)
	assert.Equal(t, querySvc.expectedServices, res.Services)
}

func TestServerGracefulExit(t *testing.T) {
	flagsSvc := flags.NewService(ports.QueryAdminHTTP)

	zapCore, logs := observer.New(zap.ErrorLevel)
	assert.Equal(t, 0, logs.Len(), "Expected initial ObservedLogs to have zero length.")

	flagsSvc.Logger = zap.New(zapCore)
	hostPort := ports.PortToHostPort(ports.QueryAdminHTTP)

	querySvc := makeQuerySvc()
	server, err := NewServer(flagsSvc.Logger, flagsSvc.HC(), querySvc.qs, nil,
		&QueryOptions{GRPCHostPort: hostPort, HTTPHostPort: hostPort},
		tenancy.NewManager(&tenancy.Options{}), jtracer.NoOp())
	require.NoError(t, err)
	require.NoError(t, server.Start())

	// Wait for servers to come up before we can call .Close()
	{
		client := newGRPCClient(t, hostPort)
		t.Cleanup(func() {
			require.NoError(t, client.conn.Close())
		})
		// using generous timeout since grpc.NewClient no longer does a handshake.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := client.GetServices(ctx, &api_v2.GetServicesRequest{})
		require.NoError(t, err)
	}

	server.Close()
	for _, logEntry := range logs.All() {
		assert.NotEqual(t, zap.ErrorLevel, logEntry.Level,
			"Error log found on server exit: %v", logEntry)
	}
}

func TestServerHandlesPortZero(t *testing.T) {
	flagsSvc := flags.NewService(ports.QueryAdminHTTP)
	zapCore, logs := observer.New(zap.InfoLevel)
	flagsSvc.Logger = zap.New(zapCore)

	querySvc := &querysvc.QueryService{}
	tracer := jtracer.NoOp()
	server, err := NewServer(flagsSvc.Logger, flagsSvc.HC(), querySvc, nil,
		&QueryOptions{GRPCHostPort: ":0", HTTPHostPort: ":0"},
		tenancy.NewManager(&tenancy.Options{}),
		tracer)
	require.NoError(t, err)
	require.NoError(t, server.Start())
	defer server.Close()

	message := logs.FilterMessage("Query server started")
	assert.Equal(t, 1, message.Len(), "Expected 'Query server started' log message.")

	onlyEntry := message.All()[0]
	port := onlyEntry.ContextMap()["port"].(int64)
	assert.Greater(t, port, int64(0))

	grpctest.ReflectionServiceValidator{
		HostPort: fmt.Sprintf(":%v", port),
		Server:   server.grpcServer,
		ExpectedServices: []string{
			"jaeger.api_v2.QueryService",
			"jaeger.api_v3.QueryService",
			"jaeger.api_v2.metrics.MetricsQueryService",
			"grpc.health.v1.Health",
		},
	}.Execute(t)
}

func TestServerHTTPTenancy(t *testing.T) {
	testCases := []struct {
		name   string
		tenant string
		errMsg string
		status int
	}{
		{
			name: "no tenant",
			// no value for tenant header
			status: 401,
		},
		{
			name:   "tenant",
			tenant: "acme",
			status: 200,
		},
	}

	serverOptions := &QueryOptions{
		HTTPHostPort: ":8080",
		GRPCHostPort: ":8080",
		QueryOptionsBase: QueryOptionsBase{
			Tenancy: tenancy.Options{
				Enabled: true,
			},
		},
	}
	tenancyMgr := tenancy.NewManager(&serverOptions.Tenancy)
	querySvc := makeQuerySvc()
	querySvc.spanReader.On("FindTraces", mock.Anything, mock.Anything).Return([]*model.Trace{mockTrace}, nil).Once()
	server, err := NewServer(zaptest.NewLogger(t), healthcheck.New(), querySvc.qs,
		nil, serverOptions, tenancyMgr, jtracer.NoOp())
	require.NoError(t, err)
	require.NoError(t, server.Start())
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			conn, clientError := net.DialTimeout("tcp", "localhost:8080", 2*time.Second)
			require.NoError(t, clientError)

			queryString := "/api/traces?service=service&start=0&end=0&operation=operation&limit=200&minDuration=20ms"
			req, err := http.NewRequest(http.MethodGet, "http://localhost:8080"+queryString, nil)
			if test.tenant != "" {
				req.Header.Add(tenancyMgr.Header, test.tenant)
			}
			require.NoError(t, err)
			req.Header.Add("Accept", "application/json")

			client := &http.Client{}
			resp, err2 := client.Do(req)
			if test.errMsg == "" {
				require.NoError(t, err2)
			} else {
				require.Error(t, err2)
				if err != nil {
					assert.Equal(t, test.errMsg, err2.Error())
				}
			}
			assert.Equal(t, test.status, resp.StatusCode)
			if err2 == nil {
				resp.Body.Close()
			}
			if conn != nil {
				require.NoError(t, conn.Close())
			}
		})
	}
}
