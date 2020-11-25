package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/buger/goreplay/capture"
	"github.com/buger/goreplay/proto"
	"github.com/buger/goreplay/size"
	"github.com/buger/goreplay/stat"
	"github.com/buger/goreplay/tcp"
)

// TCPProtocol is a number to indicate type of protocol
type TCPProtocol uint8

const (
	// ProtocolHTTP ...
	ProtocolHTTP TCPProtocol = iota
	// ProtocolBinary ...
	ProtocolBinary
)

// Set is here so that TCPProtocol can implement flag.Var
func (protocol *TCPProtocol) Set(v string) error {
	switch v {
	case "", "http":
		*protocol = ProtocolHTTP
	case "binary":
		*protocol = ProtocolBinary
	default:
		return fmt.Errorf("unsupported protocol %s", v)
	}
	return nil
}

func (protocol *TCPProtocol) String() string {
	switch *protocol {
	case ProtocolBinary:
		return "binary"
	case ProtocolHTTP:
		return "http"
	default:
		return ""
	}
}

// RAWInputConfig represents configuration that can be applied on raw input
type RAWInputConfig struct {
	capture.PcapOptions
	Expire         time.Duration      `json:"input-raw-expire"`
	CopyBufferSize size.Size          `json:"copy-buffer-size"`
	Engine         capture.EngineType `json:"input-raw-engine"`
	TrackResponse  bool               `json:"input-raw-track-response"`
	Protocol       TCPProtocol        `json:"input-raw-protocol"`
	RealIPHeader   string             `json:"input-raw-realip-header"`
	Stats          bool               `json:"input-raw-stats"`
	quit           chan bool          // Channel used only to indicate goroutine should shutdown
	host           string
	port           uint16
}

// RAWInput used for intercepting traffic for given address
type RAWInput struct {
	sync.Mutex
	RAWInputConfig
	st             *stat.Writer
	listener       *capture.Listener
	message        chan *tcp.Message
	cancelListener context.CancelFunc
}

// NewRAWInput constructor for RAWInput. Accepts raw input config as arguments.
func NewRAWInput(address string, config RAWInputConfig) (i *RAWInput) {
	i = new(RAWInput)
	i.RAWInputConfig = config
	i.message = make(chan *tcp.Message, 1000)
	i.quit = make(chan bool)
	var host, _port string
	var err error
	var port int
	host, _port, err = net.SplitHostPort(address)
	if err != nil {
		log.Fatalf("input-raw: error while parsing address: %s", err)
	}
	if _port != "" {
		port, err = strconv.Atoi(_port)
	}

	if err != nil {
		log.Fatalf("parsing port error: %v", err)
	}
	i.host = host
	i.port = uint16(port)
	if i.Stats {
		i.st = initializeStatWriter("input_raw_message.csv", "type: GOR_STAT\ntitle: raw messages info")
		statsWriteRecord(i.st, []string{
			"length", "lostdata", "source", "destination", "ipversion",
			"isincoming", "timedout", "truncated", "timestamp", "latency",
		}, "[INPUT-RAW]")
	}

	i.listen(address)

	return
}

// PluginRead reads meassage from this plugin
func (i *RAWInput) PluginRead() (*Message, error) {
	var msgTCP *tcp.Message
	var msg Message
	select {
	case <-i.quit:
		if i.st != nil {
			i.st.Close()
			i.st = nil
		}
		return nil, ErrorStopped
	case msgTCP = <-i.message:
		msg.Data = msgTCP.Data()
	}
	var msgType byte = ResponsePayload
	timestamp, latency := msgTCP.Start.UnixNano(), msgTCP.End.UnixNano()-msgTCP.Start.UnixNano()
	if msgTCP.IsIncoming {
		msgType = RequestPayload
		if i.RealIPHeader != "" {
			msg.Data = proto.SetHeader(msg.Data, []byte(i.RealIPHeader), []byte(msgTCP.SrcAddr))
		}
	}
	msg.Meta = payloadHeader(msgType, msgTCP.UUID(), timestamp, latency)

	if i.Stats && i.st != nil {
		statsWriteRecord(i.st, getStats(msgTCP.Stats, timestamp, latency), "[INPUT-RAW]")
	}
	msgTCP = nil
	return &msg, nil
}

func (i *RAWInput) listen(address string) {
	var err error
	i.listener, err = capture.NewListener(i.host, i.port, "", i.Engine, i.TrackResponse)
	if err != nil {
		log.Fatal(err)
	}
	i.listener.SetPcapOptions(i.PcapOptions)
	err = i.listener.Activate()
	if err != nil {
		log.Fatal(err)
	}
	pool := tcp.NewMessagePool(i.CopyBufferSize, i.Expire, Debug, i.handler)
	pool.MatchUUID(i.TrackResponse)
	if i.Protocol == ProtocolHTTP {
		pool.Start = http1StartHint
	}
	var ctx context.Context
	ctx, i.cancelListener = context.WithCancel(context.Background())
	errCh := i.listener.ListenBackground(ctx, pool.Handler)
	select {
	case err := <-errCh:
		log.Fatal(err)
	case <-i.listener.Reading:
		Debug(1, i)
	}
}

func (i *RAWInput) handler(m *tcp.Message) {
	i.message <- m
}

func (i *RAWInput) String() string {
	return fmt.Sprintf("Intercepting traffic from: %s:%d", i.host, i.port)
}

// GetStats returns the stats so far and reset the stats
func getStats(stat tcp.Stats, timestamp, latency int64) []string {
	var s [10]string

	s[0] = strconv.Itoa(stat.Length)    // length
	s[1] = strconv.Itoa(stat.LostData)  // lostdata
	s[2] = stat.SrcAddr                 // source
	s[3] = stat.DstAddr                 // destination
	s[4] = string(stat.IPversion + '0') // ipversion
	if stat.IsIncoming {                // isincoming
		s[5] = "true"
	} else {
		s[5] = "false"
	}
	if stat.TimedOut { // timedout
		s[6] = "true"
	} else {
		s[6] = "false"
	}
	if stat.Truncated { // truncated
		s[7] = "true"
	} else {
		s[7] = "false"
	}
	s[8] = strconv.FormatInt(timestamp, 10) // timestamp
	s[9] = strconv.FormatInt(latency, 10)   // latency
	return s[:]
}

// Close closes the input raw listener
func (i *RAWInput) Close() error {
	i.cancelListener()
	close(i.quit)
	return nil
}

func http1StartHint(pckt *tcp.Packet) (isIncoming, isOutgoing bool) {
	isIncoming = proto.HasRequestTitle(pckt.Payload)
	if isIncoming {
		return
	}
	return false, proto.HasResponseTitle(pckt.Payload)
}
