package netlibp2p

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"

	boot "github.com/blckit/go-libp2p-bootstrap"
	spec "github.com/blckit/go-spec"
	proto "github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
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

type NetworkNode struct {
	PeerID           string
	PublicKey        string
	Host             p2phost.Host
	Bootstrapper     *boot.Bootstrap
	DiscoveryService discovery.Service
	identity         peer.ID
	addresses        []ma.Multiaddr
	privKey          crypto.PrivKey
	pubKey           crypto.PubKey
	receive          chan *spec.NetworkMessage
}

func NewNode() (*NetworkNode, error) {
	n := &NetworkNode{}

	privKeyBytes, err := crypto.ConfigDecodeKey(viper.GetString("node.privateKey"))
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
	n.receive = make(chan *spec.NetworkMessage, 100)

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
	n.PeerID = n.identity.Pretty()
	n.Host.Peerstore().SetProtocols(n.identity, ConversationProtocol)

	if !viper.GetBool("node.bootstrapper.disable") {
		bs, err := n.makeBootstrapper()
		if err != nil {
			return nil, err
		}
		n.Bootstrapper = bs
	}

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

	fmt.Fprintf(os.Stderr, "Peer %s: Bootstrapping...\n", n.PeerID[2:8])
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

func (n *NetworkNode) GetPeerID() string {
	return n.PeerID
}

func (n *NetworkNode) GetReceiveChan() <-chan *spec.NetworkMessage {
	return n.receive
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

func (n *NetworkNode) Broadcast(ctx context.Context, message proto.Message, from string, p *spec.MessageProtocol) {
	peerstore := n.Host.Peerstore()
	peers := peerstore.Peers()

	for _, peer := range peers {
		toPeerID := peer.Pretty()
		if toPeerID != n.PeerID && toPeerID != from {
			protocols, _ := peerstore.SupportsProtocols(peer, ConversationProtocol)
			if protocols == nil || len(protocols) == 0 {
				// TODO ensure correct protocol
				// continue
			}

			conversationKey := makeConversationKey(p.GetBlockchainType(), toPeerID, true)
			c, err := getConversation(ctx, n.Host, conversationKey, toPeerID, nil)
			if err != nil {
				continue
			}

			msg, err := n.makeConversationMessage(p.String(), from, message)
			if err != nil {
				break
			}

			c.send(ctx, msg)

			if msg.GetHangUp() {
				c.end()
			}
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
		Message:  msg.GetMessage(),
		Protocol: p,
		From:     msg.GetFrom()}

	n.receive <- netMsg

	go n.addToPeerstore(msg.GetFrom(), msg.GetPubKey())
}

func (n *NetworkNode) makeConversationMessage(protocol string, from string, message proto.Message) (*ConversationMessage, error) {
	a, err := ptypes.MarshalAny(message)

	conversationMsg := &ConversationMessage{
		ID:        uuid.New().String(),
		Version:   ConversationProtocolVersion,
		Protocol:  protocol,
		From:      from,
		Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
		PubKey:    n.PublicKey,
		HangUp:    true,
		Message:   a}

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
