package nsq

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mreiferson/go-snappystream"
)

// IdentifyResponse represents the metadata
// returned from an IDENTIFY command to nsqd
type IdentifyResponse struct {
	MaxRdyCount int64 `json:"max_rdy_count"`
	TLSv1       bool  `json:"tls_v1"`
	Deflate     bool  `json:"deflate"`
	Snappy      bool  `json:"snappy"`
}

type msgResponse struct {
	msg     *Message
	cmd     *Command
	success bool
}

// Conn represents a connection to nsqd
//
// Conn exposes a set of callbacks for the
// various events that occur on a connection
type Conn struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	messagesInFlight int64
	maxRdyCount      int64
	rdyCount         int64
	lastRdyCount     int64
	lastMsgTimestamp int64

	sync.Mutex

	config *Config

	conn    *net.TCPConn
	tlsConn *tls.Conn
	addr    string

	// ResponseCB is called when the connection
	// receives a FrameTypeResponse from nsqd
	ResponseCB func(*Conn, []byte)

	// ErrorCB is called when the connection
	// receives a FrameTypeError from nsqd
	ErrorCB func(*Conn, []byte)

	// MessageCB is called when the connection
	// receives a FrameTypeMessage from nsqd
	MessageCB func(*Conn, *Message)

	// MessageFinishedCB is called when the connection
	// handles a FIN command from a message handler
	MessageFinishedCB func(*Conn, *Message)

	// MessageRequeuedCB is called when the connection
	// handles a REQ command from a message handler
	MessageRequeuedCB func(*Conn, *Message)

	// IOErrorCB is called when the connection experiences
	// a low-level TCP transport error
	IOErrorCB func(*Conn, error)

	// HeartbeatCB is called when the connection
	// receives a heartbeat from nsqd
	HeartbeatCB func(*Conn)

	// CloseCB is called when the connection
	// closes, after all cleanup
	CloseCB func(*Conn)

	r io.Reader
	w io.Writer

	backoffCounter int32
	rdyRetryTimer  *time.Timer

	cmdChan         chan *Command
	msgResponseChan chan *msgResponse
	exitChan        chan int
	drainReady      chan int

	closeFlag int32
	stopper   sync.Once
	wg        sync.WaitGroup

	readLoopRunning int32
}

// NewConn returns a new Conn instance
func NewConn(addr string, config *Config) *Conn {
	return &Conn{
		addr: addr,

		config: config,

		maxRdyCount:      2500,
		lastMsgTimestamp: time.Now().UnixNano(),

		cmdChan:         make(chan *Command),
		msgResponseChan: make(chan *msgResponse),
		exitChan:        make(chan int),
		drainReady:      make(chan int),
	}
}

// Connect dials and bootstraps the nsqd connection
// (including IDENTIFY) and returns the IdentifyResponse
func (c *Conn) Connect() (*IdentifyResponse, error) {
	conn, err := net.DialTimeout("tcp", c.addr, time.Second)
	if err != nil {
		return nil, err
	}
	c.conn = conn.(*net.TCPConn)
	c.r = conn
	c.w = conn

	_, err = c.Write(MagicV2)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("[%s] failed to write magic - %s", c.addr, err)
	}

	resp, err := c.identify()
	if err != nil {
		return nil, err
	}

	c.wg.Add(2)
	atomic.StoreInt32(&c.readLoopRunning, 1)
	go c.readLoop()
	go c.writeLoop()
	return resp, nil
}

// Close idempotently initiates connection close
func (c *Conn) Close() error {
	atomic.StoreInt32(&c.closeFlag, 1)
	if c.conn != nil && atomic.LoadInt64(&c.messagesInFlight) == 0 {
		return c.conn.CloseRead()
	}
	return nil
}

// IsClosing indicates whether or not the
// connection is currently in the processing of
// gracefully closing
func (c *Conn) IsClosing() bool {
	return atomic.LoadInt32(&c.closeFlag) == 1
}

// RDY returns the current RDY count
func (c *Conn) RDY() int64 {
	return atomic.LoadInt64(&c.rdyCount)
}

// LastRDY returns the previously set RDY count
func (c *Conn) LastRDY() int64 {
	return atomic.LoadInt64(&c.lastRdyCount)
}

