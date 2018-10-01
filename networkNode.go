// Copyright © 2018 J. Strobus White.
// This file is part of the blocktop blockchain development kit.
//
// Blocktop is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Blocktop is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with blocktop. If not, see <http://www.gnu.org/licenses/>.

package netlibp2p

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	push "github.com/blocktop/go-push-components"
	"github.com/fatih/color"
	"github.com/golang/glog"

	boot "github.com/blocktop/go-libp2p-bootstrap"
	spec "github.com/blocktop/go-spec"
	proto "github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	libp2p "github.com/libp2p/go-libp2p"
	crypto "github.com/libp2p/go-libp2p-crypto"
	p2phost "github.com/libp2p/go-libp2p-host"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	discovery "github.com/libp2p/go-libp2p/p2p/discovery"
	ma "github.com/multiformats/go-multiaddr"
	protobufCodec "github.com/multiformats/go-multicodec/protobuf"
	"github.com/spf13/viper"
)

var _ spec.NetworkNode = (*NetworkNode)(nil)

type NetworkNode struct {
	peerID            string
	PublicKey         string
	PrivateKey        string
	Host              p2phost.Host
	Bootstrapper      *boot.Bootstrap
	DiscoveryService  discovery.Service
	identity          peer.ID
	addresses         []ma.Multiaddr
	privKey           crypto.PrivKey
	pubKey            crypto.PubKey
	broadcastQ        *push.PushQueue
	onMessageReceived spec.MessageReceivedHandler
}

func NewNode() (*NetworkNode, error) {
	n := &NetworkNode{}

	privKeyStr := viper.GetString("node.privateKey")
	privKeyBytes, err := crypto.ConfigDecodeKey(privKeyStr)
	if err != nil {
		return nil, err
	}
	privKey, err := crypto.UnmarshalPrivateKey(privKeyBytes)
	if err != nil {
		return nil, err
	}

	pubKeyStr := viper.GetString("node.publicKey")
	pubKeyBytes, err := crypto.ConfigDecodeKey(pubKeyStr)
	if err != nil {
		return nil, err
	}
	pubKey, err := crypto.UnmarshalPublicKey(pubKeyBytes)
	if err != nil {
		return nil, err
	}

	n.privKey = privKey
	n.pubKey = pubKey
	n.PublicKey = pubKeyStr
	n.PrivateKey = privKeyStr

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(viper.GetStringSlice("node.addresses")...),
		libp2p.Identity(privKey),
		libp2p.NATPortMap()}

	host, err := libp2p.New(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	n.Host = host

	n.identity = host.ID()
	n.peerID = n.identity.Pretty()
	n.Host.Peerstore().SetProtocols(n.identity, ConversationProtocol)

	if !viper.GetBool("node.bootstrapper.disable") {
		bs, err := n.makeBootstrapper()
		if err != nil {
			return nil, err
		}
		n.Bootstrapper = bs
	}

	broadcastConcurrency := viper.GetInt("node.broadcastconcurrency")
	n.broadcastQ = push.NewPushQueue(broadcastConcurrency, 1000, func(item push.QueueItem) {
		if netMsg, ok := item.(*spec.NetworkMessage); ok {
			n.broadcastMessage(netMsg)
		} else {
			glog.Warningln("Peer %s: broadcaster received incorrect item from queue", n.peerID[2:6])
		}
	})
	n.broadcastQ.OnOverload(func(item push.QueueItem) {
		glog.Errorln(color.HiRedString("Peer %s: broadcast queue was overloaded", n.peerID[2:6]))
	})
	n.broadcastQ.Start()

	return n, nil
}

func (n *NetworkNode) makeBootstrapper() (*boot.Bootstrap, error) {
	bootCfg := boot.Config{}
	bootCfg.MinPeers = viper.GetInt("node.bootstrapper.minPeers")
	bootCfg.BootstrapPeers = viper.GetStringSlice("node.bootstrapper.bootstrapPeers")
	bootCfg.BootstrapInterval = viper.GetDuration("node.bootstrapper.checkInterval") * time.Second
	bootCfg.HardBootstrap = viper.GetDuration("node.bootstrapper.rebootstrapIntervale") * time.Second

	bs, err := boot.New(n.Host, bootCfg)
	if err != nil {
		return nil, err
	}
	return bs, nil
}

type DiscoveryNotifee struct {
	h p2phost.Host
}

func (n *DiscoveryNotifee) HandlePeerFound(pi pstore.PeerInfo) {
	n.h.Connect(context.Background(), pi)
}

func (n *NetworkNode) Bootstrap(ctx context.Context) error {
	if !viper.GetBool("node.discovery.disable") {
		interval := viper.GetDuration("node.discovery.interval")
		service, err := discovery.NewMdnsService(ctx, n.Host, interval, "discovery")
		if err != nil {
			return err
		}

		notifee := &DiscoveryNotifee{n.Host}

		service.RegisterNotifee(notifee)

		n.DiscoveryService = service
	}

	if viper.GetBool("node.bootstrapper.disable") {
		return nil
	}

	fmt.Fprintf(os.Stderr, "Peer %s: Bootstrapping...\n", n.peerID[2:8])
	err := n.Bootstrapper.Start(ctx)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	minPeers := n.Bootstrapper.GetMinPeers()
	ticker := time.NewTicker(2 * time.Second)

	for n.Bootstrapper.GetPeerCount() < minPeers {
		select {
		case <-ctx.Done():
			return errors.New("bootstrapping timed out")
		case <-ticker.C:
			n.Bootstrapper.Bootstrap(ctx)
		}
	}

	return nil
}

