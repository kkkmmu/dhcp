// Copyright 2018 the u-root Authors and Andrea Barberio. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nclient6

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
)

// Broadcast destination IP addresses as defined by RFC 3315
var (
	AllDHCPRelayAgentsAndServers = &net.UDPAddr{
		IP:   net.ParseIP("ff02::1:2"),
		Port: dhcpv6.DefaultServerPort,
	}
	AllDHCPServers = &net.UDPAddr{
		IP:   net.ParseIP("ff05::1:3"),
		Port: dhcpv6.DefaultServerPort,
	}
)

const (
	maxUDPReceivedPacketSize = 8192
	maxMessageSize           = 1500
)

var (
	// ErrNoResponse is returned when no response packet is received.
	ErrNoResponse = errors.New("no matching response packet received")
)

// pendingCh is a channel associated with a pending TransactionID.
type pendingCh struct {
	// SendAndRead closes done to indicate that it wishes for no more
	// messages for this particular XID.
	done <-chan struct{}

	// ch is used by the receive loop to distribute DHCP messages.
	ch chan<- *dhcpv6.Message
}

// Client is a DHCPv6 client.
type Client struct {
	ifaceHWAddr net.HardwareAddr
	conn        net.PacketConn
	timeout     time.Duration
	retry       int

	// bufferCap is the channel capacity for each TransactionID.
	bufferCap int

	// serverAddr is the UDP address to send all packets to.
	//
	// This may be an actual broadcast address, or a unicast address.
	serverAddr *net.UDPAddr

	// closed is an atomic bool set to 1 when done is closed.
	closed uint32

	// done is closed to unblock the receive loop.
	done chan struct{}

	// wg protects the receiveLoop.
	wg sync.WaitGroup

	pendingMu sync.Mutex
	// pending stores the distribution channels for each pending
	// TransactionID. receiveLoop uses this map to determine which channel
	// to send a new DHCP message to.
	pending map[dhcpv6.TransactionID]*pendingCh
}

// New creates a new DHCP client that sends and receives packets on the given
// interface.
func New(ifaceHWAddr net.HardwareAddr, opts ...ClientOpt) (*Client, error) {
	c := &Client{
		ifaceHWAddr: ifaceHWAddr,
		timeout:     5 * time.Second,
		retry:       3,
		serverAddr:  AllDHCPServers,
		bufferCap:   5,

		done:    make(chan struct{}),
		pending: make(map[dhcpv6.TransactionID]*pendingCh),
	}

	for _, opt := range opts {
		opt(c)
	}

	if c.conn == nil {
		return nil, fmt.Errorf("require a connection")
	}

	c.receiveLoop()
	return c, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	// Make sure not to close done twice.
	if !atomic.CompareAndSwapUint32(&c.closed, 0, 1) {
		return nil
	}

	err := c.conn.Close()

	// Closing c.done sets off a chain reaction:
	//
	// Any SendAndRead unblocks trying to receive more messages, which
	// means rem() gets called.
	//
	// rem() should be unblocking receiveLoop if it is blocked.
	//
	// receiveLoop should then exit gracefully.
	close(c.done)

	// Wait for receiveLoop to stop.
	c.wg.Wait()

	return err
}

func isErrClosing(err error) bool {
	// Unfortunately, the epoll-connection-closed error is internal to the
	// net library.
	return strings.Contains(err.Error(), "use of closed network connection")
}

func (c *Client) receiveLoop() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			// TODO: Clients can send a "max packet size" option in their
			// packets, IIRC. Choose a reasonable size and set it.
			b := make([]byte, 1500)
			n, _, err := c.conn.ReadFrom(b)
			if err != nil {
				if !isErrClosing(err) {
					log.Printf("error reading from UDP connection: %v", err)
				}
				return
			}

			msg, err := dhcpv6.MessageFromBytes(b[:n])
			if err != nil {
				// Not a valid DHCP packet; keep listening.
				continue
			}

			c.pendingMu.Lock()
			p, ok := c.pending[msg.TransactionID]
			if ok {
				select {
				case <-p.done:
					close(p.ch)
					delete(c.pending, msg.TransactionID)

				// This send may block.
				case p.ch <- msg:
				}
			}
			c.pendingMu.Unlock()
		}
	}()
}

// ClientOpt is a function that configures the Client.
type ClientOpt func(*Client)

func withBufferCap(n int) ClientOpt {
	return func(c *Client) {
		c.bufferCap = n
	}
}

// WithTimeout configures the retransmission timeout.
//
// Default is 5 seconds.
func WithTimeout(d time.Duration) ClientOpt {
	return func(c *Client) {
		c.timeout = d
	}
}

// WithRetry configures the number of retransmissions to attempt.
//
// Default is 3.
func WithRetry(r int) ClientOpt {
	return func(c *Client) {
		c.retry = r
	}
}

