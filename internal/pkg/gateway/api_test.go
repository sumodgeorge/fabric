/*
Copyright 2021 IBM All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package gateway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	cp "github.com/hyperledger/fabric-protos-go/common"
	dp "github.com/hyperledger/fabric-protos-go/discovery"
	pb "github.com/hyperledger/fabric-protos-go/gateway"
	"github.com/hyperledger/fabric-protos-go/gossip"
	"github.com/hyperledger/fabric-protos-go/msp"
	ab "github.com/hyperledger/fabric-protos-go/orderer"
	"github.com/hyperledger/fabric-protos-go/peer"
	"github.com/hyperledger/fabric/common/crypto/tlsgen"
	"github.com/hyperledger/fabric/gossip/api"
	"github.com/hyperledger/fabric/gossip/common"
	gdiscovery "github.com/hyperledger/fabric/gossip/discovery"
	"github.com/hyperledger/fabric/internal/pkg/gateway/commit"
	"github.com/hyperledger/fabric/internal/pkg/gateway/config"
	"github.com/hyperledger/fabric/internal/pkg/gateway/mocks"
	idmocks "github.com/hyperledger/fabric/internal/pkg/identity/mocks"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The following private interfaces are here purely to prevent counterfeiter creating an import cycle in the unit test
//go:generate counterfeiter -o mocks/endorserclient.go --fake-name EndorserClient . endorserClient
type endorserClient interface {
	peer.EndorserClient
}

//go:generate counterfeiter -o mocks/discovery.go --fake-name Discovery . discovery
type discovery interface {
	Discovery
}

//go:generate counterfeiter -o mocks/abclient.go --fake-name ABClient . abClient
type abClient interface {
	ab.AtomicBroadcastClient
}

//go:generate counterfeiter -o mocks/abbclient.go --fake-name ABBClient . abbClient
type abbClient interface {
	ab.AtomicBroadcast_BroadcastClient
}

//go:generate counterfeiter -o mocks/commitfinder.go --fake-name CommitFinder . commitFinder
type commitFinder interface {
	CommitFinder
}

//go:generate counterfeiter -o mocks/eventer.go --fake-name Eventer . eventer
type eventer interface {
	Eventer
}

//go:generate counterfeiter -o mocks/chaincodeeventsserver.go --fake-name ChaincodeEventsServer github.com/hyperledger/fabric-protos-go/gateway.Gateway_ChaincodeEventsServer

//go:generate counterfeiter -o mocks/aclchecker.go --fake-name ACLChecker . aclChecker
type aclChecker interface {
	ACLChecker
}

type (
	endorsementPlan   map[string][]endorserState
	endorsementLayout map[string]uint32
)

type networkMember struct {
	id       string
	endpoint string
	mspid    string
	height   uint64
}

type endpointDef struct {
	proposalResponseValue   string
	proposalResponseStatus  int32
	proposalResponseMessage string
	proposalError           error
	ordererResponse         string
	ordererStatus           int32
	ordererBroadcastError   error
	ordererSendError        error
	ordererRecvError        error
}

var defaultEndpointDef = &endpointDef{
	proposalResponseValue:  "mock_response",
	proposalResponseStatus: 200,
	ordererResponse:        "mock_orderer_response",
	ordererStatus:          200,
}

const (
	testChannel        = "test_channel"
	testChaincode      = "test_chaincode"
	endorsementTimeout = -1 * time.Second
)

type testDef struct {
	name               string
	plan               endorsementPlan
	layouts            []endorsementLayout
	members            []networkMember
	identity           []byte
	localResponse      string
	errString          string
	errDetails         []*pb.EndpointError
	endpointDefinition *endpointDef
	endorsingOrgs      []string
	postSetup          func(t *testing.T, def *preparedTest)
	expectedEndorsers  []string
	finderStatus       *commit.Status
	finderErr          error
	chaincodeEvents    []*commit.BlockChaincodeEvents
	eventErr           error
	policyErr          error
	expectedResponse   proto.Message
	expectedResponses  []proto.Message
	transientData      map[string][]byte
	interest           *peer.ChaincodeInterest
}

type preparedTest struct {
	server         *Server
	ctx            context.Context
	signedProposal *peer.SignedProposal
	localEndorser  *mocks.EndorserClient
	discovery      *mocks.Discovery
	dialer         *mocks.Dialer
	finder         *mocks.CommitFinder
	eventer        *mocks.Eventer
	eventsServer   *mocks.ChaincodeEventsServer
	policy         *mocks.ACLChecker
}

type contextKey string

var (
	localhostMock    = &endorser{endpointConfig: &endpointConfig{address: "localhost:7051", mspid: "msp1"}}
	peer1Mock        = &endorser{endpointConfig: &endpointConfig{address: "peer1:8051", mspid: "msp1"}}
	peer2Mock        = &endorser{endpointConfig: &endpointConfig{address: "peer2:9051", mspid: "msp2"}}
	peer3Mock        = &endorser{endpointConfig: &endpointConfig{address: "peer3:10051", mspid: "msp2"}}
	peer4Mock        = &endorser{endpointConfig: &endpointConfig{address: "peer4:11051", mspid: "msp3"}}
	unavailable1Mock = &endorser{endpointConfig: &endpointConfig{address: "unavailable1:12051", mspid: "msp1"}}
	unavailable2Mock = &endorser{endpointConfig: &endpointConfig{address: "unavailable1:13051", mspid: "msp1"}}
	unavailable3Mock = &endorser{endpointConfig: &endpointConfig{address: "unavailable1:14051", mspid: "msp1"}}
)

func TestEvaluate(t *testing.T) {
	tests := []testDef{
		{
			name: "single endorser",
			members: []networkMember{
				{"id1", "localhost:7051", "msp1", 5},
			},
		},
		{
			name:      "no endorsers",
			plan:      endorsementPlan{},
			members:   []networkMember{},
			errString: "rpc error: code = Unavailable desc = no endorsing peers found for channel: test_channel",
		},
		{
			name: "five endorsers, prefer local org",
			members: []networkMember{
				{"id1", "localhost:7051", "msp1", 5},
				{"id2", "peer1:8051", "msp1", 6},
				{"id3", "peer2:9051", "msp2", 6},
				{"id4", "peer3:10051", "msp2", 5},
				{"id5", "peer4:11051", "msp3", 6},
			},
			expectedEndorsers: []string{"peer1:8051"},
		},
		{
			name: "five endorsers, prefer host peer",
			members: []networkMember{
				{"id1", "localhost:7051", "msp1", 5},
				{"id2", "peer1:8051", "msp1", 5},
				{"id3", "peer2:9051", "msp2", 6},
				{"id4", "peer3:10051", "msp2", 5},
				{"id5", "peer4:11051", "msp3", 6},
			},
			expectedEndorsers: []string{"localhost:7051"},
		},
		{
			name: "five endorsers, prefer host peer despite no endpoint",
			members: []networkMember{
				{"id1", "", "msp1", 5},
				{"id2", "peer1:8051", "msp1", 5},
				{"id3", "peer2:9051", "msp2", 6},
				{"id4", "peer3:10051", "msp2", 5},
				{"id5", "peer4:11051", "msp3", 6},
			},
			expectedEndorsers: []string{"localhost:7051"},
		},
		{
			name: "evaluate with targetOrganizations, prefer local org despite block height",
			members: []networkMember{
				{"id1", "localhost:7051", "msp1", 5},
				{"id2", "peer1:8051", "msp1", 5},
				{"id3", "peer2:9051", "msp2", 6},
				{"id4", "peer3:10051", "msp2", 5},
				{"id5", "peer4:11051", "msp3", 6},
			},
			endorsingOrgs:     []string{"msp3", "msp1"},
			expectedEndorsers: []string{"localhost:7051"},
		},
		{
			name: "evaluate with targetOrganizations that doesn't include local org, prefer highest block height",
			members: []networkMember{
				{"id1", "localhost:7051", "msp1", 5},
				{"id2", "peer1:8051", "msp1", 5},
				{"id3", "peer2:9051", "msp2", 6},
				{"id4", "peer3:10051", "msp2", 5},
				{"id5", "peer4:11051", "msp3", 7},
			},
			endorsingOrgs:     []string{"msp2", "msp3"},
			expectedEndorsers: []string{"peer4:11051"},
		},
		{
			name: "process proposal fails",
			members: []networkMember{
				{"id1", "localhost:7051", "msp1", 5},
			},
			endpointDefinition: &endpointDef{
				proposalError: status.Error(codes.Aborted, "wibble"),
			},
			errString: "rpc error: code = Aborted desc = failed to evaluate transaction: wibble",
			errDetails: []*pb.EndpointError{{
				Address: "localhost:7051",
				MspId:   "msp1",
				Message: "rpc error: code = Aborted desc = wibble",
			}},
		},
		{
			name: "process proposal chaincode error",
			members: []networkMember{
				{"id2", "peer1:8051", "msp1", 5},
			},
			endpointDefinition: &endpointDef{
				proposalResponseStatus:  400,
				proposalResponseMessage: "Mock chaincode error",
			},
			errString: "rpc error: code = Unknown desc = error 400, Mock chaincode error",
			errDetails: []*pb.EndpointError{{
				Address: "peer1:8051",
				MspId:   "msp1",
				Message: "error 400, Mock chaincode error",
			}},
		},
		{
			name: "dialing endorser endpoint fails",
			members: []networkMember{
				{"id3", "peer2:9051", "msp2", 5},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.dialer.Calls(func(_ context.Context, target string, _ ...grpc.DialOption) (*grpc.ClientConn, error) {
					if target == "peer2:9051" {
						return nil, fmt.Errorf("endorser not answering")
					}
					return nil, nil
				})
			},
			errString: "rpc error: code = Unavailable desc = failed to create new connection: endorser not answering",
		},
		{
			name: "dialing orderer endpoint fails",
			members: []networkMember{
				{"id3", "peer2:9051", "msp2", 5},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.dialer.Calls(func(_ context.Context, target string, _ ...grpc.DialOption) (*grpc.ClientConn, error) {
					if target == "orderer:7050" {
						return nil, fmt.Errorf("orderer not answering")
					}
					return nil, nil
				})
			},
			errString: "rpc error: code = Unavailable desc = failed to create new connection: orderer not answering",
		},
		{
			name: "discovery returns incomplete information - no Properties",
			postSetup: func(t *testing.T, def *preparedTest) {
				def.discovery.PeersOfChannelReturns([]gdiscovery.NetworkMember{{
					Endpoint: "localhost:7051",
					PKIid:    []byte("ill-defined"),
				}})
			},
			errString: "rpc error: code = Unavailable desc = no endorsing peers found for channel: test_channel",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := prepareTest(t, &tt)

			response, err := test.server.Evaluate(test.ctx, &pb.EvaluateRequest{ProposedTransaction: test.signedProposal, TargetOrganizations: tt.endorsingOrgs})

			if tt.errString != "" {
				checkError(t, err, tt.errString, tt.errDetails)
				require.Nil(t, response)
				return
			}

			// test the assertions

			require.NoError(t, err)
			// assert the result is the payload from the proposal response returned by the local endorser
			require.Equal(t, []byte("mock_response"), response.Result.Payload, "Incorrect result")

			// check the correct endorsers (mock) were called with the right parameters
			checkEndorsers(t, tt.expectedEndorsers, test)

			// check the discovery service (mock) was invoked as expected
			expectedChannel := common.ChannelID(testChannel)
			require.Equal(t, 2, test.discovery.PeersOfChannelCallCount())
			channel := test.discovery.PeersOfChannelArgsForCall(0)
			require.Equal(t, expectedChannel, channel)
			channel = test.discovery.PeersOfChannelArgsForCall(1)
			require.Equal(t, expectedChannel, channel)

			require.Equal(t, 1, test.discovery.IdentityInfoCallCount())
		})
	}
}

func TestEndorse(t *testing.T) {
	tests := []testDef{
		{
			name: "two endorsers",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 3}}, // msp1
				"g2": {{endorser: peer2Mock, height: 3}},     // msp2
			},
			expectedEndorsers: []string{"localhost:7051", "peer2:9051"},
		},
		{
			name: "three endorsers, two groups",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 4}},                               // msp1
				"g2": {{endorser: peer3Mock, height: 4}, {endorser: peer2Mock, height: 5}}, // msp2
			},
			expectedEndorsers: []string{"localhost:7051", "peer2:9051"},
		},
		{
			name: "multiple endorsers, two groups, prefer host peer",
			plan: endorsementPlan{
				"g1": {{endorser: peer2Mock, height: 4}, {endorser: localhostMock, height: 4}, {endorser: unavailable1Mock, height: 4}}, // msp1
				"g2": {{endorser: peer3Mock, height: 4}, {endorser: peer2Mock, height: 5}},                                              // msp2
			},
			expectedEndorsers: []string{"localhost:7051", "peer2:9051"},
		},
		{
			name:              "endorse with specified orgs, despite block height",
			endorsingOrgs:     []string{"msp1", "msp3"},
			expectedEndorsers: []string{"localhost:7051", "peer4:11051"},
		},
		{
			name:              "endorse with specified orgs, doesn't include local peer",
			endorsingOrgs:     []string{"msp2", "msp3"},
			expectedEndorsers: []string{"peer2:9051", "peer4:11051"},
		},
		{
			name:          "endorse with specified orgs, but fails to satisfy one org",
			endorsingOrgs: []string{"msp2", "msp4"},
			errString:     "rpc error: code = Unavailable desc = failed to find any endorsing peers for org(s): msp4",
		},
		{
			name:          "endorse with specified orgs, but fails to satisfy two orgs",
			endorsingOrgs: []string{"msp2", "msp4", "msp5"},
			errString:     "rpc error: code = Unavailable desc = failed to find any endorsing peers for org(s): msp4, msp5",
		},
		{
			name: "endorse with multiple layouts - default choice first layout",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 4}, {endorser: peer1Mock, height: 4}}, // msp1
				"g2": {{endorser: peer2Mock, height: 3}, {endorser: peer3Mock, height: 4}},     // msp2
				"g3": {{endorser: peer4Mock, height: 5}},                                       // msp3
			},
			layouts: []endorsementLayout{
				{"g1": 1, "g2": 1},
				{"g1": 1, "g3": 1},
				{"g2": 1, "g3": 1},
			},
			expectedEndorsers: []string{"localhost:7051", "peer3:10051"},
		},
		{
			name: "endorse with multiple layouts - non-availability forces second layout",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 4}, {endorser: peer1Mock, height: 4}},           // msp1
				"g2": {{endorser: unavailable1Mock, height: 3}, {endorser: unavailable2Mock, height: 4}}, // msp2
				"g3": {{endorser: peer4Mock, height: 5}},                                                 // msp3
			},
			layouts: []endorsementLayout{
				{"g1": 1, "g2": 1},
				{"g1": 1, "g3": 1},
				{"g2": 1, "g3": 1},
			},
			expectedEndorsers: []string{"localhost:7051", "peer4:11051"},
		},
		{
			name: "non-local endorsers",
			plan: endorsementPlan{
				"g1": {{endorser: peer2Mock, height: 3}, {endorser: peer3Mock, height: 4}}, // msp2
				"g2": {{endorser: peer4Mock, height: 5}},                                   // msp3
			},
			layouts: []endorsementLayout{
				{"g1": 1, "g2": 1},
			},
			members: []networkMember{
				{"id2", "peer2:9051", "msp2", 3},
				{"id3", "peer3:10051", "msp2", 4},
				{"id4", "peer4:11051", "msp3", 5},
			},
			expectedEndorsers: []string{"peer3:10051", "peer4:11051"},
		},
		{
			name: "local endorser is not in the endorsement plan",
			plan: endorsementPlan{
				"g1": {{endorser: peer2Mock, height: 3}, {endorser: peer3Mock, height: 4}}, // msp2
				"g2": {{endorser: peer4Mock, height: 5}},                                   // msp3
			},
			layouts: []endorsementLayout{
				{"g1": 1, "g2": 1},
			},
			members: []networkMember{
				{"id1", "localhost:7051", "msp1", 3},
				{"id2", "peer2:9051", "msp2", 3},
				{"id3", "peer3:10051", "msp2", 4},
				{"id4", "peer4:11051", "msp3", 5},
			},
			expectedEndorsers: []string{"peer3:10051", "peer4:11051"},
		},
		{
			name: "non-local endorsers with transient data will fail",
			plan: endorsementPlan{
				"g1": {{endorser: peer2Mock, height: 3}, {endorser: peer3Mock, height: 4}}, // msp2
				"g2": {{endorser: peer4Mock, height: 5}},                                   // msp3
			},
			members: []networkMember{
				{"id2", "peer2:9051", "msp2", 3},
				{"id3", "peer3:10051", "msp2", 4},
				{"id4", "peer4:11051", "msp3", 5},
			},
			transientData: map[string][]byte{"transient-key": []byte("transient-value")},
			errString:     "rpc error: code = FailedPrecondition desc = no endorsers found in the gateway's organization; retry specifying endorsing organization(s) to protect transient data",
		},
		{
			name: "extra endorsers with transient data",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 4}, {endorser: peer1Mock, height: 4}}, // msp1
				"g2": {{endorser: peer4Mock, height: 5}},                                       // msp3
			},
			transientData:     map[string][]byte{"transient-key": []byte("transient-value")},
			expectedEndorsers: []string{"localhost:7051", "peer4:11051"},
		},
		{
			name: "non-local endorsers with transient data and set endorsing orgs",
			plan: endorsementPlan{
				"g1": {{endorser: peer2Mock, height: 3}, {endorser: peer3Mock, height: 4}}, // msp2
				"g2": {{endorser: peer4Mock, height: 5}},                                   // msp3
			},
			members: []networkMember{
				{"id2", "peer2:9051", "msp2", 3},
				{"id3", "peer3:10051", "msp2", 4},
				{"id4", "peer4:11051", "msp3", 5},
			},
			endorsingOrgs:     []string{"msp2", "msp3"},
			transientData:     map[string][]byte{"transient-key": []byte("transient-value")},
			expectedEndorsers: []string{"peer3:10051", "peer4:11051"},
		},
		{
			name: "endorse with multiple layouts - non-availability of peers fails on all layouts",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 4}, {endorser: peer1Mock, height: 4}},           // msp1
				"g2": {{endorser: unavailable1Mock, height: 3}, {endorser: unavailable2Mock, height: 4}}, // msp2
				"g3": {{endorser: unavailable3Mock, height: 5}},                                          // msp3
			},
			layouts: []endorsementLayout{
				{"g1": 1, "g2": 1},
				{"g1": 1, "g3": 1},
				{"g2": 1, "g3": 1},
			},
			errString: "rpc error: code = Unavailable desc = failed to select a set of endorsers that satisfy the endorsement policy",
		},
		{
			name: "non-matching responses",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 4}}, // msp1
				"g2": {{endorser: peer2Mock, height: 5}},     // msp2
			},
			localResponse: "different_response",
			errString:     "rpc error: code = Aborted desc = failed to assemble transaction: ProposalResponsePayloads do not match",
		},
		{
			name: "discovery fails",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 2}},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.discovery.PeersForEndorsementReturns(nil, fmt.Errorf("peach-melba"))
			},
			errString: "rpc error: code = Unavailable desc = discovery service failed to build endorsement plan: peach-melba",
		},
		{
			name: "discovery returns incomplete protos - nil layout",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 2}},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				ed := &dp.EndorsementDescriptor{
					Chaincode: "my_channel",
					Layouts:   []*dp.Layout{nil},
				}
				def.discovery.PeersForEndorsementReturns(ed, nil)
			},
			errString: "rpc error: code = Unavailable desc = failed to select a set of endorsers that satisfy the endorsement policy",
		},
		{
			name: "discovery returns incomplete protos - nil state info",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 2}},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				ed := &dp.EndorsementDescriptor{
					Chaincode:         "my_channel",
					Layouts:           []*dp.Layout{{QuantitiesByGroup: map[string]uint32{"g1": 1}}},
					EndorsersByGroups: map[string]*dp.Peers{"g1": {Peers: []*dp.Peer{{StateInfo: nil}}}},
				}
				def.discovery.PeersForEndorsementReturns(ed, nil)
			},
			errString: "rpc error: code = Unavailable desc = failed to select a set of endorsers that satisfy the endorsement policy",
		},
		{
			name: "process proposal fails",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 1}},
			},
			endpointDefinition: &endpointDef{
				proposalError: status.Error(codes.Aborted, "wibble"),
			},
			errString: "rpc error: code = Aborted desc = failed to endorse transaction: wibble",
			errDetails: []*pb.EndpointError{{
				Address: "localhost:7051",
				MspId:   "msp1",
				Message: "rpc error: code = Aborted desc = wibble",
			}},
		},
		{
			name: "local endorser succeeds, remote endorser fails",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 1}},
				"g2": {{endorser: peer4Mock, height: 1}},
			},
			endpointDefinition: &endpointDef{
				proposalError: status.Error(codes.Aborted, "remote-wobble"),
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.localEndorser.ProcessProposalReturns(createProposalResponse(t, localhostMock.address, "all_good", 200, ""), nil)
			},
			errString: "rpc error: code = Aborted desc = failed to endorse transaction: [{address:\"peer4:11051\" msp_id:\"msp3\" message:\"rpc error: code = Aborted desc = remote-wobble\" }]",
			errDetails: []*pb.EndpointError{{
				Address: "peer4:11051",
				MspId:   "msp3",
				Message: "rpc error: code = Aborted desc = remote-wobble",
			}},
		},
		{
			name: "process proposal chaincode error",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 2}},
			},
			endpointDefinition: &endpointDef{
				proposalResponseStatus:  400,
				proposalResponseMessage: "Mock chaincode error",
			},
			errString: "rpc error: code = Aborted desc = failed to endorse transaction: [{address:\"localhost:7051\" msp_id:\"msp1\" message:\"error 400, Mock chaincode error\" }]",
			errDetails: []*pb.EndpointError{{
				Address: "localhost:7051",
				MspId:   "msp1",
				Message: "error 400, Mock chaincode error",
			}},
		},
		{
			name: "local endorser succeeds, remote endorser chaincode error",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 1}},
				"g2": {{endorser: peer4Mock, height: 1}},
			},
			endpointDefinition: &endpointDef{
				proposalResponseStatus:  400,
				proposalResponseMessage: "Mock chaincode error",
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.localEndorser.ProcessProposalReturns(createProposalResponse(t, localhostMock.address, "all_good", 200, ""), nil)
			},
			errString: "rpc error: code = Aborted desc = failed to endorse transaction: [{address:\"peer4:11051\" msp_id:\"msp3\" message:\"error 400, Mock chaincode error\" }]",
			errDetails: []*pb.EndpointError{{
				Address: "peer4:11051",
				MspId:   "msp3",
				Message: "error 400, Mock chaincode error",
			}},
		},
		{
			name: "first endorser returns chaincode interest",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 3}},
				"g2": {{endorser: peer2Mock, height: 3}},
			},
			interest: &peer.ChaincodeInterest{
				Chaincodes: []*peer.ChaincodeCall{{
					Name:            testChaincode,
					CollectionNames: []string{"mycollection1", "mycollection2"},
					NoPrivateReads:  true,
				}},
			},
			expectedEndorsers: []string{"localhost:7051", "peer2:9051"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := prepareTest(t, &tt)

			response, err := test.server.Endorse(test.ctx, &pb.EndorseRequest{ProposedTransaction: test.signedProposal, EndorsingOrganizations: tt.endorsingOrgs})

			if tt.errString != "" {
				checkError(t, err, tt.errString, tt.errDetails)
				require.Nil(t, response)
				return
			}

			// test the assertions
			require.NoError(t, err)
			// assert the preparedTxn is the payload from the proposal response
			require.Equal(t, []byte("mock_response"), response.Result.Payload, "Incorrect response")

			// check the generated transaction envelope contains the correct endorsements
			checkTransaction(t, tt.expectedEndorsers, response.PreparedTransaction)

			// check the correct endorsers (mocks) were called with the right parameters
			checkEndorsers(t, tt.expectedEndorsers, test)
		})
	}
}

func TestSubmit(t *testing.T) {
	tests := []testDef{
		{
			name: "two endorsers",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock, height: 3}},
				"g2": {{endorser: peer1Mock, height: 3}},
			},
		},
		{
			name: "discovery fails",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock}},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.discovery.ConfigReturnsOnCall(1, nil, fmt.Errorf("jabberwocky"))
			},
			errString: "rpc error: code = Unavailable desc = jabberwocky",
		},
		{
			name: "no orderers",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock}},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.discovery.ConfigReturns(&dp.ConfigResult{
					Orderers: map[string]*dp.Endpoints{},
					Msps:     map[string]*msp.FabricMSPConfig{},
				}, nil)
			},
			errString: "rpc error: code = Unavailable desc = no broadcastClients discovered",
		},
		{
			name: "orderer broadcast fails",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock}},
			},
			endpointDefinition: &endpointDef{
				proposalResponseStatus: 200,
				ordererBroadcastError:  status.Error(codes.FailedPrecondition, "Orderer not listening!"),
			},
			errString: "rpc error: code = FailedPrecondition desc = failed to create BroadcastClient: Orderer not listening!",
			errDetails: []*pb.EndpointError{{
				Address: "orderer:7050",
				MspId:   "msp1",
				Message: "rpc error: code = FailedPrecondition desc = Orderer not listening!",
			}},
		},
		{
			name: "send to orderer fails",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock}},
			},
			endpointDefinition: &endpointDef{
				proposalResponseStatus: 200,
				ordererSendError:       status.Error(codes.Internal, "Orderer says no!"),
			},
			errString: "rpc error: code = Internal desc = failed to send transaction to orderer: Orderer says no!",
			errDetails: []*pb.EndpointError{{
				Address: "orderer:7050",
				MspId:   "msp1",
				Message: "rpc error: code = Internal desc = Orderer says no!",
			}},
		},
		{
			name: "receive from orderer fails",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock}},
			},
			endpointDefinition: &endpointDef{
				proposalResponseStatus: 200,
				ordererRecvError:       status.Error(codes.FailedPrecondition, "Orderer not happy!"),
			},
			errString: "rpc error: code = FailedPrecondition desc = failed to receive response from orderer: Orderer not happy!",
			errDetails: []*pb.EndpointError{{
				Address: "orderer:7050",
				MspId:   "msp1",
				Message: "rpc error: code = FailedPrecondition desc = Orderer not happy!",
			}},
		},
		{
			name: "orderer Send() returns nil",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock}},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.server.registry.endpointFactory.connectOrderer = func(_ *grpc.ClientConn) ab.AtomicBroadcastClient {
					abc := &mocks.ABClient{}
					abbc := &mocks.ABBClient{}
					abbc.RecvReturns(nil, nil)
					abc.BroadcastReturns(abbc, nil)
					return abc
				}
			},
			errString: "rpc error: code = Aborted desc = received nil response from orderer",
		},
		{
			name: "orderer returns unsuccessful response",
			plan: endorsementPlan{
				"g1": {{endorser: localhostMock}},
			},
			postSetup: func(t *testing.T, def *preparedTest) {
				def.server.registry.endpointFactory.connectOrderer = func(_ *grpc.ClientConn) ab.AtomicBroadcastClient {
					abc := &mocks.ABClient{}
					abbc := &mocks.ABBClient{}
					response := &ab.BroadcastResponse{
						Status: cp.Status_BAD_REQUEST,
					}
					abbc.RecvReturns(response, nil)
					abc.BroadcastReturns(abbc, nil)
					return abc
				}
			},
			errString: "rpc error: code = Aborted desc = received unsuccessful response from orderer: " + cp.Status_name[int32(cp.Status_BAD_REQUEST)],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := prepareTest(t, &tt)

			// first call endorse to prepare the tx
			endorseResponse, err := test.server.Endorse(test.ctx, &pb.EndorseRequest{ProposedTransaction: test.signedProposal})
			require.NoError(t, err)

			preparedTx := endorseResponse.GetPreparedTransaction()

			// sign the envelope
			preparedTx.Signature = []byte("mysignature")

			// submit
			submitResponse, err := test.server.Submit(test.ctx, &pb.SubmitRequest{PreparedTransaction: preparedTx})

			if tt.errString != "" {
				checkError(t, err, tt.errString, tt.errDetails)
				require.Nil(t, submitResponse)
				return
			}

			require.NoError(t, err)
			require.True(t, proto.Equal(&pb.SubmitResponse{}, submitResponse), "Incorrect response")
		})
	}
}

func TestSubmitUnsigned(t *testing.T) {
	server := &Server{}
	req := &pb.SubmitRequest{
		TransactionId:       "transaction-id",
		ChannelId:           "channel-id",
		PreparedTransaction: &cp.Envelope{},
	}
	_, err := server.Submit(context.Background(), req)
	require.Error(t, err)
	require.Equal(t, err, status.Error(codes.InvalidArgument, "prepared transaction must be signed"))
}

func TestCommitStatus(t *testing.T) {
	tests := []testDef{
		{
			name:      "error finding transaction status",
			finderErr: errors.New("FINDER_ERROR"),
			errString: "rpc error: code = FailedPrecondition desc = FINDER_ERROR",
		},
		{
			name: "returns transaction status",
			finderStatus: &commit.Status{
				Code:        peer.TxValidationCode_MVCC_READ_CONFLICT,
				BlockNumber: 101,
			},
			expectedResponse: &pb.CommitStatusResponse{
				Result:      peer.TxValidationCode_MVCC_READ_CONFLICT,
				BlockNumber: 101,
			},
		},
		{
			name: "passes channel name to finder",
			postSetup: func(t *testing.T, test *preparedTest) {
				test.finder.TransactionStatusCalls(func(ctx context.Context, channelName string, transactionID string) (*commit.Status, error) {
					require.Equal(t, testChannel, channelName)
					status := &commit.Status{
						Code:        peer.TxValidationCode_MVCC_READ_CONFLICT,
						BlockNumber: 101,
					}
					return status, nil
				})
			},
		},
		{
			name: "passes transaction ID to finder",
			postSetup: func(t *testing.T, test *preparedTest) {
				test.finder.TransactionStatusCalls(func(ctx context.Context, channelName string, transactionID string) (*commit.Status, error) {
					require.Equal(t, "TX_ID", transactionID)
					status := &commit.Status{
						Code:        peer.TxValidationCode_MVCC_READ_CONFLICT,
						BlockNumber: 101,
					}
					return status, nil
				})
			},
		},
		{
			name:      "failed policy or signature check",
			policyErr: errors.New("POLICY_ERROR"),
			errString: "rpc error: code = PermissionDenied desc = POLICY_ERROR",
		},
		{
			name: "passes channel name to policy checker",
			postSetup: func(t *testing.T, test *preparedTest) {
				test.policy.CheckACLCalls(func(policyName string, channelName string, data interface{}) error {
					require.Equal(t, testChannel, channelName)
					return nil
				})
			},
			finderStatus: &commit.Status{
				Code:        peer.TxValidationCode_MVCC_READ_CONFLICT,
				BlockNumber: 101,
			},
		},
		{
			name:     "passes identity to policy checker",
			identity: []byte("IDENTITY"),
			postSetup: func(t *testing.T, test *preparedTest) {
				test.policy.CheckACLCalls(func(policyName string, channelName string, data interface{}) error {
					require.IsType(t, &protoutil.SignedData{}, data)
					signedData := data.(*protoutil.SignedData)
					require.Equal(t, []byte("IDENTITY"), signedData.Identity)
					return nil
				})
			},
			finderStatus: &commit.Status{
				Code:        peer.TxValidationCode_MVCC_READ_CONFLICT,
				BlockNumber: 101,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := prepareTest(t, &tt)

			request := &pb.CommitStatusRequest{
				ChannelId:     testChannel,
				Identity:      tt.identity,
				TransactionId: "TX_ID",
			}
			requestBytes, err := proto.Marshal(request)
			require.NoError(t, err)

			signedRequest := &pb.SignedCommitStatusRequest{
				Request:   requestBytes,
				Signature: []byte{},
			}

			response, err := test.server.CommitStatus(test.ctx, signedRequest)

			if tt.errString != "" {
				checkError(t, err, tt.errString, tt.errDetails)
				require.Nil(t, response)
				return
			}

			require.NoError(t, err)
			if tt.expectedResponse != nil {
				require.True(t, proto.Equal(tt.expectedResponse, response), "incorrect response", response)
			}
		})
	}
}

func TestChaincodeEvents(t *testing.T) {
	closedEventsChannel := make(chan *commit.BlockChaincodeEvents)
	close(closedEventsChannel)

	tests := []testDef{
		{
			name:      "error establishing event reading",
			eventErr:  errors.New("EVENT_ERROR"),
			errString: "rpc error: code = FailedPrecondition desc = EVENT_ERROR",
		},
		{
			name: "returns chaincode events",
			chaincodeEvents: []*commit.BlockChaincodeEvents{
				{
					BlockNumber: 101,
					Events: []*peer.ChaincodeEvent{
						{
							ChaincodeId: testChaincode,
							TxId:        "TX_ID",
							EventName:   "EVENT_NAME",
							Payload:     []byte("PAYLOAD"),
						},
					},
				},
			},
			expectedResponses: []proto.Message{
				&pb.ChaincodeEventsResponse{
					BlockNumber: 101,
					Events: []*peer.ChaincodeEvent{
						{
							ChaincodeId: testChaincode,
							TxId:        "TX_ID",
							EventName:   "EVENT_NAME",
							Payload:     []byte("PAYLOAD"),
						},
					},
				},
			},
		},
		{
			name: "passes channel name to eventer",
			postSetup: func(t *testing.T, test *preparedTest) {
				test.eventer.ChaincodeEventsCalls(func(ctx context.Context, channelName string, chaincodeName string) (<-chan *commit.BlockChaincodeEvents, error) {
					require.Equal(t, testChannel, channelName)
					return closedEventsChannel, nil
				})
			},
		},
		{
			name: "passes chaincode ID to eventer",
			postSetup: func(t *testing.T, test *preparedTest) {
				test.eventer.ChaincodeEventsCalls(func(ctx context.Context, channelName string, chaincodeName string) (<-chan *commit.BlockChaincodeEvents, error) {
					require.Equal(t, testChaincode, chaincodeName)
					return closedEventsChannel, nil
				})
			},
		},
		{
			name: "returns error from send to client",
			chaincodeEvents: []*commit.BlockChaincodeEvents{
				{
					BlockNumber: 101,
					Events: []*peer.ChaincodeEvent{
						{
							ChaincodeId: testChaincode,
							TxId:        "TX_ID",
							EventName:   "EVENT_NAME",
							Payload:     []byte("PAYLOAD"),
						},
					},
				},
			},
			errString: "rpc error: code = Aborted desc = SEND_ERROR",
			postSetup: func(t *testing.T, test *preparedTest) {
				test.eventsServer.SendReturns(status.Error(codes.Aborted, "SEND_ERROR"))
			},
		},
		{
			name:      "failed policy or signature check",
			policyErr: errors.New("POLICY_ERROR"),
			errString: "rpc error: code = PermissionDenied desc = POLICY_ERROR",
		},
		{
			name: "passes channel name to policy checker",
			postSetup: func(t *testing.T, test *preparedTest) {
				test.policy.CheckACLCalls(func(policyName string, channelName string, data interface{}) error {
					require.Equal(t, testChannel, channelName)
					return nil
				})
			},
		},
		{
			name:     "passes identity to policy checker",
			identity: []byte("IDENTITY"),
			postSetup: func(t *testing.T, test *preparedTest) {
				test.policy.CheckACLCalls(func(policyName string, channelName string, data interface{}) error {
					require.IsType(t, &protoutil.SignedData{}, data)
					signedData := data.(*protoutil.SignedData)
					require.Equal(t, []byte("IDENTITY"), signedData.Identity)
					return nil
				})
			},
		},
		{
			name:      "error when no more events can be read",
			errString: "rpc error: code = Unavailable desc = failed to read events",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			test := prepareTest(t, &tt)

			request := &pb.ChaincodeEventsRequest{
				ChannelId:   testChannel,
				Identity:    tt.identity,
				ChaincodeId: testChaincode,
			}
			requestBytes, err := proto.Marshal(request)
			require.NoError(t, err)

			signedRequest := &pb.SignedChaincodeEventsRequest{
				Request:   requestBytes,
				Signature: []byte{},
			}

			err = test.server.ChaincodeEvents(signedRequest, test.eventsServer)

			if tt.errString != "" {
				checkError(t, err, tt.errString, tt.errDetails)
				return
			}

			for i, expectedResponse := range tt.expectedResponses {
				actualResponse := test.eventsServer.SendArgsForCall(i)
				require.True(t, proto.Equal(expectedResponse, actualResponse))
			}
		})
	}
}

func TestNilArgs(t *testing.T) {
	server := newServer(
		&mocks.EndorserClient{},
		&mocks.Discovery{},
		&mocks.CommitFinder{},
		&mocks.Eventer{},
		&mocks.ACLChecker{},
		common.PKIidType("id1"),
		"localhost:7051",
		"msp1",
		config.GetOptions(viper.New()),
	)
	ctx := context.Background()

	_, err := server.Evaluate(ctx, nil)
	require.ErrorIs(t, err, status.Error(codes.InvalidArgument, "an evaluate request is required"))

	_, err = server.Evaluate(ctx, &pb.EvaluateRequest{ProposedTransaction: nil})
	require.ErrorIs(t, err, status.Error(codes.InvalidArgument, "failed to unpack transaction proposal: a signed proposal is required"))

	_, err = server.Endorse(ctx, nil)
	require.ErrorIs(t, err, status.Error(codes.InvalidArgument, "an endorse request is required"))

	_, err = server.Endorse(ctx, &pb.EndorseRequest{ProposedTransaction: nil})
	require.ErrorIs(t, err, status.Error(codes.InvalidArgument, "the proposed transaction must contain a signed proposal"))

	_, err = server.Endorse(ctx, &pb.EndorseRequest{ProposedTransaction: &peer.SignedProposal{ProposalBytes: []byte("jibberish")}})
	require.ErrorContains(t, err, "rpc error: code = InvalidArgument desc = failed to unpack transaction proposal: error unmarshalling Proposal")

	_, err = server.Submit(ctx, nil)
	require.ErrorIs(t, err, status.Error(codes.InvalidArgument, "a submit request is required"))

	_, err = server.CommitStatus(ctx, nil)
	require.ErrorIs(t, err, status.Error(codes.InvalidArgument, "a commit status request is required"))
}

func TestRpcErrorWithBadDetails(t *testing.T) {
	err := rpcError(codes.InvalidArgument, "terrible error", nil)
	require.ErrorIs(t, err, status.Error(codes.InvalidArgument, "terrible error"))
}

func prepareTest(t *testing.T, tt *testDef) *preparedTest {
	localEndorser := &mocks.EndorserClient{}
	localResponse := tt.localResponse
	if localResponse == "" {
		localResponse = "mock_response"
	}
	epDef := tt.endpointDefinition
	if epDef == nil {
		epDef = defaultEndpointDef
	}
	if epDef.proposalError != nil {
		localEndorser.ProcessProposalReturns(nil, epDef.proposalError)
	} else {
		localEndorser.ProcessProposalReturns(createProposalResponseWithInterest(t, localhostMock.address, localResponse, epDef.proposalResponseStatus, epDef.proposalResponseMessage, tt.interest), nil)
	}

	mockSigner := &idmocks.SignerSerializer{}
	mockSigner.SignReturns([]byte("my_signature"), nil)

	mockFinder := &mocks.CommitFinder{}
	mockFinder.TransactionStatusReturns(tt.finderStatus, tt.finderErr)

	eventChannel := make(chan *commit.BlockChaincodeEvents, len(tt.chaincodeEvents))
	for _, event := range tt.chaincodeEvents {
		eventChannel <- event
	}
	close(eventChannel)
	mockEventer := &mocks.Eventer{}
	mockEventer.ChaincodeEventsReturns(eventChannel, tt.eventErr)

	mockPolicy := &mocks.ACLChecker{}
	mockPolicy.CheckACLReturns(tt.policyErr)

	validProposal := createProposal(t, testChannel, testChaincode, tt.transientData)
	validSignedProposal, err := protoutil.GetSignedProposal(validProposal, mockSigner)
	require.NoError(t, err)

	ca, err := tlsgen.NewCA()
	require.NoError(t, err)
	configResult := &dp.ConfigResult{
		Orderers: map[string]*dp.Endpoints{
			"msp1": {
				Endpoint: []*dp.Endpoint{
					{Host: "orderer", Port: 7050},
				},
			},
		},
		Msps: map[string]*msp.FabricMSPConfig{
			"msp1": {
				TlsRootCerts: [][]byte{ca.CertBytes()},
			},
		},
	}

	members := []networkMember{
		{"id1", "localhost:7051", "msp1", 0},
		{"id2", "peer1:8051", "msp1", 0},
		{"id3", "peer2:9051", "msp2", 0},
		{"id4", "peer3:10051", "msp2", 0},
		{"id5", "peer4:11051", "msp3", 0},
	}

	if tt.members != nil {
		members = tt.members
	}

	disc := mockDiscovery(t, tt.plan, tt.layouts, members, configResult)

	options := config.Options{
		Enabled:            true,
		EndorsementTimeout: endorsementTimeout,
	}

	server := newServer(localEndorser, disc, mockFinder, mockEventer, mockPolicy, common.PKIidType("id1"), "localhost:7051", "msp1", options)

	dialer := &mocks.Dialer{}
	dialer.Returns(nil, nil)
	server.registry.endpointFactory = createEndpointFactory(t, epDef, dialer.Spy)

	require.NoError(t, err, "Failed to sign the proposal")
	ctx := context.WithValue(context.Background(), contextKey("orange"), "apples")

	pt := &preparedTest{
		server:         server,
		ctx:            ctx,
		signedProposal: validSignedProposal,
		localEndorser:  localEndorser,
		discovery:      disc,
		dialer:         dialer,
		finder:         mockFinder,
		eventer:        mockEventer,
		eventsServer:   &mocks.ChaincodeEventsServer{},
		policy:         mockPolicy,
	}
	if tt.postSetup != nil {
		tt.postSetup(t, pt)
	}
	return pt
}

func checkError(t *testing.T, err error, errString string, details []*pb.EndpointError) {
	require.EqualError(t, err, errString)
	s, ok := status.FromError(err)
	require.True(t, ok, "Expected a gRPC status error")
	require.Len(t, s.Details(), len(details))
	for i, detail := range details {
		require.Equal(t, detail.Message, s.Details()[i].(*pb.EndpointError).Message)
		require.Equal(t, detail.MspId, s.Details()[i].(*pb.EndpointError).MspId)
		require.Equal(t, detail.Address, s.Details()[i].(*pb.EndpointError).Address)
	}
}

func checkEndorsers(t *testing.T, endorsers []string, test *preparedTest) {
	// check the correct endorsers (mock) were called with the right parameters
	if endorsers == nil {
		endorsers = []string{"localhost:7051"}
	}
	for _, e := range endorsers {
		var ec *mocks.EndorserClient
		if e == test.server.registry.localEndorser.address {
			ec = test.localEndorser
		} else {
			ec = test.server.registry.remoteEndorsers[e].client.(*mocks.EndorserClient)
		}
		require.Equal(t, 1, ec.ProcessProposalCallCount(), "Expected ProcessProposal() to be invoked on %s", e)
		ectx, prop, _ := ec.ProcessProposalArgsForCall(0)
		require.Equal(t, test.signedProposal, prop)
		require.Equal(t, "apples", ectx.Value(contextKey("orange")))
		// context timeout was set to -1s, so deadline should be in the past
		deadline, ok := ectx.Deadline()
		require.True(t, ok)
		require.Negative(t, time.Until(deadline))
	}
}

func checkTransaction(t *testing.T, expectedEndorsers []string, transaction *cp.Envelope) {
	// check the prepared transaction contains the correct endorsements
	var actualEndorsers []string

	payload, err := protoutil.UnmarshalPayload(transaction.GetPayload())
	require.NoError(t, err)
	txn, err := protoutil.UnmarshalTransaction(payload.GetData())
	require.NoError(t, err)
	for _, action := range txn.GetActions() {
		cap, err := protoutil.UnmarshalChaincodeActionPayload(action.GetPayload())
		require.NoError(t, err)
		for _, endorsement := range cap.GetAction().GetEndorsements() {
			actualEndorsers = append(actualEndorsers, string(endorsement.GetEndorser()))
		}
	}

	require.ElementsMatch(t, expectedEndorsers, actualEndorsers)
}

func mockDiscovery(t *testing.T, plan endorsementPlan, layouts []endorsementLayout, members []networkMember, config *dp.ConfigResult) *mocks.Discovery {
	discovery := &mocks.Discovery{}

	var peers []gdiscovery.NetworkMember
	var infoset []api.PeerIdentityInfo
	for _, member := range members {
		peers = append(peers, gdiscovery.NetworkMember{
			Endpoint:   member.endpoint,
			PKIid:      []byte(member.id),
			Properties: &gossip.Properties{Chaincodes: []*gossip.Chaincode{{Name: testChaincode}}, LedgerHeight: member.height},
		})
		infoset = append(infoset, api.PeerIdentityInfo{Organization: []byte(member.mspid), PKIId: []byte(member.id)})
	}
	ed := createMockEndorsementDescriptor(t, plan, layouts)
	discovery.PeersForEndorsementReturns(ed, nil)
	discovery.PeersOfChannelReturns(peers)
	discovery.IdentityInfoReturns(infoset)
	discovery.ConfigReturns(config, nil)
	return discovery
}

func createMockEndorsementDescriptor(t *testing.T, plan endorsementPlan, layouts []endorsementLayout) *dp.EndorsementDescriptor {
	quantitiesByGroup := map[string]uint32{}
	endorsersByGroups := map[string]*dp.Peers{}
	for group, endorsers := range plan {
		quantitiesByGroup[group] = 1 // for now
		var peers []*dp.Peer
		for _, endorser := range endorsers {
			peers = append(peers, createMockPeer(t, &endorser))
		}
		endorsersByGroups[group] = &dp.Peers{Peers: peers}
	}
	var layoutDef []*dp.Layout
	if layouts != nil {
		for _, layout := range layouts {
			layoutDef = append(layoutDef, &dp.Layout{QuantitiesByGroup: layout})
		}
	} else {
		// default single layout - one from each group
		layoutDef = []*dp.Layout{{QuantitiesByGroup: quantitiesByGroup}}
	}
	descriptor := &dp.EndorsementDescriptor{
		Chaincode:         "my_channel",
		Layouts:           layoutDef,
		EndorsersByGroups: endorsersByGroups,
	}
	return descriptor
}

func createMockPeer(t *testing.T, endorser *endorserState) *dp.Peer {
	aliveMsgBytes, err := proto.Marshal(
		&gossip.GossipMessage{
			Content: &gossip.GossipMessage_AliveMsg{
				AliveMsg: &gossip.AliveMessage{
					Membership: &gossip.Member{Endpoint: endorser.endorser.address},
				},
			},
		})

	require.NoError(t, err)

	stateInfoBytes, err := proto.Marshal(
		&gossip.GossipMessage{
			Content: &gossip.GossipMessage_StateInfo{
				StateInfo: &gossip.StateInfo{
					Properties: &gossip.Properties{
						LedgerHeight: endorser.height,
					},
				},
			},
		})

	require.NoError(t, err)

	return &dp.Peer{
		StateInfo: &gossip.Envelope{
			Payload: stateInfoBytes,
		},
		MembershipInfo: &gossip.Envelope{
			Payload: aliveMsgBytes,
		},
		Identity: marshal(&msp.SerializedIdentity{
			IdBytes: []byte(endorser.endorser.address),
			Mspid:   endorser.endorser.mspid,
		}, t),
	}
}

func createEndpointFactory(t *testing.T, definition *endpointDef, dialer dialer) *endpointFactory {
	var endpoint string
	return &endpointFactory{
		timeout: 5 * time.Second,
		connectEndorser: func(conn *grpc.ClientConn) peer.EndorserClient {
			e := &mocks.EndorserClient{}
			if definition.proposalError != nil {
				e.ProcessProposalReturns(nil, definition.proposalError)
			} else {
				e.ProcessProposalReturns(createProposalResponse(t, endpoint, definition.proposalResponseValue, definition.proposalResponseStatus, definition.proposalResponseMessage), nil)
			}
			return e
		},
		connectOrderer: func(_ *grpc.ClientConn) ab.AtomicBroadcastClient {
			abc := &mocks.ABClient{}
			if definition.ordererBroadcastError != nil {
				abc.BroadcastReturns(nil, definition.ordererBroadcastError)
				return abc
			}
			abbc := &mocks.ABBClient{}
			abbc.SendReturns(definition.ordererSendError)
			abbc.RecvReturns(&ab.BroadcastResponse{
				Info:   definition.ordererResponse,
				Status: cp.Status(definition.ordererStatus),
			}, definition.ordererRecvError)
			abc.BroadcastReturns(abbc, nil)
			return abc
		},
		dialer: func(ctx context.Context, target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
			endpoint = target
			return dialer(ctx, target, opts...)
		},
	}
}

func createProposal(t *testing.T, channel string, chaincode string, transient map[string][]byte, args ...[]byte) *peer.Proposal {
	invocationSpec := &peer.ChaincodeInvocationSpec{
		ChaincodeSpec: &peer.ChaincodeSpec{
			Type:        peer.ChaincodeSpec_NODE,
			ChaincodeId: &peer.ChaincodeID{Name: chaincode},
			Input:       &peer.ChaincodeInput{Args: args},
		},
	}

	proposal, _, err := protoutil.CreateChaincodeProposalWithTransient(
		cp.HeaderType_ENDORSER_TRANSACTION,
		channel,
		invocationSpec,
		[]byte{},
		transient,
	)

	require.NoError(t, err, "Failed to create the proposal")

	return proposal
}

func createProposalResponse(t *testing.T, endorser, value string, status int32, errMessage string) *peer.ProposalResponse {
	response := &peer.Response{
		Status:  status,
		Payload: []byte(value),
		Message: errMessage,
	}
	action := &peer.ChaincodeAction{
		Response: response,
	}
	payload := &peer.ProposalResponsePayload{
		ProposalHash: []byte{},
		Extension:    marshal(action, t),
	}
	endorsement := &peer.Endorsement{
		Endorser: []byte(endorser),
	}

	return &peer.ProposalResponse{
		Payload:     marshal(payload, t),
		Response:    response,
		Endorsement: endorsement,
	}
}

func createProposalResponseWithInterest(t *testing.T, endorser, value string, status int32, errMessage string, interest *peer.ChaincodeInterest) *peer.ProposalResponse {
	response := createProposalResponse(t, endorser, value, status, errMessage)
	if interest != nil {
		response.Interest = interest
	}
	return response
}

func marshal(msg proto.Message, t *testing.T) []byte {
	buf, err := proto.Marshal(msg)
	require.NoError(t, err, "Failed to marshal message")
	return buf
}
