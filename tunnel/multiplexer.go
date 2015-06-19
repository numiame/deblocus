package tunnel

import (
	"encoding/binary"
	"fmt"
	log "github.com/spance/deblocus/golang/glog"
	"io"
	"net"
	//"reflect"
	"strconv"
	"sync"
	//	"sync/atomic"
	"time"
)

const (
	FRAME_ACTION_CLOSE         = 0
	FRAME_ACTION_OPEN          = 1
	FRAME_ACTION_OPEN_N        = 2
	FRAME_ACTION_OPEN_Y        = 3
	FRAME_ACTION_DATA          = 4
	FRAME_STATUS_HALF_CLOSE    = 0xfe
	FRAME_STATUS_PENDING_CLOSE = 0xff
	FRAME_MAX_LEN              = 0xffff
	FRAME_HEADER_LEN           = 5
	FRAME_OPEN_TIMEOUT         = time.Second * 9
)

var (
	SID_SEQ uint32
	seqLock sync.Locker = new(sync.Mutex)
)

type edgeConn struct {
	conn   net.Conn
	ready  chan byte // peer status
	key    string
	target string
	status byte
}

type frame struct {
	action uint8
	sid    uint16
	length uint16
	data   []byte
	conn   *edgeConn
}

func (f *frame) String() string {
	return fmt.Sprintf("Frame{act=%d sid=%d len=%d}", f.action, f.sid, f.length)
}

func (f *frame) toNewBuffer() []byte {
	b := make([]byte, f.length+FRAME_HEADER_LEN)
	b[0] = f.action
	binary.BigEndian.PutUint16(b[1:], f.sid)
	binary.BigEndian.PutUint16(b[3:], f.length)
	if f.length > 0 {
		copy(b[FRAME_HEADER_LEN:], f.data)
	}
	return b
}

type onTunDisconnectedCallback func(old *Conn)

type multiplexer struct {
	size     int
	isClient bool
	pool     *ConnPool
	registry map[string]*edgeConn
	closed   map[string]bool
	cLock    sync.Locker
	tLock    sync.Locker
	queue    *queue
	mode     string
	onTDC    onTunDisconnectedCallback
}

func NewClientMultiplexer() *multiplexer {
	m := &multiplexer{
		size:     1,
		isClient: true,
		pool:     NewConnPool(),
		registry: make(map[string]*edgeConn),
		closed:   make(map[string]bool),
		cLock:    new(sync.Mutex),
		tLock:    new(sync.Mutex),
		mode:     "CLI",
	}
	m.queue = NewQueue(m)
	go m.queue.Listen()
	return m
}

func NewServerMultiplexer() *multiplexer {
	m := &multiplexer{
		registry: make(map[string]*edgeConn),
		closed:   make(map[string]bool),
		cLock:    new(sync.RWMutex),
		mode:     "SVR",
	}
	m.queue = NewQueue(m)
	go m.queue.Listen()
	return m
}

func (p *multiplexer) registerConn(key string, conn net.Conn) {
	p.cLock.Lock()
	defer p.cLock.Unlock()
	p.registry[key] = &edgeConn{conn: conn, key: key}
}

func (p *multiplexer) registerEdgeConn(key string, conn net.Conn, target_str string) {
	p.cLock.Lock()
	defer p.cLock.Unlock()
	p.registry[key] = &edgeConn{
		conn:   conn,
		ready:  make(chan byte, 1),
		key:    key,
		target: target_str,
	}
}

// set passively close mark
func (p *multiplexer) unregisterConn(key string, isPasv bool) (edge *edgeConn) {
	p.cLock.Lock()
	defer p.cLock.Unlock()
	if isPasv {
		p.closed[key] = true
	}
	edge = p.registry[key]
	// edge主动关闭时：remove registry 拒绝tun-queue投递
	// 被动关闭时：发完tun-queue的余货后在queue中关闭，再调用此remove registry
	if edge != nil {
		delete(p.registry, key)
		if edge.ready != nil {
			if _, y := <-edge.ready; y {
				close(edge.ready)
			}
		}
	}
	return
}

