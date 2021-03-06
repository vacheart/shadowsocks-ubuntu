package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	ss "github.com/shadowsocks/shadowsocks-go/shadowsocks"
)

const (
	socksVer5       = 5
	socksCmdConnect = 1
	directionOutput = 0
	directionInput  = 1
)

var (
	errAddrType      = errors.New("socks addr type not supported")
	errVer           = errors.New("socks version not supported")
	errMethod        = errors.New("socks only support 1 method now")
	errAuthExtraData = errors.New("socks authentication get extra data")
	errReqExtraData  = errors.New("socks request get extra data")
	errCmd           = errors.New("socks command not supported")
)

// Service is a tcp proxy service
type Service struct {
	ch              chan bool
	waitGroup       *sync.WaitGroup
	serverCipher    *ServerCipher
	debug           ss.DebugLog
	trafficListener TrafficListener
}

// ServerCipher shadowsock servier chipher
type ServerCipher struct {
	server string
	cipher *ss.Cipher
}

// TrafficListener listen sent/received traffic
type TrafficListener interface {
	Sent(int)
	Received(int)
}

// NewService return a proxy service
func NewService(serverCipher *ServerCipher) *Service {
	s := &Service{
		make(chan bool),
		&sync.WaitGroup{},
		serverCipher,
		true,
		nil,
	}
	s.waitGroup.Add(1)
	return s
}

// SetTrafficListener set listener in service
func (s *Service) SetTrafficListener(listener TrafficListener) {
	s.trafficListener = listener
}

// Serve to serve a listener
func (s *Service) Serve(listener *net.TCPListener) {
	defer s.waitGroup.Done()
	for {
		select {
		case <-s.ch:
			s.debug.Println("stopping listening on", listener.Addr())
			listener.Close()
			return
		default:
		}
		listener.SetDeadline(time.Now().Add(1e9))
		conn, err := listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				continue
			} else {
				s.debug.Println(err)
				continue
			}
		}
		s.debug.Printf("socks connect from %s\n", conn.RemoteAddr().String())
		s.waitGroup.Add(1)
		go s.handleConnection(conn)
	}
}

// Stop is a graceful method to stop service
func (s *Service) Stop() {
	close(s.ch)
	s.waitGroup.Wait()
}

func (s *Service) handleConnection(conn net.Conn) {
	defer s.waitGroup.Done()
	defer func() {
		conn.Close()
	}()

	if err := s.handShake(conn); err != nil {
		s.debug.Println("socks handshake:", err)
		return
	}

	rawaddr, addr, err := s.getRequest(conn)
	if err != nil {
		s.debug.Println("error getting request:", err)
		return
	}
	// Sending connection established message immediately to client.
	// This some round trip time for creating socks connection with the client.
	// But if connection failed, the client will get connection reset error.
	_, err = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x08, 0x43})
	if err != nil {
		s.debug.Println("send connection confirmation:", err)
	}

	s.debug.Printf("connected to %s via %s\n", addr, s.serverCipher.server)

	cipher := s.serverCipher.cipher
	serverAddrPort := s.serverCipher.server
	remote, err := ss.DialWithRawAddr(rawaddr, serverAddrPort, cipher.Copy())
	if err != nil {
		s.debug.Println(err)
		return
	}

	s.waitGroup.Add(1)
	go func() {
		defer s.waitGroup.Done()
		// remote to local
		s.pipeThenClose(remote, conn, directionInput)
	}()
	// local to remote
	s.pipeThenClose(conn, remote, directionOutput)
	s.debug.Println("closed connection to", addr)
}

