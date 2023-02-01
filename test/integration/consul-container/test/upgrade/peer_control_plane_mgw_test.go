package upgrade

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/api"
	libassert "github.com/hashicorp/consul/test/integration/consul-container/libs/assert"
	libservice "github.com/hashicorp/consul/test/integration/consul-container/libs/service"
	libtopology "github.com/hashicorp/consul/test/integration/consul-container/libs/topology"
	"github.com/hashicorp/consul/test/integration/consul-container/libs/utils"
)

// TestPeering_Upgrade_ControlPlane_MGW verifies the peering control plane traffic go through the mesh gateway
// PeerThroughMeshGateways can be inheritted by the upgraded cluster.
//
// 1. Create the basic peering topology of one dialing cluster and one accepting cluster
// 2. Set PeerThroughMeshGateways = true
// 3. Upgrade both clusters
// 4. Verify the peering is re-established through mesh gateway
func TestPeering_Upgrade_ControlPlane_MGW(t *testing.T) {
	t.Parallel()

	type testcase struct {
		oldversion    string
		targetVersion string
	}
	tcs := []testcase{
		// {
		//  TODO: API changed from 1.13 to 1.14 in , PeerName to Peer
		//  exportConfigEntry
		// 	oldversion:    "1.13",
		// 	targetVersion: *utils.TargetVersion,
		// },
		{
			oldversion:    "1.14",
			targetVersion: utils.TargetVersion,
		},
	}

	run := func(t *testing.T, tc testcase) {
		accepting, dialing := libtopology.BasicPeeringTwoClustersSetup(t, tc.oldversion)
		var (
			acceptingCluster = accepting.Cluster
			dialingCluster   = dialing.Cluster
		)

		dialingClient, err := dialingCluster.GetClient(nil, false)
		require.NoError(t, err)

		acceptingClient, err := acceptingCluster.GetClient(nil, false)
		require.NoError(t, err)

		// Enable peering control plane traffic through mesh gateway
		req := &api.MeshConfigEntry{
			Peering: &api.PeeringMeshConfig{
				PeerThroughMeshGateways: true,
			},
		}
		ok, _, err := dialingClient.ConfigEntries().Set(req, &api.WriteOptions{})
		require.True(t, ok)
		require.NoError(t, err)
		ok, _, err = acceptingClient.ConfigEntries().Set(req, &api.WriteOptions{})
		require.True(t, ok)
		require.NoError(t, err)

		// Verify control plane endpoints and traffic in gateway
		_, gatewayAdminPort := dialing.Gateway.GetAdminAddr()
		libassert.AssertUpstreamEndpointStatus(t, gatewayAdminPort, "server.dc1.peering", "HEALTHY", 1)
		libassert.AssertUpstreamEndpointStatus(t, gatewayAdminPort, "server.dc2.peering", "HEALTHY", 1)
		libassert.AssertEnvoyMetricAtLeast(t, gatewayAdminPort,
			"cluster.static-server.default.default.accepting-to-dialer.external",
			"upstream_cx_total", 1)

		// Upgrade the accepting cluster and assert peering is still ACTIVE
		require.NoError(t, acceptingCluster.StandardUpgrade(t, context.Background(), tc.targetVersion))
		libassert.PeeringStatus(t, acceptingClient, libtopology.AcceptingPeerName, api.PeeringStateActive)
		libassert.PeeringStatus(t, dialingClient, libtopology.DialingPeerName, api.PeeringStateActive)

		require.NoError(t, dialingCluster.StandardUpgrade(t, context.Background(), tc.targetVersion))
		libassert.PeeringStatus(t, acceptingClient, libtopology.AcceptingPeerName, api.PeeringStateActive)
		libassert.PeeringStatus(t, dialingClient, libtopology.DialingPeerName, api.PeeringStateActive)

		// POST upgrade validation
		//  - Restarted mesh gateway can receive consul generated configuration
		//  - control plane traffic is through mesh gateway
		//  - Register a new static-client service in dialing cluster and
		//  - set upstream to static-server service in peered cluster

		// Restart the gateway
		err = dialing.Gateway.Restart()
		require.NoError(t, err)

		// Restarted gateway should not have any measurement on data plane traffic
		libassert.AssertEnvoyMetricAtMost(t, gatewayAdminPort,
			"cluster.static-server.default.default.accepting-to-dialer.external",
			"upstream_cx_total", 0)
		// control plane metrics should be observed
		libassert.AssertEnvoyMetricAtLeast(t, gatewayAdminPort,
			"cluster.server.dc1.peering",
			"upstream_cx_total", 1)

		clientSidecarService, err := libservice.CreateAndRegisterStaticClientSidecar(dialingCluster.Servers()[0], libtopology.DialingPeerName, true)
		require.NoError(t, err)
		_, port := clientSidecarService.GetAddr()
		_, adminPort := clientSidecarService.GetAdminAddr()
		libassert.AssertUpstreamEndpointStatus(t, adminPort, fmt.Sprintf("static-server.default.%s.external", libtopology.DialingPeerName), "HEALTHY", 1)
		libassert.HTTPServiceEchoes(t, "localhost", port, "")
	}

	for _, tc := range tcs {
		t.Run(fmt.Sprintf("upgrade from %s to %s", tc.oldversion, tc.targetVersion),
			func(t *testing.T) {
				run(t, tc)
			})
		// time.Sleep(3 * time.Second)
	}
}