func (n *NetworkNode) Listen(ctx context.Context) {
	n.Host.SetStreamHandler(ConversationProtocol, func(s inet.Stream) {
		n.handleIncomingConversation(ctx, s)
	})
}

func (n *NetworkNode) PeerID() string {
	return n.peerID
}

func (n *NetworkNode) OnMessageReceived(f spec.MessageReceivedHandler) {
	n.onMessageReceived = f
}

func (n *NetworkNode) Broadcast(netMsg *spec.NetworkMessage) {
	n.broadcastQ.Put(netMsg)
}

func (n *NetworkNode) Close() {
	if n.Bootstrapper != nil {
		n.Bootstrapper.Close()
	}
	if n.DiscoveryService != nil {
		n.DiscoveryService.Close()
	}
	n.Host.RemoveStreamHandler(ConversationProtocol)
}

func (n *NetworkNode) broadcastMessage(netMsg *spec.NetworkMessage) {
	peerstore := n.Host.Peerstore()
	peers := peerstore.Peers()

	for _, peer := range peers {
		toPeerID := peer.Pretty()
		if toPeerID != n.peerID && toPeerID != netMsg.From {
			protocols, _ := peerstore.SupportsProtocols(peer, ConversationProtocol)
			if protocols == nil || len(protocols) == 0 {
				// TODO ensure correct protocol
				// continue
			}

			conversationKey := makeConversationKey(netMsg.Protocol.GetBlockchainType(), toPeerID, true)
			ctx := context.TODO()
			c, err := getConversation(ctx, n.Host, conversationKey, toPeerID, nil)
			if err != nil {
				continue
			}

			msg, err := n.makeConversationMessage(netMsg)
			if err != nil {
				break
			}

			go func() {
				c.send(ctx, msg)

				if msg.GetHangUp() {
					c.end()
				}
			}()
		}
	}
}

func (n *NetworkNode) handleIncomingConversation(ctx context.Context, stream inet.Stream) {
	msg := &ConversationMessage{}
	decoder := protobufCodec.Multicodec(nil).Decoder(bufio.NewReader(stream))
	err := decoder.Decode(msg)
	if err != nil {
		return
	}

	remotePeer := stream.Conn().RemotePeer()
	remotePeerID := remotePeer.Pretty()

	conversationKey := makeConversationKey(msg.GetProtocol(), remotePeerID, false)
	conv, err := getConversation(ctx, n.Host, conversationKey, remotePeerID, stream)
	if err != nil {
		return
	}

	if msg.GetHangUp() {
		defer conv.end()
	}

	// verify message signature
	sigStr := msg.GetSignature()
	sig, err := base64.StdEncoding.DecodeString(sigStr)
	if err != nil {
		return
	}
	msg.Signature = ""
	msgBytes, err := proto.Marshal(msg)
	if err != nil {
		return
	}

	pubKey, err := base64.StdEncoding.DecodeString(msg.GetPubKey())
	if err != nil {
		return
	}

	sigOK, err := n.Verify(msg.GetFrom(), msgBytes, sig, pubKey)
	if err != nil || !sigOK {
		return
	}
	msg.Signature = sigStr

	p := spec.NewProtocol(msg.GetProtocol())

	netMsg := &spec.NetworkMessage{
		Data:     msg.GetData(),
		Links:    msg.GetLinks(),
		Hash:			msg.GetHash(),
		Protocol: p,
		From:     msg.GetFrom()}

	n.onMessageReceived(netMsg)

	//go n.addToPeerstore(msg.GetFrom(), msg.GetPubKey())
}

func (n *NetworkNode) makeConversationMessage(netMsg *spec.NetworkMessage) (*ConversationMessage, error) {
	conversationMsg := &ConversationMessage{
		ID:        uuid.New().String(),
		Version:   ConversationProtocolVersion,
		Protocol:  netMsg.Protocol.String(),
		From:      netMsg.From,
		Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
		Hash:      netMsg.Hash,
		PubKey:    n.PublicKey,
		HangUp:    true,
		Data:      netMsg.Data,
		Links:     netMsg.Links}

	msgBytes, err := proto.Marshal(conversationMsg)
	if err != nil {
		return nil, err
	}

	sig, err := n.Sign(msgBytes)
	if err != nil {
		return nil, err
	}
	conversationMsg.Signature = toBase64(sig)

	return conversationMsg, nil
}

func (n *NetworkNode) Sign(data []byte) ([]byte, error) {
	return n.privKey.Sign(data)
}

func (n *NetworkNode) Verify(peerID string, data []byte, sig []byte, pubKey []byte) (bool, error) {
	key, err := crypto.UnmarshalPublicKey(pubKey)
	if err != nil {
		return false, err
	}
	return key.Verify(data, sig)
}

func (n *NetworkNode) addToPeerstore(peerID string, publicKey string) {
	//TODO
}

func toBase64(bytes []byte) string {
	return base64.StdEncoding.EncodeToString(bytes)
}
