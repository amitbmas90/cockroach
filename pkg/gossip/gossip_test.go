// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package gossip

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/gossip/resolver"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
)

// TestGossipInfoStore verifies operation of gossip instance infostore.
func TestGossipInfoStore(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop()
	rpcContext := rpc.NewContext(context.TODO(), &base.Config{Insecure: true}, nil, stopper)
	g := New(context.TODO(), rpcContext, rpc.NewServer(rpcContext), nil, stopper, metric.NewRegistry())
	// Have to call g.SetNodeID before call g.AddInfo
	g.SetNodeID(roachpb.NodeID(1))
	slice := []byte("b")
	if err := g.AddInfo("s", slice, time.Hour); err != nil {
		t.Fatal(err)
	}
	if val, err := g.GetInfo("s"); !bytes.Equal(val, slice) || err != nil {
		t.Errorf("error fetching string: %v", err)
	}
	if _, err := g.GetInfo("s2"); err == nil {
		t.Errorf("expected error fetching nonexistent key \"s2\"")
	}
}

func TestGossipGetNextBootstrapAddress(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop()

	resolverSpecs := []string{
		"127.0.0.1:9000",
		"127.0.0.1:9001",
		"localhost:9004",
	}

	resolvers := []resolver.Resolver{}
	for _, rs := range resolverSpecs {
		resolver, err := resolver.NewResolver(rs)
		if err == nil {
			resolvers = append(resolvers, resolver)
		}
	}
	if len(resolvers) != 3 {
		t.Errorf("expected 3 resolvers; got %d", len(resolvers))
	}
	server := rpc.NewServer(rpc.NewContext(context.TODO(), &base.Config{Insecure: true}, nil, stopper))
	g := New(context.TODO(), nil, server, resolvers, stop.NewStopper(), metric.NewRegistry())

	// Using specified resolvers, fetch bootstrap addresses 3 times
	// and verify the results match expected addresses.
	expAddresses := []string{
		"127.0.0.1:9000",
		"127.0.0.1:9001",
		"localhost:9004",
	}
	for i := 0; i < len(expAddresses); i++ {
		if addr := g.getNextBootstrapAddress(); addr == nil {
			t.Errorf("%d: unexpected nil addr when expecting %s", i, expAddresses[i])
		} else if addrStr := addr.String(); addrStr != expAddresses[i] {
			t.Errorf("%d: expected addr %s; got %s", i, expAddresses[i], addrStr)
		}
	}
}

func TestGossipRaceLogStatus(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()
	local := startGossip(1, stopper, t, metric.NewRegistry())

	local.mu.Lock()
	peer := startGossip(2, stopper, t, metric.NewRegistry())
	local.startClient(&peer.mu.is.NodeAddr, peer.mu.is.NodeID)
	local.mu.Unlock()

	// Race gossiping against LogStatus.
	gun := make(chan struct{})
	for i := uint8(0); i < 10; i++ {
		go func() {
			<-gun
			local.LogStatus()
			gun <- struct{}{}
		}()
		gun <- struct{}{}
		if err := local.AddInfo(
			strconv.FormatUint(uint64(i), 10),
			[]byte{i},
			time.Hour,
		); err != nil {
			t.Fatal(err)
		}
		<-gun
	}
	close(gun)
}