// SetRDY stores the specified RDY count
func (c *Conn) SetRDY(rdy int64) {
	atomic.StoreInt64(&c.rdyCount, rdy)
	atomic.StoreInt64(&c.lastRdyCount, rdy)
}

// MaxRDY returns the nsqd negotiated maximum
// RDY count that it will accept for this connection
func (c *Conn) MaxRDY() int64 {
	return c.maxRdyCount
}

// LastMessageTime returns a time.Time representing
// the time at which the last message was received
func (c *Conn) LastMessageTime() time.Time {
	return time.Unix(0, atomic.LoadInt64(&c.lastMsgTimestamp))
}

// Address returns the configured destination nsqd address
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// String returns the fully-qualified address
func (c *Conn) String() string {
	return c.addr
}

// Read performs a deadlined read on the underlying TCP connection
func (c *Conn) Read(p []byte) (int, error) {
	c.conn.SetReadDeadline(time.Now().Add(c.config.readTimeout))
	return c.r.Read(p)
}

// Write performs a deadlined write on the underlying TCP connection
func (c *Conn) Write(p []byte) (int, error) {
	c.conn.SetWriteDeadline(time.Now().Add(c.config.writeTimeout))
	return c.w.Write(p)
}

func (c *Conn) WriteCommand(cmd *Command) error {
	c.Lock()

	_, err := cmd.WriteTo(c)
	if err != nil {
		goto exit
	}
	err = c.Flush()

exit:
	c.Unlock()
	if err != nil {
		c.IOErrorCB(c, err)
	}
	return err
}

type flusher interface {
	Flush() error
}

// Flush writes all buffered data to the underlying TCP connection
func (c *Conn) Flush() error {
	if f, ok := c.w.(flusher); ok {
		return f.Flush()
	}
	return nil
}

// ReadUnpackedResponse reads and parses data from the underlying
// TCP connection according to the NSQ TCP protocol spec and
// returns the frameType, data or error
func (c *Conn) ReadUnpackedResponse() (int32, []byte, error) {
	resp, err := ReadResponse(c)
	if err != nil {
		return -1, nil, err
	}
	return UnpackResponse(resp)
}

func (c *Conn) identify() (*IdentifyResponse, error) {
	ci := make(map[string]interface{})
	ci["client_id"] = c.config.clientID
	ci["hostname"] = c.config.hostname
	ci["user_agent"] = c.config.userAgent
	ci["short_id"] = c.config.clientID // deprecated
	ci["long_id"] = c.config.hostname  // deprecated
	ci["tls_v1"] = c.config.tlsV1
	ci["deflate"] = c.config.deflate
	ci["deflate_level"] = c.config.deflateLevel
	ci["snappy"] = c.config.snappy
	ci["feature_negotiation"] = true
	ci["heartbeat_interval"] = int64(c.config.heartbeatInterval / time.Millisecond)
	ci["sample_rate"] = c.config.sampleRate
	ci["output_buffer_size"] = c.config.outputBufferSize
	ci["output_buffer_timeout"] = int64(c.config.outputBufferTimeout / time.Millisecond)
	cmd, err := Identify(ci)
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	err = c.WriteCommand(cmd)
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	frameType, data, err := c.ReadUnpackedResponse()
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	if frameType == FrameTypeError {
		return nil, ErrIdentify{string(data)}
	}

	// check to see if the server was able to respond w/ capabilities
	// i.e. it was a JSON response
	if data[0] != '{' {
		return nil, nil
	}

	resp := &IdentifyResponse{}
	err = json.Unmarshal(data, resp)
	if err != nil {
		return nil, ErrIdentify{err.Error()}
	}

	c.maxRdyCount = resp.MaxRdyCount

	if resp.TLSv1 {
		err := c.upgradeTLS(c.config.tlsConfig)
		if err != nil {
			return nil, ErrIdentify{err.Error()}
		}
	}

	if resp.Deflate {
		err := c.upgradeDeflate(c.config.deflateLevel)
		if err != nil {
			return nil, ErrIdentify{err.Error()}
		}
	}

	if resp.Snappy {
		err := c.upgradeSnappy()
		if err != nil {
			return nil, ErrIdentify{err.Error()}
		}
	}

	// now that connection is bootstrapped, enable read buffering
	// (and write buffering if it's not already capable of Flush())
	c.r = bufio.NewReader(c.r)
	if _, ok := c.w.(flusher); !ok {
		c.w = bufio.NewWriter(c.w)
	}

	return resp, nil
}

