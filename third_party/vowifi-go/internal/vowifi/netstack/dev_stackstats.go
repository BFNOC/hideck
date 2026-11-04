package netstack

// [dev] debug-field: the gVisor stack counters that show packets dropped or
// rejected inside the netstack, between the 3GPP IPsec transform and the
// IMS sockets.

func devStackStats(n *Network) map[string]uint64 {
	if n == nil || n.stack == nil {
		return nil
	}
	stats := n.stack.Stats()
	return map[string]uint64{
		"dropped_packets":           stats.DroppedPackets.Value(),
		"ip_malformed":              stats.IP.MalformedPacketsReceived.Value(),
		"ip_invalid_destination":    stats.IP.InvalidDestinationAddressesReceived.Value(),
		"ip_packets_delivered":      stats.IP.PacketsDelivered.Value(),
		"tcp_invalid_segments":      stats.TCP.InvalidSegmentsReceived.Value(),
		"tcp_checksum_errors":       stats.TCP.ChecksumErrors.Value(),
		"tcp_resets_sent":           stats.TCP.ResetsSent.Value(),
		"tcp_resets_received":       stats.TCP.ResetsReceived.Value(),
		"tcp_retransmits":           stats.TCP.Retransmits.Value(),
		"tcp_timeouts":              stats.TCP.Timeouts.Value(),
		"tcp_segments_received":     stats.TCP.ValidSegmentsReceived.Value(),
		"tcp_segments_sent":         stats.TCP.SegmentsSent.Value(),
		"udp_receive_buffer_errors": stats.UDP.ReceiveBufferErrors.Value(),
		"udp_malformed":             stats.UDP.MalformedPacketsReceived.Value(),
		"udp_unknown_port":          stats.UDP.UnknownPortErrors.Value(),
	}
}
