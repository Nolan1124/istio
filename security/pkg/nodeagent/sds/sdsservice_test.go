// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package sds

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authapi "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	sds "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/golang/protobuf/ptypes"
	"github.com/google/go-cmp/cmp"
	"golang.org/x/net/context"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/testing/protocmp"
	"k8s.io/apimachinery/pkg/util/uuid"

	rpc "istio.io/gogo-genproto/googleapis/google/rpc"
	"istio.io/istio/pilot/pkg/xds"
	"istio.io/istio/pilot/test/xdstest"
	ca2 "istio.io/istio/pkg/security"
	"istio.io/istio/security/pkg/credentialfetcher/plugin"
	"istio.io/istio/security/pkg/nodeagent/cache"
	"istio.io/istio/security/pkg/nodeagent/util"
	"istio.io/pkg/log"
)

const (
	// firstPartyJwt is generated in a testing K8s cluster. It is the default service account JWT.
	// No expiration time.
	FirstPartyJwt = "eyJhbGciOiJSUzI1NiIsImtpZCI6IiJ9." +
		"eyJpc3MiOiJrdWJlcm5ldGVzL3NlcnZpY2VhY2NvdW50Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9u" +
		"YW1lc3BhY2UiOiJmb28iLCJrdWJlcm5ldGVzLmlvL3NlcnZpY2VhY2NvdW50L3NlY3JldC5uYW1lIjoiaHR0cGJp" +
		"bi10b2tlbi14cWRncCIsImt1YmVybmV0ZXMuaW8vc2VydmljZWFjY291bnQvc2VydmljZS1hY2NvdW50Lm5hbWUi" +
		"OiJodHRwYmluIiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZXJ2aWNlLWFjY291bnQudWlkIjoiOTlm" +
		"NjVmNTAtNzYwYS0xMWVhLTg5ZTctNDIwMTBhODAwMWMxIiwic3ViIjoic3lzdGVtOnNlcnZpY2VhY2NvdW50OmZv" +
		"bzpodHRwYmluIn0.4kIl9TRjXEw6DfhtR-LdpxsAlYjJgC6Ly1DY_rqYY4h0haxcXB3kYZ3b2He-3fqOBryz524W" +
		"KkZscZgvs5L-sApmvlqdUG61TMAl7josB0x4IMHm1NS995LNEaXiI4driffwfopvqc_z3lVKfbF9j-mBgnCepxz3" +
		"UyWo5irFa3qcwbOUB9kuuUNGBdtbFBN5yIYLpfa9E-MtTX_zJ9fQ9j2pi8Z4ljii0tEmPmRxokHkmG_xNJjUkxKU" +
		"WZf4bLDdCEjVFyshNae-FdxiUVyeyYorTYzwZZYQch9MJeedg4keKKUOvCCJUlKixd2qAe-H7r15RPmo4AU5O5YL" +
		"65xiNg"
)

var (
	fakeRootCert         = []byte{00}
	fakeCertificateChain = []byte{01}
	fakePrivateKey       = []byte{02}

	fakePushCertificateChain = []byte{03}
	fakePushPrivateKey       = []byte{04}

	fakeToken1        = "faketoken1"
	fakeToken2        = "faketoken2"
	emptyToken        = ""
	testResourceName  = "default"
	rootResourceName  = "ROOTCA"
	extraResourceName = "extra-resource-name"
)

type TestServer struct {
	t       *testing.T
	server  *Server
	udsPath string
	store   *mockSecretStore
}

func (s *TestServer) Connect() *xds.AdsTest {
	conn, err := setupConnection(s.udsPath)
	if err != nil {
		s.t.Fatal(err)
	}
	sdsClient := sds.NewSecretDiscoveryServiceClient(conn)
	header := metadata.Pairs(credentialTokenHeaderKey, fakeToken1)
	ctx := metadata.NewOutgoingContext(context.Background(), header)
	stream, err := sdsClient.StreamSecrets(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return xds.NewAdsTest(s.t, conn, stream).WithType(SecretTypeV3)
}

type Expectation struct {
	ResourceName string
	CertChain    []byte
	Key          []byte
	RootCert     []byte
}

func (s *TestServer) Verify(resp *discovery.DiscoveryResponse, expectations ...Expectation) {
	s.t.Helper()
	if len(resp.Resources) != len(expectations) {
		s.t.Fatalf("expected %d resources, got %d", len(expectations), len(resp.Resources))
	}
	got := xdstest.ExtractTLSSecrets(s.t, resp.Resources)
	for _, e := range expectations {
		scrt := got[e.ResourceName]
		r := Expectation{
			ResourceName: e.ResourceName,
			Key:          scrt.GetTlsCertificate().GetPrivateKey().GetInlineBytes(),
			CertChain:    scrt.GetTlsCertificate().GetCertificateChain().GetInlineBytes(),
			RootCert:     scrt.GetValidationContext().GetTrustedCa().GetInlineBytes(),
		}
		if diff := cmp.Diff(e, r); diff != "" {
			s.t.Fatalf("got diff: %v", diff)
		}
	}
}

func setupSDS(t *testing.T) *TestServer {
	// Clear out global state. TODO stop using global variables
	sdsClientsMutex.Lock()
	for k := range sdsClients {
		delete(sdsClients, k)
	}
	sdsClientsMutex.Unlock()

	st := &mockSecretStore{
		checkToken:    true,
		expectedToken: fakeToken1,
	}
	opts := &ca2.Options{
		WorkloadUDSPath:   fmt.Sprintf("/tmp/workload_gotest%s.sock", string(uuid.NewUUID())),
		EnableWorkloadSDS: true,
		RecycleInterval:   time.Second * 30,
	}
	server, err := NewServer(opts, st)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		server.Stop()
	})
	return &TestServer{
		t:       t,
		server:  server,
		store:   st,
		udsPath: opts.WorkloadUDSPath,
	}
}