func (c *Conn) upgradeTLS(conf *tls.Config) error {
	c.tlsConn = tls.Client(c.conn, conf)
	err := c.tlsConn.Handshake()
	if err != nil {
		return err
	}
	c.r = c.tlsConn
	c.w = c.tlsConn
	frameType, data, err := c.ReadUnpackedResponse()
	if err != nil {
		return err
	}
	if frameType != FrameTypeResponse || !bytes.Equal(data, []byte("OK")) {
		return errors.New("invalid response from TLS upgrade")
	}
	return nil
}

func (c *Conn) upgradeDeflate(level int) error {
	conn := net.Conn(c.conn)
	if c.tlsConn != nil {
		conn = c.tlsConn
	}
	fw, _ := flate.NewWriter(conn, level)
	c.r = flate.NewReader(conn)
	c.w = fw
	frameType, data, err := c.ReadUnpackedResponse()
	if err != nil {
		return err
	}
	if frameType != FrameTypeResponse || !bytes.Equal(data, []byte("OK")) {
		return errors.New("invalid response from Deflate upgrade")
	}
	return nil
}

func (c *Conn) upgradeSnappy() error {
	conn := net.Conn(c.conn)
	if c.tlsConn != nil {
		conn = c.tlsConn
	}
	c.r = snappystream.NewReader(conn, snappystream.SkipVerifyChecksum)
	c.w = snappystream.NewWriter(conn)
	frameType, data, err := c.ReadUnpackedResponse()
	if err != nil {
		return err
	}
	if frameType != FrameTypeResponse || !bytes.Equal(data, []byte("OK")) {
		return errors.New("invalid response from Snappy upgrade")
	}
	return nil
}

func (c *Conn) readLoop() {
	for {
		if atomic.LoadInt32(&c.closeFlag) == 1 {
			goto exit
		}

		frameType, data, err := c.ReadUnpackedResponse()
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				c.IOErrorCB(c, err)
			}
			goto exit
		}

		if frameType == FrameTypeResponse && bytes.Equal(data, []byte("_heartbeat_")) {
			c.HeartbeatCB(c)
			err := c.WriteCommand(Nop())
			if err != nil {
				c.IOErrorCB(c, err)
				goto exit
			}
			continue
		}

		switch frameType {
		case FrameTypeResponse:
			c.ResponseCB(c, data)
		case FrameTypeMessage:
			msg, err := DecodeMessage(data)
			if err != nil {
				c.IOErrorCB(c, err)
				goto exit
			}
			msg.FinishCB = func(m *Message) {
				c.msgResponseChan <- &msgResponse{m, Finish(m.Id), true}
			}
			msg.RequeueCB = func(m *Message, delay time.Duration) {
				if delay == -1 {
					// linear delay
					delay = c.config.defaultRequeueDelay * time.Duration(m.Attempts)
					// bound the requeueDelay to configured max
					if delay > c.config.maxRequeueDelay {
						delay = c.config.maxRequeueDelay
					}
				}
				c.msgResponseChan <- &msgResponse{m, Requeue(m.Id, delay), false}
			}
			msg.TouchCB = func(m *Message) {
				select {
				case c.cmdChan <- Touch(m.Id):
				case <-c.exitChan:
				}
			}

			atomic.AddInt64(&c.rdyCount, -1)
			atomic.AddInt64(&c.messagesInFlight, 1)
			atomic.StoreInt64(&c.lastMsgTimestamp, time.Now().UnixNano())

			c.MessageCB(c, msg)
		case FrameTypeError:
			c.ErrorCB(c, data)
		default:
			c.IOErrorCB(c, fmt.Errorf("unknown frame type %d", frameType))
		}
	}

exit:
	atomic.StoreInt32(&c.readLoopRunning, 0)
	// start the connection close
	messagesInFlight := atomic.LoadInt64(&c.messagesInFlight)
	if messagesInFlight == 0 {
		// if we exited readLoop with no messages in flight
		// we need to explicitly trigger the close because
		// writeLoop won't
		c.close()
	} else {
		log.Printf("[%s] delaying close, %d outstanding messages",
			c, messagesInFlight)
	}
	c.wg.Done()
	log.Printf("[%s] readLoop exiting", c)
}