// check if closed passively
func (p *multiplexer) ckeckClosed(key string) bool {
	p.cLock.Lock()
	defer p.cLock.Unlock()
	t := p.closed[key]
	if t {
		delete(p.closed, key)
	}
	return t
}

func (p *multiplexer) getRegistered(key string) *edgeConn {
	p.cLock.Lock()
	defer p.cLock.Unlock()
	return p.registry[key]
}

func (p *multiplexer) HandleRequest(client net.Conn, target []byte, target_str string) {
	sid := _nextSID()
	key := p.tunKey(nil, sid)
	defer p.unregisterConn(key, false)
	p.registerEdgeConn(key, client, target_str)
	bconn := p.pool.Select()
	p.copyToTun(client, bconn, key, sid, target)
}

func (p *multiplexer) onTunDisconnected(tun *Conn) {
	p.pool.Remove(tun)
	if p.onTDC != nil {
		go p.onTDC(tun)
	}
}

func (p *multiplexer) Listen(tun *Conn, ctl *CtlThread) {
	if p.isClient {
		tun.priority = new(TSPriority)
		p.pool.Push(tun)
		defer p.onTunDisconnected(tun)
	}
	tun.SetSockOpt(1, 1, 0)
	var (
		frm      *frame
		header   = make([]byte, FRAME_HEADER_LEN)
		nr, nw   int
		er, ew   error
		now      int64
		lastTime = time.Now().Unix()
	)
	for {
		nr, er = io.ReadFull(tun, header)
		if nr == FRAME_HEADER_LEN {
			frm = _parseFrameHeader(header)
			frm.data = make([]byte, frm.length)
			nr, er = io.ReadFull(tun, frm.data)
			if log.V(6) {
				log.Infof("%s <- %s\n", p.mode, frm)
			}
		}
		if er != nil {
			log.Errorln("Read tunnel error.", er)
			return // error, abandon tunnel
		}
		key := p.tunKey(tun, frm.sid)

		switch frm.action {
		case FRAME_ACTION_CLOSE:
			if log.V(5) {
				log.Infof("%s recv CLOSE %s by peer", p.mode, key)
			}
			if edge := p.unregisterConn(key, true); edge != nil {
				frm.conn = edge
				p.queue.push(frm)
			}
		case FRAME_ACTION_DATA:
			edge := p.getRegistered(key)
			if edge == nil {
				log.Warningf("peer try send data to an unexisted socket. %s.key=%s %s", p.mode, key, frm)
				// when the edgeConn of this side is proactively closed, will enter here.
				// so need to send close for notify peer.
				_frame(header, FRAME_ACTION_CLOSE, frm.sid, nil)
				nw, ew = tun.Write(header)
				if nw != FRAME_HEADER_LEN || ew != nil {
					log.Errorln("Write tunnel error.", er)
					return // error, abandon tunnel
				}
			} else {
				frm.conn = edge
				p.queue.push(frm)
			}
		case FRAME_ACTION_OPEN:
			go p.openEgress(frm, key, tun)
		case FRAME_ACTION_OPEN_N, FRAME_ACTION_OPEN_Y:
			edge := p.getRegistered(key)
			if edge == nil {
				log.Warningf("peer send OPEN_%d to an unexisted socket. %s.key=%s\n", frm.action, p.mode, key)
			} else {
				if log.V(5) {
					log.Infof("recv action %s\n", frm)
				}
				edge.ready <- frm.action
				close(edge.ready)
			}
		default:
			log.Errorf("%s Unrecognized frame %s\n", p.mode, frm)
		}
		// prevent frequently calling, especially in high-speed transmitting.
		if now = time.Now().Unix(); (now-lastTime) > 2 && ctl != nil {
			lastTime = now
			ctl.active(now)
		}
		if p.isClient {
			tun.Update()
		}
	}
}

func (p *multiplexer) tunKey(tun *Conn, sid uint16) string {
	if p.isClient {
		return strconv.Itoa(int(sid))
	} else {
		return fmt.Sprintf("%s_%d", tun.RemoteAddr(), sid)
	}
}