func TestSDS(t *testing.T) {
	expectCert := Expectation{
		ResourceName: testResourceName,
		CertChain:    fakeCertificateChain,
		Key:          fakePrivateKey,
	}
	expectRoot := Expectation{
		ResourceName: rootResourceName,
		RootCert:     fakeRootCert,
	}
	t.Run("simple", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}}), expectCert)
		c.ExpectNoResponse()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{rootResourceName}}), expectRoot)
		c.ExpectNoResponse()
	})
	t.Run("root first", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{rootResourceName}}), expectRoot)
		c.ExpectNoResponse()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}}), expectCert)
		c.ExpectNoResponse()
	})
	t.Run("multiple single requests", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{rootResourceName}}), expectRoot)
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{rootResourceName}}), expectRoot)
		c.ExpectNoResponse()
	})
	t.Run("multiple request", func(t *testing.T) {
		// This test is legal Envoy behavior, but in practice Envoy doesn't aggregate requests
		t.Skip()
		s := setupSDS(t)
		c := s.Connect()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName, rootResourceName}}), expectCert, expectRoot)
		c.ExpectNoResponse()
	})
	t.Run("multiple parallel connections", func(t *testing.T) {
		s := setupSDS(t)
		cert := s.Connect()
		root := s.Connect()

		s.Verify(cert.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}}), expectCert)
		s.Verify(root.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{rootResourceName}}), expectRoot)
		cert.ExpectNoResponse()
		root.ExpectNoResponse()
	})
	t.Run("multiple serial connections", func(t *testing.T) {
		s := setupSDS(t)
		cert := s.Connect()
		s.Verify(cert.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}}), expectCert)
		cert.ExpectNoResponse()

		root := s.Connect()
		s.Verify(root.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{rootResourceName}}), expectRoot)
		root.ExpectNoResponse()
		cert.ExpectNoResponse()
	})
	t.Run("push", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}}), expectCert)
		conID := getClientConID(c.ID)
		// Test push new secret to proxy. This SecretItem is for StreamOne.
		if err := NotifyProxy(cache.ConnKey{ConnectionID: conID, ResourceName: testResourceName},
			s.GeneratePushSecret(conID, fakeToken1)); err != nil {
			t.Fatalf("failed to send push notification to proxy %q: %v", conID, err)
		}
	})
	t.Run("reconnect", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		res := c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}})
		s.Verify(res, expectCert)
		// Close out the connection
		c.Cleanup()

		// Reconnect with the same resources
		c = s.Connect()
		s.Verify(c.RequestResponseAck(&discovery.DiscoveryRequest{
			ResourceNames: []string{testResourceName},
			ResponseNonce: res.Nonce,
			VersionInfo:   res.VersionInfo,
		}), expectCert)
	})
	t.Run("unsubscribe", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		res := c.RequestResponseAck(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}})
		s.Verify(res, expectCert)
		c.Request(&discovery.DiscoveryRequest{
			ResourceNames: nil,
			ResponseNonce: res.Nonce,
			VersionInfo:   res.VersionInfo,
		})
		c.ExpectNoResponse()
	})
	t.Run("unknown resource", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		c.Request(&discovery.DiscoveryRequest{ResourceNames: []string{extraResourceName}})
		c.ExpectNoResponse()
	})
	t.Run("nack", func(t *testing.T) {
		s := setupSDS(t)
		c := s.Connect()
		c.RequestResponseNack(&discovery.DiscoveryRequest{ResourceNames: []string{testResourceName}})
		c.ExpectNoResponse()
	})
}

func (s *TestServer) GeneratePushSecret(conID, token string) *ca2.SecretItem {
	pushSecret := &ca2.SecretItem{
		CertificateChain: fakePushCertificateChain,
		PrivateKey:       fakePushPrivateKey,
		ResourceName:     testResourceName,
		Version:          time.Now().Format("01-02 15:04:05.000"),
		Token:            token,
	}
	s.store.secrets.Store(cache.ConnKey{ConnectionID: conID, ResourceName: testResourceName}, pushSecret)
	return pushSecret
}

func TestStreamSecretsForWorkloadSds(t *testing.T) {
	arg := ca2.Options{
		EnableWorkloadSDS: true,
		RecycleInterval:   30 * time.Second,
		GatewayUDSPath:    "",
		WorkloadUDSPath:   fmt.Sprintf("/tmp/workload_gotest%q.sock", string(uuid.NewUUID())),
	}
	testHelper(t, arg, sdsRequestStream, false)
}

// Verifies that SDS agent is using a valid token returned by credential fetcher and pushing SDS resources back successfully.
func TestStreamSecretsForCredentialFetcherGetTokenWorkloadSds(t *testing.T) {
	cf := plugin.CreateMockPlugin(FirstPartyJwt)

	arg := ca2.Options{
		EnableWorkloadSDS: true,
		RecycleInterval:   30 * time.Second,
		GatewayUDSPath:    "",
		WorkloadUDSPath:   fmt.Sprintf("/tmp/workload_gotest%q.sock", string(uuid.NewUUID())),
		UseLocalJWT:       true,
		CredFetcher:       cf,
	}
	testCredentialFetcherHelper(t, arg, sdsRequestStream, FirstPartyJwt, FirstPartyJwt)
}

// Verifies that SDS agent is using an empty token returned by credential fetcher and pushing SDS resources back unsuccessfully.
func TestStreamSecretsForCredentialFetcherGetEmptyTokenWorkloadSds(t *testing.T) {
	cf := plugin.CreateMockPlugin(emptyToken)

	arg := ca2.Options{
		EnableWorkloadSDS: true,
		RecycleInterval:   30 * time.Second,
		GatewayUDSPath:    "",
		WorkloadUDSPath:   fmt.Sprintf("/tmp/workload_gotest%q.sock", string(uuid.NewUUID())),
		UseLocalJWT:       true,
		CredFetcher:       cf,
	}
	testCredentialFetcherHelper(t, arg, sdsRequestStream, FirstPartyJwt, emptyToken)
}

