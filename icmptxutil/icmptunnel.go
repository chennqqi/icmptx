package icmptxutil

import (
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	MTU          = 1472
	protocolICMP = 1
)

type Icmptx struct {
	proxy bool

	Timeout *time.Ticker

	done chan bool

	ipaddr *net.IPAddr
	addr   string
	source string

	id int
}

type packet struct {
	bytes  []byte
	nbytes int
	peer   net.Addr
}

func NewIcmptx() *Icmptx {
	return &Icmptx{
		proxy:   false,
		Timeout: nil,
		done:    make(chan bool),
		ipaddr:  nil,
		addr:    "",
		source:  "0.0.0.0",
		id:      0,
	}
}

func (t *Icmptx) Proxy() bool {
	return t.proxy
}

func (t *Icmptx) IPAddr() *net.IPAddr {
	return t.ipaddr
}

func (t *Icmptx) Addr() string {
	return t.addr
}

func (t *Icmptx) Source() string {
	return t.source
}

func (t *Icmptx) ID() int {
	return t.id
}

func (t *Icmptx) SetIPAddr(ipaddr *net.IPAddr) {
	t.ipaddr = ipaddr
	t.addr = ipaddr.String()
}

func (t *Icmptx) SetAddr(addr string) error {
	ipaddr, err := net.ResolveIPAddr("ip4:icmp", addr)
	if err != nil {
		return err
	}

	t.ipaddr = ipaddr
	t.addr = addr
	return nil
}

func (t *Icmptx) SetSource(localaddr string) {
	t.source = localaddr
}

func (t *Icmptx) SetID(id int) {
	t.id = id
}

func (t *Icmptx) SetProxy(proxy bool) {
	t.proxy = proxy
}

func (t *Icmptx) Run() {
	fc, err := OpenTun()
	if err != nil {
		log.Fatal(err)
	}
	defer fc.Close()

	ic, err := icmp.ListenPacket("ip4:icmp", t.source)
	if err != nil {
		log.Fatal(err)
	}
	defer ic.Close()

	var wg sync.WaitGroup
	rawSock := make(chan *packet, 5)
	tunDev := make(chan *packet, 5)
	wg.Add(2)
	go t.recvICMP(ic, rawSock, &wg)
	go t.recvTun(fc, tunDev, &wg)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	t.Timeout = time.NewTicker(1 * time.Second)
	if t.proxy {
		t.Timeout.Stop()
	}
	for {
		select {
		case <-sig:
			log.Println(sig)
			close(t.done)
		case <-t.done:
			wg.Wait()
			log.Println("Wait done!")
			return
		case p := <-rawSock:
			err = t.processICMP(ic, fc, p)
			if err != nil {
				log.Fatal(err)
			}
		case r := <-tunDev:
			err = t.processTrans(ic, r)
			if err != nil {
				log.Fatal(err)
			}
		case <-t.Timeout.C:
			err = t.sendICMPMsg(ic, ipv4.ICMPTypeEcho, []byte(""))
			if err != nil {
				log.Fatal(err)
			}
		}
	}
}

// parseICMPEcho parses b as an ICMP echo request or reply message body.
func ParseICMPEcho(b []byte) (*icmp.Echo, error) {
	bodylen := len(b)
	p := &icmp.Echo{ID: int(b[0])<<8 | int(b[1]), Seq: int(b[2])<<8 | int(b[3])}
	if bodylen > 4 {
		p.Data = make([]byte, bodylen-4)
		copy(p.Data, b[4:])
	}
	return p, nil
}

func (t *Icmptx) handleSignals() {
	close(t.done)
	return
}

func (t *Icmptx) recvICMP(
	ic *icmp.PacketConn,
	recv chan<- *packet,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	for {
		select {
		case <-t.done:
			return
		default:
			rb := make([]byte, MTU)
			ic.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
			n, peer, err := ic.ReadFrom(rb)
			if err != nil {
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Timeout() {
						continue
					} else {
						close(t.done)
						return
					}
				}
			}
			recv <- &packet{bytes: rb, nbytes: n, peer: peer}
		}
	}
}

func (t *Icmptx) recvTun(
	fc net.PacketConn,
	recv chan<- *packet,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	for {
		select {
		case <-t.done:
			return
		default:
			rb := make([]byte, MTU)
			fc.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
			n, peer, err := fc.ReadFrom(rb)
			if err != nil {
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Timeout() {
						continue
					} else {
						close(t.done)
						return
					}
				}
			}
			recv <- &packet{bytes: rb, nbytes: n, peer: peer}
		}
	}
}

func (t *Icmptx) sendICMPMsg(
	c *icmp.PacketConn,
	typ ipv4.ICMPType,
	bytes []byte,
) error {
	e := &icmp.Echo{
		ID:   t.id,
		Seq:  1,
		Data: bytes,
	}
	for {
		err := sendICMPEcho(c, t.addr, typ, e)
		if err != nil {
			if neterr, ok := err.(*net.OpError); ok {
				if neterr.Err == syscall.ENOBUFS {
					continue
				}
				return err
			}
		}
		break
	}
	return nil
}

func sendICMPEcho(
	c *icmp.PacketConn,
	addr string,
	typ ipv4.ICMPType,
	e *icmp.Echo,
) error {
	wm := icmp.Message{
		Type: typ,
		Code: 0,
		Body: e,
	}
	wb, err := wm.Marshal(nil)
	if err != nil {
		return err
	}

	dst, err := net.ResolveIPAddr("ip4:icmp", addr)
	if _, err := c.WriteTo(wb, dst); err != nil {
		log.Fatal(err)
	}
	return err
}

func (t *Icmptx) processICMP(
	ic *icmp.PacketConn,
	fc net.PacketConn,
	recv *packet,
) error {
	rb := recv.bytes
	rm, err := icmp.ParseMessage(protocolICMP, rb[:recv.nbytes])
	if err != nil {
		return err
	}
	mb, err := rm.Body.Marshal(protocolICMP)
	if err != nil {
		return err
	}
	e, _ := ParseICMPEcho(mb)

	switch rm.Type {
	case ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeEcho:
		if t.id == 0 {
			t.id = e.ID
			t.SetAddr(recv.peer.String())
		}
		if e.ID == t.id {
			log.Println("write to tun")
			fc.WriteTo(e.Data, recv.peer)
		} else if rm.Type == ipv4.ICMPTypeEcho {
			sendICMPEcho(ic, recv.peer.String(), ipv4.ICMPTypeEchoReply, e)
		} else {
			log.Printf("unknown icmp reply %+v\n", rm)
		}
	default:
		log.Printf("got icmp packet %+v\n", rm)
	}
	return nil
}

func (t *Icmptx) processTrans(c *icmp.PacketConn, recv *packet) error {
	var err error
	if t.proxy {
		log.Println("proxy to client")
		err = t.sendICMPMsg(c, ipv4.ICMPTypeEchoReply, recv.bytes)
	} else {
		log.Println("proxy to server")
		t.Timeout = time.NewTicker(1 * time.Second)
		err = t.sendICMPMsg(c, ipv4.ICMPTypeEcho, recv.bytes)
	}
	return err
}