func (c *Conn) writeLoop() {
	for {
		select {
		case <-c.exitChan:
			log.Printf("[%s] breaking out of writeLoop", c)
			// Indicate drainReady because we will not pull any more off finishedMessages
			close(c.drainReady)
			goto exit
		case cmd := <-c.cmdChan:
			err := c.WriteCommand(cmd)
			if err != nil {
				log.Printf("[%s] error sending command %s - %s", c, cmd, err)
				c.close()
				continue
			}
		case resp := <-c.msgResponseChan:
			// Decrement this here so it is correct even if we can't respond to nsqd
			msgsInFlight := atomic.AddInt64(&c.messagesInFlight, -1)

			err := c.WriteCommand(resp.cmd)
			if err != nil {
				log.Printf("[%s] error sending command %s - %s", c, resp.cmd, err)
				c.close()
				continue
			}

			if resp.success {
				c.MessageFinishedCB(c, resp.msg)
			} else {
				c.MessageRequeuedCB(c, resp.msg)
			}

			if msgsInFlight == 0 &&
				atomic.LoadInt32(&c.closeFlag) == 1 {
				c.close()
				continue
			}
		}
	}

exit:
	c.wg.Done()
	log.Printf("[%s] writeLoop exiting", c)
}

func (c *Conn) close() {
	// a "clean" connection close is orchestrated as follows:
	//
	//     1. CLOSE cmd sent to nsqd
	//     2. CLOSE_WAIT response received from nsqd
	//     3. set c.closeFlag
	//     4. readLoop() exits
	//         a. if messages-in-flight > 0 delay close()
	//             i. writeLoop() continues receiving on c.finishedMessages chan
	//                 x. when messages-in-flight == 0 call close()
	//         b. else call close() immediately
	//     5. c.exitChan close
	//         a. writeLoop() exits
	//             i. c.drainReady close
	//     6a. launch cleanup() goroutine (we're racing with intraprocess
	//        routed messages, see comments below)
	//         a. wait on c.drainReady
	//         b. loop and receive on c.finishedMessages chan
	//            until messages-in-flight == 0
	//            i. ensure that readLoop has exited
	//     6b. launch waitForCleanup() goroutine
	//         b. wait on waitgroup (covers readLoop() and writeLoop()
	//            and cleanup goroutine)
	//         c. underlying TCP connection close
	//         d. trigger CloseCB()
	//
	c.stopper.Do(func() {
		log.Printf("[%s] beginning close", c)
		close(c.exitChan)
		c.conn.CloseRead()

		c.wg.Add(1)
		go c.cleanup()

		go c.waitForCleanup()
	})
}

func (c *Conn) cleanup() {
	<-c.drainReady
	ticker := time.NewTicker(100 * time.Millisecond)
	// writeLoop has exited, drain any remaining in flight messages
	for {
		// we're racing with readLoop which potentially has a message
		// for handling so infinitely loop until messagesInFlight == 0
		// and readLoop has exited
		var msgsInFlight int64
		select {
		case <-c.msgResponseChan:
			msgsInFlight = atomic.AddInt64(&c.messagesInFlight, -1)
		case <-ticker.C:
			msgsInFlight = atomic.LoadInt64(&c.messagesInFlight)
		}
		if msgsInFlight > 0 {
			log.Printf("[%s] draining... waiting for %d messages in flight", c, msgsInFlight)
			continue
		}
		// until the readLoop has exited we cannot be sure that there
		// still won't be a race
		if atomic.LoadInt32(&c.readLoopRunning) == 1 {
			log.Printf("[%s] draining... readLoop still running", c)
			continue
		}
		goto exit
	}

exit:
	ticker.Stop()
	c.wg.Done()
	log.Printf("[%s] finished draining, cleanup exiting", c)
}

func (c *Conn) waitForCleanup() {
	// this blocks until readLoop and writeLoop
	// (and cleanup goroutine above) have exited
	c.wg.Wait()
	c.conn.CloseWrite()
	log.Printf("[%s] clean close complete", c)
	c.CloseCB(c)
}
