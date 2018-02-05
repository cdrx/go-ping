// Package ping is an ICMP ping library seeking to emulate the unix "ping"
// command.
//
// Here is a very simple example that sends & receives 3 packets:
//
//	pinger, err := ping.NewPinger(false)
//	if err != nil {
//		panic(err)
//	}
//
//  stats, err := pinger.ping("www.google.com", 3, 1*time.Second, 10 * time.Second)
//	if err != nil {
//		panic(err)
//	}
//
// It sends ICMP packet(s) and waits for a response. If it receives a response,
// it calls the "receive" callback. When it's finished, it calls the "finish"
// callback.
//
// For a full ping example, see "cmd/ping/ping.go".
//
package ping

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	timeSliceLength  = 8
	protocolICMP     = 1
	protocolIPv6ICMP = 58
)

var (
	ipv4Proto = map[string]string{"ip": "ip4:icmp", "udp": "udp4"}
	ipv6Proto = map[string]string{"ip": "ip6:ipv6-icmp", "udp": "udp6"}
)

var (
	// ErrPingerClosed is returned when the pinger is closed while still running
	// pings.
	ErrPingerClosed = errors.New("pinger closed")
)

// packet represents a received and processed ICMP echo packet.
type packet struct {
	// seq is the ICMP sequence number of the echo request
	seq int

	// rtt contains the round-trip time relative to the echo request
	rtt time.Duration

	// data is the number of bytes of the received packet
	data int
}

// Statistics represent the stats of a currently running or finished
// pinger operation.
type Statistics struct {
	// PacketsRecv is the number of packets received.
	PacketsRecv int

	// PacketsSent is the number of packets sent.
	PacketsSent int

	// PacketLoss is the percentage of packets lost.
	PacketLoss float64

	// IPAddr is the address of the host being pinged.
	IPAddr *net.IPAddr

	// Addr is the string address of the host being pinged.
	Addr string

	// Rtts is all of the round-trip times sent via this pinger.
	Rtts []time.Duration

	// MinRtt is the minimum round-trip time sent via this pinger.
	MinRtt time.Duration

	// MaxRtt is the maximum round-trip time sent via this pinger.
	MaxRtt time.Duration

	// AvgRtt is the average round-trip time sent via this pinger.
	AvgRtt time.Duration

	// StdDevRtt is the standard deviation of the round-trip times sent via
	// this pinger.
	StdDevRtt time.Duration

	// BytesSent tracks the number of bytes sent in pings (including envelope)
	BytesSent int

	// BytesRecv tracks the number of bytes received in pings (including envelope)
	BytesRecv int
}

// Pinger processes pings
type Pinger struct {
	network   string
	ipv4Conn  *icmp.PacketConn
	ipv6Conn  *icmp.PacketConn
	receivers map[int]chan *packet
	done      chan bool
	closed    int32
	mx        sync.RWMutex
}

func NewPinger(privileged bool) (*Pinger, error) {
	pr := &Pinger{
		receivers: make(map[int]chan *packet),
		done:      make(chan bool),
	}
	pr.network = "udp"
	if privileged {
		pr.network = "ip"
	}

	var err error
	pr.ipv4Conn, err = icmp.ListenPacket(ipv4Proto[pr.network], "")
	if err != nil {
		return nil, fmt.Errorf("Error listening for ICMP packets on ipv4: %s\n", err.Error())
	}

	pr.ipv6Conn, err = icmp.ListenPacket(ipv6Proto[pr.network], "")
	if err != nil {
		return nil, fmt.Errorf("Error listening for ICMP packets on ipv6: %s\n", err.Error())
	}

	go pr.recvLoop(pr.ipv4Conn, true)
	go pr.recvLoop(pr.ipv6Conn, false)

	return pr, nil
}

func (pr *Pinger) Close() {
	if atomic.CompareAndSwapInt32(&pr.closed, 0, 1) {
		close(pr.done)
	}
}

