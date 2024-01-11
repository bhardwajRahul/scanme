package scanme

import (
	"fmt"
	"log"
	"net"

	"github.com/CyberRoute/scanme/utils"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/google/gopacket/routing"
)

// scanner handles scanning a single IP address.
type scanner struct {
	// iface is the interface to send packets on.
	iface *net.Interface
	// destination, gateway (if applicable), and source IP addresses to use.
	dst, gw, src net.IP

	handle *pcap.Handle

	// opts and buf allow us to easily serialize packets in the send()
	// method.
	opts gopacket.SerializeOptions
	buf  gopacket.SerializeBuffer
}

// newScanner creates a new scanner for a given destination IP address, using
// router to determine how to route packets to that IP.
func NewScanner(ip net.IP, router routing.Router) (*scanner, error) {
	s := &scanner{
		dst: ip,
		opts: gopacket.SerializeOptions{
			FixLengths:       true,
			ComputeChecksums: true,
		},
		buf: gopacket.NewSerializeBuffer(),
	}

	iface, gw, src, err := router.Route(ip)
	if err != nil {
		return nil, err
	}

	log.Printf("scanning ip %v with interface %v, gateway %v, src %v", ip, iface.Name, gw, src)
	s.gw, s.src, s.iface = gw, src, iface

	handle, err := pcap.OpenLive(iface.Name, 65535, true, pcap.BlockForever)
	if err != nil {
		return nil, fmt.Errorf("error opening pcap handle: %v", err)
	}
	s.handle = handle

	return s, nil
}

// Closes the pcap handle
func (s *scanner) Close() {
	if s.handle != nil {
		s.handle.Close()
	}
}

// send sends the given layers as a single packet on the network.
func (s *scanner) send(l ...gopacket.SerializableLayer) error {
	if err := gopacket.SerializeLayers(s.buf, s.opts, l...); err != nil {
		return err
	}
	return s.handle.WritePacketData(s.buf.Bytes())
}

func (s *scanner) sendARPRequest() (net.HardwareAddr, error) {
	arpDst := s.dst
	if s.gw != nil {
		arpDst = s.gw
	}
	handle, err := pcap.OpenLive(s.iface.Name, 65536, true, pcap.BlockForever)
	if err != nil {
		return nil, err
	}

	// Set a BPF filter to capture only ARP replies destined for our source IP
	bpfFilter := fmt.Sprintf("arp and ether dst %s", s.iface.HardwareAddr)
	if err := handle.SetBPFFilter(bpfFilter); err != nil {
		return nil, err
	}

	defer handle.Close()
	// Prepare the layers to send for an ARP request.
	eth := layers.Ethernet{
		SrcMAC:       s.iface.HardwareAddr,
		DstMAC:       net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		EthernetType: layers.EthernetTypeARP,
	}
	arp := layers.ARP{
		AddrType:          layers.LinkTypeEthernet,
		Protocol:          layers.EthernetTypeIPv4,
		HwAddressSize:     6,
		ProtAddressSize:   4,
		Operation:         layers.ARPRequest,
		SourceHwAddress:   []byte(s.iface.HardwareAddr),
		SourceProtAddress: []byte(s.src),
		DstHwAddress:      []byte{0, 0, 0, 0, 0, 0},
		DstProtAddress:    []byte(arpDst),
	}
	
	// Send a single ARP request packet (we never retry a send, since this
	// SerializeLayers clears the given write buffer, then writes all layers
	// into it so they correctly wrap each other. Note that by clearing the buffer,
	// it invalidates all slices previously returned by w.Bytes()

    if err := s.send(&eth, &arp); err != nil {
		return nil, err
	}
	for {
		data, _, err := handle.ReadPacketData()
		if err == pcap.NextErrorTimeoutExpired {
			continue
		} else if err != nil {
			return net.HardwareAddr{}, err
		}

		parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &arp)
		decoded := []gopacket.LayerType{}
		if err := parser.DecodeLayers(data, &decoded); err != nil {
			//fmt.Println(err) Errors here are due to the decoder not all layers are implemented
		}

		for _, layerType := range decoded {
			switch layerType {
			case layers.LayerTypeEthernet:
				if net.IP(arp.SourceProtAddress).Equal(net.IP(arpDst)) {
					return net.HardwareAddr(arp.SourceHwAddress), nil
				}
			}
		}
	}
}

func getFreeTCPPort() (layers.TCPPort, error) {
	// Use the library or function that returns a free TCP port as an int.
	tcpport, err := utils.GetFreeTCPPort()
	if err != nil {
		return 0, err
	}
	return layers.TCPPort(tcpport), nil
}

