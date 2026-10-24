package coretest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKSUDP verifies an IPv4 payload through a real local SOCKS5 UDP association.
func SOCKSUDP(port int, target string, payload []byte) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte{5, 1, 0})
	var hello [2]byte
	if _, err = io.ReadFull(c, hello[:]); err != nil {
		return err
	}
	c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	var header [4]byte
	if _, err = io.ReadFull(c, header[:]); err != nil {
		return err
	}
	if header[1] != 0 {
		return fmt.Errorf("SOCKS UDP associate rejected")
	}
	n := 4
	if header[3] == 4 {
		n = 16
	} else if header[3] != 1 {
		return fmt.Errorf("unexpected bind family")
	}
	bind := make([]byte, n+2)
	if _, err = io.ReadFull(c, bind); err != nil {
		return err
	}
	u, err := net.Dial("udp", net.JoinHostPort(net.IP(bind[:n]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(bind[n:])))))
	if err != nil {
		return err
	}
	defer u.Close()
	u.SetDeadline(time.Now().Add(2 * time.Second))
	host, p, _ := net.SplitHostPort(target)
	pn, _ := strconv.Atoi(p)
	packet := append([]byte{0, 0, 0, 1}, net.ParseIP(host).To4()...)
	packet = append(packet, byte(pn>>8), byte(pn))
	packet = append(packet, payload...)
	if _, err = u.Write(packet); err != nil {
		return err
	}
	b := make([]byte, 4096)
	nr, err := u.Read(b)
	if err != nil {
		return err
	}
	if nr < 10 || !bytes.Equal(b[10:nr], payload) {
		return fmt.Errorf("UDP payload mismatch")
	}
	return nil
}