func (p *multiplexer) openEgress(frm *frame, key string, tun *Conn) {
	var (
		s5      = new(S5Target)
		dstConn net.Conn
		err     error
		nw      int
	)

	dstConn, err = s5.parseSocks5Target(frm.data)
	frm.length = 0
	if err != nil {
		log.Errorf("Cannot connect to [%s] error: %s\n", s5.dst_str, err)
		frm.action = FRAME_ACTION_OPEN_N
		nw, err = tun.Write(frm.toNewBuffer())
		ThrowIf(nw != FRAME_HEADER_LEN, err)
	} else {
		p.registerConn(key, dstConn)
		dstConn.(*net.TCPConn).SetNoDelay(true)
		if log.V(5) {
			log.Infoln("established open_y", frm.sid, s5.dst_str)
		}
		frm.action = FRAME_ACTION_OPEN_Y
		nw, err = tun.Write(frm.toNewBuffer())
		ThrowIf(nw != FRAME_HEADER_LEN, err)
		p.copyToTun(dstConn, tun, key, frm.sid, nil)
	}
}

func (p *multiplexer) copyToTun(src net.Conn, tun *Conn, key string, sid uint16, target []byte) {
	var (
		buf    = make([]byte, FRAME_MAX_LEN)
		nr, nw int
		er, ew error
	)
	defer func() {
		if !p.ckeckClosed(key) { // only proactive mode could send close
			_frame(buf, FRAME_ACTION_CLOSE, sid, nil)
			nw, ew = tun.Write(buf[:FRAME_HEADER_LEN])
			//ThrowIf(nw != FRAME_HEADER_LEN, ew) // ignore
		}
		// who read, who close
		// if closed passively, there is second close
		src.Close()
	}()
	src.SetReadDeadline(ZERO_TIME)
	if target != nil { // for client:
		// new connection must send OPEN first.
		_len := _frame(buf, FRAME_ACTION_OPEN, sid, target)
		nw, ew = tun.Write(buf[:_len])
		if _len != nw || ew != nil { // close tunnel ?
			SafeClose(tun)
			return
		}
		edge := p.getRegistered(key)
		var code byte
		select {
		case code = <-edge.ready:
		case <-time.After(FRAME_OPEN_TIMEOUT):
			log.Errorf("waiting open signal timeout sid=%d target=%s", sid, edge.target)
		}
		if code != FRAME_ACTION_OPEN_Y {
			return
		}
	}
	for {
		nr, er = src.Read(buf[FRAME_HEADER_LEN:])
		if nr > 0 {
			buf[0] = FRAME_ACTION_DATA
			binary.BigEndian.PutUint16(buf[1:], sid)
			binary.BigEndian.PutUint16(buf[3:], uint16(nr))
			nr += FRAME_HEADER_LEN
			nw, ew = tun.Write(buf[:nr])
			if nr != nw || ew != nil { // close tunnel ?
				fmt.Printf("error tun.Write sid=%d tun->%s %v\n", sid, tun.RemoteAddr(), ew)
				SafeClose(tun)
				return
			}
		}
		if er != nil {
			return
		}
	}
}

func _nextSID() uint16 {
	seqLock.Lock()
	defer seqLock.Unlock()
	SID_SEQ += 1
	if SID_SEQ > 0xffff {
		SID_SEQ = 0
	}
	return uint16(SID_SEQ)
}

func _parseFrameHeader(header []byte) *frame {
	return &frame{
		header[0],
		binary.BigEndian.Uint16(header[1:]),
		binary.BigEndian.Uint16(header[3:]),
		nil, nil,
	}
}

func _frame(buf []byte, action byte, sid uint16, body []byte) int {
	var _len = FRAME_HEADER_LEN
	buf[0] = action
	binary.BigEndian.PutUint16(buf[1:], sid)
	if body != nil {
		_len += len(body)
		binary.BigEndian.PutUint16(buf[3:], uint16(len(body)))
		copy(buf[FRAME_HEADER_LEN:], body)
	} else {
		buf[3] = 0
		buf[4] = 0
	}
	return _len
}