func (pr *Pinger) recvLoop(conn *icmp.PacketConn, isIPv4 bool) {
	defer conn.Close()

	for {
		select {
		case <-pr.done:
			return
		default:
			bytes := make([]byte, 512)
			conn.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
			n, _, err := conn.ReadFrom(bytes)
			if err != nil {
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Timeout() {
						// Read timeout
						continue
					}
					fmt.Printf("Error reciving: %v\n", err)
					pr.Close()
					return
				}
			}

			bytes = bytes[:n]
			var proto int
			if !isIPv4 {
				proto = protocolIPv6ICMP
			} else {
				if pr.network == "ip" {
					bytes = ipv4Payload(bytes)
				}
				proto = protocolICMP
			}

			var m *icmp.Message
			if m, err = icmp.ParseMessage(proto, bytes); err != nil {
				fmt.Printf("Error parsing icmp message: %v\n", err)
				continue
			}

			if m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply {
				// Not an echo reply, ignore it
				continue
			}

			switch pkt := m.Body.(type) {
			case *icmp.Echo:
				if len(pkt.Data) < timeSliceLength {
					// Incomplete/corrupted packet, ignore
				} else {
					outPkt := &packet{}
					outPkt.seq = pkt.Seq
					outPkt.rtt = time.Since(bytesToTime(pkt.Data))
					outPkt.data = n
					pr.mx.RLock()
					receiver := pr.receivers[pkt.ID]
					pr.mx.RUnlock()
					if receiver != nil {
						receiver <- outPkt
					}
				}
			default:
				// Very bad, not sure how this can happen
				fmt.Printf("Error, invalid ICMP echo reply. Body type: %T, %s\n",
					pkt, pkt)
			}
		}
	}
}

// Ping pings the given addr.
//
// count tells pinger how many echo packets to send.
//
// interval is the wait time between each packet send.
//
// timeout specifies an overall timeout for the entire operation.
func (pr *Pinger) Ping(addr string, count int, interval time.Duration, timeout time.Duration) (*Statistics, error) {
	deadline := time.NewTimer(timeout)
	sendInterval := time.NewTicker(interval)

	ipaddr, err := net.ResolveIPAddr("ip", addr)
	if err != nil {
		return nil, err
	}

	ipv4 := isIPv4(ipaddr.IP)
	var conn *icmp.PacketConn
	if ipv4 {
		conn = pr.ipv4Conn
	} else {
		conn = pr.ipv6Conn
	}

	id, receiver := pr.newReceiver(count)

	received := make([]bool, count)
	rtts := make([]time.Duration, 0, count)
	maxRtt := time.Duration(0)

	seq := 0
	packetsReceived := 0
	bytesSent := 0
	bytesRecv := 0

	for {
		select {
		case <-sendInterval.C:
			n, sendErr := pr.send(conn, ipaddr, ipv4, id, seq)
			bytesSent += n
			if sendErr != nil {
				return buildStats(addr, ipaddr, seq, packetsReceived, rtts, bytesSent, bytesRecv), err
			}
			seq += 1
			if seq == count {
				// Stop sending
				sendInterval.Stop()
				if maxRtt > 0 {
					// Updated deadline to 2 * maxRtt
					deadline.Reset(2 * maxRtt)
				}
			}
		case <-deadline.C:
			if packetsReceived == 0 {
				return nil, fmt.Errorf("Deadline exceeded before receiving any responses")
			}
			return buildStats(addr, ipaddr, seq, packetsReceived, rtts, bytesSent, bytesRecv), nil
		case pkt := <-receiver:
			bytesRecv += pkt.data
			if pkt.seq < 0 || pkt.seq >= count {
				fmt.Printf("Warning: received packet with invalid sequence %d, ignoring\n", pkt.seq)
				continue
			}
			if !received[pkt.seq] {
				// First time we've received a response for this sequence
				packetsReceived += 1
				received[pkt.seq] = true
			}
			rtts = append(rtts, pkt.rtt)
			if pkt.rtt > maxRtt {
				maxRtt = pkt.rtt
			}
			if packetsReceived == count {
				return buildStats(addr, ipaddr, seq, packetsReceived, rtts, bytesSent, bytesRecv), nil
			}
		case <-pr.done:
			return buildStats(addr, ipaddr, seq, packetsReceived, rtts, bytesSent, bytesRecv), ErrPingerClosed
		}
	}
}

