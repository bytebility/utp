// Package utp implements uTP, the micro transport protocol as used with
// Bittorrent. It opts for simplicity and reliability over strict adherence to
// the (poor) spec. It allows using the underlying OS-level transport despite
// dispatching uTP on top to allow for example, shared socket use with DHT.
// Additionally, multiple uTP connections can share the same OS socket, to
// truly realize uTP's claim to be light on system and network switching
// resources.
//
// Socket is a wrapper of net.UDPConn, and performs dispatching of uTP packets
// to attached uTP Conns. Dial and Accept is done via Socket. Conn implements
// net.Conn over uTP, via aforementioned Socket.
package utp

import (
	"errors"
	"expvar"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	pprofsync "github.com/anacrolix/sync"
)

const (
	// Maximum received SYNs that haven't been accepted. If more SYNs are
	// received, a pseudo randomly selected SYN is replied to with a reset to
	// make room.
	backlog = 50

	// IPv6 min MTU is 1280, -40 for IPv6 header, and ~8 for fragment header?
	minMTU     = 1232
	recvWindow = 1 << 18 // 256KiB
	// uTP header of 20, +2 for the next extension, and 8 bytes of selective
	// ACK.
	maxHeaderSize  = 30
	maxPayloadSize = minMTU - maxHeaderSize
	maxRecvSize    = 0x2000

	// Maximum out-of-order packets to buffer.
	maxUnackedInbound = 256
	maxUnackedSends   = 256
)

var (
	ackSkippedResends = expvar.NewInt("utpAckSkippedResends")
	// Inbound packets processed by a Conn.
	deliveriesProcessed = expvar.NewInt("utpDeliveriesProcessed")
	sentStatePackets    = expvar.NewInt("utpSentStatePackets")
	unusedReads         = expvar.NewInt("utpUnusedReads")
	sendBufferPool      = sync.Pool{
		New: func() interface{} { return make([]byte, minMTU) },
	}
	// This is the latency we assume on new connections. It should be higher
	// than the latency we expect on most connections to prevent excessive
	// resending to peers that take a long time to respond, before we've got a
	// better idea of their actual latency.
	initialLatency = 400 * time.Millisecond
	// If a write isn't acked within this period, destroy the connection.
	writeTimeout      = 15 * time.Second
	packetReadTimeout = 2 * time.Minute
)

// Strongly-type guarantee of resolved network address.
type resolvedAddrStr string

type read struct {
	data []byte
	from net.Addr
}

type syn struct {
	seq_nr, conn_id uint16
	addr            string
}

var (
	mu                         pprofsync.RWMutex
	cond                       = sync.Cond{L: &mu}
	sockets                    = map[*Socket]struct{}{}
	logLevel                   = 0
	artificialPacketDropChance = 0.0
)

func init() {
	logLevel, _ = strconv.Atoi(os.Getenv("GO_UTP_LOGGING"))
	fmt.Sscanf(os.Getenv("GO_UTP_PACKET_DROP"), "%f", &artificialPacketDropChance)
}

var (
	errClosed                   = errors.New("closed")
	errNotImplemented           = errors.New("not implemented")
	errTimeout        net.Error = timeoutError{"i/o timeout"}
	errAckTimeout               = timeoutError{"timed out waiting for ack"}
)

type timeoutError struct {
	msg string
}

func (me timeoutError) Timeout() bool   { return true }
func (me timeoutError) Error() string   { return me.msg }
func (me timeoutError) Temporary() bool { return false }

type st int

func (me st) String() string {
	switch me {
	case stData:
		return "stData"
	case stFin:
		return "stFin"
	case stState:
		return "stState"
	case stReset:
		return "stReset"
	case stSyn:
		return "stSyn"
	default:
		panic(fmt.Sprintf("%d", me))
	}
}

const (
	stData  st = 0
	stFin      = 1
	stState    = 2
	stReset    = 3
	stSyn      = 4

	// Used for validating packet headers.
	stMax = stSyn
)

type recv struct {
	seen bool
	data []byte
	Type st
}

func packetDebugString(h *header, payload []byte) string {
	return fmt.Sprintf("%s->%d: %q", h.Type, h.ConnID, payload)
}

func stringAddr(s string) net.Addr {
	addr, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		panic(err)
	}
	return addr
}

// Attempt to connect to a remote uTP listener, creating a Socket just for
// this connection.
func Dial(addr string) (net.Conn, error) {
	return DialTimeout(addr, 0)
}

// Same as Dial with a timeout parameter.
func DialTimeout(addr string, timeout time.Duration) (nc net.Conn, err error) {
	s, err := NewSocket("udp", ":0")
	if err != nil {
		return
	}
	defer s.Close()
	return s.DialTimeout(addr, timeout)

}

func nowTimestamp() uint32 {
	return uint32(time.Now().UnixNano() / int64(time.Microsecond))
}

func seqLess(a, b uint16) bool {
	if b < 0x8000 {
		return a < b || a >= b-0x8000
	} else {
		return a < b && a >= b-0x8000
	}
}

type packet struct {
	h       header
	payload []byte
}
