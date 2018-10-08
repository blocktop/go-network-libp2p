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
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	p2phost "github.com/libp2p/go-libp2p-host"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"

	proto "github.com/golang/protobuf/proto"
	protobufCodec "github.com/multiformats/go-multicodec/protobuf"
)

const ConversationProtocolVersion = "0.0.1"
const ConversationProtocol = "/blocktop/conversation/" + ConversationProtocolVersion

type conversation struct {
	host          p2phost.Host
	key           string
	remotePeerID  peer.ID
	messageNumber int
	stream        inet.Stream
	cancel        context.CancelFunc
}

type conversationMap struct {
	sync.Mutex
	conversations map[string]*conversation
}

var convMap conversationMap = conversationMap{conversations: make(map[string]*conversation)}

func getConversation(ctx context.Context, host p2phost.Host, conversationKey string, toPeerID string, stream inet.Stream) (*conversation, error) {

	convMap.Lock()
	defer convMap.Unlock()

	conv, exists := convMap.conversations[conversationKey]
	if !exists {
		peerID, err := makePeerID(toPeerID)
		if err != nil {
			return nil, err
		}

		ctx2, cancel := context.WithTimeout(ctx, 5*time.Minute)

		c := &conversation{
			host:          host,
			key:           conversationKey,
			remotePeerID:  peerID,
			messageNumber: 0,
			stream:        stream,
			cancel:        cancel}
		convMap.conversations[conversationKey] = c
		conv = c
		go c.watchdog(ctx2)
	}

	return conv, nil
}

func makeConversationKey(blockchainType string, toPeerID string, outbound bool) string {
	dir := "inbound"
	if outbound {
		dir = "outbound"
	}
	return fmt.Sprintf("/%s/conversation/%s/%s/%s", blockchainType, dir, toPeerID, uuid.New().String())
}

func (c *conversation) watchdog(ctx context.Context) {
	<-ctx.Done()
	c.close()
	c.cancel()
}

func (c *conversation) end() {
	c.cancel() // triggers watchdog()
}

func (c *conversation) close() {
	convMap.Lock()
	if c.stream != nil {
		c.stream.Close()
	}

	delete(convMap.conversations, c.key)
	convMap.Unlock()
}

func (c *conversation) send(ctx context.Context, message proto.Message) error {
	if c.stream == nil {
		s, err := c.host.NewStream(ctx, c.remotePeerID, ConversationProtocol)
		if err != nil {
			return err
		}

		c.stream = s
	}

	writer := bufio.NewWriter(c.stream)
	enc := protobufCodec.Multicodec(nil).Encoder(writer)
	err := enc.Encode(message)
	if err != nil {
		return err
	}
	writer.Flush()
	return nil
}

func makePeerID(peerID string) (peer.ID, error) {
	return peer.IDB58Decode(peerID)
}