// Loop pings the given addr continuously in batches, starting at minBatch and
// exponentially increasing to maxBatch. The results of each batch are sent to
// onBatch. If onBatch returns false, looping terminates.
//
// interval is the wait time between each packet send.
//
// timeoutRTT specifies the timeout for a single round-trip. The timeout for
// for each batch is set to batchSize * timeoutRTT.
func (pr *Pinger) Loop(addr string, minBatch int, maxBatch int, interval time.Duration, timeoutRTT time.Duration, onBatch func(stats *Statistics, err error) bool) {
	batchSize := minBatch
	for {
		stats, err := pr.Ping(addr, batchSize, interval, time.Duration(batchSize)*timeoutRTT)
		if err == ErrPingerClosed {
			return
		}
		if !onBatch(stats, err) {
			return
		}
		batchSize *= 2
		if batchSize > maxBatch {
			batchSize = maxBatch
		}
	}
}

func (pr *Pinger) newReceiver(count int) (int, chan *packet) {
	receiver := make(chan *packet, count)
	var id int
	pr.mx.Lock()
	for {
		id = rand.Intn(0xffff)
		if pr.receivers[id] != nil {
			// id taken, try again
			continue
		}
		pr.receivers[id] = receiver
		pr.mx.Unlock()
		return id, receiver
	}
}

func (pr *Pinger) send(conn *icmp.PacketConn, ipaddr *net.IPAddr, isIPv4 bool, id int, seq int) (int, error) {
	var typ icmp.Type
	if isIPv4 {
		typ = ipv4.ICMPTypeEcho
	} else {
		typ = ipv6.ICMPTypeEchoRequest
	}

	var dst net.Addr = ipaddr
	if pr.network == "udp" {
		dst = &net.UDPAddr{IP: ipaddr.IP, Zone: ipaddr.Zone}
	}

	bytes, err := (&icmp.Message{
		Type: typ, Code: 0,
		Body: &icmp.Echo{
			ID:   id,
			Seq:  seq,
			Data: timeToBytes(time.Now()),
		},
	}).Marshal(nil)
	if err != nil {
		return 0, err
	}

	for {
		n, err := conn.WriteTo(bytes, dst)
		if err != nil {
			if neterr, ok := err.(*net.OpError); ok {
				if neterr.Err == syscall.ENOBUFS {
					continue
				}
			}
		}

		return n, err
	}
}

// Statistics returns the statistics of the ping.
func buildStats(addr string, ipaddr *net.IPAddr, packetsSent int, packetsRecv int, rtts []time.Duration, bytesSent int, bytesRecv int) *Statistics {
	numRtts := len(rtts)
	var min, max, total time.Duration
	for _, rtt := range rtts {
		if rtt < min {
			min = rtt
		}
		if rtt > max {
			max = rtt
		}
		total += rtt
		numRtts += 1
	}
	loss := float64(packetsSent-packetsRecv) / float64(packetsSent) * 100

	s := Statistics{
		PacketsSent: packetsSent,
		PacketsRecv: packetsRecv,
		PacketLoss:  loss,
		Rtts:        rtts,
		Addr:        addr,
		IPAddr:      ipaddr,
		MaxRtt:      max,
		MinRtt:      min,
		BytesSent:   bytesSent,
		BytesRecv:   bytesRecv,
	}
	if numRtts > 0 {
		s.AvgRtt = total / time.Duration(numRtts)
		var sumsquares time.Duration
		for _, rtt := range rtts {
			sumsquares += (rtt - s.AvgRtt) * (rtt - s.AvgRtt)
		}
		s.StdDevRtt = time.Duration(math.Sqrt(
			float64(sumsquares / time.Duration(numRtts))))
	}
	return &s
}

func byteSliceOfSize(n int) []byte {
	b := make([]byte, n)
	for i := 0; i < len(b); i++ {
		b[i] = 1
	}

	return b
}

func ipv4Payload(b []byte) []byte {
	if len(b) < ipv4.HeaderLen {
		return b
	}
	hdrlen := int(b[0]&0x0f) << 2
	return b[hdrlen:]
}

func isIPv4(ip net.IP) bool {
	return len(ip.To4()) == net.IPv4len
}

func bytesToTime(b []byte) time.Time {
	var nsec int64
	for i := uint8(0); i < 8; i++ {
		nsec += int64(b[i]) << ((7 - i) * 8)
	}
	return time.Unix(nsec/1000000000, nsec%1000000000)
}

func timeToBytes(t time.Time) []byte {
	nsec := t.UnixNano()
	b := make([]byte, 8)
	for i := uint8(0); i < 8; i++ {
		b[i] = byte((nsec >> ((7 - i) * 8)) & 0xff)
	}
	return b
}