// Validate that StreamSecrets works correctly for file mounted certs i.e. when UseLocalJWT is set to false and FileMountedCerts to true.
func TestStreamSecretsForFileMountedsWorkloadSds(t *testing.T) {
	arg := ca2.Options{
		EnableWorkloadSDS: true,
		RecycleInterval:   30 * time.Second,
		GatewayUDSPath:    "",
		WorkloadUDSPath:   fmt.Sprintf("/tmp/workload_gotest%q.sock", string(uuid.NewUUID())),
		FileMountedCerts:  true,
		UseLocalJWT:       false,
	}
	wst := &mockSecretStore{
		checkToken: false,
	}
	server, err := NewServer(&arg, wst)
	defer server.Stop()
	if err != nil {
		t.Fatalf("failed to start grpc server for sds: %v", err)
	}

	proxyID := "sidecar~127.0.0.1~id1~local"

	// Request for root certificate from file and verify response.
	rootResourceName := sendRequestForFileRootCertAndVerifyResponse(t, sdsRequestStream, arg.WorkloadUDSPath, proxyID)

	recycleConnection(getClientConID(proxyID), rootResourceName)
	// Check to make sure number of staled connections is 0.
	checkStaledConnCount(t)
}

func TestFetchSecretsForWorkloadSds(t *testing.T) {
	arg := ca2.Options{
		EnableWorkloadSDS: true,
		RecycleInterval:   30 * time.Second,
		GatewayUDSPath:    "",
		WorkloadUDSPath:   fmt.Sprintf("/tmp/workload_gotest%q.sock", string(uuid.NewUUID())),
	}
	testHelper(t, arg, sdsRequestFetch, false)
}

func TestStreamSecretsInvalidResourceName(t *testing.T) {
	arg := ca2.Options{
		EnableWorkloadSDS: true,
		RecycleInterval:   30 * time.Second,
		GatewayUDSPath:    "",
		WorkloadUDSPath:   fmt.Sprintf("/tmp/workload_gotest%s.sock", string(uuid.NewUUID())),
	}
	testHelper(t, arg, sdsRequestStream, true)
}

type secretCallback func(string, *discovery.DiscoveryRequest) (*discovery.DiscoveryResponse, error)

func testHelper(t *testing.T, arg ca2.Options, cb secretCallback, testInvalidResourceNames bool) {
	resetEnvironments()
	var wst ca2.SecretManager
	if arg.EnableWorkloadSDS {
		wst = &mockSecretStore{
			checkToken:    true,
			expectedToken: fakeToken1,
		}
	} else {
		wst = nil
	}
	server, err := NewServer(&arg, wst)
	defer server.Stop()
	if err != nil {
		t.Fatalf("failed to start grpc server for sds: %v", err)
	}

	proxyID := "sidecar~127.0.0.1~id1~local"
	if testInvalidResourceNames && arg.EnableWorkloadSDS {
		sendRequestAndVerifyResponse(t, cb, arg.WorkloadUDSPath, proxyID, testInvalidResourceNames)
		return
	}

	if arg.EnableWorkloadSDS {
		sendRequestAndVerifyResponse(t, cb, arg.WorkloadUDSPath, proxyID, testInvalidResourceNames)
		// Request for root certificate.
		sendRequestForRootCertAndVerifyResponse(t, cb, arg.WorkloadUDSPath, proxyID)

		recycleConnection(getClientConID(proxyID), testResourceName)
		recycleConnection(getClientConID(proxyID), "ROOTCA")
	}
	// Check to make sure number of staled connections is 0.
	checkStaledConnCount(t)
}

func testCredentialFetcherHelper(t *testing.T, arg ca2.Options, cb secretCallback, expectedToken, token string) {
	resetEnvironments()
	var wst ca2.SecretManager
	if arg.EnableWorkloadSDS {
		wst = &mockSecretStore{
			checkToken:    true,
			expectedToken: expectedToken,
		}
	} else {
		wst = nil
	}

	server, err := NewServer(&arg, wst)
	defer server.Stop()
	if err != nil {
		t.Fatalf("failed to start grpc server for sds: %v", err)
	}

	proxyID := "sidecar~127.0.0.1~id1~local"
	if token == emptyToken && arg.EnableWorkloadSDS {
		sendRequestAndVerifyResponseWithCredentialFetcher(t, cb, arg.WorkloadUDSPath, proxyID, token)
		return
	}

	if arg.EnableWorkloadSDS {
		sendRequestAndVerifyResponseWithCredentialFetcher(t, cb, arg.WorkloadUDSPath, proxyID, token)
		// Request for root certificate.
		sendRequestForRootCertAndVerifyResponse(t, cb, arg.WorkloadUDSPath, proxyID)

		recycleConnection(getClientConID(proxyID), testResourceName)
		recycleConnection(getClientConID(proxyID), "ROOTCA")
	}
	// Check to make sure number of staled connections is 0.
	checkStaledConnCount(t)
}

func sendRequestForRootCertAndVerifyResponse(t *testing.T, cb secretCallback, socket, proxyID string) {
	rootCertReq := &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeV3,
		ResourceNames: []string{"ROOTCA"},
		Node: &core.Node{
			Id: proxyID,
		},
	}
	resp, err := cb(socket, rootCertReq)
	if err != nil {
		t.Fatalf("failed to get root cert through SDS")
	}
	verifySDSSResponseForRootCert(t, resp, fakeRootCert)
}

func sendRequestForFileRootCertAndVerifyResponse(t *testing.T, cb secretCallback, socket, proxyID string) string {
	rootCertPath, _ := filepath.Abs("./testdata/root-cert.pem")
	rootResource := "file-root:" + rootCertPath

	rootCertReq := &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeV3,
		ResourceNames: []string{rootResource},
		Node: &core.Node{
			Id: proxyID,
		},
	}
	resp, err := cb(socket, rootCertReq)
	if err != nil {
		t.Fatalf("failed to get root cert through SDS")
	}
	verifySDSSResponseForRootCert(t, resp, fakeRootCert)
	return rootResource
}

