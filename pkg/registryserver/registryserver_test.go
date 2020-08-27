package registryserver_test

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	grpcserver "github.com/intel/pmem-csi/pkg/grpc-server"
	pmemgrpc "github.com/intel/pmem-csi/pkg/pmem-grpc"
	registry "github.com/intel/pmem-csi/pkg/pmem-registry"
	"github.com/intel/pmem-csi/pkg/registryserver"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestPmemRegistry(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Registry Suite")
}

var tmpDir string

var _ = BeforeSuite(func() {
	var err error
	tmpDir, err = ioutil.TempDir("", "pmem-test-")
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	os.RemoveAll(tmpDir)
})

var _ = Describe("pmem registry", func() {

	registryServerSocketFile := filepath.Join(tmpDir, "pmem-registry.sock")
	registryServerEndpoint := "unix://" + registryServerSocketFile

	var (
		tlsConfig          *tls.Config
		nbServer           *grpcserver.NonBlockingGRPCServer
		registryClientConn *grpc.ClientConn
		registryClient     registry.RegistryClient
		registryServer     *registryserver.RegistryServer
	)

	BeforeEach(func() {
		var err error

		registryServer = registryserver.New(nil, "pmem-csi.intel.com")

		caFile := os.ExpandEnv("${TEST_WORK}/pmem-ca/ca.pem")
		certFile := os.ExpandEnv("${TEST_WORK}/pmem-ca/pmem-registry.pem")
		keyFile := os.ExpandEnv("${TEST_WORK}/pmem-ca/pmem-registry-key.pem")
		tlsConfig, err = pmemgrpc.LoadServerTLS(caFile, certFile, keyFile, "pmem-node-controller")
		Expect(err).NotTo(HaveOccurred())

		nbServer = grpcserver.NewNonBlockingGRPCServer()
		err = nbServer.Start(registryServerEndpoint, tlsConfig, nil, registryServer)
		Expect(err).NotTo(HaveOccurred())
		_, err = os.Stat(registryServerSocketFile)
		Expect(err).NotTo(HaveOccurred())

		// set up node controller client
		nodeCertFile := os.ExpandEnv("${TEST_WORK}/pmem-ca/pmem-node-controller.pem")
		nodeCertKey := os.ExpandEnv("${TEST_WORK}/pmem-ca/pmem-node-controller-key.pem")
		tlsConfig, err = pmemgrpc.LoadClientTLS(caFile, nodeCertFile, nodeCertKey, "pmem-registry")
		Expect(err).NotTo(HaveOccurred())

		registryClientConn, err = pmemgrpc.Connect(registryServerEndpoint, tlsConfig)
		Expect(err).NotTo(HaveOccurred())
		registryClient = registry.NewRegistryClient(registryClientConn)
	})

	AfterEach(func() {
		if registryServer != nil {
			nbServer.ForceStop()
			nbServer.Wait()
		}
		os.Remove(registryServerSocketFile)
		if registryClientConn != nil {
			registryClientConn.Close()
		}
	})

	Context("Registry API", func() {
		controllerServerSocketFile := filepath.Join(tmpDir, "pmem-controller.sock")
		controllerServerEndpoint := "unix://" + controllerServerSocketFile
		var (
			nodeId      = "pmem-test"
			registerReq = registry.RegisterControllerRequest{
				NodeId:   nodeId,
				Endpoint: controllerServerEndpoint,
			}

			unregisterReq = registry.UnregisterControllerRequest{
				NodeId: nodeId,
			}
		)

		It("Register node controller", func() {
			Expect(registryClient).ShouldNot(BeNil())

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := registryClient.RegisterController(ctx, &registerReq)
			Expect(err).NotTo(HaveOccurred())

			_, err = registryServer.GetNodeController(nodeId)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Registration should fail", func() {
			Expect(registryClient).ShouldNot(BeNil())

			l := listener{}

			registryServer.AddListener(l)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := registryClient.RegisterController(ctx, &registerReq)
			Expect(err).To(HaveOccurred())

			_, err = registryServer.GetNodeController(nodeId)
			Expect(err).To(HaveOccurred())
		})

		It("Unregister node controller", func() {
			Expect(registryClient).ShouldNot(BeNil())

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := registryClient.RegisterController(ctx, &registerReq)
			Expect(err).NotTo(HaveOccurred())

			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err = registryClient.UnregisterController(ctx, &unregisterReq)
			Expect(err).NotTo(HaveOccurred())

			_, err = registryServer.GetNodeController(nodeId)
			Expect(err).To(HaveOccurred())
		})

		It("Unregister non existing node controller", func() {
			Expect(registryClient).ShouldNot(BeNil())

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := registryClient.UnregisterController(ctx, &unregisterReq)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Registry Security", func() {
		var (
			evilEndpoint = "unix:///tmp/pmem-evil.sock"
			ca           = os.ExpandEnv("${TEST_WORK}/pmem-ca/ca.pem")
			cert         = os.ExpandEnv("${TEST_WORK}/pmem-ca/pmem-node-controller.pem")
			key          = os.ExpandEnv("${TEST_WORK}/pmem-ca/pmem-node-controller-key.pem")
			wrongCert    = os.ExpandEnv("${TEST_WORK}/pmem-ca/wrong-node-controller.pem")
			wrongKey     = os.ExpandEnv("${TEST_WORK}/pmem-ca/wrong-node-controller-key.pem")

			evilCA   = os.ExpandEnv("${TEST_WORK}/evil-ca/ca.pem")
			evilCert = os.ExpandEnv("${TEST_WORK}/evil-ca/pmem-node-controller.pem")
			evilKey  = os.ExpandEnv("${TEST_WORK}/evil-ca/pmem-node-controller-key.pem")
		)

		// gRPC returns all kinds of errors when TLS fails.
		badConnectionRE := "authentication handshake failed: remote error: tls: bad certificate|all SubConns are in TransientFailure|rpc error: code = Unavailable"

		// This covers different scenarios for connections to the registry.
		cases := []struct {
			name, ca, cert, key, peerName, errorRE string
		}{
			// The exact error for the server side depends on whether TLS 1.3 is active (https://golang.org/doc/go1.12#tls_1_3).
			// It looks like error detection is less precise in that case.
			{"registry should detect man-in-the-middle", ca, evilCert, evilKey, "pmem-registry",
				badConnectionRE,
			},
			{"client should detect man-in-the-middle", evilCA, evilCert, evilKey, "pmem-registry", "transport: authentication handshake failed: x509: certificate signed by unknown authority"},
			{"client should detect wrong peer", ca, cert, key, "unknown-registry", "transport: authentication handshake failed: x509: certificate is valid for pmem-csi-scheduler, pmem-csi-scheduler.default, pmem-csi-scheduler.default.svc, pmem-csi-metrics, pmem-csi-metrics.default, pmem-csi-metrics.default.svc, pmem-registry, not unknown-registry"},
			{"server should detect wrong peer", ca, wrongCert, wrongKey, "pmem-registry",
				badConnectionRE,
			},
		}

		for _, c := range cases {
			c := c
			It(c.name, func() {
				tlsConfig, err := pmemgrpc.LoadClientTLS(c.ca, c.cert, c.key, c.peerName)
				Expect(err).NotTo(HaveOccurred())
				clientConn, err := pmemgrpc.Connect(registryServerEndpoint, tlsConfig)
				Expect(err).NotTo(HaveOccurred())
				client := registry.NewRegistryClient(clientConn)

				req := registry.RegisterControllerRequest{
					NodeId:   "pmem-evil",
					Endpoint: evilEndpoint,
				}

				_, err = client.RegisterController(context.Background(), &req)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(MatchRegexp(c.errorRE))
			})
		}
	})

})

type listener struct{}

func (l listener) OnNodeAdded(ctx context.Context, node *registryserver.NodeInfo) error {
	return fmt.Errorf("failed")
}

func (l listener) OnNodeDeleted(ctx context.Context, node *registryserver.NodeInfo) {
}
