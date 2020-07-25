// Copyright 2020 The Matrix.org Foundation C.I.C.
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

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	discovery "github.com/libp2p/go-libp2p-discovery"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/matrix-org/gomatrixserverlib"
	maddr "github.com/multiformats/go-multiaddr"
)

var logger = log.Logger("rendezvous")

type addrList []maddr.Multiaddr

func (al *addrList) String() string {
	strs := make([]string, len(*al))
	for i, addr := range *al {
		strs[i] = addr.String()
	}
	return strings.Join(strs, ",")
}

func (al *addrList) Set(value string) error {
	addr, err := maddr.NewMultiaddr(value)
	if err != nil {
		return err
	}
	*al = append(*al, addr)
	return nil
}

func StringsToAddrs(addrStrings []string) (maddrs []maddr.Multiaddr, err error) {
	for _, addrString := range addrStrings {
		addr, err := maddr.NewMultiaddr(addrString)
		if err != nil {
			return maddrs, err
		}
		maddrs = append(maddrs, addr)
	}
	return
}

type ConfigParam struct {
	RendezvousString string
	BootstrapPeers   addrList
	ListenAddresses  addrList
	ProtocolID       string
	keydb            gomatrixserverlib.KeyDatabase
}

func ParseFlags() (ConfigParam, error) {
	config := ConfigParam{}
	flag.StringVar(&config.RendezvousString, "rendezvous", "meet me here",
		"Unique string to identify group of nodes. Share this with your friends to let them connect with you")
	flag.Var(&config.BootstrapPeers, "peer", "Adds a peer multiaddress to the bootstrap list")
	flag.Var(&config.ListenAddresses, "listen", "Adds a multiaddress to the listen list")
	flag.StringVar(&config.ProtocolID, "pid", "/chat/1.1.0", "Sets a protocol id for stream headers")
	flag.Parse()

	if len(config.BootstrapPeers) == 0 {
		//config.BootstrapPeers = dht.DefaultBootstrapPeers
		ma, err := maddr.NewMultiaddr("/ip4/220.194.157.80/tcp/4001/p2p/QmP2C45o2vZfy1JXWFZDUEzrQCigMtd4r3nesvArV8dFKd")
		if err != nil {
			panic(err)
		}

		config.BootstrapPeers = append(config.BootstrapPeers, ma)
	}

	return config, nil
}

func findPeer(ctx context.Context, host host.Host,
	config ConfigParam) {

	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
	kademliaDHT, err := dht.New(ctx, host)
	if err != nil {
		panic(err)
	}

	// Bootstrap the DHT. In the default configuration, this spawns a Background
	// thread that will refresh the peer table every five minutes.
	logger.Debug("Bootstrapping the DHT")
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		panic(err)
	}

	// Let's connect to the bootstrap nodes first. They will tell us about the
	// other nodes in the network.
	var wg sync.WaitGroup
	for _, peerAddr := range config.BootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := host.Connect(ctx, *peerinfo); err != nil {
				logger.Warning(err)
			} else {
				logger.Info("Connection established with bootstrap node:", *peerinfo)
			}
		}()
	}
	wg.Wait()

	// We use a rendezvous point "meet me here" to announce our location.
	// This is like telling your friends to meet you at the Eiffel Tower.
	logger.Info("Announcing ourselves...")
	routingDiscovery := discovery.NewRoutingDiscovery(kademliaDHT)
	discovery.Advertise(ctx, routingDiscovery, config.RendezvousString)
	logger.Debug("Successfully announced!")

	// Now, look for others who have announced
	// This is like your friend telling you the location to meet you.
	logger.Debug("Searching for other peers...")
	peerChan, err := routingDiscovery.FindPeers(ctx, config.RendezvousString)
	if err != nil {
		panic(err)
	}

	for p := range peerChan {
		if p.ID == host.ID() {
			continue
		}
		logger.Debug("Found peer:", p)

		logger.Debug("Connecting to:", p)

		// stream, err := host.NewStream(ctx, peer.ID, protocol.ID(config.ProtocolID))

		// if err != nil {
		// 	logger.Warning("Connection failed:", err)
		// 	continue
		// } else {
		// 	rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))

		// 	// go writeData(rw)
		// 	// go readData(rw)
		// }

		if err := host.Connect(context.Background(), p); err != nil {
			fmt.Println("Error adding peer", p.ID.String(), "via DHT:", err)
		}
		if pubkey, err := p.ID.ExtractPublicKey(); err == nil {
			raw, _ := pubkey.Raw()
			if err := config.keydb.StoreKeys(
				context.Background(),
				map[gomatrixserverlib.PublicKeyLookupRequest]gomatrixserverlib.PublicKeyLookupResult{
					{
						ServerName: gomatrixserverlib.ServerName(p.ID.String()),
						KeyID:      "ed25519:p2pdemo",
					}: {
						VerifyKey: gomatrixserverlib.VerifyKey{
							Key: gomatrixserverlib.Base64Bytes(raw),
						},
						ValidUntilTS: math.MaxUint64 >> 1,
						ExpiredTS:    gomatrixserverlib.PublicKeyNotExpired,
					},
				},
			); err != nil {
				fmt.Println("Failed to store keys:", err)
			}
		}

		logger.Info("Connected to:", p)
	}

	fmt.Println("Discovered", len(host.Peerstore().Peers())-1, "other libp2p peer(s):")
	for _, peer := range host.Peerstore().Peers() {
		if peer != host.ID() {
			fmt.Println("-", peer)
		}
	}

}