func sendRequestAndVerifyResponse(t *testing.T, cb secretCallback, socket, proxyID string, testInvalidResourceNames bool) {
	rn := []string{testResourceName}
	// Only one resource name is allowed, add extra name to create an error.
	if testInvalidResourceNames {
		rn = append(rn, extraResourceName)
	}
	req := &discovery.DiscoveryRequest{
		ResourceNames: rn,
		TypeUrl:       SecretTypeV3,
		Node: &core.Node{
			Id: proxyID,
		},
	}

	wait := 300 * time.Millisecond
	retry := 0
	for ; retry < 5; retry++ {
		time.Sleep(wait)
		// Try to call the server
		resp, err := cb(socket, req)
		if testInvalidResourceNames {
			if ok := verifyResponseForInvalidResourceNames(err); ok {
				return
			}
		} else {
			if err == nil {
				//Verify secret.
				if err := verifySDSSResponse(resp, fakePrivateKey, fakeCertificateChain); err != nil {
					t.Errorf("failed to verify SDS response %v", err)
				}
				return
			}
		}
		wait *= 2
	}

	if retry == 5 {
		t.Fatal("failed to start grpc server for SDS")
	}
}

func sendRequestAndVerifyResponseWithCredentialFetcher(t *testing.T, cb secretCallback, socket, proxyID string, token string) {
	rn := []string{testResourceName}
	req := &discovery.DiscoveryRequest{
		ResourceNames: rn,
		TypeUrl:       SecretTypeV3,
		Node: &core.Node{
			Id: proxyID,
		},
	}

	wait := 300 * time.Millisecond
	retry := 0
	for ; retry < 5; retry++ {
		time.Sleep(wait)
		// Try to call the server
		resp, err := cb(socket, req)
		if token == emptyToken {
			if ok := verifyResponseForEmptyToken(err); ok {
				return
			}
		} else {
			if err == nil {
				//Verify secret.
				if err := verifySDSSResponse(resp, fakePrivateKey, fakeCertificateChain); err != nil {
					t.Errorf("failed to verify SDS response %v", err)
				}
				return
			}
		}
		wait *= 2
	}

	if retry == 5 {
		t.Fatal("failed to start grpc server for SDS")
	}
}

func verifyResponseForInvalidResourceNames(err error) bool {
	s := fmt.Sprintf("has more than one resourceNames [%s %s]", testResourceName, extraResourceName)
	return strings.Contains(err.Error(), s)
}

func verifyResponseForEmptyToken(err error) bool {
	s := fmt.Sprintf("rpc error: code = Unknown desc = unexpected token %s", emptyToken)
	return strings.Contains(err.Error(), s)
}

func createSDSServer(t *testing.T, socket string) (*Server, *mockSecretStore) {
	arg := ca2.Options{
		EnableWorkloadSDS: true,
		RecycleInterval:   100 * time.Second,
		WorkloadUDSPath:   socket,
	}
	st := &mockSecretStore{
		checkToken: false,
	}
	server, err := NewServer(&arg, st)
	if err != nil {
		t.Fatalf("failed to start grpc server for sds: %v", err)
	}
	return server, st
}

func createSDSStream(t *testing.T, socket, token string) (*grpc.ClientConn, sds.SecretDiscoveryService_StreamSecretsClient) {
	// Try to call the server
	conn, err := setupConnection(socket)
	if err != nil {
		t.Errorf("failed to setup connection to socket %q", socket)
	}
	sdsClient := sds.NewSecretDiscoveryServiceClient(conn)
	header := metadata.Pairs(credentialTokenHeaderKey, token)
	ctx := metadata.NewOutgoingContext(context.Background(), header)
	stream, err := sdsClient.StreamSecrets(ctx)
	if err != nil {
		t.Errorf("StreamSecrets failed: %v", err)
	}
	return conn, stream
}

// Flow for testSDSStreamTwo:
// Client         Server       TestStreamSecretsPush
//   REQ    -->
// (verify) <--    RESP
// "close stream" -----------> (close stream)
func testSDSStreamTwo(stream sds.SecretDiscoveryService_StreamSecretsClient, proxyID string,
	notifyChan chan notifyMsg) {
	req := &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeV3,
		ResourceNames: []string{testResourceName},
		Node: &core.Node{
			Id: proxyID,
		},
		// Set a non-empty version info so that StreamSecrets() starts a cache check, and cache miss
		// metric is updated accordingly.
		VersionInfo: "initial_version",
	}
	if err := stream.Send(req); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf(
			"stream two: stream.Send failed: %v", err)}
	}
	resp, err := stream.Recv()
	if err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf(
			"stream two: stream.Recv failed: %v", err)}
	}
	if err := verifySDSSResponse(resp, fakePrivateKey, fakeCertificateChain); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf(
			"stream two: SDS response verification failed: %v", err)}
	}
	notifyChan <- notifyMsg{Err: nil, Message: "close stream"}
}

