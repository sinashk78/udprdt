package rdt

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sinashk78/go-p2p-udp/packet"
)

type RDT interface {
	RdtSend([]byte, net.Addr) (int, error)
	RdtRecv() ([]byte, error)
}

type packetWrapper struct {
	pkt      packet.Packet
	destAddr net.Addr
	timer    *time.Timer
}

type SelectiveRepeatUdpRdt struct {
	sendBase    uint32
	sendNextSeq uint32
	recvBase    uint32
	windowSize  uint32
	sendMaxBuf  uint32
	recvMaxBuf  uint32

	sendBuffer []packetWrapper
	recvBuffer []packetWrapper

	sendLock *sync.RWMutex
	recvLock *sync.RWMutex

	timeout time.Duration

	udt UDT
}

func NewSelectiveRepeateUdpRdt(sendMaxBuf, recvMaxBuf, windowSize uint32, timeout time.Duration, udt UDT) RDT {
	return &SelectiveRepeatUdpRdt{
		sendBase:    1,
		sendNextSeq: 1,
		recvBase:    0,
		windowSize:  windowSize,
		sendMaxBuf:  sendMaxBuf,
		recvMaxBuf:  recvMaxBuf,
		sendBuffer:  make([]packetWrapper, sendMaxBuf),
		recvBuffer:  make([]packetWrapper, recvMaxBuf),
		sendLock:    &sync.RWMutex{},
		recvLock:    &sync.RWMutex{},
		timeout:     timeout,
		udt:         udt,
	}
}

func (rdt *SelectiveRepeatUdpRdt) RdtSend(data []byte, addr net.Addr) (int, error) {
	rdt.sendLock.Lock()
	defer rdt.sendLock.Unlock()
	// TODO if some error occurs the packets remains in the buffer make sure that's ok

	// if the window is full don't send a packet
	if rdt.sendNextSeq >= rdt.sendBase+rdt.windowSize {
		return 0, fmt.Errorf("rdt.go: too many packet in the buffer")
	}

	idx := rdt.sendNextSeq % rdt.sendMaxBuf
	rdt.sendBuffer[idx] = packetWrapper{
		pkt:      packet.Packet{Headers: packet.PacketHeader{Sequence: rdt.sendNextSeq, DataLength: uint32(len(data))}, Data: data},
		destAddr: addr,
		timer:    time.NewTimer(rdt.timeout),
	}
	binPkt, err := rdt.sendBuffer[idx].pkt.Marshal()
	if err != nil {
		return 0, err
	}

	fmt.Println("rdt.go: attempting to send packet: ", rdt.sendNextSeq)
	_, err = rdt.udt.UdtSend(binPkt, addr)
	if err != nil {
		return 0, err
	}

	printf("rdt.go: packet %d has been sent.\n", rdt.sendNextSeq)

	if rdt.sendBase == rdt.sendNextSeq {
		rdt.sendBuffer[idx].StartTimer(rdt.timeout, rdt.Timeout)
	}

	rdt.sendNextSeq++

	return len(data), nil
}