// TestGossipNoForwardSelf verifies that when a Gossip instance is full, it
// redirects clients elsewhere (in particular not to itself).
//
// NB: Stress testing this test really stresses the OS networking stack
// more than anything else. For example, on Linux it may quickly deplete
// the ephemeral port range (due to the TIME_WAIT state).
// On a box which only runs tests, this can be circumvented by running
//
//	sudo bash -c "echo 1 > /proc/sys/net/ipv4/tcp_tw_recycle"
//
// See https://vincent.bernat.im/en/blog/2014-tcp-time-wait-state-linux.html
// for details.
//
// On OSX, things similarly fall apart. See #7524 and #5218 for some discussion
// of this.
func TestGossipNoForwardSelf(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()
	local := startGossip(1, stopper, t, metric.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start one loopback client plus enough additional clients to fill the
	// incoming clients.
	peers := []*Gossip{local}
	local.server.mu.Lock()
	maxSize := local.server.mu.incoming.maxSize
	local.server.mu.Unlock()
	for i := 0; i < maxSize; i++ {
		peers = append(peers, startGossip(roachpb.NodeID(i+2), stopper, t, metric.NewRegistry()))
	}

	for _, peer := range peers {
		c := newClient(context.TODO(), local.GetNodeAddr(), makeMetrics())

		util.SucceedsSoon(t, func() error {
			conn, err := peer.rpcContext.GRPCDial(c.addr.String(), grpc.WithBlock())
			if err != nil {
				return err
			}

			stream, err := NewGossipClient(conn).Gossip(ctx)
			if err != nil {
				return err
			}

			if err := c.requestGossip(peer, stream); err != nil {
				return err
			}

			// Wait until the server responds, so we know we're connected.
			_, err = stream.Recv()
			return err
		})
	}

	numClients := len(peers) * 2
	disconnectedCh := make(chan *client)

	// Start a few overflow peers and assert that they don't get forwarded to us
	// again.
	for i := 0; i < numClients; i++ {
		local.server.mu.Lock()
		maxSize := local.server.mu.incoming.maxSize
		local.server.mu.Unlock()
		peer := startGossip(roachpb.NodeID(i+maxSize+2), stopper, t, metric.NewRegistry())

		for {
			localAddr := local.GetNodeAddr()
			c := newClient(context.TODO(), localAddr, makeMetrics())
			c.start(peer, disconnectedCh, peer.rpcContext, stopper, peer.GetNodeID(), peer.rpcContext.NewBreaker())

			disconnectedClient := <-disconnectedCh
			if disconnectedClient != c {
				t.Fatalf("expected %p to be disconnected, got %p", c, disconnectedClient)
			} else if c.forwardAddr == nil {
				// Under high load, clients sometimes fail to connect for reasons
				// unrelated to the test, so we need to permit some.
				t.Logf("node #%d: got nil forwarding address", peer.GetNodeID())
				continue
			} else if *c.forwardAddr == *localAddr {
				t.Errorf("node #%d: got local's forwarding address", peer.GetNodeID())
			}
			break
		}
	}
}

// TestGossipCullNetwork verifies that a client will be culled from
// the network periodically (at cullInterval duration intervals).
func TestGossipCullNetwork(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()
	local := startGossip(1, stopper, t, metric.NewRegistry())
	local.SetCullInterval(5 * time.Millisecond)

	local.mu.Lock()
	for i := 0; i < minPeers; i++ {
		peer := startGossip(roachpb.NodeID(i+2), stopper, t, metric.NewRegistry())
		local.startClient(peer.GetNodeAddr(), peer.GetNodeID())
	}
	local.mu.Unlock()

	const slowGossipDuration = time.Minute

	if err := util.RetryForDuration(slowGossipDuration, func() error {
		if peers := len(local.Outgoing()); peers != minPeers {
			return errors.Errorf("%d of %d peers connected", peers, minPeers)
		}
		return nil
	}); err != nil {
		t.Fatalf("condition failed to evaluate within %s: %s", slowGossipDuration, err)
	}

	local.manage()

	if err := util.RetryForDuration(slowGossipDuration, func() error {
		// Verify that a client is closed within the cull interval.
		if peers := len(local.Outgoing()); peers != minPeers-1 {
			return errors.Errorf("%d of %d peers connected", peers, minPeers-1)
		}
		return nil
	}); err != nil {
		t.Fatalf("condition failed to evaluate within %s: %s", slowGossipDuration, err)
	}
}

func TestGossipOrphanedStallDetection(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop()
	local := startGossip(1, stopper, t, metric.NewRegistry())
	local.SetStallInterval(5 * time.Millisecond)

	// Make sure we have the sentinel to ensure that its absence is not the
	// cause of stall detection.
	if err := local.AddInfo(KeySentinel, nil, time.Hour); err != nil {
		t.Fatal(err)
	}

	peerStopper := stop.NewStopper()
	peer := startGossip(2, peerStopper, t, metric.NewRegistry())

	peerNodeID := peer.GetNodeID()
	peerAddr := peer.GetNodeAddr()
	peerAddrStr := peerAddr.String()

	local.startClient(peerAddr, peerNodeID)

	util.SucceedsSoon(t, func() error {
		for _, peerID := range local.Outgoing() {
			if peerID == peerNodeID {
				return nil
			}
		}
		return errors.Errorf("node %d not yet connected", peerNodeID)
	})

	util.SucceedsSoon(t, func() error {
		for _, resolver := range local.GetResolvers() {
			if resolver.Addr() == peerAddrStr {
				return nil
			}
		}
		return errors.Errorf("node %d descriptor not yet available", peerNodeID)
	})

	local.bootstrap()
	local.manage()

	peerStopper.Stop()

	util.SucceedsSoon(t, func() error {
		for _, peerID := range local.Outgoing() {
			if peerID == peerNodeID {
				return errors.Errorf("node %d still connected", peerNodeID)
			}
		}
		return nil
	})

	peerStopper = stop.NewStopper()
	defer peerStopper.Stop()
	peer = startGossipAtAddr(peerNodeID, peerAddr, peerStopper, t, metric.NewRegistry())

	util.SucceedsSoon(t, func() error {
		for _, peerID := range local.Outgoing() {
			if peerID == peerNodeID {
				return nil
			}
		}
		return errors.Errorf("node %d not yet connected", peerNodeID)
	})
}