// Flow for testSDSStreamOne:
// Client         Server       TestStreamSecretsPush
//   REQ    -->
// (verify) <--    RESP
//   ACK    -->
// "notify push secret 1"----> (Check stats, push new secret)
// (verify) <--    RESP
//      <--------------------- "receive secret"
//   ACK    -->
// "notify push secret 2"----> (Check stats, push nil secret)
//          <--    ERROR
//      <--------------------- "receive nil secret"
// "close stream" -----------> (close stream)
func testSDSStreamOne(stream sds.SecretDiscoveryService_StreamSecretsClient, proxyID string,
	notifyChan chan notifyMsg) {
	req := &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeV3,
		ResourceNames: []string{testResourceName},
		Node: &core.Node{
			Id: proxyID,
		},
		// Set a non-empty version info so that StreamSecrets() starts a cache check, and cache miss
		// metric is updated accordingly.
		VersionInfo: "initial_version",
	}

	// Send first request and verify response
	if err := stream.Send(req); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream one: stream.Send failed: %v", err)}
	}
	resp, err := stream.Recv()
	if err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream one: stream.Recv failed: %v", err)}
	}
	if err := verifySDSSResponse(resp, fakePrivateKey, fakeCertificateChain); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf(
			"stream one: first SDS response verification failed: %v", err)}
	}

	// Send second request as an ACK and wait for notifyPush
	// The second and following requests can carry an empty node identifier.
	req.Node.Id = ""
	req.VersionInfo = resp.VersionInfo
	req.ResponseNonce = resp.Nonce
	if err = stream.Send(req); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream one: stream.Send failed: %v", err)}
	}

	notifyChan <- notifyMsg{Err: nil, Message: "notify push secret 1"}
	if notify := <-notifyChan; notify.Message == "receive secret" {
		resp, err = stream.Recv()
		if err != nil {
			notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream one: stream.Recv failed: %v", err)}
		}
		if err := verifySDSSResponse(resp, fakePushPrivateKey, fakePushCertificateChain); err != nil {
			notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf(
				"stream one: second SDS response verification failed: %v", err)}
		}
	}

	// Send third request as an ACK and wait for stream close
	req.VersionInfo = resp.VersionInfo
	req.ResponseNonce = resp.Nonce
	if err = stream.Send(req); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream one: stream.Send failed: %v", err)}
	}
	notifyChan <- notifyMsg{Err: nil, Message: "notify push secret 2"}
	if notify := <-notifyChan; notify.Message == "receive nil secret" {
		_, err = stream.Recv()
		if err == nil {
			notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream one: stream.Recv failed: %v", err)}
		}
	}
	notifyChan <- notifyMsg{Err: nil, Message: "close stream"}
}

type notifyMsg struct {
	Err     error
	Message string
}

func waitForNotificationToProceed(t *testing.T, notifyChan chan notifyMsg, proceedNotice string) {
	for {
		if notify := <-notifyChan; notify.Err != nil {
			t.Fatalf("get error from stream: %v", notify.Message)
		} else {
			if notify.Message != proceedNotice {
				t.Fatalf("push signal does not match, expected %s but got %s", proceedNotice,
					notify.Message)
			}
			return
		}
	}
}

// waitForSecretCacheCheck wait until cache hit or cache miss meets expected value and return. Or
// return directly on timeout.
func waitForSecretCacheCheck(t *testing.T, mss *mockSecretStore, expectCacheHit bool, expectValue int) {
	waitTimeout := 5 * time.Second
	checkMetric := "cache hit"
	if !expectCacheHit {
		checkMetric = "cache miss"
	}
	realVal := 0
	start := time.Now()
	for {
		if expectCacheHit {
			realVal = mss.SecretCacheHit()
			if realVal == expectValue {
				return
			}
		}
		if !expectCacheHit {
			realVal = mss.SecretCacheMiss()
			if realVal == expectValue {
				return
			}
		}
		if time.Since(start) > waitTimeout {
			t.Fatalf("%s does not meet expected value in %s, expected %d but got %d",
				checkMetric, waitTimeout.String(), expectValue, realVal)
			return
		}
		time.Sleep(1 * time.Second)
	}
}

func getClientConID(proxyID string) string {
	sdsClientsMutex.RLock()
	defer sdsClientsMutex.RUnlock()
	for k := range sdsClients {
		if strings.HasPrefix(k.ConnectionID, proxyID) {
			return k.ConnectionID
		}
	}
	return ""
}

func TestStreamSecretsPush(t *testing.T) {
	setup := StartTest(t)
	defer setup.server.Stop()

	var expectedTotalPush int64

	connOne, streamOne := createSDSStream(t, setup.socket, fakeToken1)
	defer connOne.Close()
	proxyID := "sidecar~127.0.0.1~SecretsPushStreamOne~local"
	notifyChanOne := make(chan notifyMsg)
	go testSDSStreamOne(streamOne, proxyID, notifyChanOne)
	expectedTotalPush += 2

	connTwo, streamTwo := createSDSStream(t, setup.socket, fakeToken2)
	defer connTwo.Close()
	proxyIDTwo := "sidecar~127.0.0.1~SecretsPushStreamTwo~local"
	notifyChanTwo := make(chan notifyMsg)
	go testSDSStreamTwo(streamTwo, proxyIDTwo, notifyChanTwo)
	expectedTotalPush++

	// verify that the first SDS request sent by two streams do not hit cache.
	waitForSecretCacheCheck(t, setup.secretStore, false, 2)
	waitForNotificationToProceed(t, notifyChanOne, "notify push secret 1")
	// verify that the second SDS request hits cache.
	waitForSecretCacheCheck(t, setup.secretStore, true, 1)

	// simulate logic in constructConnectionID() function.
	conID := getClientConID(proxyID)
	// Test push new secret to proxy. This SecretItem is for StreamOne.
	if err := NotifyProxy(cache.ConnKey{ConnectionID: conID, ResourceName: testResourceName},
		setup.generatePushSecret(conID, fakeToken1)); err != nil {
		t.Fatalf("failed to send push notification to proxy %q: %v", conID, err)
	}
	notifyChanOne <- notifyMsg{Err: nil, Message: "receive secret"}

	// Verify that pushed secret is stored in cache.
	key := cache.ConnKey{
		ConnectionID: conID,
		ResourceName: testResourceName,
	}
	if _, found := setup.secretStore.secrets.Load(key); !found {
		t.Fatalf("Failed to find cached secret")
	}

	waitForNotificationToProceed(t, notifyChanOne, "notify push secret 2")
	// verify that the third SDS request hits cache.
	waitForSecretCacheCheck(t, setup.secretStore, true, 2)

	// Test push nil secret(indicates close the streaming connection) to proxy.
	if err := NotifyProxy(cache.ConnKey{ConnectionID: conID, ResourceName: testResourceName}, nil); err != nil {
		t.Fatalf("failed to send push notification to proxy %q", conID)
	}
	notifyChanOne <- notifyMsg{Err: nil, Message: "receive nil secret"}

	waitForNotificationToProceed(t, notifyChanOne, "close stream")
	waitForNotificationToProceed(t, notifyChanTwo, "close stream")

	if _, found := setup.secretStore.secrets.Load(key); found {
		t.Fatalf("Found cached secret after stream close, expected the secret to not exist")
	}

	recycleConnection(getClientConID(proxyID), testResourceName)
	recycleConnection(getClientConID(proxyIDTwo), testResourceName)
	clearStaledClients()
	// Add RLock to avoid racetest fail.
	sdsClientsMutex.RLock()
	if len(sdsClients) != 0 {
		t.Fatalf("sdsClients, got %d, expected 0", len(sdsClients))
	}
	sdsClientsMutex.RUnlock()

	setup.verifyTotalPushes(expectedTotalPush)
}