func (rdt *SelectiveRepeatUdpRdt) RdtRecv() ([]byte, error) {
	go func() {
		if e := recover(); e != nil {
			fmt.Println("rdt.go: RdtRecv panic", e)
		}
	}()
	// if the the recvWindow is full don't recv the packet
start:
	// get the header to identify the payload size
	//binHeaders, err := rdt.udt.UdtRecvHeader(packet.HeaderSize)
	//if err != nil {
	//	return nil, err
	//}
	//
	//headers, err := packet.UnMarshalHeader(binHeaders)
	//if err != nil {
	//	return nil, err
	//}
	headers := packet.PacketHeader{
		Ack:        false,
		Sequence:   1,
		DataLength: 3,
	}

	printf("rdt.go: header for packet %d is: %v\n", headers.Sequence, headers)

	// {false, 1, 3}
	if headers.Ack {
		// if it's the oldest unacked packet then we move recvBase forward
		rdt.sendLock.Lock()
		if headers.Sequence == rdt.sendBase {
			rdt.sendBase++
		}
		if rdt.sendBase == rdt.sendNextSeq {
			fmt.Println(headers.Sequence % rdt.sendMaxBuf)
			rdt.sendBuffer[headers.Sequence%rdt.sendMaxBuf].StopTimer()
		} else {
			rdt.sendBuffer[headers.Sequence%rdt.sendMaxBuf].StartTimer(rdt.timeout, rdt.Timeout)
		}
		discarded, err := rdt.udt.UdtDiscard(packet.HeaderSize - 4)
		if err != nil {
			fmt.Println("rdt.go: failed to discard packet packet: ", headers.Sequence, err)
		}

		fmt.Println("rdt.go:  discarded packet: ", headers.Sequence, discarded)

		rdt.sendLock.Unlock()
		fmt.Println("rdt.go: received ack for packet: ", headers.Sequence)
		goto start
	}

	// add the packet to recvBuffer if not exists
	rdt.recvLock.Lock()
	if rdt.recvBuffer[headers.Sequence%rdt.recvMaxBuf].pkt.Headers.Sequence == headers.Sequence {
		// If the packet exists discard the packet and send ack then goto start
		// send ack

		printf("rdt.go: attempting to resend ack for packet %d\n", headers.Sequence)
		err := rdt.SendAck(headers.Sequence, rdt.recvBuffer[headers.Sequence%rdt.recvMaxBuf].destAddr)
		if err != nil {
			// TODO how to handle this
		}

		discarded, err := rdt.udt.UdtDiscard(packet.HeaderSize + rdt.recvBuffer[headers.Sequence%rdt.recvMaxBuf].pkt.Headers.DataLength)
		if err != nil {
			fmt.Println("rdt.go: failed to discard packet packet: ", headers.Sequence, err)
		}

		fmt.Println("rdt.go:  discarded packet: ", headers.Sequence, discarded)
		rdt.recvLock.Unlock()
		goto start
	}

	// recv the entire packet
	buf := make([]byte, headers.DataLength)
	_, addr, err := rdt.udt.UdtRecv(buf)
	if err != nil {
		printf("rdt.go: failed to recv packet %d\n", headers.Sequence)
		rdt.recvLock.Unlock()
		return nil, err
	}

	printf("recived packet %d with data: %s\n", headers.Sequence, string(buf))

	// we don't need to keep the data, the header is enough
	// since we don't care about order and we give the data
	// to the caller right after we received it
	// TODO handle edge cases for recvBuffer
	rdt.recvBuffer[headers.Sequence%rdt.recvMaxBuf] = packetWrapper{
		pkt:      packet.Packet{Headers: headers},
		timer:    time.NewTimer(rdt.timeout),
		destAddr: addr,
	}

	rdt.recvLock.Unlock()

	// send ack
	printf("rdt.go: attempting to send ack for packet %d\n", headers.Sequence)
	err = rdt.SendAck(headers.Sequence, addr)
	if err != nil {
		// TODO how to handle this
	}

	// TODO handle no data
	return buf[packet.HeaderSize:], nil
}

func (rdt *SelectiveRepeatUdpRdt) SendAck(sequence uint32, addr net.Addr) error {
	ack := packet.Packet{
		Headers: packet.PacketHeader{
			Ack:      true,
			Sequence: sequence,
		},
	}
	ackBin, err := ack.Marshal()
	if err != nil {
		return err
	}

	_, err = rdt.udt.UdtSend(ackBin, addr)
	return err
}

func (rdt *SelectiveRepeatUdpRdt) Timeout(sequence uint32) {
	rdt.sendLock.Lock()
	defer rdt.sendLock.Unlock()
	go rdt.sendBuffer[sequence%rdt.sendMaxBuf].StartTimer(rdt.timeout, rdt.Timeout)

	// TODO store the binPkt instead of packet.Packet in packetWrapper
	pktWrapper := rdt.sendBuffer[sequence%rdt.sendMaxBuf]
	binPkt, err := pktWrapper.pkt.Marshal()
	if err != nil {
		fmt.Println("rdt.go: something went wrong with packet retransmition")
	}

	rdt.udt.UdtSend(binPkt, pktWrapper.destAddr)
}

func (pktWrapper *packetWrapper) StartTimer(duration time.Duration, onTimeout func(uint32)) {
	// 	go func() {
	// 		// Reset the timer first
	// 		pktWrapper.StopTimer()
	// 		pktWrapper.timer.Reset(duration)

	// 		<-pktWrapper.timer.C
	// 		onTimeout(pktWrapper.pkt.Headers.Sequence)
	// 	}()
}

func (pktWrapper *packetWrapper) StopTimer() {
	// if !pktWrapper.timer.Stop() {
	// 	<-pktWrapper.timer.C
	// }
}

var logOnce = sync.Once{}
var fd = os.Stdout

func printf(format string, args ...interface{}) {
	// logOnce.Do(func() {
	// 	wd, err := os.Getwd()
	// 	if err != nil {
	// 		fmt.Println("rdt.go: failed to get working directory")
	// 		return
	// 	}
	// 	fd, err = os.Create(path.Join(wd, "logs.log"))
	// 	if err != nil {
	// 		fmt.Println("rdt.go: failed to get working directory")
	// 		return
	// 	}
	// })
	fmt.Fprintf(fd, format, args...)
}