func (s *Service) handShake(conn net.Conn) (err error) {
	const (
		idVer     = 0
		idNmethod = 1
	)
	// version identification and method selection message in theory can have
	// at most 256 methods, plus version and nmethod field in total 258 bytes
	// the current rfc defines only 3 authentication methods (plus 2 reserved),
	// so it won't be such long in practice

	buf := make([]byte, 258)

	var n int
	ss.SetReadTimeout(conn)
	// make sure we get the nmethod field
	if n, err = io.ReadAtLeast(conn, buf, idNmethod+1); err != nil {
		return
	}
	if buf[idVer] != socksVer5 {
		return errVer
	}
	nmethod := int(buf[idNmethod])
	msgLen := nmethod + 2
	if n == msgLen { // handshake done, common case
		// do nothing, jump directly to send confirmation
	} else if n < msgLen { // has more methods to read, rare case
		if _, err = io.ReadFull(conn, buf[n:msgLen]); err != nil {
			return
		}
	} else { // error, should not get extra data
		return errAuthExtraData
	}
	// send confirmation: version 5, no authentication required
	_, err = conn.Write([]byte{socksVer5, 0})
	return
}

func (s *Service) getRequest(conn net.Conn) (rawaddr []byte, host string, err error) {
	const (
		idVer   = 0
		idCmd   = 1
		idType  = 3 // address type index
		idIP0   = 4 // ip addres start index
		idDmLen = 4 // domain address length index
		idDm0   = 5 // domain address start index

		typeIPv4 = 1 // type is ipv4 address
		typeDm   = 3 // type is domain address
		typeIPv6 = 4 // type is ipv6 address

		lenIPv4   = 3 + 1 + net.IPv4len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv4 + 2port
		lenIPv6   = 3 + 1 + net.IPv6len + 2 // 3(ver+cmd+rsv) + 1addrType + ipv6 + 2port
		lenDmBase = 3 + 1 + 1 + 2           // 3 + 1addrType + 1addrLen + 2port, plus addrLen
	)
	// refer to getRequest in server.go for why set buffer size to 263
	buf := make([]byte, 263)
	var n int
	ss.SetReadTimeout(conn)
	// read till we get possible domain length field
	if n, err = io.ReadAtLeast(conn, buf, idDmLen+1); err != nil {
		return
	}
	// check version and cmd
	if buf[idVer] != socksVer5 {
		err = errVer
		return
	}
	if buf[idCmd] != socksCmdConnect {
		err = errCmd
		return
	}

	reqLen := -1
	switch buf[idType] {
	case typeIPv4:
		reqLen = lenIPv4
	case typeIPv6:
		reqLen = lenIPv6
	case typeDm:
		reqLen = int(buf[idDmLen]) + lenDmBase
	default:
		err = errAddrType
		return
	}

	if n == reqLen {
		// common case, do nothing
	} else if n < reqLen { // rare case
		if _, err = io.ReadFull(conn, buf[n:reqLen]); err != nil {
			return
		}
	} else {
		err = errReqExtraData
		return
	}

	rawaddr = buf[idType:reqLen]

	if s.debug {
		switch buf[idType] {
		case typeIPv4:
			host = net.IP(buf[idIP0 : idIP0+net.IPv4len]).String()
		case typeIPv6:
			host = net.IP(buf[idIP0 : idIP0+net.IPv6len]).String()
		case typeDm:
			host = string(buf[idDm0 : idDm0+buf[idDmLen]])
		}
		port := binary.BigEndian.Uint16(buf[reqLen-2 : reqLen])
		host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	}

	return
}

// pipeThenClose copies data from src to dst, closes dst when done.
func (s *Service) pipeThenClose(src, dst net.Conn, directionFlag int) {
	defer dst.Close()
	buf := leakyBuf.Get()
	defer leakyBuf.Put(buf)
	for {
		select {
		case <-s.ch:
			return
		default:
		}
		src.SetReadDeadline(time.Now().Add(5e9))
		n, err := src.Read(buf)
		// read may return EOF with n > 0
		// should always process n > 0 bytes before handling error
		if n > 0 {
			// Note: avoid overwrite err returned by Read.
			if n, err := dst.Write(buf[0:n]); err != nil {
				s.debug.Println("write:", err)
				break
			} else {
				if s.trafficListener != nil {
					switch directionFlag {
					case directionOutput:
						s.trafficListener.Sent(n)
					case directionInput:
						s.trafficListener.Received(n)
					}
				}
			}
		}
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				continue
			}
			break
		}
	}
}