func testSDSStreamMultiplePush(stream sds.SecretDiscoveryService_StreamSecretsClient, proxyID string,
	notifyChan chan notifyMsg) {
	req := &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeV3,
		ResourceNames: []string{testResourceName},
		Node: &core.Node{
			Id: proxyID,
		},
		// Set a non-empty version info so that StreamSecrets() starts a cache check, and cache miss
		// metric is updated accordingly.
		VersionInfo: "initial_version",
	}

	// Send first request and
	if err := stream.Send(req); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream.Send failed: %v", err)}
	}
	resp, err := stream.Recv()
	if err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream.Recv failed: %v", err)}
	}
	if err := verifySDSSResponse(resp, fakePrivateKey, fakeCertificateChain); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("SDS response verification failed: %v", err)}
	}

	// Don't send a request and force SDS server to push secret, as a duplicate push.
	notifyChan <- notifyMsg{Err: nil, Message: "notify push secret"}
	if notify := <-notifyChan; notify.Message != "receive secret" {
		errMisMatch := fmt.Errorf("received error does not match, got %v", err)
		notifyChan <- notifyMsg{Err: errMisMatch, Message: errMisMatch.Error()}
	}

	notifyChan <- notifyMsg{Err: nil, Message: "close stream"}
}

// TestStreamSecretsMultiplePush verifies that only one response is pushed per request, and that multiple
// pushes are detected and skipped.
func TestStreamSecretsMultiplePush(t *testing.T) {
	setup := StartTest(t)
	defer setup.server.Stop()

	conn, stream := createSDSStream(t, setup.socket, fakeToken1)
	defer conn.Close()
	proxyID := "sidecar~127.0.0.1~StreamMultiplePush~local"
	notifyChan := make(chan notifyMsg)
	go testSDSStreamMultiplePush(stream, proxyID, notifyChan)

	waitForNotificationToProceed(t, notifyChan, "notify push secret")
	// verify that the first SDS request does not hit cache.
	waitForSecretCacheCheck(t, setup.secretStore, false, 1)
	// simulate logic in constructConnectionID() function.
	conID := getClientConID(proxyID)
	// Test push new secret to proxy.
	if err := NotifyProxy(cache.ConnKey{ConnectionID: conID, ResourceName: testResourceName},
		setup.generatePushSecret(conID, fakeToken1)); err != nil {
		t.Fatalf("failed to send push notification to proxy %q", conID)
	}
	notifyChan <- notifyMsg{Err: nil, Message: "receive secret"}
	waitForNotificationToProceed(t, notifyChan, "close stream")
}

func testSDSStreamUpdateFailures(stream sds.SecretDiscoveryService_StreamSecretsClient, proxyID string,
	notifyChan chan notifyMsg) {
	req := &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeV3,
		ResourceNames: []string{testResourceName},
		Node: &core.Node{
			Id: proxyID,
		},
		// Set a non-empty version info so that StreamSecrets() starts a cache check, and cache miss
		// metric is updated accordingly.
		VersionInfo: "initial_version",
	}

	// Send first request and verify the response
	if err := stream.Send(req); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream.Send failed: %v", err)}
	}
	resp, err := stream.Recv()
	if err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream.Recv failed: %v", err)}
	}
	if err := verifySDSSResponse(resp, fakePrivateKey, fakeCertificateChain); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf(
			"first SDS response verification failed: %v", err)}
	}

	// Send second request as a NACK. The server side will be blocked.
	req.ErrorDetail = &status.Status{
		Code:    int32(rpc.INTERNAL),
		Message: "fake error",
	}
	if err = stream.Send(req); err != nil {
		notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream.Send failed: %v", err)}
	}

	// Wait for the server to block.
	time.Sleep(500 * time.Millisecond)

	// Notify the testing thread to update the secret on the server, which triggers a response with
	// the new secret will be sent.
	notifyChan <- notifyMsg{Err: nil, Message: "notify push secret"}
	if notify := <-notifyChan; notify.Message == "receive secret" {
		resp, err = stream.Recv()
		if err != nil {
			notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf("stream.Recv failed: %v", err)}
		}
		if err := verifySDSSResponse(resp, fakePushPrivateKey, fakePushCertificateChain); err != nil {
			notifyChan <- notifyMsg{Err: err, Message: fmt.Sprintf(
				"second SDS response verification failed: %v", err)}
		}
	}

	notifyChan <- notifyMsg{Err: nil, Message: "close stream"}
}