// WithConn configures the packet connection to use.
func WithConn(conn net.PacketConn) ClientOpt {
	return func(c *Client) {
		c.conn = conn
	}
}

// WithBroadcastAddr configures the address to broadcast to.
func WithBroadcastAddr(n *net.UDPAddr) ClientOpt {
	return func(c *Client) {
		c.serverAddr = n
	}
}

// Matcher matches DHCP packets.
type Matcher func(*dhcpv6.Message) bool

// IsMessageType returns a matcher that checks for the message type.
//
// If t is MessageTypeNone, all packets are matched.
func IsMessageType(t dhcpv6.MessageType) Matcher {
	return func(p *dhcpv6.Message) bool {
		return p.MessageType == t || t == dhcpv6.MessageTypeNone
	}
}

// Solicit sends a solicitation message and returns the first valid
// advertisement received.
func (c *Client) Solicit(ctx context.Context, modifiers ...dhcpv6.Modifier) (*dhcpv6.Message, net.Addr, error) {
	solicit, err := dhcpv6.NewSolicit(c.ifaceHWAddr, modifiers...)
	if err != nil {
		return nil, nil, err
	}
	msg, err := c.SendAndRead(ctx, AllDHCPServers, solicit, IsMessageType(dhcpv6.MessageTypeAdvertise))
	if err != nil {
		return nil, nil, err
	}
	return msg, nil, nil
}

// Request requests an IP Assignment from peer given an advertise message.
//
// If peer is nil, AllDHCPServers is used.
func (c *Client) Request(ctx context.Context, advertise *dhcpv6.Message, modifiers ...dhcpv6.Modifier) (*dhcpv6.Message, error) {
	request, err := dhcpv6.NewRequestFromAdvertise(advertise, modifiers...)
	if err != nil {
		return nil, err
	}
	return c.SendAndRead(ctx, AllDHCPServers, request, nil)
}

// send sends p to destination and returns a response channel.
//
// The returned function must be called after all desired responses have been
// received.
//
// Responses will be matched by transaction ID.
func (c *Client) send(dest net.Addr, msg *dhcpv6.Message) (<-chan *dhcpv6.Message, func(), error) {
	c.pendingMu.Lock()
	if _, ok := c.pending[msg.TransactionID]; ok {
		c.pendingMu.Unlock()
		return nil, nil, fmt.Errorf("transaction ID %s already in use", msg.TransactionID)
	}

	ch := make(chan *dhcpv6.Message, c.bufferCap)
	done := make(chan struct{})
	c.pending[msg.TransactionID] = &pendingCh{done: done, ch: ch}
	c.pendingMu.Unlock()

	cancel := func() {
		// Why can't we just close ch here?
		//
		// Because receiveLoop may potentially be blocked trying to
		// send on ch. We gotta unblock it first, so it'll unlock the
		// lock, and then we can take the lock and remove the XID from
		// the pending transaction map.
		close(done)

		c.pendingMu.Lock()
		if p, ok := c.pending[msg.TransactionID]; ok {
			close(p.ch)
			delete(c.pending, msg.TransactionID)
		}
		c.pendingMu.Unlock()
	}

	if _, err := c.conn.WriteTo(msg.ToBytes(), dest); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("error writing packet to connection: %v", err)
	}
	return ch, cancel, nil
}

// This should never be visible to a user.
var errDeadlineExceeded = errors.New("INTERNAL ERROR: deadline exceeded")

// SendAndRead sends a packet p to a destination dest and waits for the first
// response matching `match` as well as its Transaction ID.
//
// If match is nil, the first packet matching the Transaction ID is returned.
func (c *Client) SendAndRead(ctx context.Context, dest *net.UDPAddr, msg *dhcpv6.Message, match Matcher) (*dhcpv6.Message, error) {
	var response *dhcpv6.Message
	err := c.retryFn(func(timeout time.Duration) error {
		ch, rem, err := c.send(dest, msg)
		if err != nil {
			return err
		}
		defer rem()

		for {
			select {
			case <-c.done:
				return ErrNoResponse

			case <-time.After(timeout):
				return errDeadlineExceeded

			case <-ctx.Done():
				return ctx.Err()

			case packet := <-ch:
				if match == nil || match(packet) {
					response = packet
					return nil
				}
			}
		}
	})
	if err == errDeadlineExceeded {
		return nil, ErrNoResponse
	}
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) retryFn(fn func(timeout time.Duration) error) error {
	timeout := c.timeout

	// Each retry takes the amount of timeout at worst.
	for i := 0; i < c.retry || c.retry < 0; i++ {
		switch err := fn(timeout); err {
		case nil:
			// Got it!
			return nil

		case context.DeadlineExceeded:
			// Double timeout, then retry.
			timeout *= 2

		default:
			return err
		}
	}

	return errDeadlineExceeded
}