func (s *scanner) sendICMPEchoRequest() error {
	mac, err := s.sendARPRequest()
	if err != nil {
		return err
	}
	eth := layers.Ethernet{
		SrcMAC:      s.iface.HardwareAddr, // Replace with your source MAC address
		DstMAC:      mac, // Broadcast MAC for ICMP
		EthernetType: layers.EthernetTypeIPv4,
	}

	// Prepare IP layer
	ip4 := layers.IPv4{
		SrcIP:    s.src,
		DstIP:    s.dst,
		Version:  4,
		TTL:      64,
		Protocol: layers.IPProtocolICMPv4,
	}

	// Prepare ICMP layer for Echo Request
	icmp := layers.ICMPv4{
		TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoRequest, 0),
		Id:       1, // You can set any ID
		Seq:      1, // You can set any sequence number
	}
	if err := s.send(&eth, &ip4, &icmp); err != nil {
		log.Printf("error %v sending ping", err)
	}
	return nil
}

func (s *scanner) Synscan() (map[layers.TCPPort]string, error) {
	openPorts := make(map[layers.TCPPort]string)

	mac, err := s.sendARPRequest()
	if err != nil {
		return nil, err
	}

	tcpport, err := getFreeTCPPort()
	if err != nil {
		return nil, err
	}

	eth := layers.Ethernet{
		SrcMAC:       s.iface.HardwareAddr,
		DstMAC:       mac,
		EthernetType: layers.EthernetTypeIPv4,
	}
	ip4 := layers.IPv4{
		SrcIP:    s.src,
		DstIP:    s.dst,
		Version:  4,
		TTL:      64,
		Protocol: layers.IPProtocolTCP,
	}
	tcp := layers.TCP{
		SrcPort: tcpport,
		DstPort: 0, // will be incremented during the scan
		SYN:     true,
	}

	tcp.SetNetworkLayerForChecksum(&ip4)

	ipFlow := gopacket.NewFlow(layers.EndpointIPv4, s.dst, s.src)

	handle, err := pcap.OpenLive(s.iface.Name, 65535, true, pcap.BlockForever)
	if err != nil {
		return nil, err
	}
	// tcp[13] & 0x02 != 0 checks for SYN flag.
    // tcp[13] & 0x10 != 0 checks for ACK flag.
    // tcp[13] & 0x04 != 0 checks for RST flag.
	// this rule should decrease the number of packets captured, still experimenting with this :D
	bpfFilter := "icmp or (tcp and (tcp[13] & 0x02 != 0 or tcp[13] & 0x10 != 0 or tcp[13] & 0x04 != 0))"

	err = handle.SetBPFFilter(bpfFilter)
	if err != nil {
		return nil, err
	}

	defer handle.Close()

	

	s.sendICMPEchoRequest()

	for {
		// Send one packet per loop iteration until we've sent packets
		// to all of ports [1, 65535].

		if tcp.DstPort < 65535 {
			tcp.DstPort++
			if err := s.send(&eth, &ip4, &tcp); err != nil {
				log.Printf("error sending to port %v: %v", tcp.DstPort, err)
			}
		} else if tcp.DstPort == 65535 {
					log.Printf("last port scanned for %v dst port %s assuming we've seen all we can", s.dst, tcp.DstPort)
					return openPorts, nil
				}
			
		eth := &layers.Ethernet{}
		ip4 := &layers.IPv4{}
		tcp := &layers.TCP{}
		icmp := &layers.ICMPv4{}

		parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, eth, ip4, tcp, icmp)
		decodedLayers := make([]gopacket.LayerType, 0, 4)

		// Read in the next packet.
		data, _, err := handle.ReadPacketData()
		if err == pcap.NextErrorTimeoutExpired {
			continue
		} else if err != nil { 
			log.Printf("error reading packet: %v", err)
			continue
		}
		// Parse the packet. Using DecodingLayerParser to be really fast
		if err := parser.DecodeLayers(data, &decodedLayers); err != nil {
			//fmt.Println("Error", err)
			continue
		}
		for _, typ := range decodedLayers {
			switch typ {

			case layers.LayerTypeEthernet:
			 	//fmt.Println("    Eth ", eth.SrcMAC, eth.DstMAC)
			 	continue
			case layers.LayerTypeIPv4:
				//fmt.Println("    IP4 ", ip4.SrcIP, ip4.DstIP)
				if ip4.NetworkFlow() != ipFlow {
					continue
				}
			case layers.LayerTypeTCP:
				//fmt.Println("    TCP ", tcp.SrcPort, tcp.DstPort)
				if tcp.DstPort != tcpport {
					continue
				
				} else if tcp.RST {
					log.Printf("  port %v closed", tcp.SrcPort)
					continue
				} else if tcp.SYN && tcp.ACK  {
					openPorts[(tcp.SrcPort)] = "open"
					log.Printf("  port %v open", tcp.SrcPort)
					continue
				}
			case layers.LayerTypeICMPv4:
	
				switch icmp.TypeCode.Type() {
				case layers.ICMPv4TypeEchoReply:
					log.Printf("ICMP Echo Reply received from %v", ip4.SrcIP)
					// Handle ICMP Echo Reply
				case layers.ICMPv4TypeDestinationUnreachable:
					log.Printf(" port %v filtered", tcp.SrcPort)
					// Handle ICMP Destination Unreachable
				}
			}
		}
	}
}