// TestStreamSecretsUpdateFailures verifies that NACK returned by Envoy will be blocked until next
// secret update push.
// Flow:
// Client         Server       TestStreamSecretsUpdateFailures
//   REQ    -->
// (verify) <--    RESP
//   NACK   -->  (blocked)
// "notify push secret"  ----> (Check stats, push new secret)
//      <--------------------- "receive secret"
// (verify) <--    RESP
// "close stream" -----------> (close stream)
func TestStreamSecretsUpdateFailures(t *testing.T) {
	setup := StartTest(t)
	defer setup.server.Stop()

	conn, stream := createSDSStream(t, setup.socket, fakeToken1)
	defer conn.Close()
	proxyID := "sidecar~127.0.0.1~SecretsUpdateFailure~local"
	notifyChan := make(chan notifyMsg)
	go testSDSStreamUpdateFailures(stream, proxyID, notifyChan)

	waitForNotificationToProceed(t, notifyChan, "notify push secret")
	// verify that the first SDS request does not hit cache, and that the second SDS request hits cache.
	waitForSecretCacheCheck(t, setup.secretStore, false, 1)
	waitForSecretCacheCheck(t, setup.secretStore, true, 0)

	// simulate logic in constructConnectionID() function.
	conID := getClientConID(proxyID)
	// Test push new secret to proxy.
	if err := NotifyProxy(cache.ConnKey{ConnectionID: conID, ResourceName: testResourceName},
		setup.generatePushSecret(conID, fakeToken1)); err != nil {
		t.Fatalf("failed to send push notification to proxy %q: %v", conID, err)
	}
	notifyChan <- notifyMsg{Err: nil, Message: "receive secret"}
	waitForNotificationToProceed(t, notifyChan, "close stream")

	setup.verifyUpdateFailureCount(1)
}

type Setup struct {
	t                          *testing.T
	socket                     string
	server                     *Server
	secretStore                *mockSecretStore
	initialTotalPush           float64
	initialTotalUpdateFailures float64
}

// StartTest starts SDS server and checks SDS connectivity.
func StartTest(t *testing.T) *Setup {
	s := &Setup{t: t}
	// reset connectionNumber since since its value is kept in memory for all unit test cases lifetime,
	// reset since it may be updated in other test case.
	atomic.StoreInt64(&connectionNumber, 0)

	s.socket = fmt.Sprintf("/tmp/gotest%s.sock", string(uuid.NewUUID()))
	s.server, s.secretStore = createSDSServer(t, s.socket)

	if err := s.waitForSDSReady(); err != nil {
		t.Fatalf("fail to start SDS server: %v", err)
	}

	// Get initial SDS push stats.
	initialTotalPush, err := util.GetMetricsCounterValue("total_pushes")
	if err != nil {
		t.Fatalf("fail to get initial value from metric totalPush: %v", err)
	}
	initialTotalUpdateFailures, err := util.GetMetricsCounterValue("total_secret_update_failures")
	if err != nil {
		t.Fatalf("fail to get initial value from metric totalSecretUpdateFailureCounts: %v", err)
	}
	s.initialTotalPush = initialTotalPush
	s.initialTotalUpdateFailures = initialTotalUpdateFailures

	return s
}

func (s *Setup) waitForSDSReady() error {
	var conErr, streamErr error
	var conn *grpc.ClientConn
	for i := 0; i < 20; i++ {
		if conn, conErr = setupConnection(s.socket); conErr == nil {
			sdsClient := sds.NewSecretDiscoveryServiceClient(conn)
			header := metadata.Pairs(credentialTokenHeaderKey, fakeToken1)
			ctx := metadata.NewOutgoingContext(context.Background(), header)
			if _, streamErr = sdsClient.StreamSecrets(ctx); streamErr == nil {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("cannot connect SDS server, connErr: %v, streamErr: %v", conErr, streamErr)
}

func (s *Setup) verifyUpdateFailureCount(expected int64) {
	totalSecretUpdateFailureVal, err := util.GetMetricsCounterValue("total_secret_update_failures")
	if err != nil {
		s.t.Errorf("Fail to get value from metric totalSecretUpdateFailureCounts: %v", err)
	}
	totalSecretUpdateFailureVal -= s.initialTotalUpdateFailures
	if totalSecretUpdateFailureVal != float64(expected) {
		s.t.Errorf("unexpected metric totalSecretUpdateFailureCounts: expected %d but got %v",
			expected, totalSecretUpdateFailureVal)
	}
}

func (s *Setup) verifyTotalPushes(expected int64) {
	totalPushVal, err := util.GetMetricsCounterValue("total_pushes")
	if err != nil {
		s.t.Errorf("Fail to get value from metric totalPush: %v", err)
	}
	totalPushVal -= s.initialTotalPush
	if totalPushVal != float64(expected) {
		s.t.Errorf("unexpected metric totalPush: expected %d but got %v", expected, totalPushVal)
	}
}

func (s *Setup) generatePushSecret(conID, token string) *ca2.SecretItem {
	pushSecret := &ca2.SecretItem{
		CertificateChain: fakePushCertificateChain,
		PrivateKey:       fakePushPrivateKey,
		ResourceName:     testResourceName,
		Version:          time.Now().Format("01-02 15:04:05.000"),
		Token:            token,
	}
	s.secretStore.secrets.Store(cache.ConnKey{ConnectionID: conID, ResourceName: testResourceName}, pushSecret)
	return pushSecret
}

func verifySDSSResponse(resp *discovery.DiscoveryResponse, expectedPrivateKey []byte, expectedCertChain []byte) error {
	pb := &authapi.Secret{}
	if resp == nil {
		return fmt.Errorf("response is nil")
	}
	if err := ptypes.UnmarshalAny(resp.Resources[0], pb); err != nil {
		return fmt.Errorf("unmarshalAny SDS response failed: %v", err)
	}

	expectedResponseSecret := &authapi.Secret{
		Name: testResourceName,
		Type: &authapi.Secret_TlsCertificate{
			TlsCertificate: &authapi.TlsCertificate{
				CertificateChain: &core.DataSource{
					Specifier: &core.DataSource_InlineBytes{
						InlineBytes: expectedCertChain,
					},
				},
				PrivateKey: &core.DataSource{
					Specifier: &core.DataSource_InlineBytes{
						InlineBytes: expectedPrivateKey,
					},
				},
			},
		},
	}
	if !cmp.Equal(pb, expectedResponseSecret, protocmp.Transform()) {
		return fmt.Errorf("verification of SDS response failed: secret key: got %+v, want %+v",
			pb, expectedResponseSecret)
	}
	return nil
}

func verifySDSSResponseForRootCert(t *testing.T, resp *discovery.DiscoveryResponse, expectedRootCert []byte) {
	pb := &authapi.Secret{}
	if err := ptypes.UnmarshalAny(resp.Resources[0], pb); err != nil {
		t.Fatalf("UnmarshalAny SDS response failed: %v", err)
	}

	expectedResponseSecret := &authapi.Secret{
		Name: "ROOTCA",
		Type: &authapi.Secret_ValidationContext{
			ValidationContext: &authapi.CertificateValidationContext{
				TrustedCa: &core.DataSource{
					Specifier: &core.DataSource_InlineBytes{
						InlineBytes: expectedRootCert,
					},
				},
			},
		},
	}
	if !cmp.Equal(pb, expectedResponseSecret, protocmp.Transform()) {
		t.Errorf("secret key: got %+v, want %+v", pb, expectedResponseSecret)
	}
}

func sdsRequestStream(socket string, req *discovery.DiscoveryRequest) (*discovery.DiscoveryResponse, error) {
	conn, err := setupConnection(socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	sdsClient := sds.NewSecretDiscoveryServiceClient(conn)
	header := metadata.Pairs(credentialTokenHeaderKey, fakeToken1)
	ctx := metadata.NewOutgoingContext(context.Background(), header)
	stream, err := sdsClient.StreamSecrets(ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(req)
	if err != nil {
		return nil, err
	}
	res, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	return res, nil
}

func sdsRequestFetch(socket string, req *discovery.DiscoveryRequest) (*discovery.DiscoveryResponse, error) {
	conn, err := setupConnection(socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	sdsClient := sds.NewSecretDiscoveryServiceClient(conn)
	header := metadata.Pairs(credentialTokenHeaderKey, fakeToken1)
	ctx := metadata.NewOutgoingContext(context.Background(), header)
	resp, err := sdsClient.FetchSecrets(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func setupConnection(socket string) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	opts = append(opts, grpc.WithInsecure(), grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}))

	conn, err := grpc.Dial(socket, opts...)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

type mockSecretStore struct {
	checkToken      bool
	secrets         sync.Map
	secretCacheHit  int
	secretCacheMiss int
	mutex           sync.RWMutex
	expectedToken   string
}

func (ms *mockSecretStore) SecretCacheHit() int {
	ms.mutex.RLock()
	defer ms.mutex.RUnlock()
	return ms.secretCacheHit
}

func (ms *mockSecretStore) SecretCacheMiss() int {
	ms.mutex.RLock()
	defer ms.mutex.RUnlock()
	return ms.secretCacheMiss
}

func (ms *mockSecretStore) GenerateSecret(ctx context.Context, conID, resourceName, token string) (*ca2.SecretItem, error) {
	if ms.checkToken && ms.expectedToken != token {
		return nil, fmt.Errorf("unexpected token %q", token)
	}

	key := cache.ConnKey{
		ConnectionID: conID,
		ResourceName: resourceName,
	}
	if resourceName == testResourceName {
		s := &ca2.SecretItem{
			CertificateChain: fakeCertificateChain,
			PrivateKey:       fakePrivateKey,
			ResourceName:     testResourceName,
			Version:          time.Now().Format("01-02 15:04:05.000"),
			Token:            token,
		}
		log.Info("Store secret for key: ", key, ". token: ", token)
		ms.secrets.Store(key, s)
		return s, nil
	}

	if resourceName == cache.RootCertReqResourceName || strings.HasPrefix(resourceName, "file-root:") {
		s := &ca2.SecretItem{
			RootCert:     fakeRootCert,
			ResourceName: cache.RootCertReqResourceName,
			Version:      time.Now().Format("01-02 15:04:05.000"),
			Token:        token,
		}
		log.Info("Store root cert for key: ", key, ". token: ", token)
		ms.secrets.Store(key, s)
		return s, nil
	}

	return nil, fmt.Errorf("unexpected resourceName %q", resourceName)
}

func (ms *mockSecretStore) SecretExist(conID, spiffeID, token, version string) bool {
	ms.mutex.Lock()
	defer ms.mutex.Unlock()
	key := cache.ConnKey{
		ConnectionID: conID,
		ResourceName: spiffeID,
	}
	val, found := ms.secrets.Load(key)
	if !found {
		log.Infof("cannot find secret %v in cache", key)
		ms.secretCacheMiss++
		return false
	}
	cs := val.(*ca2.SecretItem)
	log.Infof("key is: %v. Token: %v", key, cs.Token)
	if spiffeID != cs.ResourceName {
		log.Infof("resource name not match: %s vs %s", spiffeID, cs.ResourceName)
		ms.secretCacheMiss++
		return false
	}
	if token != cs.Token {
		log.Infof("token does not match %+v vs %+v", token, cs.Token)
		ms.secretCacheMiss++
		return false
	}
	if version != cs.Version {
		log.Infof("version does not match %s vs %s", version, cs.Version)
		ms.secretCacheMiss++
		return false
	}
	log.Infof("requested secret matches cache")
	ms.secretCacheHit++
	return true
}

func (ms *mockSecretStore) DeleteSecret(conID, resourceName string) {
	key := cache.ConnKey{
		ConnectionID: conID,
		ResourceName: resourceName,
	}
	ms.secrets.Delete(key)
}

func (ms *mockSecretStore) ShouldWaitForGatewaySecret(connectionID, resourceName, token string, fileMountedCertsOnly bool) bool {
	return false
}

func checkStaledConnCount(t *testing.T) {
	// Manually clear staled clients instead of waiting for ticker.
	clearStaledClients()
	metricName := "total_stale_connections"
	staleConnections, err := util.GetMetricsCounterValue(metricName)
	if err != nil {
		t.Errorf("Failed to get metric value for %s: %v", metricName, err)
	}
	if staleConnections != float64(0) {
		t.Errorf("expect %q to be 0, got %f", metricName, staleConnections)
	}
}
